package nodejs_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestHTTPSServerIdNoCollision verifies a guest cannot force an https server id
// to collide with a live http server (which would evict+leak it and hang).
func TestHTTPSServerIdNoCollision(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	_ = js
	done := make(chan error, 1)
	go func() {
		_, err := rt.RunScript(context.Background(), `
			const http = require("http");
			const https = require("https");
			globalThis.r = {};
			const httpSrv = http.createServer((req, res) => res.end("http"));
			httpSrv.listen(0, "127.0.0.1", () => {
				// Force the next https id to collide with the http server's id.
				globalThis.__node_next_https = httpSrv._id - 1;
				const { cert, key } = require("tls").generateSelfSigned("localhost");
				const httpsSrv = https.createServer({ cert, key }, (req, res) => res.end("https"));
				httpsSrv.listen(0, "127.0.0.1", () => {
					r.bothLive = httpSrv._id !== httpsSrv._id;
					httpsSrv.close();
					httpSrv.close();
				});
				httpsSrv.on("error", (e) => { r.err = String(e); httpSrv.close(); });
			});
		`)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("https/http id collision hung the loop")
	}
	if got := evalStr(t, js, "String(r.bothLive ?? false)"); got != "true" {
		t.Errorf("http and https share an id (collision): bothLive=%q", got)
	}
}

// TestHeadersSentAfterWriteHead verifies res.headersSent is true right after
// writeHead (framework error-middleware branches on it).
func TestHeadersSentAfterWriteHead(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	port, _ := startServer(t, js, rt, `
		const http = require("http");
		globalThis.r = {};
		const server = http.createServer((req, res) => {
			res.writeHead(200, { "content-type": "text/plain" });
			r.sent = res.headersSent;
			res.end("ok");
		});
		server.listen(0);
		globalThis.__server = server;
		globalThis.PORT = server.address().port;
	`)
	resp, err := http.Get("http://127.0.0.1:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := evalStr(t, js, "String(r.sent ?? false)"); got != "true" {
		t.Errorf("headersSent after writeHead = %q, want true", got)
	}
}

// TestHTTPServerClientDisconnectClosesResponse verifies a client disconnect
// mid-response fires res.on('close') on the guest handler — the canonical SSE /
// long-poll cancellation hook that a bodyless GET would otherwise never see.
func TestHTTPServerClientDisconnectClosesResponse(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	port, _ := startServer(t, js, rt, `
		const http = require("http");
		globalThis.__closed = false;
		globalThis.__server = http.createServer((req, res) => {
			res.writeHead(200, { "Content-Type": "text/event-stream" });
			res.write("data: hi\n\n");
			const iv = setInterval(() => { try { res.write("data: tick\n\n"); } catch {} }, 20);
			res.on("close", () => { __closed = true; clearInterval(iv); });
		});
		__server.listen(0);
		globalThis.PORT = __server.address().port;
	`)

	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	fmt.Fprintf(conn, "GET /sse HTTP/1.1\r\nHost: x\r\n\r\n")
	buf := make([]byte, 64)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Read(buf) // read some of the response, then drop the connection
	conn.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if evalStr(t, js, `String(__closed)`) == "true" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := evalStr(t, js, `String(__closed)`); got != "true" {
		t.Errorf("res.on('close') did not fire on client disconnect (got %q)", got)
	}
}

// TestHTTPServerCloseGraceful verifies server.close() lets an in-flight request
// finish (full body, not truncated) and fires its callback only afterwards.
func TestHTTPServerCloseGraceful(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = { order: [] };
		const http = require("http");
		const server = http.createServer((req, res) => {
			setTimeout(() => res.end("done"), 60);
		});
		server.listen(0, () => {
			const port = server.address().port;
			http.get({ host: "127.0.0.1", port, path: "/" }, (res) => {
				let b = "";
				res.on("data", (c) => { b += c; });
				res.on("end", () => r.order.push("response:" + b));
			});
			// Close while the request is still in flight.
			setTimeout(() => server.close(() => r.order.push("closed")), 20);
		});
	`)
	if got := evalStr(t, js, `r.order.join(",")`); got != "response:done,closed" {
		t.Errorf("order = %q, want response:done,closed (close was not graceful)", got)
	}
}

// TestHTTPResponseContentLength verifies res.end(body) sets Content-Length (not
// Transfer-Encoding: chunked), matching Node.
func TestHTTPResponseContentLength(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	port, _ := startServer(t, js, rt, `
		const http = require("http");
		globalThis.__server = http.createServer((req, res) => { res.end("hi"); });
		__server.listen(0);
		globalThis.PORT = __server.address().port;
	`)
	resp, err := http.Get("http://127.0.0.1:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Length"); got != "2" {
		t.Errorf("Content-Length = %q, want 2", got)
	}
	if got := resp.TransferEncoding; len(got) != 0 {
		t.Errorf("Transfer-Encoding = %v, want none (should not be chunked)", got)
	}
}
