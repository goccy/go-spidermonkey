package nodejs_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestHTTPRequestPathOverride verifies http.request(url, { path }) uses the
// overridden path, not the URL's original path.
func TestHTTPRequestPathOverride(t *testing.T) {
	gotPath := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotPath <- r.URL.Path:
		default:
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))
	runScript(t, rt, `
		const http = require("http");
		const req = http.request(BASE + "/original", { path: "/override" }, (res) => { res.resume?.(); });
		req.on("error", () => {});
		req.end();
	`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = rt.Wait(ctx)
	select {
	case p := <-gotPath:
		if p != "/override" {
			t.Errorf("server saw path %q, want /override", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server never received the request")
	}
}

// TestConsoleLogNoArgs verifies console.log() with no arguments prints a blank
// line, not the literal "undefined" (a round-10 regression).
func TestConsoleLogNoArgs(t *testing.T) {
	var out strings.Builder
	_, rt := newRuntime(t, spidermonkey.Config{Stdout: &out})
	runScript(t, rt, `console.log(); console.log("after");`)
	got := out.String()
	if strings.Contains(got, "undefined") {
		t.Errorf("console.log() printed %q, want a blank line (no 'undefined')", got)
	}
	if !strings.Contains(got, "after") {
		t.Errorf("output missing 'after': %q", got)
	}
}

// TestBufferBinaryAccessors verifies the previously-missing Buffer read/write
// accessors exist and round-trip.
func TestBufferBinaryAccessors(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		let b = Buffer.alloc(8);
		b.writeFloatLE(1.5, 0);
		r.floatLE = b.readFloatLE(0);
		b = Buffer.alloc(8); b.writeInt32LE(-1000, 0); r.i32le = b.readInt32LE(0);
		b = Buffer.alloc(8); b.writeBigInt64LE(-123n, 0); r.big = String(b.readBigInt64LE(0));
		b = Buffer.alloc(8); b.writeInt16LE(-2, 0); r.i16 = b.readInt16LE(0);
		b = Buffer.from([1,2,3,4,5,6]); r.uintLE = b.readUIntLE(0, 3);
		b = Buffer.from([0x01,0x02,0x03,0x04]); b.swap32(); r.swapped = b.toString("hex");
		b = Buffer.from([0xaa,0xbb]); b.swap16(); r.sw16 = b.toString("hex");
	`)
	for expr, want := range map[string]string{
		"String(r.floatLE)": "1.5",
		"String(r.i32le)":   "-1000",
		"r.big":             "-123",
		"String(r.i16)":     "-2",
		"String(r.uintLE)":  "197121", // 1 + 2*256 + 3*65536
		"r.swapped":         "04030201",
		"r.sw16":            "bbaa",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// TestBufferLenientDecoding verifies hex/base64 decoding is lenient like Node:
// invalid hex stops (no zero-fill), invalid base64 is ignored (no throw).
func TestBufferLenientDecoding(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		r.hexBad = Buffer.from("41zz42", "hex").toString("hex");   // Node: "41"
		r.hexOdd = Buffer.from("4", "hex").length;                 // Node: 0
		try { r.b64 = Buffer.from("a*b*c", "base64").length >= 0 ? "ok" : "no"; }
		catch { r.b64 = "threw"; }
	`)
	if got := evalStr(t, js, "r.hexBad"); got != "41" {
		t.Errorf("invalid hex decode = %q, want 41", got)
	}
	if got := evalStr(t, js, "String(r.hexOdd)"); got != "0" {
		t.Errorf("odd hex nibble length = %q, want 0", got)
	}
	if got := evalStr(t, js, "r.b64"); got != "ok" {
		t.Errorf("lenient base64 = %q, want ok (no throw)", got)
	}
}

// TestQuerystringMaxKeys verifies querystring.parse caps at 1000 keys by default
// and honors maxKeys: 0 (unlimited).
func TestQuerystringMaxKeys(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const qs = require("querystring");
		globalThis.r = {};
		let big = "";
		for (let i = 0; i < 1500; i++) big += "k" + i + "=1&";
		r.capped = Object.keys(qs.parse(big)).length;
		r.unlimited = Object.keys(qs.parse(big, "&", "=", { maxKeys: 0 })).length;
	`)
	if got := evalStr(t, js, "String(r.capped)"); got != "1000" {
		t.Errorf("default maxKeys = %q, want 1000", got)
	}
	if got := evalStr(t, js, "String(r.unlimited)"); got != "1500" {
		t.Errorf("maxKeys:0 = %q, want 1500", got)
	}
}
