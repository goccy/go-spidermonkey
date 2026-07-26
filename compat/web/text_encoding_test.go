package web_test

import (
	"strings"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestTextDecoderStreamFatalErrorsReader verifies a fatal decode error on the
// writable side errors the readable side (a pending reader rejects) instead of
// hanging forever. If it hung, drainWeb's Wait would time out and fail.
func TestTextDecoderStreamFatalErrorsReader(t *testing.T) {
	js, w := newWeb(t, spidermonkey.Config{})
	eval(t, js, `
		globalThis.__r = {};
		const ds = new TextDecoderStream("utf-8", { fatal: true });
		const reader = ds.readable.getReader();
		const wr = ds.writable.getWriter();
		wr.write(new Uint8Array([0xff, 0xfe])).catch(() => {});
		reader.read()
			.then(() => { __r.res = "resolved"; })
			.catch(e => { __r.res = "rejected:" + (e && e.name); });
	`)
	drainWeb(t, w)
	if got := evalString(t, js, `__r.res ?? "HANG"`); !strings.HasPrefix(got, "rejected") {
		t.Errorf("fatal decode reader outcome = %q, want a rejection (not a hang)", got)
	}
}

// TestTextDecoderStreamCancelPropagates verifies the same for a
// fetch-body-style pipeThrough(TextDecoderStream()).
func TestTextDecoderStreamCancelPropagates(t *testing.T) {
	js, w := newWeb(t, spidermonkey.Config{})
	eval(t, js, `
		globalThis.__r = {};
		(async () => {
			let sourceCancelled = false;
			const source = new ReadableStream({
				pull(c) { c.enqueue(new Uint8Array([104, 105])); }, // "hi"
				cancel() { sourceCancelled = true; },
			});
			const out = source.pipeThrough(new TextDecoderStream());
			const reader = out.getReader();
			await reader.read();
			await reader.cancel();
			for (let i = 0; i < 5; i++) await new Promise(r => setTimeout(r, 5));
			__r.sourceCancelled = sourceCancelled;
		})().catch(e => { __r.err = String(e && e.message || e); });
	`)
	drainWeb(t, w)
	if got := evalString(t, js, `__r.err ?? ""`); got != "" {
		t.Fatalf("threw: %s", got)
	}
	if got := evalString(t, js, `String(__r.sourceCancelled)`); got != "true" {
		t.Errorf("TextDecoderStream output cancel did not reach the source = %q, want true", got)
	}
}

// TestTextDecoderWindows1252 verifies the windows-1252/latin1/iso-8859-1 labels
// decode 0x80-0x9F as the windows-1252 characters (euro, smart quotes), not C1.
func TestTextDecoderWindows1252(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	if got := evalString(t, js, `
		Array.from(new TextDecoder("windows-1252").decode(new Uint8Array([0x80,0x85,0x91,0x92,0x93,0x94,0x96,0x99])))
			.map(c => c.codePointAt(0).toString(16)).join(",")
	`); got != "20ac,2026,2018,2019,201c,201d,2013,2122" {
		t.Errorf("windows-1252 decode = %q, want 20ac,2026,2018,2019,201c,201d,2013,2122", got)
	}
	// The high half (0xA0-0xFF) is unchanged.
	if got := evalString(t, js, `new TextDecoder("latin1").decode(new Uint8Array([0xe9, 0x61]))`); got != "éa" {
		t.Errorf("latin1 high-half decode = %q, want éa", got)
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
