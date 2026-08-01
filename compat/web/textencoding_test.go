package web_test

import (
	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
	"strings"
	"testing"
)

// TextDecoder is required to know every encoding in the WHATWG standard, not
// just the three that need no table. Shift_JIS, GBK, Big5, EUC-KR, ISO-2022-JP,
// the ISO-8859 and windows-125x families and utf-16be all threw a RangeError
// here, which is indistinguishable from a typo in the label.
func TestTextDecoderLegacyEncodings(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	w, err := web.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer w.Close()

	got := evalString(t, js, `(() => {
		const out = [];
		const dec = (label, bytes, opts) =>
			new TextDecoder(label, opts).decode(new Uint8Array(bytes));

		// Shift_JIS: 0x93 0xFA 0x96 0x7B is 日本.
		out.push("sjis:" + dec("shift_jis", [0x93, 0xFA, 0x96, 0x7B]));
		// GBK: 0xD6 0xD0 0xCE 0xC4 is 中文.
		out.push("gbk:" + dec("gbk", [0xD6, 0xD0, 0xCE, 0xC4]));
		// Big5: 0xA4 0xA4 0xA4 0xE5 is 中文.
		out.push("big5:" + dec("big5", [0xA4, 0xA4, 0xA4, 0xE5]));
		// EUC-KR: 0xC7 0xD1 0xB1 0xDB is 한글.
		out.push("euckr:" + dec("euc-kr", [0xC7, 0xD1, 0xB1, 0xDB]));
		// utf-16be, which differs from the built-in utf-16le only in byte order.
		out.push("be:" + dec("utf-16be", [0x00, 0x41, 0x00, 0x42]));
		// windows-1251 (Cyrillic): 0xCF 0xF0 0xE8 is При.
		out.push("cp1251:" + dec("windows-1251", [0xCF, 0xF0, 0xE8]));

		// The canonical name is reported, whatever label was used.
		out.push("name:" + new TextDecoder("SJIS").encoding);
		out.push("name2:" + new TextDecoder("csbig5").encoding);

		// A label that names no encoding is still a RangeError.
		try { new TextDecoder("no-such-encoding"); out.push("bogus:NO-THROW"); }
		catch (e) { out.push("bogus:" + e.name); }

		// fatal:true reports malformed input instead of substituting U+FFFD.
		try { dec("shift_jis", [0x93], { fatal: true }); out.push("fatal:NO-THROW"); }
		catch (e) { out.push("fatal:" + e.name); }
		out.push("lenient:" + (dec("shift_jis", [0x93]).length > 0 ? "replaced" : "EMPTY"));

		return out.join(" | ");
	})()`)

	want := "sjis:日本 | gbk:中文 | big5:中文 | euckr:한글 | be:AB | cp1251:При | " +
		"name:shift_jis | name2:big5 | bogus:RangeError | fatal:TypeError | lenient:replaced"
	if got != want {
		t.Errorf("legacy decoding =\n %s\nwant\n %s", got, want)
	}
}

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

// TestTextDecoderMaximalSubpart verifies lenient UTF-8 decoding emits ONE
// U+FFFD per maximal subpart of an ill-formed sequence (WHATWG UTF-8 decoder),
// not one per byte. The second-byte boundary checks matter: E0 -> A0-BF,
// ED -> 80-9F, F0 -> 90-BF, F4 -> 80-8F.
func TestTextDecoderMaximalSubpart(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	for _, tc := range []struct {
		bytes string // JS array literal
		want  string
	}{
		// Truncated lead+continuation: ONE replacement.
		{`[0xE2, 0x82]`, "�"},
		{`[0xF0, 0x9F, 0x92]`, "�"},
		{`[0xC2]`, "�"},
		// Standalone invalid byte: one replacement.
		{`[0xFF]`, "�"},
		{`[0x80]`, "�"},
		// F0 needs 0x90-0xBF next: 0x80 fails the boundary, so F0 is one
		// error and 0x80 restarts (and is itself invalid as a lead).
		{`[0xF0, 0x80]`, "��"},
		// Overlong E0 80 80: E0 needs 0xA0-0xBF next.
		{`[0xE0, 0x80, 0x80]`, "���"},
		// CESU-8 surrogate ED A0 80: ED caps the second byte at 0x9F.
		{`[0xED, 0xA0, 0x80]`, "���"},
		// Above U+10FFFF: F4 caps the second byte at 0x8F.
		{`[0xF4, 0x90, 0x80, 0x80]`, "����"},
		// A failing continuation byte that is itself a valid ASCII char is
		// reprocessed and kept ("maximal subpart" boundary behavior).
		{`[0x61, 0xE2, 0x82, 0x62]`, "a�b"},
		{`[0x61, 0xF0, 0x9F, 0x92, 0x61]`, "a�a"},
		// Well-formed sequences still decode.
		{`[0xE0, 0xA0, 0x80]`, "ࠀ"},
		{`[0xE2, 0x82, 0xAC]`, "€"},
		{`[0xF0, 0x9F, 0x92, 0xA9]`, "\U0001F4A9"},
		{`[0xED, 0x9F, 0xBF]`, "퟿"},
	} {
		expr := `new TextDecoder().decode(new Uint8Array(` + tc.bytes + `))`
		if got := evalString(t, js, expr); got != tc.want {
			t.Errorf("%s = %q, want %q", expr, got, tc.want)
		}
	}
}

// TestTextDecoderFatalStillThrows verifies fatal mode rejects the same
// ill-formed inputs instead of substituting replacements.
func TestTextDecoderFatalStillThrows(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	for _, bytes := range []string{`[0xE2, 0x82]`, `[0xFF]`, `[0xF0, 0x80]`, `[0xED, 0xA0, 0x80]`} {
		expr := `(() => { try { new TextDecoder("utf-8", { fatal: true }).decode(new Uint8Array(` + bytes + `)); return "no-throw"; } catch (e) { return e instanceof TypeError ? "TypeError" : "other"; } })()`
		if got := evalString(t, js, expr); got != "TypeError" {
			t.Errorf("fatal decode of %s: got %q, want TypeError", bytes, got)
		}
	}
}

// TestTextDecoderStreamingAcrossChunks verifies a code point split across
// streaming chunks is still assembled (the maximal-subpart fix must not break
// the streaming holdback).
func TestTextDecoderStreamingAcrossChunks(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	if got := evalString(t, js, `
		const d = new TextDecoder();
		const a = d.decode(new Uint8Array([0xE2]), { stream: true });
		const b = d.decode(new Uint8Array([0x82, 0xAC]));
		[JSON.stringify(a), b].join("|")
	`); got != `""|`+"€" {
		t.Errorf("streaming = %q", got)
	}
	// A chunk ending in a truncated sequence with no follow-up flush yields
	// exactly one replacement at the end.
	if got := evalString(t, js, `
		const d2 = new TextDecoder();
		const p1 = d2.decode(new Uint8Array([0x61, 0xE2, 0x82]), { stream: true });
		const p2 = d2.decode();
		p1 + "|" + p2
	`); got != "a|�" {
		t.Errorf("streaming flush = %q", got)
	}
}
