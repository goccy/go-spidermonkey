package web_test

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// URL.createObjectURL and FileReader are the two halves of FileAPI that were
// missing: one names a Blob so anything taking a URL can read it, the other is
// how code written against a browser reads a Blob at all. Without the first,
// the suite's whole FileAPI/url group could not start.
func TestObjectURLAndFileReader(t *testing.T) {
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
		globalThis.__out = [];
		globalThis.__done = (async () => {
			const out = globalThis.__out;
			const blob = new Blob(["hello world"], { type: "text/plain" });

			// A blob: URL is fetchable, and carries the Blob's type and size.
			const u = URL.createObjectURL(blob);
			out.push("scheme:" + u.slice(0, 5));
			const res = await fetch(u);
			out.push("fetch:" + res.status + ":" + res.headers.get("content-type") + ":" + (await res.text()));

			// Revoking it makes the URL stop resolving.
			URL.revokeObjectURL(u);
			try { await fetch(u); out.push("revoked:NO-THROW"); }
			catch (e) { out.push("revoked:" + e.name); }

			// FileReader delivers through events, in order, with the result set
			// by the time 'load' fires.
			const read = (method, ...args) => new Promise((resolve) => {
				const fr = new FileReader();
				const seen = [];
				for (const ev of ["loadstart", "load", "loadend"]) fr.addEventListener(ev, () => seen.push(ev));
				fr.onloadend = () => resolve(seen.join(">") + "=" + String(fr.result).slice(0, 40) + ":" + fr.readyState);
				fr[method](...args);
			});
			out.push("text:" + await read("readAsText", blob));
			out.push("dataurl:" + await read("readAsDataURL", blob));
			out.push("binary:" + await read("readAsBinaryString", blob));
			const abuf = await read("readAsArrayBuffer", blob);
			out.push("arraybuffer:" + abuf.split("=")[0] + "=" + (abuf.includes("[object ArrayBuffer]") ? "ArrayBuffer" : "OTHER"));

			// A second read while one is in flight is an InvalidStateError.
			const fr = new FileReader();
			fr.readAsText(blob);
			try { fr.readAsText(blob); out.push("reentrant:NO-THROW"); }
			catch (e) { out.push("reentrant:" + e.name); }

			globalThis.__r = out.join(" | ");
		})().catch((e) => { globalThis.__r = "THREW " + e.name + ": " + e.message + " after " + globalThis.__out.join(" | "); });
	`); err != nil {
		t.Fatalf("eval: %v", err)
	}
	if err := w.Wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}

	got := evalString(t, js, `String(globalThis.__r)`)
	want := "scheme:blob: | fetch:200:text/plain:hello world | revoked:TypeError | " +
		"text:loadstart>load>loadend=hello world:2 | " +
		"dataurl:loadstart>load>loadend=data:text/plain;base64,aGVsbG8gd29ybGQ=:2 | " +
		"binary:loadstart>load>loadend=hello world:2 | " +
		"arraybuffer:loadstart>load>loadend=ArrayBuffer | " +
		"reentrant:InvalidStateError"
	if got != want {
		t.Errorf("FileAPI =\n %s\nwant\n %s", got, want)
	}
}
