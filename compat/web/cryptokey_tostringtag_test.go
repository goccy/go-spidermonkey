package web_test

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// Every Web IDL interface carries a Symbol.toStringTag, and CryptoKey is the
// one the Web Crypto tests check on every key they produce. Its absence failed
// not only that assertion but everything downstream that reused the key —
// ~3,000 WPT subtests, which is 7 percentage points of the whole run.
func TestCryptoKeyHasToStringTag(t *testing.T) {
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

	got, err := js.Eval(context.Background(), `
		(async () => {
			const key = await crypto.subtle.generateKey(
				{ name: "HMAC", hash: "SHA-256" }, true, ["sign", "verify"]);
			globalThis.__tag = Object.prototype.toString.call(key) + "|" +
				key[Symbol.toStringTag] + "|" +
				["type", "extractable", "algorithm", "usages"]
					.filter((n) => typeof Object.getOwnPropertyDescriptor(CryptoKey.prototype, n)?.get === "function")
					.join(",") + "|own:" + Object.keys(key).join(",");
		})()
	`)
	if err != nil || got.Error != nil {
		t.Fatalf("eval: %v %v", err, got.Error)
	}
	if err := w.Wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	v, err := js.Eval(context.Background(), `String(globalThis.__tag)`)
	if err != nil {
		t.Fatal(err)
	}
	want := "[object CryptoKey]|CryptoKey|type,extractable,algorithm,usages|own:"
	if v.Value.String() != want {
		t.Errorf("CryptoKey shape = %q, want %q", v.Value.String(), want)
	}
}
