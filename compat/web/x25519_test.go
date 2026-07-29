package web_test

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// X25519 is the key-agreement sibling of the Ed25519 signing key that was
// already here, and it was the only curve in the Web Crypto suite's agreement
// set with no implementation — every generateKey, deriveBits and import case
// for it failed as unsupported.
func TestX25519(t *testing.T) {
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
			const a = await crypto.subtle.generateKey({name:"X25519"}, true, ["deriveBits"]);
			const b = await crypto.subtle.generateKey({name:"X25519"}, true, ["deriveBits"]);
			out.push("gen:" + a.privateKey.algorithm.name + "/" + a.privateKey.type + "/" + a.publicKey.type);

			// Both sides must agree on the shared secret — the property that makes
			// it a key agreement at all.
			const s1 = new Uint8Array(await crypto.subtle.deriveBits({name:"X25519", public: b.publicKey}, a.privateKey, 256));
			const s2 = new Uint8Array(await crypto.subtle.deriveBits({name:"X25519", public: a.publicKey}, b.privateKey, 256));
			out.push("derive:" + s1.length + ":" + (s1.join() === s2.join() ? "agree" : "DIFFER"));

			// A private JWK round-trips, and the imported key derives the same secret.
			const jwk = await crypto.subtle.exportKey("jwk", a.privateKey);
			const back = await crypto.subtle.importKey("jwk", jwk, {name:"X25519"}, true, ["deriveBits"]);
			const s3 = new Uint8Array(await crypto.subtle.deriveBits({name:"X25519", public: b.publicKey}, back, 256));
			out.push("jwk:" + jwk.kty + "/" + jwk.crv + ":" + (s3.join() === s1.join() ? "same" : "DIFFER"));

			// A raw public key round-trips too.
			const raw = await crypto.subtle.exportKey("raw", a.publicKey);
			out.push("raw:" + raw.byteLength);

			// A usage this algorithm cannot have is a SyntaxError, as for any other.
			try { await crypto.subtle.generateKey({name:"X25519"}, true, ["sign"]); out.push("usage:NO-THROW"); }
			catch (e) { out.push("usage:" + e.name); }

			// A KDF derivation of ZERO bits is a legal request for an empty key;
			// only a null length is an error. Treating zero as an error broke
			// every "with 0 length" case in the suite.
			const pw = await crypto.subtle.importKey("raw", new TextEncoder().encode("pw"), {name:"PBKDF2"}, false, ["deriveBits"]);
			const zero = await crypto.subtle.deriveBits({name:"PBKDF2", salt:new Uint8Array([1]), iterations:10, hash:"SHA-256"}, pw, 0);
			out.push("kdf0:" + zero.byteLength);
			try { await crypto.subtle.deriveBits({name:"PBKDF2", salt:new Uint8Array([1]), iterations:10, hash:"SHA-256"}, pw, null); out.push("kdfnull:NO-THROW"); }
			catch (e) { out.push("kdfnull:" + e.name); }

			globalThis.__r = out.join(" | ");
		})();
	`); err != nil {
		t.Fatalf("eval: %v", err)
	}
	if err := w.Wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	v, err := js.Eval(context.Background(), `String(globalThis.__r)`)
	if err != nil {
		t.Fatal(err)
	}
	want := "gen:X25519/private/public | derive:32:agree | jwk:OKP/X25519:same | raw:32 | usage:SyntaxError | " +
		"kdf0:0 | kdfnull:OperationError"
	if got := v.Value.String(); got != want {
		t.Errorf("X25519 =\n %s\nwant\n %s", got, want)
	}
}
