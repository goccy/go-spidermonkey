package web_test

// A header value may contain any byte but NUL, LF and CR as far as fetch is
// concerned, and the Web Platform Tests check that such a value reaches the
// server. net/http's Transport refuses to send it — that rule lives in
// validateHeaders, not in the code that writes the request — so fetch here falls
// back to writing the request itself (see fetchraw.go).
//
// This cannot be tested through the WPT harness: that harness serves the suite
// with net/http, whose readRequest rejects the same values on ARRIVAL with a
// 400, where the suite's own Python server accepts them. So the listener below
// is a raw one, which is the only way to observe what actually goes on the wire.

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// rawEchoListener accepts one HTTP/1.1 request, records its header block
// verbatim, and answers 200. It parses nothing: the point is to see the bytes.
func rawEchoListener(t *testing.T) (addr string, headers <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ch := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		br := bufio.NewReader(conn)
		var b strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				break
			}
			b.WriteString(line)
			if line == "\r\n" || line == "\n" {
				break
			}
		}
		ch <- b.String()
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"))
	}()
	return ln.Addr().String(), ch
}

func TestFetchSendsControlCharacterHeaderValues(t *testing.T) {
	addr, headers := rawEchoListener(t)

	js, err := spidermonkey.New(spidermonkey.Config{
		Resolve: func(string) bool { return true },
		Dial:    func(string, string, string, int) bool { return true },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	w, err := web.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer w.Close()

	// U+0001 is a legal header value per fetch and is exactly what net/http's
	// Transport refuses; U+0009 must survive as itself in the middle of a value.
	if _, err := js.Eval(context.Background(), `
		globalThis.__done = fetch("http://`+addr+`/", { headers: { "x-ctl": "ab", "x-tab": "c	d" } })
			.then((r) => { globalThis.__status = r.status; })
			.catch((e) => { globalThis.__status = "THREW " + e.message; });
	`); err != nil {
		t.Fatalf("eval: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	st, err := js.Eval(context.Background(), `String(globalThis.__status)`)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Value.String(); got != "200" {
		t.Fatalf("fetch status = %s, want 200", got)
	}

	select {
	case block := <-headers:
		if !strings.Contains(block, "a\x01b") {
			t.Errorf("the header block does not carry the control character:\n%q", block)
		}
		if !strings.Contains(block, "c\td") {
			t.Errorf("the header block does not carry the tab:\n%q", block)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no request arrived")
	}
}
