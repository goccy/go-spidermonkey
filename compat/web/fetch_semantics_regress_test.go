package web_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestFetchRedirectFollow verifies redirect:"follow" (the default) follows the
// hop, exposes the FINAL URL, and reports redirected=true.
func TestFetchRedirectFollow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a":
			http.Redirect(w, r, "/b", http.StatusFound)
		case "/b":
			w.Write([]byte("dest-b"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	js, w := newWeb(t, spidermonkey.Config{})
	eval(t, js, `globalThis.BASE = `+fmt.Sprintf("%q", srv.URL)+`; globalThis.__r = {};
		fetch(BASE + "/a").then(res => {
			__r.redirected = res.redirected;
			__r.url = res.url;
			__r.status = res.status;
			return res.text();
		}).then(t => { __r.text = t; }).catch(e => { __r.err = String(e && e.message || e); });`)
	drainWeb(t, w)
	if got := evalString(t, js, `__r.err ?? ""`); got != "" {
		t.Fatalf("threw: %s", got)
	}
	if got := evalString(t, js, `String(__r.redirected)`); got != "true" {
		t.Errorf("redirected = %s, want true", got)
	}
	if got := evalString(t, js, `__r.text`); got != "dest-b" {
		t.Errorf("text = %q, want dest-b", got)
	}
	if got := evalString(t, js, `__r.url`); !strings.HasSuffix(got, "/b") {
		t.Errorf("url = %q, want final URL ending /b", got)
	}
	if got := evalString(t, js, `String(__r.status)`); got != "200" {
		t.Errorf("status = %s, want 200", got)
	}
}

// TestFetchRedirectManual verifies redirect:"manual" does NOT follow: the 3xx is
// returned as-is with its Location header visible and redirected=false.
func TestFetchRedirectManual(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a":
			http.Redirect(w, r, "/b", http.StatusFound)
		case "/b":
			w.Write([]byte("dest-b"))
		}
	}))
	defer srv.Close()

	js, w := newWeb(t, spidermonkey.Config{})
	eval(t, js, `globalThis.BASE = `+fmt.Sprintf("%q", srv.URL)+`; globalThis.__r = {};
		fetch(BASE + "/a", { redirect: "manual" }).then(res => {
			__r.status = res.status;
			__r.redirected = res.redirected;
			__r.location = res.headers.get("location");
		}).catch(e => { __r.err = String(e && e.message || e); });`)
	drainWeb(t, w)
	if got := evalString(t, js, `__r.err ?? ""`); got != "" {
		t.Fatalf("threw: %s", got)
	}
	if got := evalString(t, js, `String(__r.status)`); got != "302" {
		t.Errorf("status = %s, want 302 (not followed)", got)
	}
	if got := evalString(t, js, `String(__r.redirected)`); got != "false" {
		t.Errorf("redirected = %s, want false", got)
	}
	if got := evalString(t, js, `__r.location`); !strings.HasSuffix(got, "/b") {
		t.Errorf("location = %q, want the Location header ending /b", got)
	}
}

// TestFetchRedirectError verifies redirect:"error" rejects with a TypeError when
// a redirect is encountered.
func TestFetchRedirectError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/a" {
			http.Redirect(w, r, "/b", http.StatusFound)
			return
		}
		w.Write([]byte("dest-b"))
	}))
	defer srv.Close()

	js, w := newWeb(t, spidermonkey.Config{})
	eval(t, js, `globalThis.BASE = `+fmt.Sprintf("%q", srv.URL)+`; globalThis.__r = {};
		fetch(BASE + "/a", { redirect: "error" })
			.then(() => { __r.outcome = "resolved"; })
			.catch(e => { __r.outcome = "rejected"; __r.isType = e instanceof TypeError; __r.name = e.name; });`)
	drainWeb(t, w)
	if got := evalString(t, js, `__r.outcome`); got != "rejected" {
		t.Fatalf("outcome = %q, want rejected", got)
	}
	if got := evalString(t, js, `String(__r.isType)`); got != "true" {
		t.Errorf("rejection instanceof TypeError = %s, want true", got)
	}
}

// TestFetchRedirectStripsAuthorizationCrossOrigin verifies Authorization is
// stripped on a cross-origin redirect (undici/fetch semantics; origin includes
// the port) but preserved on a same-origin redirect.
func TestFetchRedirectStripsAuthorizationCrossOrigin(t *testing.T) {
	var upstreamAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth = r.Header.Get("Authorization")
		w.Write([]byte("upstream"))
	}))
	defer upstream.Close()

	var sameAuth string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cross":
			http.Redirect(w, r, upstream.URL+"/end", http.StatusFound)
		case "/same":
			http.Redirect(w, r, "/same-end", http.StatusFound)
		case "/same-end":
			sameAuth = r.Header.Get("Authorization")
			w.Write([]byte("same"))
		}
	}))
	defer origin.Close()

	js, w := newWeb(t, spidermonkey.Config{})
	eval(t, js, `globalThis.ORIGIN = `+fmt.Sprintf("%q", origin.URL)+`; globalThis.__r = {};
		Promise.all([
			fetch(ORIGIN + "/cross", { headers: { Authorization: "Bearer secret" } }).then(r => r.text()),
			fetch(ORIGIN + "/same", { headers: { Authorization: "Bearer secret" } }).then(r => r.text()),
		]).then(() => { __r.done = true; }).catch(e => { __r.err = String(e && e.message || e); });`)
	drainWeb(t, w)
	if got := evalString(t, js, `__r.err ?? ""`); got != "" {
		t.Fatalf("threw: %s", got)
	}
	if upstreamAuth != "" {
		t.Errorf("cross-origin Authorization = %q, want stripped (empty)", upstreamAuth)
	}
	if sameAuth != "Bearer secret" {
		t.Errorf("same-origin Authorization = %q, want preserved (Bearer secret)", sameAuth)
	}
}

// TestFetchAbortPreAborted verifies a signal already aborted at call time rejects
// with a DOMException whose name is "AbortError" (not a plain string).
func TestFetchAbortPreAborted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("never reached"))
	}))
	defer srv.Close()

	js, w := newWeb(t, spidermonkey.Config{})
	eval(t, js, `globalThis.BASE = `+fmt.Sprintf("%q", srv.URL)+`; globalThis.__r = {};
		const c = new AbortController();
		c.abort();
		fetch(BASE, { signal: c.signal })
			.then(() => { __r.outcome = "resolved"; })
			.catch(e => { __r.outcome = "rejected"; __r.name = e.name; __r.isDOM = e instanceof DOMException; });`)
	drainWeb(t, w)
	if got := evalString(t, js, `__r.outcome`); got != "rejected" {
		t.Fatalf("outcome = %q, want rejected", got)
	}
	if got := evalString(t, js, `__r.name`); got != "AbortError" {
		t.Errorf("rejection name = %q, want AbortError", got)
	}
	if got := evalString(t, js, `String(__r.isDOM)`); got != "true" {
		t.Errorf("rejection instanceof DOMException = %s, want true", got)
	}
}

// TestFetchAbortMidFlight verifies a signal aborted WHILE the request is in
// flight cancels it and rejects with a DOMException AbortError, and that a
// normally-completing fetch removes its abort listener (no leak).
func TestFetchAbortMidFlight(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fast" {
			w.Write([]byte("fast-ok"))
			return
		}
		// Slow endpoint: block until the client aborts (ctx cancelled) or 5s.
		select {
		case <-time.After(5 * time.Second):
			w.Write([]byte("late"))
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	js, w := newWeb(t, spidermonkey.Config{})
	eval(t, js, `globalThis.BASE = `+fmt.Sprintf("%q", srv.URL)+`; globalThis.__r = {};
		const c = new AbortController();
		setTimeout(() => c.abort(), 30);
		fetch(BASE + "/slow", { signal: c.signal })
			.then(() => { __r.outcome = "resolved"; })
			.catch(e => { __r.outcome = "rejected"; __r.name = e.name; __r.isDOM = e instanceof DOMException; });

		// A normally-completing fetch must detach its abort listener.
		const c2 = new AbortController();
		fetch(BASE + "/fast", { signal: c2.signal }).then(r => r.text()).then(txt => {
			__r.normal = txt;
			const list = c2.signal._listeners.get("abort");
			__r.listenersAfter = list ? list.length : 0;
		});`)
	drainWeb(t, w)
	if got := evalString(t, js, `__r.outcome`); got != "rejected" {
		t.Fatalf("mid-flight outcome = %q, want rejected", got)
	}
	if got := evalString(t, js, `__r.name`); got != "AbortError" {
		t.Errorf("mid-flight rejection name = %q, want AbortError", got)
	}
	if got := evalString(t, js, `String(__r.isDOM)`); got != "true" {
		t.Errorf("mid-flight rejection instanceof DOMException = %s, want true", got)
	}
	if got := evalString(t, js, `__r.normal`); got != "fast-ok" {
		t.Errorf("normal fetch text = %q, want fast-ok", got)
	}
	if got := evalString(t, js, `String(__r.listenersAfter)`); got != "0" {
		t.Errorf("abort listeners after normal completion = %s, want 0 (leak)", got)
	}
}

// TestFetchResponseHeadersAreRealHeaders verifies the response's headers is a
// genuine Headers instance: the full prototype works, new Headers(res.headers)
// round-trips every header, iteration works, and multiple Set-Cookie survive.
func TestFetchResponseHeadersAreRealHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("X-Multi", "a")
		w.Header().Add("X-Multi", "b")
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Add("Set-Cookie", "a=1")
		w.Header().Add("Set-Cookie", "b=2")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	js, ww := newWeb(t, spidermonkey.Config{})
	eval(t, js, `globalThis.BASE = `+fmt.Sprintf("%q", srv.URL)+`; globalThis.__r = {};
		fetch(BASE).then(res => {
			const h = res.headers;
			__r.isHeaders = h instanceof Headers;
			__r.multi = h.get("x-multi");
			__r.hasCT = h.has("content-type");
			const seen = []; h.forEach((v, k) => { if (k === "x-multi") seen.push(v); });
			__r.forEach = seen.join(",");
			__r.keys = [...h.keys()].includes("x-multi");
			__r.values = [...h.values()].includes("a, b");
			__r.iter = [...h].some(([k]) => k === "x-multi");
			__r.cookies = h.getSetCookie().join("||");
			const copy = new Headers(res.headers);
			__r.copyMulti = copy.get("x-multi");
			__r.copyCookies = copy.getSetCookie().join("||");
			// mutators operate on the instance
			h.append("x-added", "1"); h.set("x-set", "2"); h.delete("content-type");
			__r.afterMut = [h.has("content-type"), h.get("x-added"), h.get("x-set")].join("|");
			return res.text();
		}).then(t => { __r.body = t; }).catch(e => { __r.err = String(e && e.stack || e); });`)
	drainWeb(t, ww)
	if got := evalString(t, js, `__r.err ?? ""`); got != "" {
		t.Fatalf("threw: %s", got)
	}
	checks := []struct{ expr, want, desc string }{
		{`String(__r.isHeaders)`, "true", "res.headers instanceof Headers"},
		{`__r.multi`, "a, b", "get combines multiple values"},
		{`String(__r.hasCT)`, "true", "has(content-type)"},
		{`__r.forEach`, "a, b", "forEach yields combined value"},
		{`String(__r.keys)`, "true", "keys() includes x-multi"},
		{`String(__r.values)`, "true", "values() includes combined value"},
		{`String(__r.iter)`, "true", "[...headers] iterates"},
		{`__r.cookies`, "a=1||b=2", "getSetCookie keeps cookies separate"},
		{`__r.copyMulti`, "a, b", "new Headers(res.headers) round-trips multi"},
		{`__r.copyCookies`, "a=1||b=2", "new Headers(res.headers) round-trips cookies"},
		{`__r.afterMut`, "false|1|2", "append/set/delete mutate the instance"},
		{`__r.body`, "ok", "body still readable"},
	}
	for _, c := range checks {
		if got := evalString(t, js, c.expr); got != c.want {
			t.Errorf("%s: %s = %q, want %q", c.desc, c.expr, got, c.want)
		}
	}
}

// TestFetchURLCredentialsRejected verifies a URL carrying credentials is rejected
// with a TypeError (undici/Node semantics — NOT converted to Basic auth), both at
// fetch() and at new Request() construction.
func TestFetchURLCredentialsRejected(t *testing.T) {
	js, w := newWeb(t, spidermonkey.Config{})
	eval(t, js, `globalThis.__r = {};
		try { new Request("http://user:pass@example.com/"); __r.reqCtor = "no-throw"; }
		catch (e) { __r.reqCtor = e.name; }
		fetch("http://user:pass@example.com/")
			.then(() => { __r.outcome = "resolved"; })
			.catch(e => { __r.outcome = "rejected"; __r.name = e.name; __r.isType = e instanceof TypeError; __r.msg = e.message; });`)
	drainWeb(t, w)
	if got := evalString(t, js, `__r.reqCtor`); got != "TypeError" {
		t.Errorf("new Request(creds) = %q, want TypeError thrown", got)
	}
	if got := evalString(t, js, `__r.outcome`); got != "rejected" {
		t.Fatalf("fetch(creds) outcome = %q, want rejected", got)
	}
	if got := evalString(t, js, `String(__r.isType)`); got != "true" {
		t.Errorf("fetch(creds) rejection instanceof TypeError = %s, want true", got)
	}
	if got := evalString(t, js, `__r.msg`); !strings.Contains(got, "credentials") {
		t.Errorf("fetch(creds) message = %q, want mention of credentials", got)
	}
}
