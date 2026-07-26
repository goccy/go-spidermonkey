package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer lets the guest write from the event-loop goroutine while the test
// reads from its own goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestApp boots the Next.js server inside the runtime and serves it through an
// httptest server via the runtime's http.Handler, asserting the /api/hello
// Route Handler's JSON. The boot is slow; that is expected.
func TestApp(t *testing.T) {
	// PORT=0 lets the guest's own listener take an ephemeral port; the test
	// reaches the app through rt.HTTPHandler, not that socket.
	var stderr syncBuffer
	js, rt, err := start(io.Discard, &stderr, "PORT=0")
	if err != nil {
		t.Fatal(err)
	}
	defer js.Close()
	defer rt.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rt.Wait(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("event loop did not stop after cancel")
		}
	}()

	// Wait until the server is registered. rt.Wait returning first means the
	// loop drained without a server — i.e. boot failed.
	var handler http.Handler
	deadline := time.Now().Add(60 * time.Second)
	for {
		if h, ok := rt.HTTPHandler(); ok {
			handler = h
			break
		}
		select {
		case werr := <-done:
			t.Fatalf("server exited before it was ready: %v\nstderr:\n%s", werr, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not become ready\nstderr:\n%s", stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/api/hello")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	const want = `{"hello":"from route handler","method":"GET"}`
	if got := strings.TrimSpace(string(body)); got != want {
		t.Fatalf("output mismatch:\n got: %s\nwant: %s", got, want)
	}
}
