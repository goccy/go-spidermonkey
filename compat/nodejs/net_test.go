package nodejs_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func TestNetClientEchoesGoServer(t *testing.T) {
	// A Go TCP server that upper-cases each line; the guest connects, sends,
	// and reads the reply through node:net.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
		r := bufio.NewReader(conn)
		line, _ := r.ReadString('\n')
		fmt.Fprint(conn, "ECHO:"+line)
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = {};
		const sock = net.connect(PORT, "127.0.0.1", () => {
			r.connected = true;
			sock.write("hello\n");
		});
		let buf = "";
		sock.setEncoding("utf8");
		sock.on("data", (d) => { buf += d; sock.end(); });
		sock.on("close", () => { r.reply = buf; });
		sock.on("error", (e) => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("socket error: %s", got)
	}
	if !evalVal(t, js, `r.connected`).Bool() {
		t.Error("connect callback did not fire")
	}
	if got := evalStr(t, js, `r.reply`); got != "ECHO:hello\n" {
		t.Errorf("reply = %q, want %q", got, "ECHO:hello\n")
	}
}

func TestNetServerAcceptsGoClient(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	port, waitDone := startServer(t, js, rt, `
		const net = require("net");
		const server = net.createServer((sock) => {
			sock.on("data", (d) => sock.write("srv:" + d.toString()));
		});
		server.listen(0, "127.0.0.1");
		globalThis.__server = server;
		globalThis.PORT = server.address().port;
	`)
	_ = waitDone

	conn, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "ping")
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); got != "srv:ping" {
		t.Errorf("server reply = %q, want srv:ping", got)
	}
}

func TestNetConnectDenied(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{
		Dial: func(network, host, ip string, port int) bool { return false },
	})
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = {};
		const sock = net.connect(9999, "127.0.0.1");
		sock.on("error", (e) => { r.code = e.code; });
	`)
	if got := evalStr(t, js, `r.code`); got != "EACCES" {
		t.Errorf("denied connect code = %q, want EACCES", got)
	}
}

func TestHTTPRequestClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("X-Echo", "1")
			fmt.Fprintf(w, "got:%s", body)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hello client")
	}))
	defer srv.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))

	runScript(t, rt, `
		const http = require("http");
		globalThis.r = {};
		http.get(BASE + "/", (res) => {
			r.status = res.statusCode;
			r.ct = res.headers["content-type"];
			let body = "";
			res.setEncoding("utf8");
			res.on("data", (c) => { body += c; });
			res.on("end", () => { r.body = body; });
		}).on("error", (e) => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("http.get error: %s", got)
	}
	if got := evalVal(t, js, `r.status`).Int(); got != 200 {
		t.Errorf("status = %d", got)
	}
	if got := evalStr(t, js, `r.body`); got != "hello client" {
		t.Errorf("body = %q", got)
	}

	// POST with a body.
	runScript(t, rt, `
		globalThis.r2 = {};
		const req = require("http").request(BASE + "/echo", { method: "POST" }, (res) => {
			r2.echoHeader = res.headers["x-echo"];
			let body = "";
			res.on("data", (c) => { body += c.toString(); });
			res.on("end", () => { r2.body = body; });
		});
		req.write("payload");
		req.end();
	`)
	if got := evalStr(t, js, `r2.body`); got != "got:payload" {
		t.Errorf("POST echo = %q", got)
	}
	if got := evalStr(t, js, `r2.echoHeader`); got != "1" {
		t.Errorf("response header = %q", got)
	}
}

// A write issued BEFORE the socket connects must be buffered and flushed on
// connect (not lost), and the connect itself must not block the loop.
func TestNetWriteBeforeConnectBuffered(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
		r := bufio.NewReader(conn)
		line, _ := r.ReadString('\n')
		fmt.Fprint(conn, "GOT:"+line)
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("PORT", spidermonkey.ValueOf(port))
	runScript(t, rt, `
		const net = require("net");
		globalThis.r = {};
		const sock = net.connect(PORT, "127.0.0.1");
		sock.write("early\n"); // BEFORE 'connect' fires — must be buffered
		let buf = "";
		sock.setEncoding("utf8");
		sock.on("data", (d) => { buf += d; sock.end(); });
		sock.on("close", () => { r.reply = buf; });
		sock.on("error", (e) => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("socket error: %s", got)
	}
	if got := evalStr(t, js, `r.reply`); got != "GOT:early\n" {
		t.Fatalf("reply = %q, want GOT:early\\n (pre-connect write lost?)", got)
	}
}
