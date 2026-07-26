package nodejs_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
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
