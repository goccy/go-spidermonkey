package web_test

import (
	"context"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
	"testing"
)

// CompressionStream and DecompressionStream are WinterTC WEB apis and must be
// present in a web-only embedding. They used to exist only under compat/nodejs,
// where the zlib host op happened to live, so compat/web had neither — the Web
// Platform Tests scored zero across the entire `compression` directory.
func TestCompressionStreamRoundTrip(t *testing.T) {
	var out string
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
		globalThis.__r = (async () => {
			const text = "hello ".repeat(200);
			const enc = new TextEncoder().encode(text);
			const results = [];
			for (const format of ["gzip", "deflate", "deflate-raw"]) {
				const packed = await new Response(
					new Blob([enc]).stream().pipeThrough(new CompressionStream(format))
				).arrayBuffer();
				const back = await new Response(
					new Blob([packed]).stream().pipeThrough(new DecompressionStream(format))
				).text();
				results.push(format + ":" + (back === text) + ":" + (packed.byteLength < enc.length));
			}
			// An unknown format is a TypeError, and corrupt input errors the
			// readable rather than hanging.
			try { new CompressionStream("nope"); results.push("badformat:no-throw"); }
			catch (e) { results.push("badformat:" + e.name); }
			try {
				await new Response(
					new Blob([new Uint8Array([1, 2, 3])]).stream().pipeThrough(new DecompressionStream("gzip"))
				).text();
				results.push("corrupt:no-throw");
			} catch (e) { results.push("corrupt:threw"); }
			return results.join(" ");
		})();
	`); err != nil {
		t.Fatalf("eval: %v", err)
	}
	if err := w.Wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if _, err := js.Eval(context.Background(), `globalThis.__r.then((v) => { globalThis.__v = v; });`); err != nil {
		t.Fatal(err)
	}
	if err := w.Wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	rv, err := js.Eval(context.Background(), `String(globalThis.__v)`)
	if err != nil {
		t.Fatal(err)
	}
	out = rv.Value.String()
	want := "gzip:true:true deflate:true:true deflate-raw:true:true badformat:TypeError corrupt:threw"
	if out != want {
		t.Errorf("compression round trip = %q\nwant %q", out, want)
	}
}

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
