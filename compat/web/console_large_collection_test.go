package web_test

import (
	"context"
	"strings"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// Logging a large collection must print a bounded line, as Node's util.inspect
// does (maxArrayLength, 100 entries, then "... N more items").
//
// Rendering every element built an intermediate proportional to the collection,
// so `console.log(new Array(10_000_000).fill("x"))` — exactly what WPT's
// console-log-large-array test does — exhausted the interpreter's entire memory
// budget and aborted the guest.
func TestConsoleLogLargeCollectionIsBounded(t *testing.T) {
	var out strings.Builder
	js, err := spidermonkey.New(spidermonkey.Config{MaxMemoryBytes: 256 << 20, Stdout: &out, Stderr: &out})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	if _, err := web.Install(js); err != nil {
		t.Fatalf("Install: %v", err)
	}

	r, err := js.Eval(context.Background(), `
		console.log(new Array(10000000).fill("x"));
		console.log(new Uint8Array(10000000));
		console.log(new Set(new Array(1000).keys()));
		console.log(new Map([[1, "a"], [2, "b"]]));
		"done"
	`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("threw: %v", r.Error)
	}

	got := out.String()
	if !strings.Contains(got, "... 9999900 more items") {
		t.Errorf("array line is not bounded:\n%s", firstLine(got))
	}
	if !strings.Contains(got, "... 900 more items") {
		t.Errorf("Set line is not bounded:\n%s", got)
	}
	// A small Map must still print in full, with no truncation note.
	if !strings.Contains(got, `Map(2) { 1 => "a", 2 => "b" }`) {
		t.Errorf("small Map line changed:\n%s", got)
	}
	// The whole output stays small — that is the property the memory blowup broke.
	if len(got) > 64<<10 {
		t.Errorf("output is %d bytes; expected a few kilobytes", len(got))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
