package nodejs_test

import (
	"context"
	"io"
	"net/http"
	"testing"
	"testing/fstest"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
)

// Closing the Runtime while a request is still in flight must release the
// request's host goroutine — including when the guest has already called
// server.close().
//
// That combination is the leak: server.close() is a GRACEFUL shutdown, so it
// removes the server from the runtime's table and waits (on an unbounded
// context) for every in-flight ServeHTTP to finish. Runtime.Close can no longer
// find that server to hard-close it, and the only signal it does send — closing
// the request's done channel — was not something the serving goroutine watched.
// This asserts the liveness property — Close returns and the client is
// released — rather than the leak itself: the leak was observed as parked
// goroutines in a long external-suite run, and this fixture does not reproduce
// that exact interleaving.
func TestCloseReleasesUnfinishedResponse(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{
		FS:      fstest.MapFS{},
		Listen:  func(network, addr string) bool { return true },
		Dial:    func(network, host, ip string, port int) bool { return true },
		Resolve: func(host string) bool { return true },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	rt, err := nodejs.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// A server whose handler never answers: the request reaches the guest and
	// stays there.
	// The listening server keeps the loop alive, so this run ends at its
	// deadline by design; keep it short.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := rt.RunScript(ctx, `
		const http = require("http");
		globalThis.__reached = 0;
		const server = http.createServer((req, res) => {
			globalThis.__reached++;
			// Never responds — and asks for a graceful close while this request is
			// still open, which is the path that used to leak.
			server.close();
		});
		server.listen(0, "127.0.0.1", () => { globalThis.__port = server.address().port; });
	`); err != nil {
		// The loop stays alive for the listening server; a deadline here is the
		// expected way out, not a failure.
		t.Logf("RunScript returned: %v", err)
	}
	pv, err := js.Eval(context.Background(), "globalThis.__port || 0")
	if err != nil {
		t.Fatal(err)
	}
	port := pv.Value.Int()
	if port == 0 {
		t.Skip("server did not report a port; nothing to measure")
	}

	// Fire a request that will never be answered, from the HOST side so the
	// connection is real, and give it time to reach the guest handler.
	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		c := &http.Client{Timeout: 30 * time.Second}
		resp, err := c.Get("http://127.0.0.1:" + itoa(port) + "/")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	time.Sleep(300 * time.Millisecond)

	// Closing must return promptly AND let the in-flight request go: the client
	// gets an answer (an aborted connection or an error), rather than hanging
	// until its own timeout.
	closed := make(chan error, 1)
	go func() { closed <- rt.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Logf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Runtime.Close did not return within 10s")
	}
	select {
	case <-reqDone:
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight request still blocked 10s after the Runtime was closed: " +
			"its serving goroutine leaked")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
