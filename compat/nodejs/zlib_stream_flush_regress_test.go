package nodejs_test

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// zlib Transform streams used to buffer ALL input until end() and had no
// .flush() method at all (Express's compression middleware calls
// stream.flush() for SSE, which threw TypeError). The streams are now
// incremental: flush() (Z_SYNC_FLUSH) makes everything written so far
// decodable mid-stream, and the final output still decompresses to the full
// input.
func TestZlibStreamFlushMidStreamRoundTrip(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const zlib = require("zlib");
		globalThis.r = {};
		const gz = zlib.createGzip();
		const chunks = [];
		gz.on("data", (c) => chunks.push(c));
		gz.on("end", () => { r.finalHex = Buffer.concat(chunks).toString("hex"); });
		r.flushType = typeof gz.flush;
		gz.write("data: hello\n\n");
		gz.flush(() => { r.flushCbRan = true; });
		setTimeout(() => {
			// Everything emitted up to the flush point must already decode the
			// first chunk (verified host-side with Go's gzip reader below).
			r.afterFlushHex = Buffer.concat(chunks).toString("hex");
			gz.write("data: world\n\n");
			gz.end();
		}, 20);
	`)
	if got := evalStr(t, js, `r.flushType`); got != "function" {
		t.Fatalf("gzip stream .flush type = %q, want function", got)
	}
	if got := evalStr(t, js, `String(r.flushCbRan)`); got != "true" {
		t.Errorf("flush callback did not run")
	}

	afterFlush, err := hex.DecodeString(evalStr(t, js, `r.afterFlushHex`))
	if err != nil {
		t.Fatal(err)
	}
	final, err := hex.DecodeString(evalStr(t, js, `r.finalHex`))
	if err != nil {
		t.Fatal(err)
	}
	if len(afterFlush) == 0 {
		t.Fatal("no compressed bytes emitted before end() despite flush()")
	}

	// The bytes available right after flush() must decode the first SSE chunk
	// (the stream is truncated, so tolerate the resulting UnexpectedEOF).
	zr, err := gzip.NewReader(bytes.NewReader(afterFlush))
	if err != nil {
		t.Fatalf("gzip.NewReader(afterFlush): %v", err)
	}
	prefix, err := io.ReadAll(zr)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("reading flushed prefix: %v", err)
	}
	if got, want := string(prefix), "data: hello\n\n"; got != want {
		t.Errorf("decoded flush prefix = %q, want %q", got, want)
	}

	// The complete stream must be a valid gzip of the full input.
	zr2, err := gzip.NewReader(bytes.NewReader(final))
	if err != nil {
		t.Fatalf("gzip.NewReader(final): %v", err)
	}
	full, err := io.ReadAll(zr2)
	if err != nil {
		t.Fatalf("reading final stream: %v", err)
	}
	if got, want := string(full), "data: hello\n\ndata: world\n\n"; got != want {
		t.Errorf("decoded final stream = %q, want %q", got, want)
	}
}

// Decompression streams are incremental too: bytes compressed and
// sync-flushed on one side become 'data' on a createGunzip() BEFORE either
// stream ends (the streaming-proxy pattern), and gunzip exposes a callable
// .flush() as well.
func TestZlibDecompressStreamEmitsBeforeEnd(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const zlib = require("zlib");
		globalThis.r = {};
		const gz = zlib.createGzip();
		const gun = zlib.createGunzip();
		const outs = [];
		gun.on("data", (c) => outs.push(c.toString()));
		gun.on("end", () => { r.final = outs.join(""); });
		r.gunFlushType = typeof gun.flush;
		gun.flush(() => { r.gunFlushCbRan = true; }); // must not throw
		gz.on("data", (c) => gun.write(c));
		gz.on("end", () => gun.end());
		gz.write("hello ");
		gz.flush();
		setTimeout(() => {
			r.beforeEnd = outs.join(""); // first chunk decoded before any end()
			gz.end("world");
		}, 20);
	`)
	for expr, want := range map[string]string{
		"r.gunFlushType":          "function",
		"String(r.gunFlushCbRan)": "true",
		"r.beforeEnd":             "hello ",
		"r.final":                 "hello world",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// flush() accepts every Node arg form — (), (cb), (kind), (kind, cb) — and
// works for deflate and brotli streams; multi-chunk write + flush + end still
// round-trips through the one-shot decompressors.
func TestZlibStreamFlushArgFormsAndOtherCodecs(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const zlib = require("zlib");
		globalThis.r = { cbs: 0 };
		const cases = [
			["createDeflate", "inflateSync"],
			["createDeflateRaw", "inflateRawSync"],
			["createBrotliCompress", "brotliDecompressSync"],
			["createGzip", "gunzipSync"],
		];
		for (const [create, decompressSync] of cases) {
			const s = zlib[create]();
			const chunks = [];
			s.on("data", (c) => chunks.push(c));
			s.on("end", () => {
				r[create] = zlib[decompressSync](Buffer.concat(chunks)).toString();
			});
			s.write("alpha ");
			s.flush();
			s.flush(() => r.cbs++);
			s.flush(zlib.constants.Z_SYNC_FLUSH);
			s.flush(zlib.constants.Z_FULL_FLUSH, () => r.cbs++);
			s.write("beta ");
			s.end("gamma");
		}
	`)
	for _, create := range []string{"createDeflate", "createDeflateRaw", "createBrotliCompress", "createGzip"} {
		if got := evalStr(t, js, `r["`+create+`"]`); got != "alpha beta gamma" {
			t.Errorf("%s round trip with mid-stream flushes = %q, want %q", create, got, "alpha beta gamma")
		}
	}
	if got := evalStr(t, js, `String(r.cbs)`); got != "8" {
		t.Errorf("flush callbacks ran %s times, want 8", got)
	}
}

// A gzip stream produced by Go with mid-stream Flush() calls decompresses
// incrementally in the guest and matches the original payload.
func TestZlibGunzipStreamOfGoFlushedGzip(t *testing.T) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte("first segment|")); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil { // Z_SYNC_FLUSH
		t.Fatal(err)
	}
	cut := buf.Len() // guest receives the flushed prefix as its first chunk
	if _, err := w.Write([]byte("second segment")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	js, rt := newRuntime(t, spidermonkey.Config{})
	u8, err := js.NewBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer u8.Free()
	js.Global().Set("goGzipFlushed", u8)
	js.Global().Set("goGzipCut", spidermonkey.ValueOf(cut))

	runScript(t, rt, `
		const zlib = require("zlib");
		globalThis.r = {};
		const gun = zlib.createGunzip();
		const outs = [];
		gun.on("data", (c) => outs.push(c.toString()));
		gun.on("end", () => { r.final = outs.join(""); });
		const all = Buffer.from(goGzipFlushed);
		gun.write(all.subarray(0, goGzipCut));
		setTimeout(() => {
			r.afterFirstChunk = outs.join(""); // sync-flushed prefix already decoded
			gun.end(all.subarray(goGzipCut));
		}, 20);
	`)
	if got := evalStr(t, js, `r.afterFirstChunk`); got != "first segment|" {
		t.Errorf("decoded after first chunk = %q, want %q", got, "first segment|")
	}
	if got := evalStr(t, js, `r.final`); got != "first segment|second segment" {
		t.Errorf("final decoded = %q, want %q", got, "first segment|second segment")
	}
}
