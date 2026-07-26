package web_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// newEchoServer reflects request headers and body back so a test can assert what
// the fetch layer actually sent.
func newEchoServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/echo-headers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo-Foo", r.Header.Get("X-Foo"))
		w.Header().Set("X-Echo-Count", r.Header.Get("X-Count"))
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/echo-body", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Req-CT", r.Header.Get("Content-Type"))
		w.Write(b)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestFetchHeadersInstanceAndNumericValues verifies fetch accepts a Headers
// instance and non-string header values (WHATWG) instead of throwing at the
// host boundary (the old JSON.stringify-of-a-Map path).
func TestFetchHeadersInstanceAndNumericValues(t *testing.T) {
	srv := newEchoServer(t)
	js, w := newWeb(t, spidermonkey.Config{})
	eval(t, js, `globalThis.BASE = `+fmt.Sprintf("%q", srv.URL))
	eval(t, js, `
		globalThis.__r = {};
		Promise.all([
			fetch(BASE + "/echo-headers", { headers: new Headers({ "X-Foo": "bar" }) })
				.then(res => { __r.foo = res.headers.get("X-Echo-Foo"); }),
			fetch(BASE + "/echo-headers", { headers: { "X-Count": 5 } })
				.then(res => { __r.count = res.headers.get("X-Echo-Count"); }),
		]).catch(e => { __r.err = String(e); });
	`)
	drainWeb(t, w)
	if got := evalString(t, js, `__r.err ?? ""`); got != "" {
		t.Fatalf("fetch threw/rejected: %s", got)
	}
	if got := evalString(t, js, `__r.foo`); got != "bar" {
		t.Errorf("Headers-instance header not sent: X-Echo-Foo = %q", got)
	}
	if got := evalString(t, js, `__r.count`); got != "5" {
		t.Errorf("numeric header value not coerced/sent: X-Echo-Count = %q", got)
	}
}

// TestFetchURLSearchParamsBody verifies a URLSearchParams body is extracted to
// application/x-www-form-urlencoded instead of throwing at the host boundary.
func TestFetchURLSearchParamsBody(t *testing.T) {
	srv := newEchoServer(t)
	js, w := newWeb(t, spidermonkey.Config{})
	eval(t, js, `globalThis.BASE = `+fmt.Sprintf("%q", srv.URL))
	eval(t, js, `
		globalThis.__r = {};
		fetch(BASE + "/echo-body", { method: "POST", body: new URLSearchParams({ q: "hi there", n: "2" }) })
			.then(res => { __r.ct = res.headers.get("X-Req-CT"); return res.text(); })
			.then(t => { __r.body = t; })
			.catch(e => { __r.err = String(e); });
	`)
	drainWeb(t, w)
	if got := evalString(t, js, `__r.err ?? ""`); got != "" {
		t.Fatalf("fetch threw/rejected: %s", got)
	}
	if got := evalString(t, js, `__r.ct`); !strings.HasPrefix(got, "application/x-www-form-urlencoded") {
		t.Errorf("content-type = %q, want application/x-www-form-urlencoded", got)
	}
	if got := evalString(t, js, `__r.body`); got != "q=hi+there&n=2" {
		t.Errorf("urlencoded body = %q, want q=hi+there&n=2", got)
	}
}

// TestFetchBodyUsedAndDoubleRead verifies Response.bodyUsed flips to true after a
// consume and a second body read rejects (TypeError) instead of resolving empty.
func TestFetchBodyUsedAndDoubleRead(t *testing.T) {
	srv := newEchoServer(t)
	js, w := newWeb(t, spidermonkey.Config{})
	eval(t, js, `globalThis.BASE = `+fmt.Sprintf("%q", srv.URL))
	eval(t, js, `
		globalThis.__r = {};
		fetch(BASE + "/echo-headers").then(async res => {
			__r.first = await res.text();
			__r.used = res.bodyUsed;
			try { await res.text(); __r.second = "resolved"; }
			catch (e) { __r.second = "rejected:" + (e && e.name); }
		}).catch(e => { __r.err = String(e); });
	`)
	drainWeb(t, w)
	if got := evalString(t, js, `__r.err ?? ""`); got != "" {
		t.Fatalf("fetch threw/rejected: %s", got)
	}
	if got := evalString(t, js, `String(__r.used)`); got != "true" {
		t.Errorf("bodyUsed after consume = %q, want true", got)
	}
	if got := evalString(t, js, `__r.second`); !strings.HasPrefix(got, "rejected") {
		t.Errorf("second body read = %q, want a rejection", got)
	}
}

// TestFetchNormalizationErrorRejects verifies a fetch whose init fails
// normalization (a malformed headers init) returns a REJECTED promise rather
// than throwing synchronously — a sync throw is uncatchable by fetch(...).catch.
func TestFetchNormalizationErrorRejects(t *testing.T) {
	js, w := newWeb(t, spidermonkey.Config{})
	// If fetch threw synchronously, this eval would throw and fail the test at the
	// eval boundary; instead the throw must become a rejection the .catch sees.
	eval(t, js, `
		globalThis.__r = "pending";
		fetch("http://example.invalid/", { headers: [["only-one-element"]] })
			.then(() => { __r = "resolved"; })
			.catch(() => { __r = "rejected"; });
	`)
	drainWeb(t, w)
	if got := evalString(t, js, `__r`); got != "rejected" {
		t.Errorf("fetch with a malformed headers init = %q, want rejected (not a sync throw)", got)
	}
}

// TestResponseWhatwgFixes covers several WHATWG Response/URL/File conformance
// fixes: json() content-type override, redirect()/status validation, URL port
// parsing, guest-body double-read guard, and File.lastModified default.
func TestResponseWhatwgFixes(t *testing.T) {
	js, w := newWeb(t, spidermonkey.Config{})
	eval(t, js, `
		globalThis.__r = {};
		(async () => {
			__r.problemCT = Response.json({}, { headers: { "content-type": "application/problem+json" } }).headers.get("content-type");
			__r.defaultCT = Response.json({}).headers.get("content-type");

			try { Response.redirect("http://x/", 200); __r.badRedirect = "no-throw"; } catch (e) { __r.badRedirect = e.name; }
			__r.goodLoc = Response.redirect("http://x/y", 302).headers.get("location");

			try { new Response("x", { status: 700 }); __r.badStatus = "no-throw"; } catch (e) { __r.badStatus = e.name; }
			__r.okStatus = new Response("x", { status: 503 }).status;

			const u = new URL("http://h:8080/");
			u.port = "abc"; __r.portAbc = u.port;      // ignored -> stays 8080
			u.port = "99999"; __r.portBig = u.port;    // >65535 ignored -> 8080
			u.port = "9090"; __r.portOk = u.port;      // valid

			const rq = new Request("http://x/", { method: "POST", body: "hi" });
			await rq.text();
			try { await rq.text(); __r.doubleRead = "resolved"; } catch (e) { __r.doubleRead = e.name; }

			__r.fileMtime = new File(["x"], "a.txt").lastModified > 0;
		})().catch(e => { __r.err = String(e && e.message || e); });
	`)
	drainWeb(t, w)
	if got := evalString(t, js, `__r.err ?? ""`); got != "" {
		t.Fatalf("threw: %s", got)
	}
	for _, c := range []struct{ expr, want, msg string }{
		{`__r.problemCT`, "application/problem+json", "json() overrode caller content-type"},
		{`__r.defaultCT`, "application/json", "json() default content-type"},
		{`__r.badRedirect`, "RangeError", "redirect(url,200) should throw RangeError"},
		{`__r.goodLoc`, "http://x/y", "redirect location"},
		{`__r.badStatus`, "RangeError", "new Response status 700 should throw"},
		{`__r.okStatus`, "503", "valid status"},
		{`__r.portAbc`, "8080", "invalid port 'abc' should be ignored"},
		{`__r.portBig`, "8080", "port >65535 should be ignored"},
		{`__r.portOk`, "9090", "valid port"},
		{`__r.doubleRead`, "TypeError", "second body read should throw TypeError"},
		{`__r.fileMtime`, "true", "File.lastModified should default to now"},
	} {
		if got := evalString(t, js, c.expr); got != c.want {
			t.Errorf("%s: got %q, want %q", c.msg, got, c.want)
		}
	}
}

// TestFetchResponseBlobAndType verifies the native fetch Response has a working
// blob() consumer and a type field.
func TestFetchResponseBlobAndType(t *testing.T) {
	srv := newEchoServer(t)
	js, w := newWeb(t, spidermonkey.Config{})
	eval(t, js, `globalThis.BASE = `+fmt.Sprintf("%q", srv.URL)+`;
		globalThis.__r = {};
		fetch(BASE + "/echo-body", { method: "POST", body: "blob-payload" }).then(async (res) => {
			__r.type = res.type;
			const b = await res.blob();
			__r.isBlob = b instanceof Blob;
			__r.blobText = await b.text();
		}).catch(e => { __r.err = String(e && e.message || e); });
	`)
	drainWeb(t, w)
	if got := evalString(t, js, `__r.err ?? ""`); got != "" {
		t.Fatalf("threw: %s", got)
	}
	if got := evalString(t, js, `__r.type`); got != "basic" {
		t.Errorf("res.type = %q, want basic", got)
	}
	if got := evalString(t, js, `String(__r.isBlob)`); got != "true" {
		t.Errorf("res.blob() didn't return a Blob: %q", got)
	}
	if got := evalString(t, js, `__r.blobText`); got != "blob-payload" {
		t.Errorf("blob text = %q, want blob-payload", got)
	}
}

// TestFetchFormDataBinaryFile verifies res.formData() preserves the exact bytes
// of a binary file part (0x80-0x9F must NOT be mangled by the win1252 decoder).
func TestFetchFormDataBinaryFile(t *testing.T) {
	fileBytes := []byte{0x80, 0x85, 0x91, 0xff, 0x00, 0x41}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const boundary = "testbnd123"
		w.Header().Set("Content-Type", "multipart/form-data; boundary="+boundary)
		var b bytes.Buffer
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Disposition: form-data; name=\"field\"\r\n\r\nJosé\r\n") // UTF-8 value
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Disposition: form-data; name=\"file\"; filename=\"bin.dat\"\r\n")
		b.WriteString("Content-Type: application/octet-stream\r\n\r\n")
		b.Write(fileBytes)
		b.WriteString("\r\n--" + boundary + "--\r\n")
		w.Write(b.Bytes())
	}))
	defer srv.Close()

	js, w := newWeb(t, spidermonkey.Config{})
	eval(t, js, `globalThis.BASE = `+fmt.Sprintf("%q", srv.URL)+`;
		globalThis.__r = {};
		fetch(BASE).then((res) => res.formData()).then(async (fd) => {
			__r.field = fd.get("field");
			const f = fd.get("file");
			__r.fileName = f.name;
			__r.bytes = Array.from(new Uint8Array(await f.arrayBuffer())).join(",");
		}).catch((e) => { __r.err = String(e && e.message || e); });
	`)
	drainWeb(t, w)
	if got := evalString(t, js, `__r.err ?? ""`); got != "" {
		t.Fatalf("threw: %s", got)
	}
	if got := evalString(t, js, `__r.field`); got != "José" {
		t.Errorf("field = %q, want José (UTF-8 text field decoded as latin1)", got)
	}
	if got := evalString(t, js, `__r.fileName`); got != "bin.dat" {
		t.Errorf("file name = %q, want bin.dat", got)
	}
	if got := evalString(t, js, `__r.bytes`); got != "128,133,145,255,0,65" {
		t.Errorf("binary file bytes = %q, want 128,133,145,255,0,65 (win1252 corruption)", got)
	}
}

// TestResponseErrorClone verifies Response.error().clone() (status 0) doesn't
// throw the new 200-599 RangeError.
func TestResponseErrorClone(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	if got := evalString(t, js, `
		(() => {
			try { const c = Response.error().clone(); return "status:" + c.status; }
			catch (e) { return "threw:" + e.name; }
		})()
	`); got != "status:0" {
		t.Errorf("Response.error().clone() = %q, want status:0 (RangeError regression)", got)
	}
}

// TestResponseBlobBody verifies a Blob body is read as its bytes (not serialized
// to "[object Blob]"), its type becomes the Content-Type, and response.blob()
// round-trips.
func TestResponseBlobBody(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const r = new Response(new Blob(["hello"], { type: "text/plain" }));
			__c.text = await r.text();
			__c.ctype = r.headers.get("content-type");
			const r2 = new Response(new Blob(["xyz"]));
			const b = await r2.blob();
			__c.blobText = await b.text();
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.err ?? ""`); got != "" {
		t.Fatalf("unexpected error: %s", got)
	}
	if got := evalString(t, js, `__c.text`); got != "hello" {
		t.Errorf("Blob body text = %q, want hello", got)
	}
	if got := evalString(t, js, `__c.ctype`); got != "text/plain" {
		t.Errorf("content-type = %q, want text/plain", got)
	}
	if got := evalString(t, js, `__c.blobText`); got != "xyz" {
		t.Errorf("response.blob() text = %q, want xyz", got)
	}
}

// TestFormDataBody verifies a FormData body serializes to multipart with a
// matching multipart Content-Type (not "[object FormData]").
func TestFormDataBody(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const fd = new FormData();
			fd.append("a", "1");
			fd.append("b", "two");
			const r = new Response(fd);
			__c.ctype = r.headers.get("content-type") || "";
			__c.body = await r.text();
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.err ?? ""`); got != "" {
		t.Fatalf("unexpected error: %s", got)
	}
	if got := evalString(t, js, `__c.ctype.startsWith("multipart/form-data; boundary=") ? "ok" : __c.ctype`); got != "ok" {
		t.Errorf("content-type = %q, want multipart/form-data; boundary=...", got)
	}
	if got := evalString(t, js, `(__c.body.includes('name="a"') && __c.body.includes("two")) ? "ok" : "no"`); got != "ok" {
		t.Errorf("multipart body missing expected parts: %q", evalString(t, js, "__c.body"))
	}
}

// TestHeadersSetCookieIterator verifies iterating Headers yields each Set-Cookie
// as its own entry (not comma-joined, which would corrupt Expires dates).
func TestHeadersSetCookieIterator(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	if got := evalString(t, js, `(() => {
		const h = new Headers();
		h.append("set-cookie", "a=1; Expires=Wed, 09 Jun 2100 10:18:14 GMT");
		h.append("set-cookie", "b=2");
		const cookies = [...h].filter(([k]) => k === "set-cookie").map(([, v]) => v);
		return cookies.length + "|" + cookies[0] + "|" + cookies[1];
	})()`); got != "2|a=1; Expires=Wed, 09 Jun 2100 10:18:14 GMT|b=2" {
		t.Errorf("set-cookie iterator = %q, want two separate entries", got)
	}
}

// TestFormDataHeaderInjection verifies a FormData field name containing quotes /
// CRLF is escaped in the multipart Content-Disposition (no header injection), and
// the boundary is not the old predictable counter.
func TestFormDataHeaderInjection(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const fd = new FormData();
			fd.append('na"me\r\nX-Evil: 1', "val");
			const r = new Response(fd);
			__c.body = await r.text();
			__c.ctype = r.headers.get("content-type");
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.err ?? ""`); got != "" {
		t.Fatalf("unexpected error: %s", got)
	}
	body := evalString(t, js, "__c.body")
	if strings.Contains(body, "\r\nX-Evil: 1") {
		t.Errorf("CRLF in field name was not escaped (header injection): %q", body)
	}
	if !strings.Contains(body, "%0D%0A") || !strings.Contains(body, "%22") {
		t.Errorf("field name special chars were not percent-escaped: %q", body)
	}
	if ct := evalString(t, js, "__c.ctype"); strings.Contains(ct, "GSMFormBoundary0x") {
		t.Errorf("boundary is the old predictable counter: %q", ct)
	}
}
