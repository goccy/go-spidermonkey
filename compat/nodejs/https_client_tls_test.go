package nodejs_test

import (
	"context"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// An https request carries its own TLS settings — rejectUnauthorized, ca,
// servername — and they never reached the host transport, so every request to
// a self-signed server failed the handshake against the system roots. That is
// what `rejectUnauthorized: false` exists for, and it is how nearly every
// https test (and every local dev server) is set up.
func TestHTTPSClientRequestTLSOptions(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{
		Resolve: func(host string) bool { return true },
		Dial:    func(network, host, ip string, port int) bool { return true },
		Listen:  func(network, addr string) bool { return true },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := rt.RunScript(ctx, `
		const https = require("https");
		const { cert, key } = require("tls").generateSelfSigned("localhost");
		globalThis.r = {};
		const server = https.createServer({ cert, key }, (req, res) => res.end("hello"));
		server.listen(0, "127.0.0.1", () => {
			const port = server.address().port;
			const get = (opts, tag, then) => {
				const req = https.request({ host: "127.0.0.1", port, path: "/", ...opts }, (res) => {
					let body = "";
					res.setEncoding("utf8");
					res.on("data", (c) => { body += c; });
					res.on("end", () => { r[tag] = res.statusCode + ":" + body; then(); });
				});
				req.on("error", (e) => { r[tag] = "ERR " + e.message; then(); });
				req.end();
			};
			// Verification off: the self-signed certificate is accepted.
			get({ rejectUnauthorized: false }, "insecure", () => {
				// The same server, trusted explicitly through the ca option.
				get({ ca: cert, servername: "localhost" }, "withCA", () => {
					// And with neither, the handshake must still be REJECTED —
					// the fix must not have made https unconditionally trusting.
					get({}, "verified", () => server.close());
				});
			});
		});
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}

	if got := evalStr(t, js, `r.insecure`); got != "200:hello" {
		t.Errorf("rejectUnauthorized:false = %q, want 200:hello", got)
	}
	if got := evalStr(t, js, `r.withCA`); got != "200:hello" {
		t.Errorf("explicit ca = %q, want 200:hello", got)
	}
	if got := evalStr(t, js, `r.verified`); got == "200:hello" {
		t.Errorf("default verification accepted a self-signed certificate (%q)", got)
	}
}
