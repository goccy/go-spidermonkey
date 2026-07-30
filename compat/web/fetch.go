package web

// fetch: everything lives on the Go side, wired into the guest through the
// public object API (the shape proven by the repo's fetch feasibility test).
// fetch itself is a Go closure assigned to globalThis.fetch; the Response is
// a plain guest object whose fields are Set from Go and whose methods are Go
// closures; the body is a ReadableStream constructed around an
// underlyingSource whose pull/cancel are Go closures over the http.Response
// body, so chunked responses arrive incrementally over the bytes bridge.
//
// The HTTP client enforces the interpreter's Config permissions: Resolve per
// hostname lookup, Dial per outbound connection (per resolved address).
//
// Redirects: init.redirect selects "follow" (default; hops are counted so the
// Response reports redirected/url correctly and Go strips Authorization/Cookie
// on a cross-origin hop), "manual" (the 3xx is returned as-is with Location
// visible), or "error" (a redirect rejects with a TypeError). init.signal is
// tied to the in-flight request's context: an abort cancels it mid-flight and
// rejects with a DOMException AbortError. Response headers are shipped as data
// and rebuilt into a real Headers instance on the JS side.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/internal/eventloop"
)

// fetchAPI owns the Go side of fetch: the permission-enforcing HTTP client
// (built lazily from the Config the first call carries), the guest builtins
// it keeps resolved, and the set of response bodies still open.
type fetchAPI struct {
	js           *spidermonkey.JS
	loop         *eventloop.Loop
	promiseCls   *spidermonkey.Object // Promise constructor
	jsonObj      *spidermonkey.Object // JSON namespace object
	streamCls    *spidermonkey.Object // ReadableStream class (from builtins.js)
	deferredFn   *spidermonkey.Object // __web_deferred(): {promise, resolve, reject}
	typeErrorCls *spidermonkey.Object // TypeError constructor (redirect:"error" rejection)

	// roots, when set, are trusted in ADDITION to the system pool. It is how an
	// embedding reaches a server whose certificate is not publicly signed — a
	// private CA, or a test server that mints its own.
	roots      *x509.CertPool
	clientOnce sync.Once
	client     *http.Client

	mu     sync.Mutex
	open   map[*fetchStream]struct{}
	calls  map[*asyncCall]struct{}       // in-flight async fetch operations
	aborts map[string]context.CancelFunc // abortId -> cancel for the in-flight request
}

// errFetchRedirect / errTooManyRedirects are the CheckRedirect sentinels: the
// first surfaces redirect:"error", the second caps redirect:"follow" chains.
var (
	errFetchRedirect    = errors.New("fetch: redirect mode is \"error\" and the server returned a redirect")
	errTooManyRedirects = errors.New("fetch: too many redirects")
	// errOnlyIfCached is what cache mode "only-if-cached" reports when nothing is
	// stored: the standard makes that a network error, not an empty response.
	errOnlyIfCached = errors.New("fetch: cache mode is \"only-if-cached\" and there is no stored response")
)

// asyncCall tracks one in-flight async fetch op so a pooled instance (cfworkers)
// can cancel it at the request boundary — otherwise a fire-and-forget fetch's
// late loop.Post would run against the NEXT request, corrupting its pending
// accounting (and its connection would leak).
type asyncCall struct {
	cancel    context.CancelFunc
	cancelled bool
}

// deferred is an externally-settled guest promise (the {promise, resolve,
// reject} triple from __web_deferred). Used to make a blocking op — the HTTP
// round-trip, a body read — resolve ASYNCHRONOUSLY so it never wedges the single
// loop goroutine (a fetch to a same-instance server would otherwise deadlock).
type deferred struct {
	promise, resolve, reject *spidermonkey.Object
}

func (d *deferred) free() {
	// Only the resolve/reject function handles are released here. d.promise's
	// handle IS the value returned to the guest from fetch(); freeing it from Go
	// can unroot an object the engine's pending .then reactions still touch (a
	// use-after-free crash), so its handle is deliberately not released here.
	d.resolve.Free()
	d.reject.Free()
}

func (a *fetchAPI) newDeferred() (*deferred, error) {
	v, err := a.deferredFn.Call()
	if err != nil {
		return nil, err
	}
	o := v.Object()
	if o == nil {
		return nil, fmt.Errorf("fetch: __web_deferred returned non-object")
	}
	defer o.Free()
	get := func(name string) (*spidermonkey.Object, error) {
		fv, err := o.Get(name)
		if err != nil {
			return nil, err
		}
		fo := fv.Object()
		if fo == nil {
			return nil, fmt.Errorf("fetch: deferred.%s missing", name)
		}
		return fo, nil
	}
	promise, err := get("promise")
	if err != nil {
		return nil, err
	}
	resolve, err := get("resolve")
	if err != nil {
		promise.Free()
		return nil, err
	}
	reject, err := get("reject")
	if err != nil {
		promise.Free()
		resolve.Free()
		return nil, err
	}
	return &deferred{promise: promise, resolve: resolve, reject: reject}, nil
}

// asyncPromise returns a pending promise immediately and runs `work` OFF the
// loop with a cancellable ctx. `work` returns a settle closure that runs ON the
// loop to build the resolve/reject value (guest objects may only be built
// there): settle(cancelled) returns (value, isReject) and, when cancelled==true,
// only performs cleanup (close the body) — the loop was reset out from under it,
// so its pending was already discarded and it must NOT DonePending or resolve.
func (a *fetchAPI) asyncPromise(work func(ctx context.Context, cancel context.CancelFunc) func(cancelled bool) (spidermonkey.Value, bool)) (spidermonkey.Value, error) {
	ctx, cancel := context.WithCancel(context.Background())
	return a.asyncPromiseCtx(ctx, cancel, work)
}

// asyncPromiseCtx is asyncPromise with a caller-provided ctx/cancel — fetchFunc
// creates them up front (on the loop) so it can register `cancel` under the
// AbortSignal's id BEFORE the guest can wire its abort listener, closing the
// race where an abort fires before the goroutine has started the request.
func (a *fetchAPI) asyncPromiseCtx(ctx context.Context, cancel context.CancelFunc, work func(ctx context.Context, cancel context.CancelFunc) func(cancelled bool) (spidermonkey.Value, bool)) (spidermonkey.Value, error) {
	d, err := a.newDeferred()
	if err != nil {
		return nil, err
	}
	call := &asyncCall{cancel: cancel}
	a.mu.Lock()
	a.calls[call] = struct{}{}
	a.mu.Unlock()
	a.loop.AddPending("fetch")
	go func() {
		// NOTE: asyncPromise does NOT cancel `cancel` itself — for a round-trip the
		// response body is tied to ctx and is read LATER, so cancelling here would
		// break the body read ("context canceled"). `work` owns cancel: the
		// round-trip hands it to the stream (finish() cancels), a consumer cancels
		// after it has read. cancelInflight also calls it (idempotent) to abort.
		settle := work(ctx, cancel)
		a.loop.Post(func() error {
			a.mu.Lock()
			cancelled := call.cancelled
			delete(a.calls, call)
			a.mu.Unlock()
			// Free resolve/reject now; free the promise handle on the NEXT turn,
			// after this turn's microtasks (the .then reactions) drain — freeing it
			// now would unroot an object those reactions still touch (a UAF crash).
			defer func() {
				d.free()
				p := d.promise
				a.loop.Post(func() error { p.Free(); return nil })
			}()
			val, isReject := settle(cancelled)
			if cancelled {
				// The instance was reset for reuse (pending already zeroed); do not
				// touch the loop's accounting or settle a promise nobody awaits.
				return nil
			}
			defer a.loop.DonePending("fetch")
			target := d.resolve
			if isReject {
				target = d.reject
			}
			_, _ = target.Call(val)
			if o, ok := val.(*spidermonkey.Object); ok {
				o.Free()
			}
			return nil
		})
	}()
	return d.promise, nil
}

// cancelInflight cancels and abandons every in-flight async fetch. Called at the
// pooled-request boundary (ResetPerRequest) so no goroutine's Post survives into
// the next request. Their Post closures see call.cancelled and no-op.
func (a *fetchAPI) cancelInflight() {
	a.mu.Lock()
	calls := make([]*asyncCall, 0, len(a.calls))
	for c := range a.calls {
		c.cancelled = true
		calls = append(calls, c)
	}
	a.mu.Unlock()
	for _, c := range calls {
		c.cancel()
	}
}

// inflightCount reports how many async fetch ops have not yet run their Post.
func (a *fetchAPI) inflightCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

// A response body has exactly one consumer: the streaming ReadableStream (pull,
// on the loop) OR the buffered readAll (off the loop). Claiming enforces that so
// the two never touch st.body concurrently (a data race + concurrent Read on the
// same http.Body).
const (
	consumerNone = iota
	consumerStream
	consumerBuffered
)

// fetchStream is one response body: the io.Reader the Go-provided pull/cancel
// closures drive.
type fetchStream struct {
	api       *fetchAPI
	body      io.ReadCloser
	cancelCtx context.CancelFunc // cancels the request ctx once the body is done
	buf       []byte
	mu        sync.Mutex // guards done/claimed/reading
	done      bool
	claimed   int  // consumerNone | consumerStream | consumerBuffered
	reading   bool // a buffered readAll is in progress (blocks a second one)
}

func installFetch(js *spidermonkey.JS, loop *eventloop.Loop, roots *x509.CertPool) (*fetchAPI, error) {
	a := &fetchAPI{js: js, loop: loop, roots: roots, open: map[*fetchStream]struct{}{}, calls: map[*asyncCall]struct{}{}, aborts: map[string]context.CancelFunc{}}
	// A deferred-promise factory so Go can settle a promise asynchronously.
	if r, err := js.Eval(context.Background(),
		`(globalThis.__web_deferred = () => { let resolve, reject; const promise = new Promise((res, rej) => { resolve = res; reject = rej; }); return { promise, resolve, reject }; }), 0`); err != nil {
		return nil, fmt.Errorf("install __web_deferred: %w", err)
	} else if r.Error != nil {
		return nil, fmt.Errorf("install __web_deferred threw: %w", r.Error)
	}
	for name, dst := range map[string]**spidermonkey.Object{
		"Promise":        &a.promiseCls,
		"JSON":           &a.jsonObj,
		"ReadableStream": &a.streamCls,
		"__web_deferred": &a.deferredFn,
		"TypeError":      &a.typeErrorCls,
	} {
		v, err := js.Global().Get(name)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", name, err)
		}
		o := v.Object()
		if o == nil {
			return nil, fmt.Errorf("resolve %s: not an object", name)
		}
		*dst = o
	}
	// The JS builtins wrap this as globalThis.fetch, normalizing headers
	// (Headers instances / non-string values) and body (URLSearchParams /
	// FormData / Blob) before the host boundary — this native entry only accepts
	// a URL string, a plain {string:string} headers object, and a Uint8Array body.
	if err := js.Global().DefineFunc("__native_fetch", a.fetchFunc); err != nil {
		return nil, err
	}
	// __native_fetch_abort(id): the JS fetch wrapper's AbortSignal listener calls
	// this to cancel the Go request context of an in-flight fetch.
	if err := js.Global().DefineFunc("__native_fetch_abort", a.fetchAbort); err != nil {
		return nil, err
	}
	return a, nil
}

// fetchAbort cancels the in-flight request registered under id (from the JS
// AbortSignal's 'abort' listener). It runs on the loop goroutine; cancelling the
// context makes the off-loop client.Do return promptly with a cancel error.
func (a *fetchAPI) fetchAbort(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	id := args[0].String()
	a.mu.Lock()
	cancel := a.aborts[id]
	delete(a.aborts, id)
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return spidermonkey.Undefined(), nil
}

// newHTTPClient builds the transport that enforces Config.Resolve (per
// hostname) and Config.Dial (per resolved address, so a DNS answer cannot
// smuggle a connection past the allow-list).
func newHTTPClient(cfg spidermonkey.Config, roots *x509.CertPool) *http.Client {
	dial := permissionDial(cfg)
	// The transport is wrapped so that a request whose header values net/http
	// refuses to write is still sent — over the same permission-checked dial.
	// See fetchraw.go.
	std := &http.Transport{DialContext: dial, TLSClientConfig: tlsConfig(roots)}
	// The cache wraps the transport rather than the client, so it also sees the
	// hops of a redirect chain — each of which is a request of its own.
	inner := &permissiveTransport{std: std, dial: dial}
	return &http.Client{Transport: &cachingTransport{next: inner, cache: newResponseCache()}}
}

// permissionDial is the dialer every outbound connection this package makes goes
// through, whatever protocol is layered on it: Config.Resolve decides whether a
// name may be looked up, and Config.Dial decides whether each RESOLVED address
// may be connected to — so a DNS answer cannot smuggle a connection past the
// allow-list.
// tlsConfig trusts roots IN ADDITION to the system pool, or returns nil to use
// the system pool alone. A nil *tls.Config is what net/http wants when there is
// nothing to add — replacing it with an empty one would still be correct, but
// this way the default path is visibly the default.
func tlsConfig(roots *x509.CertPool) *tls.Config {
	if roots == nil {
		return nil
	}
	return &tls.Config{RootCAs: roots}
}

func permissionDial(cfg spidermonkey.Config) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		port, _ := strconv.Atoi(portStr)
		if ip := net.ParseIP(host); ip != nil {
			// Literal IP: no name was resolved, so host is "".
			if cfg.Dial == nil || !cfg.Dial(network, "", ip.String(), port) {
				return nil, fmt.Errorf("dial %s: permission denied", addr)
			}
			return dialer.DialContext(ctx, network, addr)
		}
		if cfg.Resolve == nil || !cfg.Resolve(host) {
			return nil, fmt.Errorf("resolve %q: permission denied", host)
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			if cfg.Dial == nil || !cfg.Dial(network, host, ip.String(), port) {
				lastErr = fmt.Errorf("dial %s (%s): permission denied", addr, ip)
				continue
			}
			conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), portStr))
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("dial %s: no addresses", addr)
		}
		return nil, lastErr
	}
}

// dropCache empties the HTTP cache. A pooled instance calls it between requests
// so one request cannot observe what another fetched.
func (a *fetchAPI) dropCache() {
	if a.client == nil {
		return
	}
	if ct, ok := a.client.Transport.(*cachingTransport); ok {
		ct.cache.reset()
	}
}

// checkRequestPermission applies Config.Resolve/Dial to a request's target
// host before it is sent.
func checkRequestPermission(cfg spidermonkey.Config, req *http.Request) error {
	host := req.URL.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		port, _ := strconv.Atoi(req.URL.Port())
		if port == 0 {
			if req.URL.Scheme == "https" {
				port = 443
			} else {
				port = 80
			}
		}
		if cfg.Dial == nil || !cfg.Dial("tcp", "", ip.String(), port) {
			return fmt.Errorf("dial %s:%d: permission denied", host, port)
		}
		return nil
	}
	if cfg.Resolve == nil || !cfg.Resolve(host) {
		return fmt.Errorf("resolve %q: permission denied", host)
	}
	return nil
}

// sameOrigin reports whether two URLs share an origin (scheme + hostname +
// effective port), the unit fetch uses to decide cross-origin header stripping.
// fetchUserAgent is what this runtime calls itself on the wire.
const fetchUserAgent = "go-spidermonkey"

func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && a.Hostname() == b.Hostname() && originPort(a) == originPort(b)
}

func originPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// promise wraps v via the guest's own Promise[method] (resolve | reject).
func (a *fetchAPI) promise(method string, v spidermonkey.Value) (spidermonkey.Value, error) {
	return a.promiseCls.CallMethod(method, v)
}

// fetchFunc is globalThis.fetch: (input, init?) => Promise<Response>. A
// transport failure resolves to a REJECTED promise (fetch semantics), not a
// throw.
func (a *fetchAPI) fetchFunc(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	a.clientOnce.Do(func() { a.client = newHTTPClient(cfg, a.roots) })
	if len(args) < 1 {
		return nil, fmt.Errorf("fetch: an input URL is required")
	}
	url := args[0].String()
	method := "GET"
	redirectMode := "follow"
	cacheModeName := "default"
	abortID := ""
	var reqBody io.Reader
	headers := map[string]string{}

	if len(args) > 1 && args[1].IsObject() {
		init := args[1].Object()
		defer init.Free()
		// scalar reads a primitive init field, freeing the value's root if the
		// guest passed an object (which would otherwise pin it for the
		// interpreter's life — reachable even when the fetch is later denied).
		scalar := func(name string) (spidermonkey.Value, bool) {
			v, err := init.Get(name)
			if err != nil || v == nil || v.IsUndefined() {
				return nil, false
			}
			if o := v.Object(); o != nil {
				o.Free()
				return nil, false
			}
			return v, true
		}
		if v, ok := scalar("method"); ok {
			method = strings.ToUpper(v.String())
		}
		if v, ok := scalar("redirect"); ok {
			redirectMode = v.String()
		}
		if v, ok := scalar("cache"); ok {
			cacheModeName = v.String()
		}
		// The guest wraps init.signal as a string id (see the JS fetch wrapper) so
		// its 'abort' listener can reach __native_fetch_abort. Pre-aborted signals
		// are rejected guest-side before the host is ever called.
		if v, ok := scalar("__abortId"); ok {
			abortID = v.String()
		}
		if v, err := init.Get("body"); err == nil {
			if o := v.Object(); o != nil {
				data, berr := o.Bytes() // Uint8Array/ArrayBuffer body: guest -> host binary
				o.Free()
				if berr != nil {
					return nil, berr
				}
				reqBody = bytes.NewReader(data)
			} else if v.Export() != nil { // string body; null/undefined mean none
				reqBody = strings.NewReader(v.String())
			}
		}
		if v, err := init.Get("headers"); err == nil {
			if o := v.Object(); o != nil {
				// Plain-object headers carry data, not identity: serialize
				// with the guest's own JSON builtin and decode host-side.
				s, serr := a.jsonObj.CallMethod("stringify", o)
				o.Free()
				if serr != nil {
					return nil, serr
				}
				if err := json.Unmarshal([]byte(s.String()), &headers); err != nil {
					return nil, fmt.Errorf("fetch: bad headers: %w", err)
				}
			}
		}
	}

	// A data: URL carries its own body: there is nothing to request, nothing to
	// resolve and nothing to authorize, so it resolves here rather than going
	// near the transport.
	if strings.HasPrefix(strings.ToLower(url), "data:") {
		resp, derr := dataResponse(url, method)
		if derr != nil {
			if te, terr := a.typeErrorCls.New(spidermonkey.ValueOf(derr.Error())); terr == nil {
				return a.promise("reject", te)
			}
			return a.promise("reject", spidermonkey.ValueOf(derr.Error()))
		}
		respObj, oerr := a.newResponse(resp, false, func() {})
		if oerr != nil {
			return a.promise("reject", spidermonkey.ValueOf(oerr.Error()))
		}
		return a.promise("resolve", respObj)
	}

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return a.promise("reject", spidermonkey.ValueOf(err.Error()))
	}
	// Per-request permission check. The transport's DialContext enforces the
	// same hooks per connection, but a pooled keep-alive connection skips the
	// dial — this check keeps every request under the policy regardless.
	if err := checkRequestPermission(cfg, req); err != nil {
		return a.promise("reject", spidermonkey.ValueOf(err.Error()))
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// Identify the runtime. Go's transport otherwise sends "Go-http-client/1.1",
	// which names the transport rather than the platform, and a fetch with no
	// User-Agent at all is something no user agent does.
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", fetchUserAgent)
	}
	// A per-request client selects the redirect policy without disturbing the
	// shared transport (its connection pool is reused). Go's redirect handling
	// already strips Authorization/Cookie on a cross-origin hop, so "follow" only
	// needs to count hops (for Response.redirected) and cap the chain.
	hops := 0
	reqClient := &http.Client{Transport: a.client.Transport}
	switch redirectMode {
	case "manual":
		reqClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	case "error":
		reqClient.CheckRedirect = func(*http.Request, []*http.Request) error { return errFetchRedirect }
	default: // "follow" and any unrecognized value
		reqClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			hops = len(via)
			if hops >= 10 {
				return errTooManyRedirects
			}
			// Strip sensitive headers on a cross-origin hop (origin = scheme+host+
			// port), matching undici/fetch. Go's own redirect stripping keys on the
			// hostname alone, so it would leak Authorization/Cookie across ports on
			// the same host — this closes that gap.
			if n := len(via); n > 0 && !sameOrigin(via[n-1].URL, req.URL) {
				req.Header.Del("Authorization")
				req.Header.Del("Cookie")
			}
			return nil
		}
	}
	// ctx/cancel are created here (on the loop) so the AbortSignal id can be
	// registered before the guest wires its abort listener — no lost-abort race.
	ctx, cancel := context.WithCancel(withCacheMode(context.Background(), cacheModeName))
	if abortID != "" {
		a.mu.Lock()
		a.aborts[abortID] = cancel
		a.mu.Unlock()
	}
	// Run the round-trip OFF the loop and resolve a pending promise on the loop.
	// Doing client.Do() inline would block the single loop goroutine for the whole
	// round-trip — and DEADLOCK if the target is a server on this same instance
	// (its request could never be dispatched). Build the Response on the loop.
	return a.asyncPromiseCtx(ctx, cancel, func(ctx context.Context, cancel context.CancelFunc) func(bool) (spidermonkey.Value, bool) {
		resp, derr := reqClient.Do(req.WithContext(ctx))
		return func(cancelled bool) (spidermonkey.Value, bool) {
			if abortID != "" {
				a.mu.Lock()
				delete(a.aborts, abortID)
				a.mu.Unlock()
			}
			if cancelled || derr != nil {
				cancel() // no body will be read; release ctx now
				if resp != nil {
					resp.Body.Close()
				}
			}
			if cancelled {
				return nil, false
			}
			if derr != nil {
				if errors.Is(derr, errFetchRedirect) {
					if te, terr := a.typeErrorCls.New(spidermonkey.ValueOf("Fetch redirect mode is \"error\" but a redirect was returned")); terr == nil {
						return te, true
					}
				}
				return spidermonkey.ValueOf(derr.Error()), true
			}
			// The body outlives the round-trip; the stream owns `cancel` and calls
			// it in finish() once the body is consumed/closed, so ctx stays valid
			// while the body is read.
			respObj, oerr := a.newResponse(resp, hops > 0, cancel)
			if oerr != nil {
				cancel()
				resp.Body.Close()
				return spidermonkey.ValueOf(oerr.Error()), true
			}
			return respObj, false
		}
	})
}

// newResponse builds the Response as a guest object composed entirely from
// Go: data fields via Set, behavior via Go closures over the http.Response,
// and the body via `new ReadableStream(source)` where source.pull/cancel are
// Go closures.
func (a *fetchAPI) newResponse(resp *http.Response, redirected bool, cancel context.CancelFunc) (*spidermonkey.Object, error) {
	js := a.js
	r, err := js.NewObject()
	if err != nil {
		return nil, err
	}
	fields := map[string]spidermonkey.Value{
		"url":        spidermonkey.ValueOf(resp.Request.URL.String()),
		"status":     spidermonkey.ValueOf(resp.StatusCode),
		"statusText": spidermonkey.ValueOf(http.StatusText(resp.StatusCode)),
		"ok":         spidermonkey.ValueOf(resp.StatusCode >= 200 && resp.StatusCode <= 299),
		"redirected": spidermonkey.ValueOf(redirected),
		"bodyUsed":   spidermonkey.ValueOf(false),
	}
	for name, v := range fields {
		if err := r.Set(name, v); err != nil {
			return nil, err
		}
	}

	// Headers as data: name/value pairs preserving every value (each Set-Cookie
	// kept separate). The JS fetch wrapper turns __headerEntries into a real
	// Headers instance, so forEach/set/append/delete/keys/values/iteration and
	// `new Headers(res.headers)` all work — a live Go object could not.
	pairs := []any{} // never nil, so it marshals as [] not null
	for name, vals := range resp.Header {
		for _, v := range vals {
			pairs = append(pairs, []any{name, v})
		}
	}
	if err := r.Set("__headerEntries", spidermonkey.ValueOf(pairs)); err != nil {
		return nil, err
	}

	// body: new ReadableStream({ pull, cancel }) with Go closures.
	st := &fetchStream{api: a, body: resp.Body, cancelCtx: cancel}
	a.mu.Lock()
	a.open[st] = struct{}{}
	a.mu.Unlock()

	source, err := js.NewObject()
	if err != nil {
		return nil, err
	}
	if err := source.DefineFunc("pull", st.pull); err != nil {
		return nil, err
	}
	if err := source.DefineFunc("cancel", st.cancel); err != nil {
		return nil, err
	}
	bodyV, err := a.streamCls.New(source)
	source.Free()
	if err != nil {
		return nil, err
	}
	if err := r.Set("body", bodyV); err != nil {
		return nil, err
	}
	bodyV.Object().Free()

	// Consumers: read the REST of the body to EOF host-side and wrap the
	// result. Each returns a Promise, built with the guest's own machinery.
	// Each consumer drains the body OFF the loop (up to 100 MiB) and resolves a
	// pending promise, so a slow/large body never blocks the loop goroutine.
	consumers := map[string]spidermonkey.Func{
		"bytes": func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
			return a.asyncPromise(func(ctx context.Context, cancel context.CancelFunc) func(bool) (spidermonkey.Value, bool) {
				data, rerr := st.readAll(consumerBuffered)
				cancel() // the buffered read is done; its ctx guards nothing further
				return func(cancelled bool) (spidermonkey.Value, bool) {
					if cancelled {
						return nil, false
					}
					if rerr != nil {
						return spidermonkey.ValueOf(rerr.Error()), true
					}
					u8, e := js.NewBytes(data)
					if e != nil {
						return spidermonkey.ValueOf(e.Error()), true
					}
					return u8, false
				}
			})
		},
		"arrayBuffer": func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
			return a.asyncPromise(func(ctx context.Context, cancel context.CancelFunc) func(bool) (spidermonkey.Value, bool) {
				data, rerr := st.readAll(consumerBuffered)
				cancel() // the buffered read is done; its ctx guards nothing further
				return func(cancelled bool) (spidermonkey.Value, bool) {
					if cancelled {
						return nil, false
					}
					if rerr != nil {
						return spidermonkey.ValueOf(rerr.Error()), true
					}
					u8, e := js.NewBytes(data)
					if e != nil {
						return spidermonkey.ValueOf(e.Error()), true
					}
					defer u8.Free()
					buf, e := u8.Get("buffer")
					if e != nil {
						return spidermonkey.ValueOf(e.Error()), true
					}
					return buf, false
				}
			})
		},
		"text": func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
			return a.asyncPromise(func(ctx context.Context, cancel context.CancelFunc) func(bool) (spidermonkey.Value, bool) {
				data, rerr := st.readAll(consumerBuffered)
				cancel() // the buffered read is done; its ctx guards nothing further
				return func(cancelled bool) (spidermonkey.Value, bool) {
					if cancelled {
						return nil, false
					}
					if rerr != nil {
						return spidermonkey.ValueOf(rerr.Error()), true
					}
					return spidermonkey.ValueOf(string(data)), false
				}
			})
		},
		"json": func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
			return a.asyncPromise(func(ctx context.Context, cancel context.CancelFunc) func(bool) (spidermonkey.Value, bool) {
				data, rerr := st.readAll(consumerBuffered)
				cancel() // the buffered read is done; its ctx guards nothing further
				return func(cancelled bool) (spidermonkey.Value, bool) {
					if cancelled {
						return nil, false
					}
					if rerr != nil {
						return spidermonkey.ValueOf(rerr.Error()), true
					}
					parsed, perr := a.jsonObj.CallMethod("parse", spidermonkey.ValueOf(string(data)))
					if perr != nil {
						return spidermonkey.ValueOf(perr.Error()), true
					}
					return parsed, false
				}
			})
		},
	}
	for name, fn := range consumers {
		if err := r.DefineFunc(name, fn); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// pull is the underlyingSource.pull the guest stream calls with its controller.
// It reads ONE chunk OFF the loop and returns a PENDING promise, then enqueues
// the chunk (or closes at EOF) on the loop. Reading on the loop (as it used to)
// blocks the single loop goroutine for the whole read — freezing every timer,
// other fetch, and http dispatch, and deadlocking a stream from a same-instance
// server whose backpressure ack is delivered through the loop.
func (st *fetchStream) pull(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 || args[0].Object() == nil {
		return nil, fmt.Errorf("pull: want the stream controller")
	}
	ctrl := args[0].Object()
	// Claim the body for the streaming consumer. If a buffered read already
	// claimed it (a .text()/.json() call), don't touch the body concurrently.
	st.mu.Lock()
	if st.done || st.claimed == consumerBuffered || st.reading {
		st.mu.Unlock()
		_, err := ctrl.CallMethod("close")
		ctrl.Free()
		return spidermonkey.Undefined(), err
	}
	st.claimed = consumerStream
	st.mu.Unlock()
	if st.buf == nil {
		st.buf = make([]byte, 32<<10)
	}
	// The ReadableStream serializes pulls (it awaits this promise before pulling
	// again), so st.buf has a single reader at a time.
	return st.api.asyncPromise(func(ctx context.Context, cancel context.CancelFunc) func(bool) (spidermonkey.Value, bool) {
		n, rerr := st.body.Read(st.buf)
		return func(cancelled bool) (spidermonkey.Value, bool) {
			cancel()          // the pull's own ctx guards nothing (body lifetime is st.cancelCtx)
			defer ctrl.Free() // drop the controller pin once we've used it
			if cancelled {
				return nil, false
			}
			if n > 0 {
				u8, uerr := st.api.js.NewBytes(st.buf[:n])
				if uerr != nil {
					return spidermonkey.ValueOf(uerr.Error()), true
				}
				_, cerr := ctrl.CallMethod("enqueue", u8)
				u8.Free()
				if cerr != nil {
					return spidermonkey.ValueOf(cerr.Error()), true
				}
				return spidermonkey.Undefined(), false
			}
			if rerr == io.EOF {
				st.finish()
				if _, cerr := ctrl.CallMethod("close"); cerr != nil {
					return spidermonkey.ValueOf(cerr.Error()), true
				}
				return spidermonkey.Undefined(), false
			}
			if rerr != nil {
				st.finish()
				return spidermonkey.ValueOf(rerr.Error()), true
			}
			return spidermonkey.ValueOf("response body read made no progress"), true
		}
	})
}

// cancel is the underlyingSource.cancel: the guest is done with the body.
func (st *fetchStream) cancel(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	st.finish()
	return spidermonkey.Undefined(), nil
}

// maxFetchBody caps a buffered fetch response body so an allow-listed but
// hostile/huge endpoint can't OOM the host via r.text()/json()/bytes().
const maxFetchBody = 100 << 20 // 100 MiB

// errBodyConsumed is the failure a second body read produces; the message
// matches the WHATWG/undici TypeError text so guest code can recognize it.
var errBodyConsumed = errors.New("Body has already been consumed.")

// readAll drains the remaining body to EOF (the bytes/text/json path). It runs
// OFF the loop, so it claims the body against a concurrent stream pull and a
// second buffered read. mode is consumerBuffered.
func (st *fetchStream) readAll(mode int) ([]byte, error) {
	st.mu.Lock()
	if st.done || st.reading {
		st.mu.Unlock()
		// A second consume must reject (TypeError-shaped), not silently resolve to
		// an empty body — otherwise a double `.text()`/`.json()` yields "" / a parse
		// error instead of the documented "already consumed" failure.
		return nil, errBodyConsumed
	}
	if st.claimed == consumerStream {
		st.mu.Unlock()
		return nil, errBodyConsumed
	}
	st.claimed = mode
	st.reading = true
	st.mu.Unlock()
	// The read runs WITHOUT the lock held (it can take a while); the `reading`
	// flag already excludes any other consumer, so there is no concurrent Read.
	data, err := io.ReadAll(io.LimitReader(st.body, maxFetchBody+1))
	st.finish()
	if err == nil && int64(len(data)) > maxFetchBody {
		return nil, fmt.Errorf("response body exceeds %d bytes", maxFetchBody)
	}
	return data, err
}

func (st *fetchStream) finish() {
	st.mu.Lock()
	if st.done {
		st.mu.Unlock()
		return
	}
	st.done = true
	st.mu.Unlock()
	st.body.Close()
	if st.cancelCtx != nil {
		st.cancelCtx() // release the request ctx now that the body is consumed
	}
	a := st.api
	a.mu.Lock()
	delete(a.open, st)
	a.mu.Unlock()
}

// openStreams reports how many response bodies are still open.
func (a *fetchAPI) openStreams() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.open)
}

// closeOpenStreams finishes every still-open response body (closing its TCP
// connection). Called between pooled requests so a handler that fetched but
// never consumed the body doesn't leak the connection/goroutine into the next
// request on the same instance.
func (a *fetchAPI) closeOpenStreams() {
	a.mu.Lock()
	streams := make([]*fetchStream, 0, len(a.open))
	for st := range a.open {
		streams = append(streams, st)
	}
	a.mu.Unlock()
	for _, st := range streams {
		st.finish()
	}
}

// closeAll releases the cached engine handles. Idempotent: Close is reachable
// more than once (an embedder that closes explicitly still has its deferred
// close run), and releasing a handle twice would delete the guest's GC root
// for it twice.
func (a *fetchAPI) closeAll() {
	a.closeOpenStreams()
	for _, o := range []**spidermonkey.Object{&a.promiseCls, &a.jsonObj, &a.streamCls, &a.deferredFn, &a.typeErrorCls} {
		if *o != nil {
			(*o).Free()
			*o = nil
		}
	}
}
