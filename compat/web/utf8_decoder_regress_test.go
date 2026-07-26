package web_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
