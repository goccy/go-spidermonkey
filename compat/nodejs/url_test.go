package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestURLRejectsOutOfRangePort verifies new URL() throws for a port > 65535.
func TestURLRejectsOutOfRangePort(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		try { new URL("http://h:99999/"); r.threw = false; } catch { r.threw = true; }
		r.canParse = (typeof URL.canParse === "function") ? URL.canParse("http://h:99999") : "n/a";
		r.valid = new URL("http://h:8080/").port;
	`)
	if !evalVal(t, js, "r.threw").Bool() {
		t.Error("new URL with port 99999 did not throw")
	}
	if got := evalStr(t, js, "String(r.valid)"); got != "8080" {
		t.Errorf("valid port = %q, want 8080", got)
	}
}

// TestURLResolvePathOnlyBase verifies url.resolve() with a scheme-less base
// returns a path-only result, not one carrying the internal placeholder host.
func TestURLResolvePathOnlyBase(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const url = require("url");
		r.pathBase = url.resolve("/one/two/three", "four");
		r.absBase = url.resolve("http://ex.com/a/b", "c");
	`)
	if got := evalStr(t, js, `r.pathBase`); got != "/one/two/four" {
		t.Errorf("resolve path-only base = %q, want /one/two/four", got)
	}
	if got := evalStr(t, js, `r.absBase`); got != "http://ex.com/a/c" {
		t.Errorf("resolve absolute base = %q, want http://ex.com/a/c", got)
	}
}
