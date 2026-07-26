package nodejs_test

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
