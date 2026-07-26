package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// buffer.transcode to latin1/ascii used to map unmappable characters with
// &0xFF (garbage bytes); Node substitutes "?" (0x3f) for any code point the
// target single-byte encoding cannot represent.
func TestBufferTranscodeUnmappableBecomesQuestionMark(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const { transcode } = require("buffer");
		globalThis.r = {};
		// "h" (0x68), "é" (0xe9, fits latin1), "€" (U+20AC, unmappable).
		const src = Buffer.from("hé€", "utf8");
		r.latin1 = transcode(src, "utf8", "latin1").toString("hex");
		// ascii: anything above 0x7f becomes "?" too.
		r.ascii = transcode(src, "utf8", "ascii").toString("hex");
		// Pure-ASCII input passes through untouched in both targets.
		const plain = Buffer.from("abc", "utf8");
		r.plainLatin1 = transcode(plain, "utf8", "latin1").toString("hex");
		r.plainAscii = transcode(plain, "utf8", "ascii").toString("hex");
		// An astral code point (one character, a surrogate pair in UTF-16)
		// must yield a single "?" byte, not two.
		r.astral = transcode(Buffer.from("a\u{1F600}b", "utf8"), "utf8", "latin1").toString("hex");
		// A non-single-byte target still round-trips through the default path.
		r.ucs2 = transcode(Buffer.from("hi", "utf8"), "utf8", "ucs2").toString("hex");
	`)
	for expr, want := range map[string]string{
		"r.latin1":      "68e93f",
		"r.ascii":       "683f3f",
		"r.plainLatin1": "616263",
		"r.plainAscii":  "616263",
		"r.astral":      "613f62",
		"r.ucs2":        "68006900",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}
