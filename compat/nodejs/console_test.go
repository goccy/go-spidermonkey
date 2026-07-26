package nodejs_test

import (
	"strings"
	"testing"

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
