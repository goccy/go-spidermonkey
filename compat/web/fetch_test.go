package web_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

// Nothing enforced the cross-origin rules: every fetch resolved regardless of
// mode or of what the server permitted, so a page could read a response it has
// no right to. This pins the four outcomes the Fetch spec defines, plus the
// two headers a user agent is required to send and did not.
func TestFetchCrossOriginRules(t *testing.T) {
	var gotOrigin, gotReferer, gotUA string
	mux := http.NewServeMux()
	mux.HandleFunc("/open", func(w http.ResponseWriter, r *http.Request) {
		gotOrigin, gotReferer, gotUA = r.Header.Get("Origin"), r.Header.Get("Referer"), r.Header.Get("User-Agent")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write([]byte("open"))
	})
	mux.HandleFunc("/closed", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("closed"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	js, err := spidermonkey.New(spidermonkey.Config{
		Resolve: func(host string) bool { return true },
		Dial:    func(network, host, ip string, port int) bool { return true },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	w, err := web.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer w.Close()

	// The environment's own URL. Everything below is cross-origin to it, since
	// the test server is on its own port.
	if _, err := js.Eval(context.Background(), `globalThis.location = { href: "http://example.test/page/index.html", origin: "http://example.test" };`); err != nil {
		t.Fatal(err)
	}

	if _, err := js.Eval(context.Background(), `
		globalThis.__done = (async () => {
			const base = "`+srv.URL+`";
			const out = [];
			const step = async (name, f) => {
				try { out.push(name + "=" + await f()); }
				catch (e) { out.push(name + "!" + (e && e.name ? e.name : String(e))); }
			};
			// A server that permits the origin: the response is readable and its
			// type says it came through CORS.
			await step("cors-allowed", async () => {
				const r = await fetch(base + "/open");
				return r.type + ":" + r.status + ":" + (await r.text());
			});
			// A server that does not: the fetch REJECTS, it does not resolve with
			// an unreadable response.
			await step("cors-denied", async () => {
				const r = await fetch(base + "/closed");
				return "READ:" + (await r.text());
			});
			// no-cors is allowed to make the request but not to see the answer.
			await step("no-cors", async () => {
				const r = await fetch(base + "/closed", { mode: "no-cors" });
				return r.type + ":" + r.status + ":" + ((await r.text()) === "" ? "empty" : "LEAKED");
			});
			// same-origin refuses before the request is made.
			await step("same-origin", async () => {
				const r = await fetch(base + "/open", { mode: "same-origin" });
				return "SENT";
			});
			// A data: URL is fetched by its own scheme handler, not through CORS.
			// Its origin is "null", so judging it by origin rejects every one.
			await step("data-url", async () => {
				const r = await fetch("data:text/plain,hi");
				return r.status + ":" + (await r.text());
			});
			globalThis.__r = out.join(" | ");
		})();
	`); err != nil {
		t.Fatalf("eval: %v", err)
	}
	if err := w.Wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}

	got := evalString(t, js, `String(globalThis.__r)`)
	want := "cors-allowed=cors:200:open | cors-denied!TypeError | no-cors=opaque:0:empty | same-origin!TypeError | data-url=200:hi"
	if got != want {
		t.Errorf("cross-origin rules =\n %s\nwant\n %s", got, want)
	}

	// A user agent states who it is and where it came from.
	if gotOrigin != "http://example.test" {
		t.Errorf("Origin = %q, want http://example.test", gotOrigin)
	}
	// Cross-origin under the default policy: the origin only, never the path.
	// It is written as a URL, so the origin carries its root path — which is
	// exactly what the suite compares against.
	if gotReferer != "http://example.test/" {
		t.Errorf("Referer = %q, want the bare origin (strict-origin-when-cross-origin)", gotReferer)
	}
	if gotUA == "" {
		t.Error("User-Agent was not sent")
	}
}

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

// A header value may contain any byte but NUL, LF and CR as far as fetch is
// concerned, and the Web Platform Tests check that such a value reaches the
// server. net/http's Transport refuses to send it — that rule lives in
// validateHeaders, not in the code that writes the request — so fetch here falls
// back to writing the request itself (see the raw-fetch section of fetch.go).
//
// This cannot be tested through the WPT harness: that harness serves the suite
// with net/http, whose readRequest rejects the same values on ARRIVAL with a
// 400, where the suite's own Python server accepts them. So the listener below
// is a raw one, which is the only way to observe what actually goes on the wire.

// rawEchoListener accepts one HTTP/1.1 request, records its header block
// verbatim, and answers 200. It parses nothing: the point is to see the bytes.
func rawEchoListener(t *testing.T) (addr string, headers <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ch := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		br := bufio.NewReader(conn)
		var b strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				break
			}
			b.WriteString(line)
			if line == "\r\n" || line == "\n" {
				break
			}
		}
		ch <- b.String()
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"))
	}()
	return ln.Addr().String(), ch
}

func TestFetchSendsControlCharacterHeaderValues(t *testing.T) {
	addr, headers := rawEchoListener(t)

	js, err := spidermonkey.New(spidermonkey.Config{
		Resolve: func(string) bool { return true },
		Dial:    func(string, string, string, int) bool { return true },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	w, err := web.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer w.Close()

	// U+0001 is a legal header value per fetch and is exactly what net/http's
	// Transport refuses; U+0009 must survive as itself in the middle of a value.
	if _, err := js.Eval(context.Background(), `
		globalThis.__done = fetch("http://`+addr+`/", { headers: { "x-ctl": "ab", "x-tab": "c	d" } })
			.then((r) => { globalThis.__status = r.status; })
			.catch((e) => { globalThis.__status = "THREW " + e.message; });
	`); err != nil {
		t.Fatalf("eval: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	st, err := js.Eval(context.Background(), `String(globalThis.__status)`)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Value.String(); got != "200" {
		t.Fatalf("fetch status = %s, want 200", got)
	}

	select {
	case block := <-headers:
		if !strings.Contains(block, "a\x01b") {
			t.Errorf("the header block does not carry the control character:\n%q", block)
		}
		if !strings.Contains(block, "c\td") {
			t.Errorf("the header block does not carry the tab:\n%q", block)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no request arrived")
	}
}

// The HTTP cache is tested by counting what reaches the origin. That is the only
// thing a cache is for: every assertion below is about a request that did or did
// not happen, not about the bytes that came back.

// countingOrigin serves a body that never changes, with the freshness the caller
// asks for, and records every request it sees.
func countingOrigin(t *testing.T, cacheControl string) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var hits, conditional atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			conditional.Add(1)
		}
		w.Header().Set("ETag", `"v1"`)
		if cacheControl != "" {
			w.Header().Set("Cache-Control", cacheControl)
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("body"))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &conditional
}

// fetchTwice runs two fetches of the same URL under the given cache mode and
// returns what the guest saw.
func fetchTwice(t *testing.T, url, mode string) string {
	t.Helper()
	js, err := spidermonkey.New(spidermonkey.Config{
		Resolve: func(string) bool { return true },
		Dial:    func(string, string, string, int) bool { return true },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	w, err := web.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer w.Close()

	script := fmt.Sprintf(`
		globalThis.__r = "?";
		(async () => {
			const opts = %q === "" ? undefined : { cache: %q };
			// Both bodies are read. A response whose body is never read is an
			// incomplete load, and the cache does not store one — the bytes were
			// never accepted, so there is nothing to have stored.
			const a = await fetch(%q, opts);
			const abody = await a.text();
			const b = await fetch(%q, opts);
			globalThis.__r = a.status + "," + b.status + "," + (await b.text()) + "," + abody;
		})().catch((e) => { globalThis.__r = "THREW " + e.message; });
	`, mode, mode, url, url)
	if _, err := js.Eval(context.Background(), script); err != nil {
		t.Fatalf("eval: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	r, err := js.Eval(context.Background(), `String(globalThis.__r)`)
	if err != nil {
		t.Fatal(err)
	}
	return r.Value.String()
}

func TestHTTPCacheModes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cacheControl  string
		mode          string
		wantHits      int32
		wantCondition int32
		note          string
	}{
		{"fresh default serves the second from store", "max-age=300", "", 1, 0,
			"a fresh entry means the network is not touched at all"},
		{"stale default revalidates", "max-age=0", "", 2, 1,
			"a stale entry is revalidated, not refetched"},
		{"no-store never stores", "max-age=300", "no-store", 2, 0,
			"neither request may consult or write the cache"},
		{"reload always goes out unconditionally", "max-age=300", "reload", 2, 0,
			"reload skips the stored response and sends no validator"},
		{"no-cache revalidates a fresh entry", "max-age=300", "no-cache", 2, 1,
			"no-cache means do not use a stored response WITHOUT checking"},
		{"force-cache uses a stale entry", "max-age=0", "force-cache", 1, 0,
			"force-cache takes what is stored however stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits, conditional := countingOrigin(t, tc.cacheControl)
			got := fetchTwice(t, srv.URL+"/x", tc.mode)
			if got != "200,200,body,body" {
				t.Fatalf("fetch results = %q, want \"200,200,body,body\"", got)
			}
			if h := hits.Load(); h != tc.wantHits {
				t.Errorf("origin saw %d requests, want %d (%s)", h, tc.wantHits, tc.note)
			}
			if c := conditional.Load(); c != tc.wantCondition {
				t.Errorf("origin saw %d conditional requests, want %d (%s)", c, tc.wantCondition, tc.note)
			}
		})
	}
}

// TestHTTPCacheOnlyIfCached pins the one mode that turns a miss into a failure.
func TestHTTPCacheOnlyIfCached(t *testing.T) {
	srv, hits, _ := countingOrigin(t, "max-age=300")

	js, err := spidermonkey.New(spidermonkey.Config{
		Resolve: func(string) bool { return true },
		Dial:    func(string, string, string, int) bool { return true },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	w, err := web.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer w.Close()

	if _, err := js.Eval(context.Background(), fmt.Sprintf(`
		globalThis.__r = "?";
		(async () => {
			// Nothing is stored yet, so this must be a network error.
			let first = "resolved";
			try { await fetch(%q, { cache: "only-if-cached", mode: "same-origin" }); }
			catch (e) { first = "rejected"; }
			// Store it, then ask again: now it comes from the cache.
			const warm = await fetch(%q);
			await warm.text();
			const again = await fetch(%q, { cache: "only-if-cached", mode: "same-origin" });
			globalThis.__r = first + "," + again.status;
		})().catch((e) => { globalThis.__r = "THREW " + e.message; });
	`, srv.URL+"/y", srv.URL+"/y", srv.URL+"/y")); err != nil {
		t.Fatalf("eval: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	r, err := js.Eval(context.Background(), `String(globalThis.__r)`)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Value.String(); got != "rejected,200" {
		t.Errorf("only-if-cached = %q, want \"rejected,200\"", got)
	}
	// One request only: the miss never went out, and the third came from store.
	if h := hits.Load(); h != 1 {
		t.Errorf("origin saw %d requests, want 1", h)
	}
}
