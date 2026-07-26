package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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

// TestQuerystringMaxKeysCountsDuplicates verifies duplicate keys count against
// the maxKeys cap (can't be bypassed by repeating one key).
func TestQuerystringMaxKeysCountsDuplicates(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const qs = require("querystring");
		globalThis.r = {};
		r.n = qs.parse("a=1&a=2&a=3", "&", "=", { maxKeys: 2 }).a.length;
	`)
	if got := evalStr(t, js, "String(r.n)"); got != "2" {
		t.Errorf("duplicate-key maxKeys cap = %q, want 2", got)
	}
}

// TestQuerystringStringifyCoercion verifies non-primitive/null values coerce to
// "" (Node), not "null"/"[object Object]".
func TestQuerystringStringifyCoercion(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const qs = require("querystring");
		globalThis.r = {};
		r.arr = qs.stringify({ a: [1, null, 2] });
		r.obj = qs.stringify({ a: { b: 1 } });
	`)
	if got := evalStr(t, js, "r.arr"); got != "a=1&a=&a=2" {
		t.Errorf("qs array = %q, want a=1&a=&a=2", got)
	}
	if got := evalStr(t, js, "r.obj"); got != "a=" {
		t.Errorf("qs object = %q, want a=", got)
	}
}
