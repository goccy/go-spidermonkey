package nodejs_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
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
