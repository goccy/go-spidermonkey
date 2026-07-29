package web_test

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// CompressionStream offered gzip, deflate and deflate-raw but not brotli, even
// though the codec behind it has had brotli all along for node:zlib. Every
// brotli case failed as an unsupported format.
func TestCompressionStreamBrotli(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	w, err := web.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer w.Close()

	if _, err := js.Eval(context.Background(), `
		globalThis.__done = (async () => {
			const text = "brotli ".repeat(200);
			const bytes = new TextEncoder().encode(text);
			const through = async (stream) => {
				const w = stream.writable.getWriter();
				w.write(bytes.slice()); w.close();
				const out = [];
				for await (const c of stream.readable) out.push(c);
				let n = 0; for (const c of out) n += c.length;
				const all = new Uint8Array(n);
				let o = 0; for (const c of out) { all.set(c, o); o += c.length; }
				return all;
			};
			const packed = await through(new CompressionStream("brotli"));
			// Round-tripping is the property that matters; the size is the reason
			// anyone reaches for it.
			const back = new TextDecoder().decode(await (async () => {
				const s = new DecompressionStream("brotli");
				const wr = s.writable.getWriter();
				wr.write(packed); wr.close();
				const out = [];
				for await (const c of s.readable) out.push(c);
				let n = 0; for (const c of out) n += c.length;
				const all = new Uint8Array(n);
				let o = 0; for (const c of out) { all.set(c, o); o += c.length; }
				return all;
			})());
			globalThis.__r = (back === text ? "roundtrip" : "MISMATCH") +
				":" + (packed.length < bytes.length ? "smaller" : "NOT-SMALLER");
		})().catch((e) => { globalThis.__r = "THREW " + e.name + ": " + e.message; });
	`); err != nil {
		t.Fatalf("eval: %v", err)
	}
	if err := w.Wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if got := evalString(t, js, `String(globalThis.__r)`); got != "roundtrip:smaller" {
		t.Errorf("brotli = %q, want roundtrip:smaller", got)
	}
}
