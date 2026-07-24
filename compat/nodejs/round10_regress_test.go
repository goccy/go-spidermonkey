package nodejs_test

import (
	"strings"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestConsoleFormatSpecifiers verifies console.log substitutes printf-style
// format specifiers (via util.format) rather than printing them literally.
func TestConsoleFormatSpecifiers(t *testing.T) {
	var out strings.Builder
	js, rt := newRuntime(t, spidermonkey.Config{Stdout: &out})
	_ = js
	runScript(t, rt, `
		console.log("%s world", "hello");
		console.log("count: %d", 5);
		console.log("%j", { a: 1 });
		console.log("no specifiers", 1, 2);
	`)
	got := out.String()
	for _, want := range []string{"hello world", "count: 5", `{"a":1}`, "no specifiers 1 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("console output %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "%s world hello") {
		t.Error("format specifier was not substituted")
	}
}

// TestTimersPromisesAbort verifies timers/promises setTimeout rejects with
// AbortError when its signal is (already) aborted.
func TestTimersPromisesAbort(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		(async () => {
			const { setTimeout: sleep } = require("timers/promises");
			const c = new AbortController(); c.abort();
			try { await sleep(50, "v", { signal: c.signal }); r.outcome = "resolved"; }
			catch (e) { r.outcome = "rejected:" + e.name; }
		})().catch(e => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("unexpected error: %s", got)
	}
	if got := evalStr(t, js, `r.outcome`); got != "rejected:AbortError" {
		t.Errorf("aborted timers/promises = %q, want rejected:AbortError", got)
	}
}

// TestTimersPromisesAbortMidWait verifies aborting during the wait rejects and
// doesn't hang the loop.
func TestTimersPromisesAbortMidWait(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	done := make(chan struct{})
	go func() {
		runScript(t, rt, `
			globalThis.r = {};
			(async () => {
				const { setTimeout: sleep } = require("timers/promises");
				const c = new AbortController();
				setTimeout(() => c.abort(), 10);
				try { await sleep(100000, "v", { signal: c.signal }); r.outcome = "resolved"; }
				catch (e) { r.outcome = "rejected:" + e.name; }
			})().catch(e => { r.err = String(e); });
		`)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timers/promises abort-mid-wait hung the loop")
	}
	if got := evalStr(t, js, `r.outcome`); got != "rejected:AbortError" {
		t.Errorf("abort-mid-wait = %q, want rejected:AbortError", got)
	}
}

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
