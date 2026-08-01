package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestStringDecoderEndSingleReplacement verifies StringDecoder.end() flushes a
// truncated trailing UTF-8 sequence as exactly ONE U+FFFD (the WHATWG
// maximal-subpart rule), not one per pending byte.
func TestStringDecoderEndSingleReplacement(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const { StringDecoder } = require("string_decoder");
		globalThis.r = {};
		{
			const d = new StringDecoder("utf8");
			const w = d.write(Buffer.from([0x61, 0xE2, 0x82])); // "a" + truncated €
			r.truncated3 = JSON.stringify(w + d.end());
		}
		{
			const d = new StringDecoder("utf8");
			const w = d.write(Buffer.from([0xF0, 0x9F, 0x92])); // truncated 4-byte
			r.truncated4 = JSON.stringify(w + d.end());
		}
		{
			const d = new StringDecoder("utf8");
			// Split across writes: must still assemble.
			const w1 = d.write(Buffer.from([0xE2]));
			const w2 = d.write(Buffer.from([0x82, 0xAC]));
			r.split = JSON.stringify(w1 + w2 + d.end());
		}
	`)
	for _, tc := range [][2]string{
		{`r.truncated3`, `"a` + "�" + `"`},
		{`r.truncated4`, `"` + "�" + `"`},
		{`r.split`, `"` + "€" + `"`},
	} {
		if got := evalStr(t, js, tc[0]); got != tc[1] {
			t.Errorf("%s = %s, want %s", tc[0], got, tc[1])
		}
	}
}
