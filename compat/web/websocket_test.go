package web_test

// WebSocket is tested against a real server on loopback, because everything
// that can go wrong here is on the wire: the type a message comes back as, the
// close code the peer echoed, whether bufferedAmount was charged in bytes or in
// UTF-16 code units.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// echoServer answers every message with the same bytes and the same type, and
// lets the library echo the close frame — which is what makes a close code and
// reason observable from the client.
func echoServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols:    []string{"echo"},
			CompressionMode: websocket.CompressionDisabled,
			OriginPatterns:  []string{"*"},
		})
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			typ, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), typ, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runWS evaluates script with `URL` bound to the echo server's ws:// URL and
// returns String(globalThis.__r) once the loop has settled.
func runWS(t *testing.T, wsURL, script string) string {
	t.Helper()
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

	full := fmt.Sprintf("globalThis.__r = %q;\nconst URL_ = %q;\n%s", "?", wsURL, script)
	if r, err := js.Eval(context.Background(), full); err != nil {
		t.Fatalf("eval: %v", err)
	} else if r.Error != nil {
		t.Fatalf("threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	r, err := js.Eval(context.Background(), `String(globalThis.__r)`)
	if err != nil {
		t.Fatal(err)
	}
	return r.Value.String()
}

func wsURLOf(srv *httptest.Server) string {
	return "ws" + srv.URL[len("http"):]
}

func TestWebSocketEchoAndClose(t *testing.T) {
	url := wsURLOf(echoServer(t))
	for _, tc := range []struct{ name, script, want string }{
		{
			// bufferedAmount is charged in bytes on the wire, and readable back
			// synchronously: it may only change at a task boundary.
			name: "text round trip charges bufferedAmount in UTF-8 bytes",
			script: `
				const ws = new WebSocket(URL_, "echo");
				ws.onopen = () => {
					ws.send("héllo");
					globalThis.__buffered = ws.bufferedAmount;
				};
				ws.onmessage = (e) => {
					globalThis.__r = e.data + "," + globalThis.__buffered + "," + ws.protocol;
					ws.close();
				};`,
			want: "héllo,6,echo",
		},
		{
			// close() with no code sends an EMPTY close frame; the peer echoes an
			// empty one, and that is what 1005 means.
			name: "close with no argument reports 1005 and is clean",
			script: `
				const ws = new WebSocket(URL_);
				ws.onopen = () => ws.close();
				ws.onclose = (e) => { globalThis.__r = e.code + "," + e.reason + "," + e.wasClean; };`,
			want: "1005,,true",
		},
		{
			name: "close(code, reason) is echoed back verbatim",
			script: `
				const ws = new WebSocket(URL_);
				ws.onopen = () => ws.close(3001, "because");
				ws.onclose = (e) => { globalThis.__r = e.code + "," + e.reason + "," + e.wasClean; };`,
			want: "3001,because,true",
		},
		{
			// readyState is CLOSING the instant close() returns: the handshake has
			// been started, not finished.
			name: "readyState goes CONNECTING OPEN CLOSING CLOSED",
			script: `
				const ws = new WebSocket(URL_);
				const seen = [ws.readyState];
				ws.onopen = () => { seen.push(ws.readyState); ws.close(); seen.push(ws.readyState); };
				ws.onclose = () => { seen.push(ws.readyState); globalThis.__r = seen.join(""); };`,
			want: "0123",
		},
		{
			// A binary message arrives as a Blob by default, and as an ArrayBuffer
			// when binaryType says so.
			name: "binary round trip honours binaryType",
			script: `
				const ws = new WebSocket(URL_);
				ws.binaryType = "arraybuffer";
				ws.onopen = () => ws.send(new Uint8Array([1, 2, 3]));
				ws.onmessage = (e) => {
					const v = new Uint8Array(e.data);
					globalThis.__r = (e.data instanceof ArrayBuffer) + ":" + v.join("-");
					ws.close();
				};`,
			want: "true:1-2-3",
		},
		{
			// A view sends its own window into the buffer, not the whole buffer.
			name: "a typed-array view sends only its own range",
			script: `
				const ws = new WebSocket(URL_);
				ws.binaryType = "arraybuffer";
				const buf = new Uint8Array([9, 8, 7, 6, 5]).buffer;
				ws.onopen = () => { ws.send(new Uint8Array(buf, 1, 3)); globalThis.__b = ws.bufferedAmount; };
				ws.onmessage = (e) => {
					globalThis.__r = new Uint8Array(e.data).join("-") + "," + globalThis.__b;
					ws.close();
				};`,
			want: "8-7-6,3",
		},
		{
			name: "a Blob is sent as its bytes",
			script: `
				const ws = new WebSocket(URL_);
				ws.binaryType = "arraybuffer";
				ws.onopen = () => ws.send(new Blob(["hi"]));
				ws.onmessage = (e) => {
					globalThis.__r = new TextDecoder().decode(e.data);
					ws.close();
				};`,
			want: "hi",
		},
		{
			// An unknown binaryType is DROPPED, not an error: it is an enumeration.
			name: "an unknown binaryType is ignored",
			script: `
				const ws = new WebSocket(URL_);
				ws.binaryType = "nonsense";
				globalThis.__r = ws.binaryType;
				ws.close();`,
			want: "blob",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runWS(t, url, tc.script); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWebSocketRejectsBadArguments pins the synchronous failures — the ones a
// caller sees before any byte leaves.
func TestWebSocketRejectsBadArguments(t *testing.T) {
	url := wsURLOf(echoServer(t))
	script := `
		const out = [];
		const name = (fn) => { try { fn(); return "no-throw"; } catch (e) { return e.name; } };
		// A URL that is not a WebSocket URL, and one with a fragment.
		out.push(name(() => new WebSocket("ftp://example.org/")));
		out.push(name(() => new WebSocket(URL_ + "#frag")));
		// A subprotocol that is not a token, and one repeated case-insensitively.
		out.push(name(() => new WebSocket(URL_, "a b")));
		out.push(name(() => new WebSocket(URL_, ["echo", "eCho"])));
		// http/https are accepted and normalized to ws/wss.
		const httpURL = URL_.replace(/^ws:/, "http:");
		const viaHTTP = new WebSocket(httpURL);
		out.push(viaHTTP.url === URL_ + "/" || viaHTTP.url === URL_ ? "normalized" : viaHTTP.url);
		viaHTTP.close();
		// send() before the socket is open is an error; a close code outside the
		// permitted set is an error; a reason over 123 bytes is an error.
		const ws = new WebSocket(URL_);
		out.push(name(() => ws.send("x")));
		out.push(name(() => ws.close(1005)));
		out.push(name(() => ws.close(1000, "x".repeat(124))));
		out.push(name(() => ws.close("only a reason")));
		ws.close();
		globalThis.__r = out.join(",");`
	want := "SyntaxError,SyntaxError,SyntaxError,SyntaxError,normalized," +
		"InvalidStateError,InvalidAccessError,SyntaxError,InvalidAccessError"
	if got := runWS(t, url, script); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// TestWebSocketFailedConnection is the "fail the WebSocket connection" path: an
// error event, then a close event with 1006 and wasClean false.
func TestWebSocketFailedConnection(t *testing.T) {
	// Port 21 is on fetch's blocked-port list, so the connection fails without a
	// dial being attempted at all.
	got := runWS(t, "ws://localhost:21/", `
		const ws = new WebSocket(URL_);
		const seen = [];
		ws.onerror = () => seen.push("error");
		ws.onclose = (e) => {
			seen.push("close:" + e.code + ":" + e.wasClean);
			globalThis.__r = seen.join(",");
		};`)
	if want := "error,close:1006:false"; got != want {
		t.Errorf("blocked port: got %q, want %q", got, want)
	}
}

// TestWebSocketPermissionDenied holds the socket to the interpreter's network
// policy: a host Dial refuses must not become an open connection.
func TestWebSocketPermissionDenied(t *testing.T) {
	srv := echoServer(t)
	js, err := spidermonkey.New(spidermonkey.Config{
		Resolve: func(string) bool { return true },
		Dial:    func(string, string, string, int) bool { return false },
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

	if _, err := js.Eval(context.Background(), fmt.Sprintf(`
		globalThis.__r = "?";
		const ws = new WebSocket(%q);
		ws.onopen = () => { globalThis.__r = "OPENED"; };
		ws.onclose = (e) => { globalThis.__r = "closed:" + e.code; };`, wsURLOf(srv))); err != nil {
		t.Fatalf("eval: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	r, err := js.Eval(context.Background(), `String(globalThis.__r)`)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Value.String(); got != "closed:1006" {
		t.Errorf("a denied dial gave %q, want \"closed:1006\"", got)
	}
}
