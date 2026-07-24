package nodejs_test

import (
	spidermonkey "github.com/goccy/go-spidermonkey"
	"testing"
)

func TestBufferEncodings(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		// utf16le round trip
		const b = Buffer.from("héllo", "utf16le");
		r.len = b.length;               // 5 chars * 2 = 10
		r.back = b.toString("utf16le");
		r.ucs2 = Buffer.from("AB", "ucs2").toString("hex"); // 41 00 42 00
		// latin1
		r.latin1 = Buffer.from([0xe9, 0x61]).toString("latin1"); // é a
		// TextDecoder latin1 + utf-16le
		r.tdLatin1 = new TextDecoder("latin1").decode(new Uint8Array([0xe9, 0x61]));
		r.td16 = new TextDecoder("utf-16le").decode(new Uint8Array([0x48, 0x00, 0x69, 0x00]));
	`)
	for expr, want := range map[string]string{
		"String(r.len)": "10",
		"r.back":        "héllo",
		"r.ucs2":        "41004200",
		"r.latin1":      "éa",
		"r.tdLatin1":    "éa",
		"r.td16":        "Hi",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}
