package nodejs_test

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"io"
	"math/rand"
	"testing"
	"time"
)

func TestZlibRoundTrips(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const zlib = require("zlib");
		globalThis.r = {};
		const input = "the quick brown fox ".repeat(50);
		for (const [comp, decomp] of [
			["gzipSync", "gunzipSync"],
			["deflateSync", "inflateSync"],
			["deflateRawSync", "inflateRawSync"],
			["brotliCompressSync", "brotliDecompressSync"],
		]) {
			const packed = zlib[comp](Buffer.from(input));
			const back = zlib[decomp](packed).toString("utf8");
			r[comp] = back === input ? "ok:" + (packed.length < input.length) : "MISMATCH";
		}
		// gzip output must be smaller for this repetitive input.
		r.gzipHex = zlib.gzipSync(Buffer.from(input)).slice(0, 2).toString("hex");
	`)
	for _, comp := range []string{"gzipSync", "deflateSync", "deflateRawSync", "brotliCompressSync"} {
		if got := evalStr(t, js, `r["`+comp+`"]`); got != "ok:true" {
			t.Errorf("%s round trip = %s", comp, got)
		}
	}
	// gzip magic bytes 1f 8b.
	if got := evalStr(t, js, `r.gzipHex`); got != "1f8b" {
		t.Errorf("gzip magic = %s, want 1f8b", got)
	}
}

func TestZlibGunzipsGoGzip(t *testing.T) {
	// A gzip stream produced by Go must decompress in the guest.
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write([]byte("hello from Go gzip"))
	w.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	u8, err := js.NewBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer u8.Free()
	js.Global().Set("goGzip", u8)

	runScript(t, rt, `
		const zlib = require("zlib");
		globalThis.result = zlib.gunzipSync(Buffer.from(goGzip)).toString("utf8");
	`)
	if got := evalStr(t, js, `result`); got != "hello from Go gzip" {
		t.Errorf("gunzip of Go gzip = %q", got)
	}
}

func TestZlibStreams(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const zlib = require("zlib");
		globalThis.r = {};
		const gz = zlib.createGzip();
		const chunks = [];
		gz.on("data", (c) => chunks.push(c));
		gz.on("end", () => {
			const packed = Buffer.concat(chunks);
			r.packed = packed.length;
			r.back = zlib.gunzipSync(packed).toString("utf8");
		});
		gz.write("hello ");
		gz.write("streaming ");
		gz.end("gzip");
	`)
	// The Transform emits at flush; the loop drives it. Wait already ran.
	if got := evalStr(t, js, `r.back`); got != "hello streaming gzip" {
		t.Errorf("gzip stream round trip = %q", got)
	}
}

func TestCompressionStream(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		globalThis.r = {};
		(async () => {
			const input = new TextEncoder().encode("compress me ".repeat(20));
			const cs = new CompressionStream("gzip");
			const writer = cs.writable.getWriter();
			writer.write(input);
			writer.close();
			const reader = cs.readable.getReader();
			const parts = [];
			for (;;) { const { value, done } = await reader.read(); if (done) break; parts.push(...value); }
			const packed = new Uint8Array(parts);
			r.magic = packed[0] === 0x1f && packed[1] === 0x8b;

			const ds = new DecompressionStream("gzip");
			const w2 = ds.writable.getWriter();
			w2.write(packed);
			w2.close();
			const r2 = ds.readable.getReader();
			const back = [];
			for (;;) { const { value, done } = await r2.read(); if (done) break; back.push(...value); }
			r.back = new TextDecoder().decode(new Uint8Array(back));
		})().catch((e) => { r.err = String(e && e.stack || e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("CompressionStream rejected: %s", got)
	}
	if !evalVal(t, js, `r.magic`).Bool() {
		t.Error("gzip magic bytes missing from CompressionStream output")
	}
	if got := evalStr(t, js, `r.back`); got != "compress me compress me compress me compress me compress me compress me compress me compress me compress me compress me compress me compress me compress me compress me compress me compress me compress me compress me compress me compress me " {
		t.Errorf("DecompressionStream round trip = %q", got)
	}
	_ = io.Discard
}

// TestDecompressionStreamErrorsOnCorrupt verifies a corrupt DecompressionStream
// input rejects the consumer rather than hanging.
func TestDecompressionStreamErrorsOnCorrupt(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	r, err := rt.RunScript(context.Background(), `
		globalThis.r = {};
		const ds = new DecompressionStream("gzip");
		const w = ds.writable.getWriter();
		w.write(new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8])); // not valid gzip
		w.close();
		const reader = ds.readable.getReader();
		reader.read().then(
			() => { r.outcome = "resolved"; },
			() => { r.outcome = "rejected"; },
		);
	`)
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if r.Error != nil {
		// The uncaught throw from close() may surface; that's acceptable as long
		// as it doesn't hang. The important assertion is r.outcome below.
		_ = r.Error
	}
	if got := evalStr(t, js, "String(r.outcome ?? 'hung')"); got != "rejected" {
		t.Errorf("corrupt DecompressionStream outcome = %q, want rejected", got)
	}
}

// TestZlibUnzipDeflate verifies zlib.unzipSync decompresses a zlib-deflate
// stream (not only gzip).
func TestZlibUnzipDeflate(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const zlib = require("zlib");
		globalThis.r = {};
		r.out = zlib.unzipSync(zlib.deflateSync(Buffer.from("hi there"))).toString();
		r.gz = zlib.unzipSync(zlib.gzipSync(Buffer.from("hi there"))).toString();
	`)
	if got := evalStr(t, js, "r.out"); got != "hi there" {
		t.Errorf("unzip(deflate) = %q, want 'hi there'", got)
	}
	if got := evalStr(t, js, "r.gz"); got != "hi there" {
		t.Errorf("unzip(gzip) = %q, want 'hi there'", got)
	}
}

// gzipCompress returns data gzip-compressed with Go's writer.
func gzipCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Streaming decompression used to re-run the codec over the ENTIRE
// accumulated input on every chunk, making an N-chunk decode O(N²): a 4 MB
// payload in 8 KiB chunks took ~0.4 s and 8 MB ~1.7 s (ratio ~4, i.e.
// quadratic). With a persistent decoder the cost must scale linearly with the
// input, and the round trip must stay byte-identical.
func TestGunzipStreamChunkedDecodeScalesLinearly(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	decode := func(name string, payload []byte) time.Duration {
		t.Helper()
		gz := gzipCompress(t, payload)
		u8, err := js.NewBytes(gz)
		if err != nil {
			t.Fatal(err)
		}
		defer u8.Free()
		js.Global().Set("gzData", u8)
		start := time.Now()
		runScript(t, rt, `{
			const zlib = require("zlib");
			globalThis.r = {};
			const gun = zlib.createGunzip();
			const outs = [];
			gun.on("data", (c) => outs.push(c));
			gun.on("error", (e) => { r.err = String(e); });
			gun.on("end", () => {
				const all = Buffer.concat(outs);
				r.len = all.length;
				r.sha = require("crypto").createHash("sha256").update(all).digest("hex");
			});
			const src = Buffer.from(gzData);
			for (let off = 0; off < src.length; off += 8192) {
				gun.write(src.subarray(off, Math.min(off + 8192, src.length)));
			}
			gun.end();
		}`)
		elapsed := time.Since(start)
		if errStr := evalStr(t, js, `String(r.err ?? "")`); errStr != "" {
			t.Fatalf("%s: stream errored: %s", name, errStr)
		}
		if got, want := evalStr(t, js, `String(r.len)`), fmt.Sprint(len(payload)); got != want {
			t.Fatalf("%s: decoded length = %s, want %s", name, got, want)
		}
		wantSha := sha256.Sum256(payload)
		if got, want := evalStr(t, js, `r.sha`), hex.EncodeToString(wantSha[:]); got != want {
			t.Fatalf("%s: decoded sha256 = %s, want %s (round trip not byte-identical)", name, got, want)
		}
		return elapsed
	}

	// Incompressible payloads so the chunk count (the O(N²) driver) is large.
	rng := rand.New(rand.NewSource(1))
	payload4 := make([]byte, 4<<20)
	rng.Read(payload4)
	payload8 := make([]byte, 8<<20)
	rng.Read(payload8)

	d4 := decode("4MB", payload4)
	d8 := decode("8MB", payload8)
	t.Logf("chunked gunzip: 4MB in %v, 8MB in %v", d4, d8)

	// Linear scaling means doubling the input roughly doubles the time. The
	// quadratic implementation gave a ratio around 4; assert < 3.0 with a
	// generous CI-safe absolute floor so timer noise on a fast machine (or a
	// slow, loaded one) can't flake the test.
	floor := d4
	if floor < 50*time.Millisecond {
		floor = 50 * time.Millisecond
	}
	if limit := 3 * floor; d8 > limit {
		t.Errorf("8MB decode took %v, more than 3x the 4MB decode (%v; limit %v) — decompression is not linear in chunk count", d8, d4, limit)
	}
}

// Corrupt compressed input mid-stream must surface as a guest 'error' event
// (Z_DATA_ERROR style), never a panic, a hang, or a wedged decoder goroutine.
func TestGunzipStreamCorruptInputEmitsError(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	gz := gzipCompress(t, bytes.Repeat([]byte("the quick brown fox "), 500))
	// Corrupt the deflate body (past the 10-byte header) thoroughly.
	for i := 20; i < len(gz)-8; i++ {
		gz[i] ^= 0xa5
	}
	u8, err := js.NewBytes(gz)
	if err != nil {
		t.Fatal(err)
	}
	defer u8.Free()
	js.Global().Set("gzBad", u8)

	runScript(t, rt, `{
		const zlib = require("zlib");
		globalThis.r = { errored: false };
		const gun = zlib.createGunzip();
		gun.on("data", () => {});
		gun.on("error", (e) => { r.errored = true; r.msg = String(e.message || e); });
		gun.on("end", () => { r.ended = true; });
		const src = Buffer.from(gzBad);
		for (let off = 0; off < src.length; off += 64) {
			gun.write(src.subarray(off, Math.min(off + 64, src.length)));
		}
		gun.end();
	}`)
	if got := evalStr(t, js, `String(r.errored)`); got != "true" {
		t.Fatalf("corrupt gunzip input did not emit 'error' (ended=%s)", evalStr(t, js, `String(!!r.ended)`))
	}
	if msg := evalStr(t, js, `String(r.msg ?? "")`); msg == "" {
		t.Errorf("error event carried no message")
	}
}

// A truncated stream must fail at end() with the Z_BUF_ERROR-style
// "unexpected end of file", and the failure must terminate (no wedged
// decoder waiting for input that never comes).
func TestGunzipStreamTruncatedInputErrorsAtEnd(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	gz := gzipCompress(t, bytes.Repeat([]byte("0123456789"), 1000))
	u8, err := js.NewBytes(gz[:len(gz)/2])
	if err != nil {
		t.Fatal(err)
	}
	defer u8.Free()
	js.Global().Set("gzHalf", u8)

	runScript(t, rt, `{
		const zlib = require("zlib");
		globalThis.r = {};
		const gun = zlib.createGunzip();
		gun.on("data", () => {});
		gun.on("error", (e) => { r.code = e.code; r.msg = String(e.message || e); });
		gun.write(Buffer.from(gzHalf));
		gun.end();
	}`)
	if got := evalStr(t, js, `String(r.msg ?? "")`); got != "unexpected end of file" {
		t.Errorf("truncated stream error = %q, want %q", got, "unexpected end of file")
	}
	if got := evalStr(t, js, `String(r.code ?? "")`); got != "Z_BUF_ERROR" {
		t.Errorf("truncated stream code = %q, want Z_BUF_ERROR", got)
	}
}

// createUnzip sniffs the wrapper from the first bytes; that must keep working
// when the input arrives one byte at a time (the sniff buffer spans writes),
// for both wrappers.
func TestUnzipStreamSniffsWrapperAcrossTinyChunks(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	payload := []byte("unzip sniffing across chunk boundaries")
	gz := gzipCompress(t, payload)
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	for name, comp := range map[string][]byte{"gzip": gz, "deflate": zbuf.Bytes()} {
		u8, err := js.NewBytes(comp)
		if err != nil {
			t.Fatal(err)
		}
		js.Global().Set("compData", u8)
		runScript(t, rt, `{
			const zlib = require("zlib");
			globalThis.r = {};
			const un = zlib.createUnzip();
			const outs = [];
			un.on("data", (c) => outs.push(c));
			un.on("error", (e) => { r.err = String(e); });
			un.on("end", () => { r.text = Buffer.concat(outs).toString(); });
			const src = Buffer.from(compData);
			for (let i = 0; i < src.length; i++) un.write(src.subarray(i, i + 1));
			un.end();
		}`)
		u8.Free()
		if errStr := evalStr(t, js, `String(r.err ?? "")`); errStr != "" {
			t.Fatalf("%s: unzip stream errored: %s", name, errStr)
		}
		if got := evalStr(t, js, `r.text`); got != string(payload) {
			t.Errorf("%s: unzip round trip = %q, want %q", name, got, payload)
		}
	}
}

// destroy() mid-stream frees the host stream (and its decoder goroutine), and
// a stream simply abandoned without end()/destroy() must be reaped by Runtime
// teardown rather than lingering for the process lifetime. The Close() in the
// test cleanup exercises the teardown path; the internal reap test asserts
// the goroutine actually exits.
func TestGunzipStreamDestroyAndTeardownFreeHostStreams(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	gz := gzipCompress(t, bytes.Repeat([]byte("abandon me "), 2000))
	u8, err := js.NewBytes(gz)
	if err != nil {
		t.Fatal(err)
	}
	defer u8.Free()
	js.Global().Set("gzData", u8)

	runScript(t, rt, `{
		const zlib = require("zlib");
		globalThis.r = {};
		const src = Buffer.from(gzData);
		// destroy() mid-decode.
		const g1 = zlib.createGunzip();
		g1.on("data", () => {});
		g1.on("error", () => {});
		g1.write(src.subarray(0, 100));
		g1.destroy();
		r.destroyed = g1.destroyed;
		// Abandoned: partial input, never ended, never destroyed. Runtime
		// teardown (rt.Close in the test cleanup) must reap it.
		const g2 = zlib.createGunzip();
		g2.on("data", () => {});
		g2.write(src.subarray(0, 100));
	}`)
	if got := evalStr(t, js, `String(r.destroyed)`); got != "true" {
		t.Errorf("destroy() did not mark the stream destroyed")
	}
	// Explicit Close here (t.Cleanup would do it too) so a hang in teardown
	// fails THIS test with a timeout instead of poisoning later cleanup.
	if err := rt.Close(); err != nil {
		t.Fatalf("Runtime.Close with live zlib streams: %v", err)
	}
}

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
