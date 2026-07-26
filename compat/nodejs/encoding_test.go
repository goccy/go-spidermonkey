package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
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

// TestStringDecoderMultiByteSplit verifies StringDecoder holds back an incomplete
// trailing unit for multi-byte encodings, so a utf16le code unit split across two
// writes is decoded correctly instead of dropping/garbling the character.
func TestStringDecoderMultiByteSplit(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const { StringDecoder } = require("string_decoder");
		globalThis.r = {};
		// "AB" in utf16le is 41 00 42 00; split so the second unit straddles the
		// chunk boundary: [41 00 42] then [00].
		const d = new StringDecoder("utf16le");
		let out = d.write(Buffer.from([0x41, 0x00, 0x42]));
		out += d.write(Buffer.from([0x00]));
		out += d.end();
		r.utf16 = out;
	`)
	if got := evalStr(t, js, "r.utf16"); got != "AB" {
		t.Errorf("StringDecoder utf16le split = %q, want %q", got, "AB")
	}
}

// TestTextEncoderEncodeInto verifies encodeInto exists and reports read/written.
func TestTextEncoderEncodeInto(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const dest = new Uint8Array(8);
		const res = new TextEncoder().encodeInto("abc", dest);
		r.read = res.read;
		r.written = res.written;
		r.bytes = Array.from(dest.subarray(0, 3)).join(",");
	`)
	if got := evalVal(t, js, "r.read").Int(); got != 3 {
		t.Errorf("read = %d, want 3", got)
	}
	if got := evalVal(t, js, "r.written").Int(); got != 3 {
		t.Errorf("written = %d, want 3", got)
	}
	if got := evalStr(t, js, "r.bytes"); got != "97,98,99" {
		t.Errorf("encoded bytes = %q, want 97,98,99", got)
	}
}

// TestBase64StopsAtPadding verifies base64 decode terminates at padding (Node),
// not continuing to decode trailing tokens.
func TestBase64StopsAtPadding(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		r.concat = Buffer.from("SGVsbG8=SGVsbG8=", "base64").toString();  // "Hello"
		r.simple = Buffer.from("YQ==Yg==", "base64").toString("hex");    // "61"
	`)
	if got := evalStr(t, js, "r.concat"); got != "Hello" {
		t.Errorf("base64 concat = %q, want Hello", got)
	}
	if got := evalStr(t, js, "r.simple"); got != "61" {
		t.Errorf("base64 padded = %q, want 61", got)
	}
}
