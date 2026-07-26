package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestReadlinePromisesQuestion verifies node:readline/promises question() returns
// a Promise that resolves with the entered line.
func TestReadlinePromisesQuestion(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { createInterface } = require("node:readline/promises");
		const { Readable } = require("stream");
		const input = new Readable({ read() {} });
		const rl = createInterface({ input });
		const p = rl.question("name? ");
		r.isPromise = p instanceof Promise;
		p.then((a) => { r.answer = a; });
		input.push("Alice\n");
	`)
	if got := evalStr(t, js, `String(r.isPromise)`); got != "true" {
		t.Errorf("readline/promises question() returned non-Promise: %q", got)
	}
	if got := evalStr(t, js, `String(r.answer)`); got != "Alice" {
		t.Errorf("readline/promises answer = %q, want Alice", got)
	}
}

// TestReadlinePromisesAbortDoesNotSwallowLine verifies that aborting a
// readline/promises question() rejects it AND detaches the pending callback, so
// the next input line is delivered as a 'line' event, not silently swallowed.
func TestReadlinePromisesAbortDoesNotSwallowLine(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const { createInterface } = require("node:readline/promises");
		const { Readable } = require("stream");
		const input = new Readable({ read() {} });
		const rl = createInterface({ input });
		rl.on("line", (l) => { r.line = l; });
		const ac = new AbortController();
		const p = rl.question("name? ", { signal: ac.signal });
		p.then(() => { r.rejected = "resolved"; }).catch((e) => { r.rejected = e.name; });
		ac.abort();
		input.push("Alice\n"); // must reach the 'line' handler, not the aborted question
	`)
	if got := evalStr(t, js, `String(r.rejected)`); got != "AbortError" {
		t.Errorf("aborted question = %q, want AbortError", got)
	}
	if got := evalStr(t, js, `String(r.line ?? "")`); got != "Alice" {
		t.Errorf("next line after abort = %q, want Alice (swallowed)", got)
	}
}
