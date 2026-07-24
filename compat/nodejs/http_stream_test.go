package nodejs_test

import (
	spidermonkey "github.com/goccy/go-spidermonkey"
	"io"
	"net/http"
	"testing"
	"time"
)

// streamingReader emits N chunks with a small delay, so the server sees them
// incrementally rather than all at once.
type streamingReader struct {
	chunks [][]byte
	i      int
}

func (s *streamingReader) Read(p []byte) (int, error) {
	if s.i >= len(s.chunks) {
		return 0, io.EOF
	}
	time.Sleep(15 * time.Millisecond)
	n := copy(p, s.chunks[s.i])
	s.i++
	return n, nil
}

func TestHTTPServerStreamsRequestBody(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	port, _ := startServer(t, js, rt, `
		const http = require("http");
		globalThis.r = { chunkSizes: [] };
		const server = http.createServer((req, res) => {
			req.on("data", (c) => { r.chunkSizes.push(c.length); });
			req.on("end", () => { res.end("received " + r.chunkSizes.length + " chunks"); });
		});
		server.listen(0);
		globalThis.__server = server;
		globalThis.PORT = server.address().port;
	`)
	body := &streamingReader{chunks: [][]byte{[]byte("aaaa"), []byte("bbbb"), []byte("cccc")}}
	req, _ := http.NewRequest("POST", "http://127.0.0.1:"+port+"/", body)
	req.ContentLength = 12
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(out) != "received 3 chunks" && string(out) != "received 2 chunks" && string(out) != "received 1 chunks" {
		t.Fatalf("body = %q", out)
	}
	// The server must have observed MORE THAN ONE chunk (proving streaming);
	// a buffered impl would deliver exactly 1.
	if got := evalStr(t, js, "String(r.chunkSizes.length)"); got == "1" {
		t.Errorf("request body was buffered, not streamed (1 chunk)")
	}
}
