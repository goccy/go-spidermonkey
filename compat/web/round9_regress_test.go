package web_test

import (
	"strings"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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

// TestWritableStreamSinkErrorState verifies a sink write() failure errors the
// stream: writer.closed rejects (no hang) and further writes are refused.
func TestWritableStreamSinkErrorState(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const ws = new WritableStream({ write() { throw new Error("boom"); } });
			const w = ws.getWriter();
			let closedRejected = false;
			w.closed.catch(() => { closedRejected = true; });
			try { await w.write("x"); } catch { __c.firstThrew = true; }
			// closed must settle (reject), not hang.
			await Promise.race([w.closed.catch(() => {}), new Promise((r) => setTimeout(r, 100))]);
			__c.closedRejected = closedRejected;
			try { await w.write("y"); __c.secondThrew = false; } catch { __c.secondThrew = true; }
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `String(__c.firstThrew)`); got != "true" {
		t.Errorf("first write did not reject: %q", got)
	}
	if got := evalString(t, js, `String(__c.closedRejected)`); got != "true" {
		t.Errorf("writer.closed did not reject after a sink error (hang): %q", got)
	}
	if got := evalString(t, js, `String(__c.secondThrew)`); got != "true" {
		t.Errorf("second write after error did not reject: %q", got)
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

// TestTextEncoderStreamSurrogateSplit verifies a surrogate pair split across two
// writes is not corrupted to U+FFFD.
func TestTextEncoderStreamSurrogateSplit(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const ts = new TextEncoderStream();
			const chunks = [];
			const reader = ts.readable.getReader();
			const pump = (async () => { for (;;) { const { value, done } = await reader.read(); if (done) break; chunks.push(value); } })();
			const w = ts.writable.getWriter();
			await w.write("\ud83d"); // high surrogate of 😀
			await w.write("\ude00"); // low surrogate
			await w.close();
			await pump;
			const total = chunks.reduce((n, c) => n + c.length, 0);
			const out = new Uint8Array(total); let o = 0;
			for (const c of chunks) { out.set(c, o); o += c.length; }
			__c.bytes = Array.from(out).join(",");
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.err ?? ""`); got != "" {
		t.Fatalf("unexpected error: %s", got)
	}
	// 😀 (U+1F600) is f0 9f 98 80 = 240,159,152,128.
	if got := evalString(t, js, `__c.bytes`); got != "240,159,152,128" {
		t.Errorf("split-surrogate encode = %q, want 240,159,152,128", got)
	}
}
