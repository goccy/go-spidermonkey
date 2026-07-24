package nodejs_test

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestCipherKeyLengthValidated verifies createCipheriv rejects a key whose length
// doesn't match the named AES variant, rather than silently downgrading (e.g.
// aes-256-gcm with a 16-byte key quietly using AES-128).
func TestCipherKeyLengthValidated(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const crypto = require("crypto");
		globalThis.r = {};
		const chk = (name, fn) => { try { fn(); r[name] = "ok"; } catch (e) { r[name] = "threw"; } };
		const iv = Buffer.alloc(12);
		chk("mismatch", () => crypto.createCipheriv("aes-256-gcm", Buffer.alloc(16), iv).final());
		chk("match", () => crypto.createCipheriv("aes-256-gcm", Buffer.alloc(32), iv).final());
	`)
	if got := evalStr(t, js, "r.mismatch"); got != "threw" {
		t.Errorf("aes-256-gcm with 16-byte key = %q, want threw", got)
	}
	if got := evalStr(t, js, "r.match"); got != "ok" {
		t.Errorf("aes-256-gcm with 32-byte key = %q, want ok", got)
	}
}

// TestDecompressionStreamErrorsOnCorrupt verifies a corrupt DecompressionStream
// input rejects the consumer rather than hanging.
func TestDecompressionStreamErrorsOnCorrupt(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	r, err := rt.RunScript(context.Background(), `
		globalThis.r = {};
		const ds = new DecompressionStream("gzip");
		const w = ds.writable.getWriter();
		w.write(new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8])); // not valid gzip
		w.close();
		const reader = ds.readable.getReader();
		reader.read().then(
			() => { r.outcome = "resolved"; },
			() => { r.outcome = "rejected"; },
		);
	`)
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if r.Error != nil {
		// The uncaught throw from close() may surface; that's acceptable as long
		// as it doesn't hang. The important assertion is r.outcome below.
		_ = r.Error
	}
	if got := evalStr(t, js, "String(r.outcome ?? 'hung')"); got != "rejected" {
		t.Errorf("corrupt DecompressionStream outcome = %q, want rejected", got)
	}
}

// TestAssertStructuralSetMap verifies deepStrictEqual matches Set elements and
// Map keys structurally, so equal-shaped object members compare equal.
func TestAssertStructuralSetMap(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const assert = require("assert");
		globalThis.r = {};
		const eq = (name, a, b, wantEqual) => {
			let threw = false;
			try { assert.deepStrictEqual(a, b); } catch { threw = true; }
			r[name] = (threw === !wantEqual);
		};
		eq("setObjEq", new Set([{a:1}]), new Set([{a:1}]), true);
		eq("mapObjKeyEq", new Map([[{k:1}, 2]]), new Map([[{k:1}, 2]]), true);
		eq("setObjNeq", new Set([{a:1}]), new Set([{a:2}]), false);
	`)
	for _, name := range []string{"setObjEq", "mapObjKeyEq", "setObjNeq"} {
		if !evalVal(t, js, "r."+name).Bool() {
			t.Errorf("structural deepStrictEqual case %q incorrect", name)
		}
	}
}
