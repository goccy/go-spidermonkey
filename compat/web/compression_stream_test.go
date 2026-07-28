package web_test

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
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
