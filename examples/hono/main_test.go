package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestApp serves the Hono app through an in-process HTTP server and asserts a
// route's response — proving the unmodified Hono framework runs correctly and
// sees the Go-bound env value.
func TestApp(t *testing.T) {
	h, closePool, err := newHandler()
	if err != nil {
		t.Fatal(err)
	}
	defer closePool()

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/items/42?v=7")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	const want = `{"id":"42","v":"7","greeting":"bound from Go"}`
	if string(body) != want {
		t.Fatalf("output mismatch:\n got: %s\nwant: %s", body, want)
	}
}
