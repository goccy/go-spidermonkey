package nodejs_test

import (
	"context"
	"crypto/tls"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHTTPSServerRealTLS(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	port, _ := startServer(t, js, rt, `
		const https = require("https");
		const tls = require("tls");
		const { cert, key } = tls.generateSelfSigned("localhost");
		const server = https.createServer({ cert, key }, (req, res) => {
			res.setHeader("Content-Type", "text/plain");
			res.end("secure hello over " + (req.socket.encrypted ? "TLS" : "plain"));
		});
		server.listen(0, "127.0.0.1");
		globalThis.__server = server;
		globalThis.PORT = server.address().port;
	`)

	// A real TLS client (skip verify: self-signed).
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		Timeout:   10 * time.Second,
	}
	resp, err := client.Get("https://127.0.0.1:" + port + "/")
	if err != nil {
		t.Fatalf("TLS GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.TLS == nil {
		t.Error("connection was not actually TLS")
	}
	if string(body) != "secure hello over TLS" {
		t.Errorf("body = %q", body)
	}
}

func TestTLSSocketEchoesGoTLSServer(t *testing.T) {
	// A Go TLS server; the guest connects via tls.connect and echoes a line.
	cert, err := tls.X509KeyPair(testCertPEM, testKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		conn.Write(append([]byte("tls-echo:"), buf[:n]...))
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	runScript(t, rt, `
		const tls = require("tls");
		globalThis.r = {};
		const sock = tls.connect({ port: PORT, host: "127.0.0.1", rejectUnauthorized: false }, () => {
			sock.write("ping");
		});
		let buf = "";
		sock.setEncoding("utf8");
		sock.on("data", (d) => { buf += d; sock.end(); });
		sock.on("close", () => { r.reply = buf; });
		sock.on("error", (e) => { r.err = String(e); });
	`)
	if got := evalStr(t, js, "r.err ?? ''"); got != "" {
		t.Fatalf("tls socket error: %s", got)
	}
	if got := evalStr(t, js, "r.reply"); got != "tls-echo:ping" {
		t.Errorf("tls reply = %q", got)
	}
	_ = context.Background
}

// tlsTestServer starts a TLS listener presenting testCert and accepting a
// single connection (reading a little to complete the handshake). It returns
// the bound port.
func tlsTestServer(t *testing.T) int {
	t.Helper()
	cert, err := tls.X509KeyPair(testCertPEM, testKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 64)
				c.Read(buf) // drive the handshake to completion
			}(conn)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// A private-CA server verifies successfully when the guest passes the CA in the
// `ca` option and a matching `servername`; the socket becomes authorized.
func TestTLSConnectTrustsCAOption(t *testing.T) {
	port := tlsTestServer(t)
	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	js.Global().Set("CA", spidermonkey.ValueOf(string(testCertPEM)))

	runScript(t, rt, `
		const tls = require("tls");
		globalThis.r = {};
		const sock = tls.connect({ port: PORT, host: "127.0.0.1", servername: "localhost", ca: [CA] }, () => {
			r.authorized = sock.authorized;
			r.authError = String(sock.authorizationError);
			sock.end();
			r.done = true;
		});
		sock.on("error", (e) => { r.err = String(e); r.code = e.code; r.done = true; });
	`)
	if !waitForCondition(t, js, `globalThis.r && r.done`) {
		t.Fatal("connect did not settle")
	}
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("connect errored: %s (code %s)", got, evalStr(t, js, `String(r.code)`))
	}
	if got := evalStr(t, js, `String(r.authorized)`); got != "true" {
		t.Errorf("authorized = %s, want true", got)
	}
}

// getPeerCertificate() returns the real peer leaf certificate fields.
func TestTLSGetPeerCertificate(t *testing.T) {
	port := tlsTestServer(t)
	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	js.Global().Set("CA", spidermonkey.ValueOf(string(testCertPEM)))

	runScript(t, rt, `
		const tls = require("tls");
		globalThis.r = {};
		const sock = tls.connect({ port: PORT, host: "127.0.0.1", servername: "localhost", ca: [CA] }, () => {
			const c = sock.getPeerCertificate();
			r.cn = (c.subject && c.subject.CN) || "";
			r.issuerCN = (c.issuer && c.issuer.CN) || "";
			r.san = c.subjectaltname || "";
			r.fp = c.fingerprint || "";
			r.fp256 = c.fingerprint256 || "";
			r.serial = c.serialNumber || "";
			r.rawIsBuf = Buffer.isBuffer(c.raw);
			r.rawLen = c.raw ? c.raw.length : 0;
			r.validFrom = c.valid_from || "";
			sock.end();
			r.done = true;
		});
		sock.on("error", (e) => { r.err = String(e); r.done = true; });
	`)
	if !waitForCondition(t, js, `globalThis.r && r.done`) {
		t.Fatal("connect did not settle")
	}
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("connect errored: %s", got)
	}
	if got := evalStr(t, js, `r.cn`); got != "localhost" {
		t.Errorf("subject.CN = %q, want localhost", got)
	}
	if got := evalStr(t, js, `r.san`); !strings.Contains(got, "localhost") {
		t.Errorf("subjectaltname = %q, want contains localhost", got)
	}
	if got := evalStr(t, js, `r.fp`); !strings.Contains(got, ":") {
		t.Errorf("fingerprint = %q, want colon-hex", got)
	}
	if got := evalStr(t, js, `String(r.rawIsBuf)`); got != "true" {
		t.Errorf("raw is Buffer = %s, want true", got)
	}
	if got := evalStr(t, js, `String(r.rawLen > 0)`); got != "true" {
		t.Errorf("raw length = %s, want > 0", evalStr(t, js, `String(r.rawLen)`))
	}
	if got := evalStr(t, js, `String(r.serial.length > 0)`); got != "true" {
		t.Errorf("serialNumber empty")
	}
}

// A verification failure (untrusted self-signed cert, no ca provided, default
// rejectUnauthorized) must surface a certificate error — NOT a mislabeled
// ECONNREFUSED.
func TestTLSConnectVerificationFailureIsCertError(t *testing.T) {
	port := tlsTestServer(t)
	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))

	runScript(t, rt, `
		const tls = require("tls");
		globalThis.r = {};
		const sock = tls.connect({ port: PORT, host: "127.0.0.1" }, () => { r.connected = true; r.done = true; });
		sock.on("error", (e) => { r.err = String(e.message || e); r.code = String(e.code); r.authErr = String(sock.authorizationError); r.done = true; });
	`)
	if !waitForCondition(t, js, `globalThis.r && r.done`) {
		t.Fatal("connect did not settle")
	}
	if got := evalStr(t, js, `String(r.connected)`); got == "true" {
		t.Fatal("connection unexpectedly authorized against system roots")
	}
	code := evalStr(t, js, `r.code`)
	msg := evalStr(t, js, `r.err`)
	if code == "ECONNREFUSED" {
		t.Errorf("cert failure mislabeled as ECONNREFUSED")
	}
	if !strings.Contains(msg, "certificate") && !strings.Contains(strings.ToUpper(code), "CERT") && !strings.Contains(code, "VERIFY") {
		t.Errorf("verification failure not cert-ish: code=%q msg=%q", code, msg)
	}
	// A verification failure must be surfaced on socket.authorizationError,
	// including codes like UNABLE_TO_VERIFY_LEAF_SIGNATURE that contain neither
	// "CERT" nor "TLS" (the host marks it structurally, not by code text).
	if got := evalStr(t, js, `r.authErr`); got != code {
		t.Errorf("authorizationError = %q, want the error code %q", got, code)
	}
}

// A servername that does not match the certificate SANs fails with an
// altname-invalid cert error (not ECONNREFUSED).
func TestTLSConnectHostnameMismatchIsCertError(t *testing.T) {
	port := tlsTestServer(t)
	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	js.Global().Set("CA", spidermonkey.ValueOf(string(testCertPEM)))

	runScript(t, rt, `
		const tls = require("tls");
		globalThis.r = {};
		const sock = tls.connect({ port: PORT, host: "127.0.0.1", servername: "not-the-cert.example", ca: [CA] }, () => { r.connected = true; r.done = true; });
		sock.on("error", (e) => { r.err = String(e.message || e); r.code = String(e.code); r.done = true; });
	`)
	if !waitForCondition(t, js, `globalThis.r && r.done`) {
		t.Fatal("connect did not settle")
	}
	if got := evalStr(t, js, `String(r.connected)`); got == "true" {
		t.Fatal("mismatched servername unexpectedly authorized")
	}
	if got := evalStr(t, js, `r.code`); got == "ECONNREFUSED" {
		t.Errorf("altname failure mislabeled as ECONNREFUSED")
	}
}

// new tls.TLSSocket(existingSocket) (STARTTLS upgrade) is not supported and must
// throw a clear error rather than return a write-swallowing dead socket.
func TestTLSSocketUpgradeThrows(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const tls = require("tls");
		const net = require("net");
		globalThis.r = {};
		try {
			const plain = new net.Socket();
			const up = new tls.TLSSocket(plain);
			r.threw = false;
		} catch (e) {
			r.threw = true;
			r.msg = String(e.message);
		}
	`)
	if got := evalStr(t, js, `String(r.threw)`); got != "true" {
		t.Fatalf("tls.TLSSocket(socket) did not throw")
	}
	if got := evalStr(t, js, `r.msg`); !strings.Contains(got, "not supported") {
		t.Errorf("throw message = %q, want contains 'not supported'", got)
	}
}

// A malformed `ca` bundle must surface a cert error AND not leak the socket's
// callback handles (the early return used to skip the handle-free loop that
// every other exit path runs). We can't read the host pinned-handle count from
// the guest, so this drives many bad-CA connects and asserts they all settle
// as cert errors without wedging the runtime — the fix is the freeObjects call
// on this path; this guards against the error path regressing.
func TestTLSConnectMalformedCADoesNotLeak(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const tls = require("tls");
		globalThis.r = { codes: [], done: 0 };
		const N = 25;
		for (let i = 0; i < N; i++) {
			const sock = tls.connect(
				{ port: 1, host: "127.0.0.1", ca: "-----BEGIN CERTIFICATE-----\nnot a real pem\n-----END CERTIFICATE-----" },
				() => {});
			sock.on("error", (e) => { r.codes.push(String(e.code || e.message)); r.done++; });
		}
	`)
	if !waitForCondition(t, js, `globalThis.r && r.done === 25`) {
		t.Fatal("malformed-CA connects did not all settle")
	}
	// Every call must report the malformed-CA cert error (and, with the fix,
	// free its callback handles rather than leaking them).
	if got := evalStr(t, js, `r.codes.filter((c) => c === "ERR_TLS_CERT_ALTNAME_INVALID").length + "/" + r.codes.length`); got != "25/25" {
		t.Errorf("malformed-CA connects = %q, want 25/25 ERR_TLS_CERT_ALTNAME_INVALID", got)
	}
}

// Test-only self-signed cert/key for TLS tests (generated once).

var testCertPEM = []byte(`-----BEGIN CERTIFICATE-----
MIIBWjCCAQCgAwIBAgIBATAKBggqhkjOPQQDAjAUMRIwEAYDVQQDEwlsb2NhbGhv
c3QwHhcNMjYwNzI0MDMxMTAwWhcNMzYwNzIxMDQxMTAwWjAUMRIwEAYDVQQDEwls
b2NhbGhvc3QwWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAAS5B30VoncprgjMOtXE
qlWlcNCUJXRBk13a+EATCELWfMuHfj936XYcZi2OJna4RmNvO4FCY7ag/FwUGAxo
SrkPo0MwQTAOBgNVHQ8BAf8EBAMCB4AwEwYDVR0lBAwwCgYIKwYBBQUHAwEwGgYD
VR0RBBMwEYIJbG9jYWxob3N0hwR/AAABMAoGCCqGSM49BAMCA0gAMEUCIHhi6LKQ
ePaubxEPQ+rhXoWxkuN3+CMk+IhFJMsu8RCPAiEA2HZYekA3YvAZ/fS4Yu5wLILA
PMrEDqzFlP5uH4L+Muk=
-----END CERTIFICATE-----
`)

var testKeyPEM = []byte(`-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgFeK+T4wLYFPSeSS6
bYrtDcN5bfGsqWpho+/A1ydQtjehRANCAAS5B30VoncprgjMOtXEqlWlcNCUJXRB
k13a+EATCELWfMuHfj936XYcZi2OJna4RmNvO4FCY7ag/FwUGAxoSrkP
-----END PRIVATE KEY-----
`)
