package web

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/andybalholm/brotli"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/internal/eventloop"
	"github.com/klauspost/compress/zstd"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

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

// fetchAPI owns the Go side of fetch: the permission-enforcing HTTP client
// (built lazily from the Config the first call carries), the guest builtins
// it keeps resolved, and the set of response bodies still open.
type fetchAPI struct {
	js             *spidermonkey.JS
	loop           *eventloop.Loop
	promiseCls     *spidermonkey.Object // Promise constructor
	jsonObj        *spidermonkey.Object // JSON namespace object
	streamCls      *spidermonkey.Object // ReadableStream class (from builtins.js)
	deferredFn     *spidermonkey.Object // __web_deferred(): {promise, resolve, reject}
	typeErrorCls   *spidermonkey.Object // TypeError constructor (redirect:"error" rejection)
	syntaxErrorCls *spidermonkey.Object // SyntaxError constructor (json() parse failures)

	// roots, when set, are trusted in ADDITION to the system pool. It is how an
	// embedding reaches a server whose certificate is not publicly signed — a
	// private CA, or a test server that mints its own.
	roots      *x509.CertPool
	clientOnce sync.Once
	client     *http.Client
	// jar is the cookie store a credentialed request reads and writes. The
	// guest resolves the credentials mode per hop; the host only needs to
	// know whether THIS request carries cookies.
	jar     http.CookieJar
	jarOnce sync.Once

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
		"SyntaxError":    &a.syntaxErrorCls,
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
	// __native_fetch_sync(url, init): the blocking round-trip synchronous
	// XMLHttpRequest is defined in terms of. See the synchronous-fetch section
	// below for what it costs.
	if err := js.Global().DefineFunc("__native_fetch_sync", a.fetchSync); err != nil {
		return nil, err
	}
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
// headerByFold reads a header whose stored key may not be canonical.
func headerByFold(h http.Header, name string) string {
	for k, vals := range h {
		if strings.EqualFold(k, name) && len(vals) > 0 {
			return vals[0]
		}
	}
	return ""
}

func delHeaderFold(h http.Header, name string) {
	for k := range h {
		if strings.EqualFold(k, name) {
			delete(h, k)
		}
	}
}

func (a *fetchAPI) cookieJar() http.CookieJar {
	a.jarOnce.Do(func() {
		jar, err := cookiejar.New(nil)
		if err == nil {
			a.jar = jar
		}
	})
	return a.jar
}

func newHTTPClient(cfg spidermonkey.Config, roots *x509.CertPool) *http.Client {
	dial := permissionDial(cfg)
	// The transport is wrapped so that a request whose header values net/http
	// refuses to write is still sent — over the same permission-checked dial.
	// See the raw-fetch section below.
	std := &http.Transport{DialContext: dial, TLSClientConfig: tlsConfig(roots)}
	// The cache wraps the transport rather than the client, so it also sees the
	// hops of a redirect chain — each of which is a request of its own.
	inner := &permissiveTransport{std: std, dial: dial}
	// Decoding sits ABOVE the cache, so a cache hit is decoded too and the cache
	// stores the bytes the origin actually sent. See the decode section below.
	return &http.Client{Transport: &decodingTransport{
		next: &cachingTransport{next: inner, cache: newResponseCache()},
	}}
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
	// The cache sits under the decoding transport, so the chain is walked rather
	// than type-asserted at one level — a layer added above it must not silently
	// stop the cache being cleared between pooled requests.
	for t := a.client.Transport; t != nil; {
		switch v := t.(type) {
		case *decodingTransport:
			t = v.next
		case *cachingTransport:
			v.cache.reset()
			return
		default:
			return
		}
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

// typeError builds a guest TypeError to reject with. A body that failed to read
// rejects with one — the standard says so, and a caller writing
// `catch (e) { if (e instanceof TypeError) ... }` cannot do anything with the
// bare string this used to hand back. A failure to construct the error is
// reported as the string it would have carried, because there is nothing better
// left to say.
func (a *fetchAPI) typeError(message string) spidermonkey.Value {
	if a.typeErrorCls == nil {
		return spidermonkey.ValueOf(message)
	}
	v, err := a.typeErrorCls.New(spidermonkey.ValueOf(message))
	if err != nil {
		return spidermonkey.ValueOf(message)
	}
	return v
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
	credentials := ""
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
		if v, ok := scalar("credentials"); ok {
			credentials = v.String()
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
	// Names go on the wire in the CASE the caller wrote them: Header.Set
	// would canonicalize, and net/http writes map keys as they are.
	for k, v := range headers {
		req.Header[k] = append(req.Header[k], v)
	}
	// Identify the runtime. Go's transport otherwise sends "Go-http-client/1.1",
	// which names the transport rather than the platform, and a fetch with no
	// User-Agent at all is something no user agent does. The lookup folds case
	// because the keys above no longer arrive canonicalized.
	if headerByFold(req.Header, "User-Agent") == "" {
		req.Header.Set("User-Agent", fetchUserAgent)
	}
	// A per-request client selects the redirect policy without disturbing the
	// shared transport (its connection pool is reused). Go's redirect handling
	// already strips Authorization/Cookie on a cross-origin hop, so "follow" only
	// needs to count hops (for Response.redirected) and cap the chain.
	hops := 0
	reqClient := &http.Client{Transport: a.client.Transport}
	if credentials == "include" {
		reqClient.Jar = a.cookieJar()
	}
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
				delHeaderFold(req.Header, "Authorization")
				delHeaderFold(req.Header, "Cookie")
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

// defineEmptyConsumers gives a body-less response the same consumer surface as
// any other: each answers with nothing rather than being absent, so a caller
// that reads a 204 gets "" instead of a TypeError about a missing method.
func (a *fetchAPI) defineEmptyConsumers(r *spidermonkey.Object) error {
	empty := map[string]spidermonkey.Func{
		"bytes": func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
			u8, err := a.js.NewBytes(nil)
			if err != nil {
				return nil, err
			}
			return a.promise("resolve", u8)
		},
		"arrayBuffer": func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
			u8, err := a.js.NewBytes(nil)
			if err != nil {
				return nil, err
			}
			buf, err := u8.Get("buffer")
			if err != nil {
				return nil, err
			}
			return a.promise("resolve", buf)
		},
		"text": func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
			return a.promise("resolve", spidermonkey.ValueOf(""))
		},
		"json": func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
			// An empty body is not JSON, and saying so is the same answer a body
			// that failed to parse gets.
			return a.promise("reject", a.typeError("Unexpected end of JSON input"))
		},
	}
	for name, fn := range empty {
		if err := r.DefineFunc(name, fn); err != nil {
			return err
		}
	}
	return nil
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

	// A null-body status HAS no body: 204, 205 and 304 are defined to carry
	// none, so `response.body` is null rather than a stream that closes at once.
	// The connection is still released.
	switch resp.StatusCode {
	case 204, 205, 304:
		resp.Body.Close()
		if err := r.Set("body", spidermonkey.Null()); err != nil {
			return nil, err
		}
		if err := a.defineEmptyConsumers(r); err != nil {
			return nil, err
		}
		cancel()
		return r, nil
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
	// A response body is a BYTE stream: it carries bytes, and a caller may read
	// it through a buffer of their own (getReader({mode:"byob"})). Declared as an
	// ordinary stream it refused every BYOB reader.
	if err := source.Set("type", spidermonkey.ValueOf("bytes")); err != nil {
		source.Free()
		return nil, err
	}
	// highWaterMark 0: nothing is read from the connection until a consumer
	// asks. At the default the stream pulls as soon as it is constructed — which
	// claims the body for the STREAM before the response has even been handed to
	// the caller, so a later res.text() found it already consumed. (A byte
	// stream's default is already 0; it is set anyway, because the default is
	// not the reason.)
	strategy, err := js.NewObject()
	if err != nil {
		source.Free()
		return nil, err
	}
	if err := strategy.Set("highWaterMark", spidermonkey.ValueOf(0)); err != nil {
		source.Free()
		strategy.Free()
		return nil, err
	}
	bodyV, err := a.streamCls.New(source, strategy)
	source.Free()
	strategy.Free()
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
						return a.typeError(rerr.Error()), true
					}
					u8, e := js.NewBytes(data)
					if e != nil {
						return a.typeError(e.Error()), true
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
						return a.typeError(rerr.Error()), true
					}
					u8, e := js.NewBytes(data)
					if e != nil {
						return a.typeError(e.Error()), true
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
						return a.typeError(rerr.Error()), true
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
						return a.typeError(rerr.Error()), true
					}
					// A UTF-8 BOM is not part of the JSON text.
					if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
						data = data[3:]
					}
					parsed, perr := a.jsonObj.CallMethod("parse", spidermonkey.ValueOf(string(data)))
					if perr != nil {
						// The rejection is a real SyntaxError, which is what a
						// caller expecting JSON.parse's failure mode checks for.
						if se, serr := a.syntaxErrorCls.New(spidermonkey.ValueOf(perr.Error())); serr == nil {
							return se, true
						}
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

// ------------------------- decoding a response's Content-Encoding.
//
// fetch decodes the content codings a response declares before the body reaches
// the caller — `res.text()` on a gzipped resource gives the text, not the gzip.
// net/http does this for gzip only, and only when IT chose to ask for gzip; a
// server that compresses without being asked, or that uses br or zstd, hands
// back bytes nothing unwraps. So the decoding is here, as a RoundTripper above
// the cache: a cache hit is decoded too, and what the cache stores stays the
// bytes the origin actually sent.
//
// A corrupt body must FAIL rather than truncate. Every reader below reports its
// own error, and because the decode is streaming that error arrives at the read
// that hits the damage — which is what the caller's body promise rejects with.

// decodingTransport unwraps the content codings of every response it passes on.
type decodingTransport struct{ next http.RoundTripper }

func (t *decodingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Announce what can be decoded. Without this a server has no reason to
	// compress at all, and net/http's own transparent gzip only happens when it
	// added the header itself — which it will not, now that one is present.
	out := req.Clone(req.Context())
	if out.Header.Get("Accept-Encoding") == "" {
		out.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	}
	resp, err := t.next.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	return decodeBody(resp), nil
}

// decodeBody replaces the body with a decoded one when the response declares a
// coding this runtime understands. A coding it does not understand is left
// alone: the bytes are then what the caller asked for, and pretending otherwise
// would be worse than handing them over.
func decodeBody(resp *http.Response) *http.Response {
	coding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if coding == "" || coding == "identity" {
		return resp
	}
	// A list of codings is applied in order, so it is undone in reverse.
	parts := strings.Split(coding, ",")
	body := resp.Body
	decoded := false
	for i := len(parts) - 1; i >= 0; i-- {
		next, ok := decodeReader(strings.TrimSpace(parts[i]), body)
		if !ok {
			// An unknown coding stops the unwrapping: anything further out was
			// applied on top of it and cannot be reached through it either.
			break
		}
		body = next
		decoded = true
	}
	if !decoded {
		return resp
	}
	resp.Body = body
	// The declared length described the ENCODED bytes and no longer describes
	// anything the caller can see, so it goes — as does the coding itself, which
	// has been undone.
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return resp
}

// decodeReader wraps r in the decoder for one coding, reporting whether the
// coding is one this runtime knows.
//
// The decoders are all LAZY about their headers: a gzip.Reader validates its
// header on construction, which would mean a corrupt body failed at
// RoundTrip time rather than at the read — and a body error has to reach the
// caller as a rejected body promise, not as a failed fetch. So construction is
// deferred to the first Read.
func decodeReader(coding string, r io.ReadCloser) (io.ReadCloser, bool) {
	switch coding {
	case "gzip", "x-gzip":
		return &lazyDecoder{src: r, open: func(in io.Reader) (io.Reader, error) { return gzip.NewReader(in) }}, true
	case "deflate":
		return &lazyDecoder{src: r, open: func(in io.Reader) (io.Reader, error) { return zlib.NewReader(in) }}, true
	case "br":
		return &lazyDecoder{src: r, open: func(in io.Reader) (io.Reader, error) { return brotli.NewReader(in), nil }}, true
	case "zstd":
		return &lazyDecoder{src: r, open: func(in io.Reader) (io.Reader, error) {
			// Window sizes above the default are refused rather than allocated:
			// a response can otherwise name a window that costs the host hundreds
			// of megabytes per body. The suite checks exactly that refusal.
			d, err := zstd.NewReader(in, zstd.WithDecoderMaxWindow(1<<23), zstd.WithDecoderConcurrency(1))
			if err != nil {
				return nil, err
			}
			return d.IOReadCloser(), nil
		}}, true
	}
	return nil, false
}

// lazyDecoder defers building its decoder until the first Read, so a malformed
// stream is reported to whoever reads the body rather than to whoever made the
// request.
type lazyDecoder struct {
	src  io.ReadCloser
	open func(io.Reader) (io.Reader, error)
	r    io.Reader
	err  error
}

func (d *lazyDecoder) Read(p []byte) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	if d.r == nil {
		r, err := d.open(d.src)
		if err != nil {
			d.err = err
			return 0, err
		}
		d.r = r
	}
	n, err := d.r.Read(p)
	if err != nil && err != io.EOF {
		d.err = err
	}
	return n, err
}

func (d *lazyDecoder) Close() error {
	if c, ok := d.r.(io.Closer); ok {
		_ = c.Close()
	}
	return d.src.Close()
}

// ------------------------- sending a request whose headers net/http's
// Transport refuses.
//
// A header value may hold any byte but NUL, LF and CR as far as fetch is
// concerned. net/http is stricter: Transport.roundTrip runs validateHeaders,
// which rejects every control character except HTAB, and the request never
// leaves. That is RFC 9110's field-vchar rule, and enforcing it on send is a
// deliberate choice — but it is enforced by the TRANSPORT, not by the code that
// writes the request, and Go's own server does not enforce it on receive at all
// (net/textproto trims a field value and keeps whatever is left). So a Go
// client cannot send what a Go server will happily accept.
//
// The way around it is not a hand-written HTTP implementation. Request.Write is
// public, documented as writing "an HTTP/1.1 request, which is the header and
// body, in wire format", and validates only the field NAME; ReadResponse parses
// the reply. Between them the whole exchange is standard-library code, and this
// file is the dozen lines that connect them.
//
// The raw path is taken ONLY for a request the standard path would refuse, so
// nothing in ordinary use loses connection pooling or HTTP/2 — and HTTP/2 could
// not carry such a value anyway, since its own header check is the same one.

// isCTLNotLWS mirrors the rule httpguts.ValidHeaderFieldValue applies: a
// control character other than HTAB or space is what makes the Transport refuse.
func isCTLNotLWS(b byte) bool {
	return (b < 0x20 || b == 0x7f) && b != '\t' && b != ' '
}

// needsRawWrite reports whether net/http would refuse to send these headers.
func needsRawWrite(h http.Header) bool {
	for _, vv := range h {
		for _, v := range vv {
			for i := 0; i < len(v); i++ {
				if isCTLNotLWS(v[i]) {
					return true
				}
			}
		}
	}
	return false
}

// permissiveTransport delegates to the standard Transport, and falls back to
// writing the request over a connection of its own for the requests the standard
// Transport will not send.
type permissiveTransport struct {
	std  *http.Transport
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (t *permissiveTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !needsRawWrite(req.Header) {
		return t.std.RoundTrip(req)
	}
	return t.roundTripRaw(req)
}

// closingBody ties the response body's lifetime to the connection's, since this
// path has no pool to return it to.
type closingBody struct {
	*bufio.Reader
	conn net.Conn
	body interface{ Close() error }
}

func (b *closingBody) Read(p []byte) (int, error) { return b.Reader.Read(p) }

func (b *closingBody) Close() error {
	if b.body != nil {
		_ = b.body.Close()
	}
	return b.conn.Close()
}

func (t *permissiveTransport) roundTripRaw(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	host := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		if req.URL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	conn, err := t.dial(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, err
	}
	if req.URL.Scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: host})
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = tc
	}
	// The context has no transport to cancel through here, so it cancels by
	// closing the connection — which is what makes an aborted fetch return
	// promptly rather than waiting on the peer.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write request: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := readResponsePermissive(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read response: %w", err)
	}
	resp.Body = &closingBody{Reader: bufio.NewReader(resp.Body), conn: conn, body: resp.Body}
	return resp, nil
}

// readResponsePermissive parses a response the way http.ReadResponse would,
// except that it does not validate header values. ReadResponse goes through
// net/textproto, whose ReadMIMEHeader rejects a control character in a value —
// so a server that echoes back the header this path exists to SEND could not be
// read. The framing is still the standard library's: a chunked body is decoded by
// httputil, and a counted one by io.LimitReader.
func readResponsePermissive(br *bufio.Reader, req *http.Request) (*http.Response, error) {
	statusLine, err := readCRLFLine(br)
	if err != nil {
		return nil, err
	}
	proto, rest, ok := strings.Cut(statusLine, " ")
	if !ok {
		return nil, fmt.Errorf("bad status line %q", statusLine)
	}
	codeText, reason, _ := strings.Cut(rest, " ")
	code, err := strconv.Atoi(codeText)
	if err != nil {
		return nil, fmt.Errorf("bad status code %q", codeText)
	}
	header := http.Header{}
	for {
		line, err := readCRLFLine(br)
		if err != nil {
			return nil, err
		}
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("bad header line %q", line)
		}
		// Only the optional whitespace around the value is removed. Whatever else
		// the value holds is the value — that is the whole point of this path.
		header.Add(strings.TrimSpace(name), strings.Trim(value, " \t"))
	}
	resp := &http.Response{
		Status:     codeText + " " + reason,
		StatusCode: code,
		Proto:      proto,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     header,
		Request:    req,
	}
	switch {
	case strings.EqualFold(header.Get("Transfer-Encoding"), "chunked"):
		resp.ContentLength = -1
		resp.TransferEncoding = []string{"chunked"}
		resp.Body = io.NopCloser(httputil.NewChunkedReader(br))
	case header.Get("Content-Length") != "":
		n, cerr := strconv.ParseInt(header.Get("Content-Length"), 10, 64)
		if cerr != nil {
			return nil, fmt.Errorf("bad Content-Length %q", header.Get("Content-Length"))
		}
		resp.ContentLength = n
		resp.Body = io.NopCloser(io.LimitReader(br, n))
	default:
		// No framing: the body runs to end of connection, which is why this path
		// does not reuse the connection.
		resp.ContentLength = -1
		resp.Body = io.NopCloser(br)
	}
	return resp, nil
}

// readCRLFLine reads one header line and returns it without its terminator.
func readCRLFLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// ------------------------- the blocking round-trip behind synchronous
// XMLHttpRequest.
//
// A synchronous send() must not return until the response has arrived. There is
// one loop goroutine, so "not return" means it is occupied for the whole
// round-trip: no timer fires, no promise settles, no other host call runs. That
// is not an implementation shortcoming — it is what the caller asked for, and it
// is why the specification itself deprecates the mode. What it costs is stated
// here rather than in a comment somewhere the caller will not read: a request to
// a server this same instance is serving CANNOT complete, because the request
// that would answer it can never be dispatched.
//
// It is a separate entry point from fetch rather than a flag on it because the
// two differ in the only thing that matters about fetch's shape: fetch hands the
// body back as a stream and settles a promise, and neither exists here. What
// they DO share — the client, the permission hooks, the redirect policy — is
// shared by construction, so a URL denied to one is denied to the other.

// maxSyncBody bounds a synchronous response, which is buffered whole: there is
// no reader to apply backpressure and nothing to stream it into.
const maxSyncBody = 100 << 20

// fetchSync(url, init) -> { status, statusText, url, headers, body } or
// { error }. init carries method, headers, body and timeoutMs; there is no
// signal, because nothing can abort a call that owns the only thread.
func (a *fetchAPI) fetchSync(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	a.clientOnce.Do(func() { a.client = newHTTPClient(cfg, a.roots) })
	if len(args) < 1 {
		return nil, fmt.Errorf("fetch_sync: an input URL is required")
	}
	url := args[0].String()
	method := "GET"
	headers := map[string]string{}
	var body io.Reader
	timeout := time.Duration(0)

	if len(args) > 1 && args[1].IsObject() {
		init := args[1].Object()
		defer init.Free()
		if v, err := init.Get("method"); err == nil && v != nil && !v.IsUndefined() {
			if o := v.Object(); o != nil {
				o.Free()
			} else {
				method = strings.ToUpper(v.String())
			}
		}
		if v, err := init.Get("timeoutMs"); err == nil && v != nil && !v.IsUndefined() {
			if o := v.Object(); o != nil {
				o.Free()
			} else if ms := v.Float(); ms > 0 {
				timeout = time.Duration(ms) * time.Millisecond
			}
		}
		if v, err := init.Get("body"); err == nil {
			if o := v.Object(); o != nil {
				data, berr := o.Bytes()
				o.Free()
				if berr != nil {
					return nil, berr
				}
				body = strings.NewReader(string(data))
			} else if v.Export() != nil {
				body = strings.NewReader(v.String())
			}
		}
		if v, err := init.Get("headers"); err == nil {
			if o := v.Object(); o != nil {
				s, serr := a.jsonObj.CallMethod("stringify", o)
				o.Free()
				if serr != nil {
					return nil, serr
				}
				if err := json.Unmarshal([]byte(s.String()), &headers); err != nil {
					return nil, fmt.Errorf("fetch_sync: bad headers: %w", err)
				}
			}
		}
	}

	if strings.HasPrefix(strings.ToLower(url), "data:") {
		resp, derr := dataResponse(url, method)
		if derr != nil {
			return a.syncError(derr.Error())
		}
		return a.syncResponse(resp, url)
	}

	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return a.syncError(err.Error())
	}
	if err := checkRequestPermission(cfg, req); err != nil {
		return a.syncError(err.Error())
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", fetchUserAgent)
	}
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	client := &http.Client{Transport: a.client.Transport}
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errTooManyRedirects
		}
		if n := len(via); n > 0 && !sameOrigin(via[n-1].URL, r.URL) {
			r.Header.Del("Authorization")
			r.Header.Del("Cookie")
		}
		return nil
	}
	resp, derr := client.Do(req.WithContext(ctx))
	if derr != nil {
		// A deadline is reported as its own kind rather than left to be recognised
		// from the message: the caller has to raise TimeoutError for it and
		// NetworkError for everything else, and that is a decision, not a string.
		if ctx.Err() == context.DeadlineExceeded {
			return a.syncFailure(derr.Error(), true)
		}
		return a.syncError(derr.Error())
	}
	defer resp.Body.Close()
	final := url
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return a.syncResponse(resp, final)
}

// syncResponse turns a finished round-trip into the plain object the guest
// reads. The body is read HERE, not handed over as a stream: a synchronous
// caller has no way to read one.
func (a *fetchAPI) syncResponse(resp *http.Response, finalURL string) (spidermonkey.Value, error) {
	data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxSyncBody+1))
	if rerr != nil {
		return a.syncError(rerr.Error())
	}
	if len(data) > maxSyncBody {
		return a.syncError(fmt.Sprintf("response body exceeds %d bytes", maxSyncBody))
	}
	out, err := a.js.NewObject()
	if err != nil {
		return nil, err
	}
	pairs := make([]any, 0, len(resp.Header))
	for name, values := range resp.Header {
		for _, v := range values {
			pairs = append(pairs, []any{name, v})
		}
	}
	if err := out.Set("status", spidermonkey.ValueOf(float64(resp.StatusCode))); err != nil {
		return nil, err
	}
	if err := out.Set("statusText", spidermonkey.ValueOf(statusTextOf(resp))); err != nil {
		return nil, err
	}
	if err := out.Set("url", spidermonkey.ValueOf(finalURL)); err != nil {
		return nil, err
	}
	if err := out.Set("headers", spidermonkey.ValueOf(pairs)); err != nil {
		return nil, err
	}
	u8, err := a.js.NewBytes(data)
	if err != nil {
		return nil, err
	}
	serr := out.Set("body", u8)
	u8.Free()
	if serr != nil {
		return nil, serr
	}
	return out, nil
}

// statusTextOf is the reason phrase the server sent, which is not always the
// canonical one for the code — a test that sets its own is checking exactly
// that it survives.
func statusTextOf(resp *http.Response) string {
	if _, phrase, ok := strings.Cut(resp.Status, " "); ok {
		return phrase
	}
	return http.StatusText(resp.StatusCode)
}

func (a *fetchAPI) syncError(message string) (spidermonkey.Value, error) {
	return a.syncFailure(message, false)
}

func (a *fetchAPI) syncFailure(message string, timedOut bool) (spidermonkey.Value, error) {
	out, err := a.js.NewObject()
	if err != nil {
		return nil, err
	}
	if err := out.Set("error", spidermonkey.ValueOf(message)); err != nil {
		return nil, err
	}
	if timedOut {
		if err := out.Set("timedOut", spidermonkey.ValueOf(true)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ------------------------- fetch() of a data: URL.
//
// A data URL carries its own body, so there is no request to make and no
// permission to check — which is also why it must be handled before the HTTP
// path rather than inside it. Without this, `fetch("data:…")` failed at the
// transport and the whole WPT fetch/data-urls directory scored 2 of 154.

// defaultDataMIME is what a data URL means when it names no type, per the
// WHATWG data-url processor.
const defaultDataMIME = "text/plain;charset=US-ASCII"

// parseDataURL splits a data: URL into its MIME type and decoded body.
func parseDataURL(raw string) (mime string, body []byte, err error) {
	rest, ok := cutSchemePrefix(raw, "data:")
	if !ok {
		return "", nil, fmt.Errorf("not a data URL")
	}
	// The FIRST comma separates the metadata from the data; a comma inside the
	// data is content, not a separator.
	head, data, ok := strings.Cut(rest, ",")
	if !ok {
		return "", nil, fmt.Errorf("data URL has no comma")
	}
	head = strings.TrimSpace(head)
	isBase64 := false
	// ";base64" must be the LAST parameter, matched case-insensitively, and its
	// trailing whitespace is ignored.
	if i := strings.LastIndex(strings.ToLower(head), ";base64"); i >= 0 &&
		strings.TrimSpace(head[i+len(";base64"):]) == "" {
		isBase64 = true
		head = head[:i]
	}
	// A type that begins with ";" is a parameter list with no type, and gets
	// "text/plain" — and ONLY that. The charset is not a default: it belongs to
	// the fallback below, for a type that does not parse at all. Prepending the
	// whole default here gave "text/plain;charset=US-ASCII;charset=x" for
	// ";charset=x", where the caller's charset is the one that counts.
	if strings.HasPrefix(head, ";") {
		head = "text/plain" + head
	}
	// The Content-Type is the PARSED type serialized back, not the raw text:
	// that is what lowercases it, normalizes the whitespace around parameters
	// and drops malformed ones. A type that does not parse falls back to the
	// default rather than being passed through.
	if m, ok := parseMIMEType(head); ok {
		head = m.String()
	} else {
		head = defaultDataMIME
	}

	// The data segment is percent-encoded regardless of base64.
	decoded, uerr := url.PathUnescape(data)
	if uerr != nil {
		// A stray "%" is data, not an error, in a data URL.
		decoded = data
	}
	if !isBase64 {
		return head, []byte(decoded), nil
	}
	// Forgiving base64: whitespace is stripped, padding is optional.
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '\f':
			return -1
		}
		return r
	}, decoded)
	if n := len(cleaned) % 4; n == 1 {
		return "", nil, fmt.Errorf("invalid base64 in data URL")
	} else if n != 0 {
		cleaned += strings.Repeat("=", 4-n)
	}
	out, berr := base64.StdEncoding.DecodeString(cleaned)
	if berr != nil {
		// Accept the URL-safe alphabet too, as browsers do.
		out, berr = base64.URLEncoding.DecodeString(cleaned)
		if berr != nil {
			return "", nil, fmt.Errorf("invalid base64 in data URL")
		}
	}
	return head, out, nil
}

func cutSchemePrefix(raw, scheme string) (string, bool) {
	if len(raw) < len(scheme) || !strings.EqualFold(raw[:len(scheme)], scheme) {
		return "", false
	}
	return raw[len(scheme):], true
}

// dataResponse turns a data: URL into the response fetch resolves with: always
// 200 OK, the URL's own media type, and its decoded bytes.
func dataResponse(raw, method string) (*http.Response, error) {
	mime, body, err := parseDataURL(raw)
	if err != nil {
		return nil, err
	}
	u, uerr := url.Parse(raw)
	if uerr != nil {
		return nil, fmt.Errorf("invalid data URL")
	}
	h := http.Header{}
	h.Set("Content-Type", mime)
	// A HEAD gets the headers and no body, as it would over HTTP.
	rc := io.NopCloser(strings.NewReader(string(body)))
	length := int64(len(body))
	if strings.EqualFold(method, "HEAD") {
		rc = io.NopCloser(strings.NewReader(""))
	}
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		Header:        h,
		Body:          rc,
		ContentLength: length,
		Request:       &http.Request{Method: method, URL: u},
	}, nil
}

// ------------------------- the HTTP cache behind fetch's `cache` option.
//
// fetch does not define caching of its own — it defers to HTTP's, and then adds
// six modes that say how much of it to obey. So this is RFC 9111's core
// (freshness, revalidation, Vary) with the modes layered on top, as a
// RoundTripper: that is where a cache belongs in Go, and it composes with the
// permissive transport in the raw-fetch section without either knowing about
// the other.
//
// What it deliberately does not do: range requests and 206 responses go
// straight past it. A partial response is not the resource, and storing one as
// though it were is how a cache starts returning truncated bodies.
//
// The store is per-installation and in memory, bounded by total body bytes with
// the oldest entries evicted first. A cache with no ceiling is a memory leak
// with a hit rate.

// cacheMode is fetch's `cache` option.
type cacheMode string

const (
	cacheDefault      cacheMode = "default"
	cacheNoStore      cacheMode = "no-store"
	cacheReload       cacheMode = "reload"
	cacheNoCache      cacheMode = "no-cache"
	cacheForceCache   cacheMode = "force-cache"
	cacheOnlyIfCached cacheMode = "only-if-cached"
)

// cacheModeKey carries the mode from the fetch op to the transport. It travels
// on the request context because the mode is a property of the fetch call, not
// of the connection or the client.
type cacheModeKey struct{}

func withCacheMode(ctx context.Context, mode string) context.Context {
	switch cacheMode(mode) {
	case cacheNoStore, cacheReload, cacheNoCache, cacheForceCache, cacheOnlyIfCached:
		return context.WithValue(ctx, cacheModeKey{}, cacheMode(mode))
	}
	return ctx
}

func cacheModeOf(req *http.Request) cacheMode {
	if m, ok := req.Context().Value(cacheModeKey{}).(cacheMode); ok {
		return m
	}
	return cacheDefault
}

// maxCacheBytes bounds the total stored body size.
const maxCacheBytes = 32 << 20

// cacheEntry is a stored response, byte for byte. The times are what freshness
// is computed from: HTTP's age arithmetic needs to know when the request went
// out and when the response came back, not just what the headers say.
type cacheEntry struct {
	status    int
	header    http.Header
	body      []byte
	requested time.Time
	received  time.Time
	// varyKey is the request-header values the response said it varies by. An
	// entry is only reusable for a request whose values match.
	varyKey string
	stored  time.Time
}

// responseCache stores, per primary key, one entry PER VARIANT: a response
// with Vary: Foo selects on the request's Foo value, and storing a second
// variant must not evict the first — a cache holds the variants side by side,
// or Vary would make it useless for exactly the resources that use it.
type responseCache struct {
	mu      sync.Mutex
	entries map[string][]*cacheEntry
	bytes   int
}

func newResponseCache() *responseCache {
	return &responseCache{entries: map[string][]*cacheEntry{}}
}

// key is the primary cache key: the method and the full URL. Vary is handled
// separately, as a check on the entry rather than as part of the key, so that a
// request can find the entry and then discover it does not apply.
func cacheKey(req *http.Request) string { return req.Method + " " + req.URL.String() }

// varyKeyFor builds the value an entry's Vary directive selects on. A Vary of
// "*" never matches, which is the standard's way of saying "do not reuse this".
func varyKeyFor(vary string, h http.Header) (string, bool) {
	if vary == "" {
		return "", true
	}
	var parts []string
	for _, name := range strings.Split(vary, ",") {
		name = strings.TrimSpace(name)
		if name == "*" {
			return "", false
		}
		parts = append(parts, name+"="+h.Get(name))
	}
	return strings.Join(parts, "\n"), true
}

func (c *responseCache) get(req *http.Request) *cacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries[cacheKey(req)] {
		// Each variant's OWN Vary decides what it selects on; the header can
		// differ between variants when the origin changed its mind.
		want, ok := varyKeyFor(e.header.Get("Vary"), req.Header)
		if ok && want == e.varyKey {
			return e
		}
	}
	return nil
}

func (c *responseCache) put(req *http.Request, e *cacheEntry) {
	key, ok := varyKeyFor(e.header.Get("Vary"), req.Header)
	if !ok {
		return
	}
	e.varyKey = key
	e.stored = time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	pk := cacheKey(req)
	variants := c.entries[pk]
	replaced := false
	for i, old := range variants {
		if old.varyKey == e.varyKey {
			c.bytes -= len(old.body)
			variants[i] = e
			replaced = true
			break
		}
	}
	if !replaced {
		variants = append(variants, e)
	}
	c.entries[pk] = variants
	c.bytes += len(e.body)
	// Evict oldest-first until under the ceiling.
	for c.bytes > maxCacheBytes {
		oldestKey, oldestIdx := "", -1
		oldest := time.Now().Add(time.Hour)
		for k, list := range c.entries {
			for i, v := range list {
				if v == e {
					continue // never evict what was just stored
				}
				if v.stored.Before(oldest) {
					oldestKey, oldestIdx, oldest = k, i, v.stored
				}
			}
		}
		if oldestIdx < 0 {
			return
		}
		list := c.entries[oldestKey]
		c.bytes -= len(list[oldestIdx].body)
		list = append(list[:oldestIdx], list[oldestIdx+1:]...)
		if len(list) == 0 {
			delete(c.entries, oldestKey)
		} else {
			c.entries[oldestKey] = list
		}
	}
}

func (c *responseCache) reset() {
	c.mu.Lock()
	c.entries = map[string][]*cacheEntry{}
	c.bytes = 0
	c.mu.Unlock()
}

// ---------------------------------------------------------- freshness

// directives parses a Cache-Control field into a map. A directive with no value
// maps to the empty string, which is enough to test for its presence.
func directives(h http.Header) map[string]string {
	out := map[string]string{}
	for _, v := range h.Values("Cache-Control") {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			name, value, _ := strings.Cut(part, "=")
			out[strings.ToLower(strings.TrimSpace(name))] = strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return out
}

// heuristicStatus lists the codes RFC 9111 calls heuristically cacheable: a
// response with one of these and no explicit lifetime may still be assigned
// one. Any other status gets a heuristic only when the response opts in with
// Cache-Control: public.
var heuristicStatus = map[int]bool{
	200: true, 203: true, 204: true, 300: true, 301: true, 308: true,
	404: true, 405: true, 410: true, 414: true, 501: true,
}

// hasExplicitLifetime reports whether the response states its own freshness.
// s-maxage deliberately does not count: it addresses shared caches, and this
// cache is a private one — it is fetch's own, serving a single user agent.
func hasExplicitLifetime(h http.Header) bool {
	if _, ok := directives(h)["max-age"]; ok {
		return true
	}
	return h.Get("Expires") != ""
}

// freshnessLifetime is how long the response may be reused without asking:
// max-age, then Expires, then — with nothing explicit — the heuristic the
// standard suggests, one tenth of the time since Last-Modified. The heuristic
// applies only where the standard allows one: a heuristically-cacheable status,
// or an explicit Cache-Control: public.
func (e *cacheEntry) freshnessLifetime() time.Duration {
	cc := directives(e.header)
	if v, ok := cc["max-age"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	base := e.received
	if d := e.header.Get("Date"); d != "" {
		if dt, derr := http.ParseTime(d); derr == nil {
			base = dt
		}
	}
	if exp := e.header.Get("Expires"); exp != "" {
		t, err := http.ParseTime(exp)
		if err != nil || t.Before(base) {
			// An unparseable or past Expires means already expired.
			return 0
		}
		return t.Sub(base)
	}
	_, public := cc["public"]
	if !heuristicStatus[e.status] && !public {
		return 0
	}
	if lm := e.header.Get("Last-Modified"); lm != "" {
		if t, err := http.ParseTime(lm); err == nil && base.After(t) {
			return base.Sub(t) / 10
		}
	}
	return 0
}

// freshened is this entry with the 304's headers layered over its own and its
// clock restarted — a new value, because the stored one may be being served
// concurrently and is treated as immutable once in the cache.
func (e *cacheEntry) freshened(h http.Header, requested time.Time) *cacheEntry {
	out := &cacheEntry{
		status:    e.status,
		header:    e.header.Clone(),
		body:      e.body,
		requested: requested,
		received:  time.Now(),
		varyKey:   e.varyKey,
	}
	for k, v := range h {
		out.header[k] = append([]string(nil), v...)
	}
	return out
}

// age is how old the stored response is now, including any Age the server
// reported when it arrived.
func (e *cacheEntry) age(now time.Time) time.Duration {
	var reported time.Duration
	if a := e.header.Get("Age"); a != "" {
		if n, err := strconv.Atoi(a); err == nil {
			reported = time.Duration(n) * time.Second
		}
	}
	return reported + now.Sub(e.received)
}

// storable reports whether a response may be kept at all. The status matters
// only when the response says nothing about its own lifetime: an explicit
// max-age or Expires makes any final status storable — the origin has said how
// long it is good for — while without one only the heuristically-cacheable
// statuses may be kept.
func storable(req *http.Request, resp *http.Response) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	// A partial response is not the resource. See the file comment.
	if resp.StatusCode == http.StatusPartialContent || req.Header.Get("Range") != "" {
		return false
	}
	if resp.StatusCode < 200 {
		return false
	}
	if !hasExplicitLifetime(resp.Header) && !heuristicStatus[resp.StatusCode] {
		// One more way in: a heuristic is allowed for any status that opts in
		// with Cache-Control: public (see freshnessLifetime).
		if _, public := directives(resp.Header)["public"]; !public {
			return false
		}
	}
	if _, ok := directives(resp.Header)["no-store"]; ok {
		return false
	}
	// Cache-Control: private is NOT a reason to refuse. It forbids SHARED
	// caches; this cache is a private one — it belongs to the single user agent
	// doing the fetching, which is exactly who private admits.
	return true
}

// ------------------------------------------------------- the transport

type cachingTransport struct {
	next  http.RoundTripper
	cache *responseCache

	// revalidating holds the cache keys with a background revalidation already
	// in flight, so a burst of requests inside the stale-while-revalidate window
	// costs one origin request rather than one each.
	revalidatingMu sync.Mutex
	revalidating   map[string]bool
}

// staleWhileRevalidate reads the response's RFC 5861 window, if it granted one.
func staleWhileRevalidate(h http.Header) (time.Duration, bool) {
	v, ok := directives(h)["stale-while-revalidate"]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

// revalidateInBackground refreshes a stale entry without making the caller
// wait. The request is rebuilt free of the caller's context — the caller may
// be gone before the origin answers, and this revalidation is the cache's
// business, not the caller's — but bounded, so an origin that never answers
// cannot accumulate goroutines.
func (t *cachingTransport) revalidateInBackground(req *http.Request, entry *cacheEntry) {
	key := cacheKey(req)
	t.revalidatingMu.Lock()
	if t.revalidating == nil {
		t.revalidating = map[string]bool{}
	}
	if t.revalidating[key] {
		t.revalidatingMu.Unlock()
		return
	}
	t.revalidating[key] = true
	t.revalidatingMu.Unlock()

	out := req.Clone(context.Background())
	out.Body = nil
	if etag := entry.header.Get("ETag"); etag != "" {
		out.Header.Set("If-None-Match", etag)
	} else if lm := entry.header.Get("Last-Modified"); lm != "" {
		out.Header.Set("If-Modified-Since", lm)
	}
	go func() {
		defer func() {
			t.revalidatingMu.Lock()
			delete(t.revalidating, key)
			t.revalidatingMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		requested := time.Now()
		resp, err := t.next.RoundTrip(out.WithContext(ctx))
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotModified {
			t.cache.put(req, entry.freshened(resp.Header, requested))
			return
		}
		if !storable(req, resp) {
			return
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxEntryBytes))
		if err != nil {
			return
		}
		t.cache.put(req, &cacheEntry{
			status: resp.StatusCode, header: resp.Header.Clone(), body: body,
			requested: requested, received: time.Now(),
		})
	}()
}

// serve turns a stored entry back into a response. The body is a fresh reader
// over the stored bytes each time, so two callers cannot consume one another's.
func (e *cacheEntry) serve(req *http.Request) *http.Response {
	h := make(http.Header, len(e.header))
	for k, v := range e.header {
		h[k] = append([]string(nil), v...)
	}
	return &http.Response{
		Status:        strconv.Itoa(e.status) + " " + http.StatusText(e.status),
		StatusCode:    e.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader(e.body)),
		ContentLength: int64(len(e.body)),
		Request:       req,
	}
}

// addModeHeaders applies the request headers the mode implies. These are the
// standard's own words: a reload says "no-cache" to everything in the path, and
// a no-cache says "max-age=0", which is the difference between "do not use a
// stored response" and "do not use one without checking".
func addModeHeaders(req *http.Request, mode cacheMode) {
	switch mode {
	case cacheNoStore, cacheReload:
		if req.Header.Get("Pragma") == "" {
			req.Header.Set("Pragma", "no-cache")
		}
		if req.Header.Get("Cache-Control") == "" {
			req.Header.Set("Cache-Control", "no-cache")
		}
	case cacheNoCache:
		if req.Header.Get("Cache-Control") == "" {
			req.Header.Set("Cache-Control", "max-age=0")
		}
	}
}

// conditionalHeaders are the request headers that make the request its own
// validation. A request carrying one is asking the ORIGIN a question about the
// resource's state, so the cache must not answer for it.
var conditionalHeaders = []string{
	"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range",
}

func isConditional(h http.Header) bool {
	for _, name := range conditionalHeaders {
		if h.Get(name) != "" {
			return true
		}
	}
	return false
}

// reqDirectives is the caller's own Cache-Control (RFC 9111 §5.2.1) — the
// header a page author writes on a fetch, distinct from fetch's cache MODE.
// Both express constraints on the cache; the header is the finer instrument.
type reqDirectives struct {
	noCache, noStore, onlyIfCached bool
	maxAge                         time.Duration
	hasMaxAge                      bool
	maxStale                       time.Duration
	hasMaxStale, maxStaleAny       bool
	minFresh                       time.Duration
	hasMinFresh                    bool
}

func parseReqDirectives(h http.Header) reqDirectives {
	var rd reqDirectives
	cc := directives(h)
	_, rd.noCache = cc["no-cache"]
	_, rd.noStore = cc["no-store"]
	_, rd.onlyIfCached = cc["only-if-cached"]
	if v, ok := cc["max-age"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			rd.maxAge, rd.hasMaxAge = time.Duration(n)*time.Second, true
		}
	}
	if v, ok := cc["max-stale"]; ok {
		rd.hasMaxStale = true
		if v == "" {
			// max-stale with no value accepts any staleness at all.
			rd.maxStaleAny = true
		} else if n, err := strconv.Atoi(v); err == nil {
			rd.maxStale = time.Duration(n) * time.Second
		}
	}
	if v, ok := cc["min-fresh"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			rd.minFresh, rd.hasMinFresh = time.Duration(n)*time.Second, true
		}
	}
	return rd
}

// usableWithoutValidation decides whether a stored entry may answer this
// request directly. The response's own constraints come first (its no-cache,
// its lifetime), then the request's: each directive can only make the cache
// LESS willing, except max-stale, which widens the window for an entry that is
// merely stale — never for one whose origin said must-revalidate.
func usableWithoutValidation(e *cacheEntry, rd reqDirectives, now time.Time) bool {
	cc := directives(e.header)
	if _, ok := cc["no-cache"]; ok {
		return false
	}
	age, lifetime := e.age(now), e.freshnessLifetime()
	usable := age < lifetime
	if rd.hasMaxAge && age > rd.maxAge {
		usable = false
	}
	if rd.hasMinFresh && lifetime-age < rd.minFresh {
		usable = false
	}
	if !usable && rd.hasMaxStale {
		if _, must := cc["must-revalidate"]; !must && (rd.maxStaleAny || age-lifetime <= rd.maxStale) {
			usable = true
		}
	}
	return usable
}

// gatewayTimeout is the answer RFC 9111 gives an only-if-cached request that
// nothing stored can satisfy: a synthesized 504, not a network attempt.
func gatewayTimeout(req *http.Request) *http.Response {
	return &http.Response{
		Status:        "504 Gateway Timeout",
		StatusCode:    http.StatusGatewayTimeout,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{},
		Body:          io.NopCloser(bytes.NewReader(nil)),
		ContentLength: 0,
		Request:       req,
	}
}

// invalidate drops the entries a successful unsafe request makes stale: the
// request's own URL, and any same-origin Location / Content-Location target the
// response names — those are the resources the origin just said it changed.
func (c *responseCache) invalidate(req *http.Request, resp *http.Response) {
	drop := func(u *url.URL) {
		if u == nil || u.Hostname() != req.URL.Hostname() {
			return
		}
		key := http.MethodGet + " " + u.String()
		c.mu.Lock()
		for _, old := range c.entries[key] {
			c.bytes -= len(old.body)
		}
		delete(c.entries, key)
		c.mu.Unlock()
	}
	drop(req.URL)
	for _, name := range []string{"Location", "Content-Location"} {
		if v := resp.Header.Get(name); v != "" {
			if u, err := req.URL.Parse(v); err == nil {
				drop(u)
			}
		}
	}
}

func (t *cachingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	mode := cacheModeOf(req)
	rd := parseReqDirectives(req.Header)
	consult := mode != cacheNoStore && mode != cacheReload && !rd.noStore
	store := mode != cacheNoStore && !rd.noStore
	if isConditional(req.Header) {
		// The caller wrote its own validator: it wants the origin's answer, and a
		// stored response is not that answer.
		consult, store = false, false
	}

	var entry *cacheEntry
	if consult && (req.Method == http.MethodGet || req.Method == http.MethodHead) {
		entry = t.cache.get(req)
	}
	if entry != nil {
		switch mode {
		case cacheForceCache, cacheOnlyIfCached:
			// Both take a stored response however stale it is; they differ only in
			// what happens when there is none.
			return entry.serve(req), nil
		case cacheDefault:
			// The request's own only-if-cached directive takes whatever is stored;
			// its no-cache forbids answering without revalidating, whatever the
			// entry's freshness.
			if rd.onlyIfCached {
				return entry.serve(req), nil
			}
			now := time.Now()
			if !rd.noCache && usableWithoutValidation(entry, rd, now) {
				return entry.serve(req), nil
			}
			// stale-while-revalidate (RFC 5861): within the window the response
			// granted, a stale entry is served IMMEDIATELY and the revalidation
			// happens behind the caller's back — that trade of staleness for
			// latency is the whole point of the directive. Only when the request
			// itself imposes no freshness demands of its own.
			if !rd.noCache && !rd.hasMaxAge && !rd.hasMinFresh {
				if swr, ok := staleWhileRevalidate(entry.header); ok &&
					entry.age(now)-entry.freshnessLifetime() <= swr {
					t.revalidateInBackground(req, entry)
					return entry.serve(req), nil
				}
			}
		}
	}
	if entry == nil && mode == cacheOnlyIfCached {
		// The standard makes this a network error rather than an empty response:
		// the caller asked for the cache and there is nothing there.
		return nil, errOnlyIfCached
	}
	if entry == nil && rd.onlyIfCached && mode != cacheNoStore && mode != cacheReload {
		// As a HEADER, only-if-cached answers differently than the fetch MODE
		// does: HTTP's own rule is a synthesized 504, not an error.
		return gatewayTimeout(req), nil
	}

	out := req.Clone(req.Context())
	addModeHeaders(out, mode)
	// A stale entry with a validator is revalidated rather than refetched, which
	// is the whole economy of an HTTP cache: the server answers 304 and no body
	// crosses the network.
	if entry != nil && mode != cacheReload {
		if etag := entry.header.Get("ETag"); etag != "" && out.Header.Get("If-None-Match") == "" {
			out.Header.Set("If-None-Match", etag)
		} else if lm := entry.header.Get("Last-Modified"); lm != "" && out.Header.Get("If-Modified-Since") == "" {
			out.Header.Set("If-Modified-Since", lm)
		}
	}

	requested := time.Now()
	resp, err := t.next.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	// A successful unsafe request invalidates what it changed: its own URL, and
	// the Location / Content-Location resources the response names.
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
	default:
		if resp.StatusCode < 400 {
			t.cache.invalidate(req, resp)
		}
	}
	if resp.StatusCode == http.StatusNotModified && entry != nil {
		// Freshen the stored headers and serve the stored body. The response's
		// own headers win where it sends them, since they are the newer word.
		// The freshening builds a NEW entry rather than mutating the stored one:
		// another request may be serving that entry's headers right now.
		resp.Body.Close()
		fresh := entry.freshened(resp.Header, requested)
		if store {
			t.cache.put(req, fresh)
		}
		return fresh.serve(req), nil
	}
	if !store || !storable(req, resp) {
		return resp, nil
	}
	// The body is stored as the caller reads it, not read ahead of them. Reading
	// it here first would turn every cacheable response into a buffered one, which
	// is exactly what a streaming proxy must not do — and a cache is no reason for
	// the first byte to wait for the last.
	stored := &cacheEntry{
		status:    resp.StatusCode,
		header:    resp.Header.Clone(),
		requested: requested,
		received:  time.Now(),
	}
	resp.Body = &cacheFiller{
		rc:    resp.Body,
		limit: maxEntryBytes,
		// The declared length is how completeness is recognised without an EOF: a
		// consumer that reads exactly Content-Length bytes and closes never sees
		// one, and that consumer is the common case here.
		expect: resp.ContentLength,
		done: func(body []byte) {
			stored.body = body
			t.cache.put(req, stored)
		},
	}
	return resp, nil
}

// maxEntryBytes caps one stored body. A response larger than this streams
// through unstored: holding it would evict everything else to cache one thing.
const maxEntryBytes = maxCacheBytes / 4

// cacheFiller copies what the caller reads into a buffer, and stores the entry
// once the body has been read to its end. A body that errors, or outgrows the
// cap, is simply not stored — the caller's read is unaffected either way.
type cacheFiller struct {
	rc     io.ReadCloser
	buf    bytes.Buffer
	limit  int
	expect int64 // the declared Content-Length, or -1 when unknown
	done   func([]byte)
	giveUp bool
	stored bool
}

// finish stores the body, once. A body that errored, outgrew the cap, or is
// short of its declared length is not stored: half a response is not a response.
func (f *cacheFiller) finish() {
	if f.giveUp || f.stored {
		return
	}
	f.stored = true
	f.done(append([]byte(nil), f.buf.Bytes()...))
}

func (f *cacheFiller) Read(p []byte) (int, error) {
	n, err := f.rc.Read(p)
	if n > 0 && !f.giveUp {
		if f.buf.Len()+n > f.limit {
			f.giveUp = true
			f.buf.Reset()
		} else {
			f.buf.Write(p[:n])
		}
	}
	if err == io.EOF {
		f.finish()
	} else if err != nil {
		f.giveUp = true
	} else if f.expect >= 0 && int64(f.buf.Len()) >= f.expect {
		// Every byte the response promised has arrived; a consumer that stops here
		// never calls Read again.
		f.finish()
	}
	return n, err
}

func (f *cacheFiller) Close() error {
	if f.expect >= 0 && int64(f.buf.Len()) >= f.expect {
		f.finish()
	}
	return f.rc.Close()
}
