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
// Current subset (grown as the flagships demand): the call is synchronous
// under the hood — fetch returns an already-settled promise, so two fetches
// in a Promise.all run sequentially; response headers expose get/has;
// init.signal is honored only when already aborted at call time.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// fetchAPI owns the Go side of fetch: the permission-enforcing HTTP client
// (built lazily from the Config the first call carries), the guest builtins
// it keeps resolved, and the set of response bodies still open.
type fetchAPI struct {
	js         *spidermonkey.JS
	promiseCls *spidermonkey.Object // Promise constructor
	jsonObj    *spidermonkey.Object // JSON namespace object
	streamCls  *spidermonkey.Object // ReadableStream class (from builtins.js)

	clientOnce sync.Once
	client     *http.Client

	mu   sync.Mutex
	open map[*fetchStream]struct{}
}

// fetchStream is one response body: the io.Reader the Go-provided pull/cancel
// closures drive.
type fetchStream struct {
	api  *fetchAPI
	body io.ReadCloser
	buf  []byte
	done bool
}

func installFetch(js *spidermonkey.JS) (*fetchAPI, error) {
	a := &fetchAPI{js: js, open: map[*fetchStream]struct{}{}}
	for name, dst := range map[string]**spidermonkey.Object{
		"Promise":        &a.promiseCls,
		"JSON":           &a.jsonObj,
		"ReadableStream": &a.streamCls,
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
	if err := js.Global().DefineFunc("fetch", a.fetchFunc); err != nil {
		return nil, err
	}
	return a, nil
}

// newHTTPClient builds the transport that enforces Config.Resolve (per
// hostname) and Config.Dial (per resolved address, so a DNS answer cannot
// smuggle a connection past the allow-list).
func newHTTPClient(cfg spidermonkey.Config) *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		port, _ := strconv.Atoi(portStr)
		if ip := net.ParseIP(host); ip != nil {
			// Literal IP: no name was resolved, so host is "".
			if cfg.Dial != nil && !cfg.Dial(network, "", ip.String(), port) {
				return nil, fmt.Errorf("dial %s: permission denied", addr)
			}
			return dialer.DialContext(ctx, network, addr)
		}
		if cfg.Resolve != nil && !cfg.Resolve(host) {
			return nil, fmt.Errorf("resolve %q: permission denied", host)
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			if cfg.Dial != nil && !cfg.Dial(network, host, ip.String(), port) {
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
	return &http.Client{Transport: &http.Transport{DialContext: dial}}
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
		if cfg.Dial != nil && !cfg.Dial("tcp", "", ip.String(), port) {
			return fmt.Errorf("dial %s:%d: permission denied", host, port)
		}
		return nil
	}
	if cfg.Resolve != nil && !cfg.Resolve(host) {
		return fmt.Errorf("resolve %q: permission denied", host)
	}
	return nil
}

// promise wraps v via the guest's own Promise[method] (resolve | reject).
func (a *fetchAPI) promise(method string, v spidermonkey.Value) (spidermonkey.Value, error) {
	return a.promiseCls.CallMethod(method, v)
}

// fetchFunc is globalThis.fetch: (input, init?) => Promise<Response>. A
// transport failure resolves to a REJECTED promise (fetch semantics), not a
// throw.
func (a *fetchAPI) fetchFunc(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	a.clientOnce.Do(func() { a.client = newHTTPClient(cfg) })
	if len(args) < 1 {
		return nil, fmt.Errorf("fetch: an input URL is required")
	}
	url := args[0].String()
	method := "GET"
	var reqBody io.Reader
	headers := map[string]string{}

	if len(args) > 1 && args[1].IsObject() {
		init := args[1].Object()
		defer init.Free()
		if v, err := init.Get("method"); err == nil && !v.IsUndefined() {
			method = strings.ToUpper(v.String())
		}
		if v, err := init.Get("signal"); err == nil {
			if o := v.Object(); o != nil {
				aborted, _ := o.Get("aborted")
				reason, _ := o.Get("reason")
				o.Free()
				if aborted != nil && aborted.Bool() {
					msg := "The operation was aborted"
					if reason != nil && !reason.IsUndefined() {
						msg = reason.String()
					}
					return a.promise("reject", spidermonkey.ValueOf(msg))
				}
			}
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
	resp, err := a.client.Do(req)
	if err != nil {
		return a.promise("reject", spidermonkey.ValueOf(err.Error()))
	}

	respObj, err := a.newResponse(resp)
	if err != nil {
		return nil, err
	}
	return a.promise("resolve", respObj)
}

// newResponse builds the Response as a guest object composed entirely from
// Go: data fields via Set, behavior via Go closures over the http.Response,
// and the body via `new ReadableStream(source)` where source.pull/cancel are
// Go closures.
func (a *fetchAPI) newResponse(resp *http.Response) (*spidermonkey.Object, error) {
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
		"redirected": spidermonkey.ValueOf(false),
		"bodyUsed":   spidermonkey.ValueOf(false),
	}
	for name, v := range fields {
		if err := r.Set(name, v); err != nil {
			return nil, err
		}
	}

	// headers.get / headers.has: Go closures over the live http.Header —
	// case-insensitivity for free, no copying.
	hdr := resp.Header
	headersObj, err := js.NewObject()
	if err != nil {
		return nil, err
	}
	if err := headersObj.DefineFunc("get", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		if len(args) < 1 || len(hdr.Values(args[0].String())) == 0 {
			return spidermonkey.Null(), nil
		}
		return spidermonkey.ValueOf(hdr.Get(args[0].String())), nil
	}); err != nil {
		return nil, err
	}
	if err := headersObj.DefineFunc("has", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		return spidermonkey.ValueOf(len(args) > 0 && len(hdr.Values(args[0].String())) > 0), nil
	}); err != nil {
		return nil, err
	}
	if err := r.Set("headers", headersObj); err != nil {
		return nil, err
	}
	headersObj.Free()

	// body: new ReadableStream({ pull, cancel }) with Go closures.
	st := &fetchStream{api: a, body: resp.Body}
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
	consumeU8 := func() (*spidermonkey.Object, error) {
		data, err := st.readAll()
		if err != nil {
			return nil, err
		}
		return js.NewBytes(data)
	}
	consumers := map[string]spidermonkey.Func{
		"bytes": func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
			u8, err := consumeU8()
			if err != nil {
				return nil, err
			}
			defer u8.Free()
			return a.promise("resolve", u8)
		},
		"arrayBuffer": func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
			u8, err := consumeU8()
			if err != nil {
				return nil, err
			}
			defer u8.Free()
			buf, err := u8.Get("buffer")
			if err != nil {
				return nil, err
			}
			return a.promise("resolve", buf)
		},
		"text": func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
			data, err := st.readAll()
			if err != nil {
				return nil, err
			}
			return a.promise("resolve", spidermonkey.ValueOf(string(data)))
		},
		"json": func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
			data, err := st.readAll()
			if err != nil {
				return nil, err
			}
			parsed, perr := a.jsonObj.CallMethod("parse", spidermonkey.ValueOf(string(data)))
			if perr != nil {
				return a.promise("reject", spidermonkey.ValueOf(perr.Error()))
			}
			return a.promise("resolve", parsed)
		},
	}
	for name, fn := range consumers {
		if err := r.DefineFunc(name, fn); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// pull is the underlyingSource.pull the guest stream calls with its
// controller: read ONE chunk from the HTTP body and enqueue it as a real
// Uint8Array, or close at EOF.
func (st *fetchStream) pull(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 || args[0].Object() == nil {
		return nil, fmt.Errorf("pull: want the stream controller")
	}
	ctrl := args[0].Object()
	defer ctrl.Free() // the stream owns its controller; drop the arg pin
	if st.done {
		_, err := ctrl.CallMethod("close")
		return spidermonkey.Undefined(), err
	}
	if st.buf == nil {
		st.buf = make([]byte, 32<<10)
	}
	n, err := st.body.Read(st.buf) // blocks until a chunk arrives, EOF, or an error
	if n > 0 {
		u8, uerr := st.api.js.NewBytes(st.buf[:n]) // host -> guest binary
		if uerr != nil {
			return nil, uerr
		}
		_, cerr := ctrl.CallMethod("enqueue", u8)
		u8.Free() // the stream queue references the array now
		if cerr != nil {
			return nil, cerr
		}
		return spidermonkey.Undefined(), nil
	}
	if err == io.EOF {
		st.finish()
		_, cerr := ctrl.CallMethod("close")
		return spidermonkey.Undefined(), cerr
	}
	if err != nil {
		st.finish()
		return nil, err // throws -> the pending read() rejects
	}
	return nil, fmt.Errorf("response body read made no progress")
}

// cancel is the underlyingSource.cancel: the guest is done with the body.
func (st *fetchStream) cancel(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	st.finish()
	return spidermonkey.Undefined(), nil
}

// readAll drains the remaining body to EOF (the bytes/text/json path).
func (st *fetchStream) readAll() ([]byte, error) {
	if st.done {
		return nil, nil
	}
	data, err := io.ReadAll(st.body)
	st.finish()
	return data, err
}

func (st *fetchStream) finish() {
	a := st.api
	a.mu.Lock()
	defer a.mu.Unlock()
	if st.done {
		return
	}
	st.done = true
	st.body.Close()
	delete(a.open, st)
}

// openStreams reports how many response bodies are still open.
func (a *fetchAPI) openStreams() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.open)
}

func (a *fetchAPI) closeAll() {
	a.mu.Lock()
	streams := make([]*fetchStream, 0, len(a.open))
	for st := range a.open {
		streams = append(streams, st)
	}
	a.mu.Unlock()
	for _, st := range streams {
		st.finish()
	}
	for _, o := range []*spidermonkey.Object{a.promiseCls, a.jsonObj, a.streamCls} {
		if o != nil {
			o.Free()
		}
	}
}
