package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestApp runs the application, capturing its stdout, and asserts the exact
// output — proving jose signs and verifies a JWT correctly on the WinterTC
// surface.
func TestApp(t *testing.T) {
	var out bytes.Buffer
	if err := run(&out); err != nil {
		t.Fatal(err)
	}
	const want = `{"sub":"alice","role":"admin","iat":1720000000,"alg":"HS256","verified":true}`
	if got := strings.TrimSpace(out.String()); got != want {
		t.Fatalf("output mismatch:\n got: %s\nwant: %s", got, want)
	}
}
