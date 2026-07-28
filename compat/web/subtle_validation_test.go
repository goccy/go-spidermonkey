package web_test

import (
	"context"
	"strings"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// Web Crypto specifies WHICH error a bad call produces, and checks the call
// before it checks whether the algorithm is supported: a malformed algorithm is
// a TypeError, a usage the algorithm cannot have is a SyntaxError, a bad key
// length is an OperationError.
//
// None of this was checked, so calls that must fail SUCCEEDED — the single
// largest group of WebCryptoAPI failures, because the suite runs these cases
// for every algorithm it knows.
func TestSubtleRejectsBadArguments(t *testing.T) {
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
			const out = [];
			const check = async (name, fn) => {
				try { await fn(); out.push(name + "=NO-THROW"); }
				catch (e) { out.push(name + "=" + e.name); }
			};
			await check("empty-algorithm", () => crypto.subtle.generateKey({}, true, ["encrypt"]));
			await check("null-algorithm", () => crypto.subtle.generateKey(null, true, ["encrypt"]));
			await check("wrong-usage", () => crypto.subtle.generateKey({name:"AES-CBC",length:128}, true, ["sign"]));
			await check("unknown-usage", () => crypto.subtle.generateKey({name:"AES-CBC",length:128}, true, ["nope"]));
			await check("empty-usages", () => crypto.subtle.generateKey({name:"AES-CBC",length:128}, true, []));
			await check("bad-length", () => crypto.subtle.generateKey({name:"AES-CBC",length:127}, true, ["encrypt"]));
			await check("unsupported", () => crypto.subtle.generateKey({name:"NO-SUCH-ALG"}, true, ["encrypt"]));
			await check("import-wrong-usage", () =>
				crypto.subtle.importKey("raw", new Uint8Array(16), {name:"AES-CBC"}, true, ["sign"]));
			// A well-formed call still works.
			await check("ok", async () => {
				const k = await crypto.subtle.generateKey({name:"AES-CBC",length:128}, true, ["encrypt"]);
				if (k.algorithm.length !== 128) throw new Error("bad key");
				return "";
			});
			globalThis.__res = out.join(" ");
		})();
	`); err != nil {
		t.Fatalf("eval: %v", err)
	}
	if err := w.Wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	v, err := js.Eval(context.Background(), `String(globalThis.__res)`)
	if err != nil {
		t.Fatal(err)
	}
	want := "empty-algorithm=TypeError null-algorithm=TypeError wrong-usage=SyntaxError " +
		"unknown-usage=SyntaxError empty-usages=SyntaxError bad-length=OperationError " +
		"unsupported=NotSupportedError import-wrong-usage=SyntaxError ok=NO-THROW"
	if got := v.Value.String(); got != want {
		t.Errorf("errors =\n %s\nwant\n %s", strings.ReplaceAll(got, " ", "\n "), strings.ReplaceAll(want, " ", "\n "))
	}
}
