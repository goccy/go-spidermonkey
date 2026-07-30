package web

// fetchsync.go: the blocking round-trip behind synchronous XMLHttpRequest.
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

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
