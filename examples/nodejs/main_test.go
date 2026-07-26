package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestApp runs the application, capturing its stdout, and asserts the exact
// output — proving the unmodified lodash and commander packages run correctly.
func TestApp(t *testing.T) {
	var out bytes.Buffer
	if err := run(&out); err != nil {
		t.Fatal(err)
	}
	const want = `{"greeting":"hello carol","names":["alice","bob","carol"],"byAge":{"30":2,"34":1}}`
	if got := strings.TrimSpace(out.String()); got != want {
		t.Fatalf("output mismatch:\n got: %s\nwant: %s", got, want)
	}
}
