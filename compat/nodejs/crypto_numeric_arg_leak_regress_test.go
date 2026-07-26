package nodejs_test

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestCryptoNumericArgObjectDoesNotLeak guards against a guest-triggerable
// unbounded memory leak: several node:crypto host ops read a numeric parameter
// with Value.Int(), which returns 0 for an object receiver WITHOUT releasing
// the persistent GC root minted for it at decode. Their JS wrappers forward the
// parameter raw, so a guest that passes an object (e.g. a large Uint8Array)
// where a number is expected pins that object — and its backing store — for the
// interpreter's life on every call. Under a memory cap this exhausts memory.
//
// The cases use SUCCESS-path parameters — saltLength (PKCS#1 v1.5 sign ignores
// it) and hkdf keylen — so passing an object is silently accepted and leaks with
// no guest-visible error, accumulating across the loop. Each iteration allocates
// and drops a fresh large array used as the hostile numeric argument. With the
// roots freed, the arrays are collectable and every iteration completes; if they
// leak the guest OOMs long before the loop ends and the Eval errors.
func TestCryptoNumericArgObjectDoesNotLeak(t *testing.T) {
	const script = `
		const crypto = require("crypto");
		const N = 200;              // 200 * 2 MiB per site = 400 MiB pinned if leaked
		const SZ = 2 * 1024 * 1024; // >> the 256 MiB cap when accumulated
		const { privateKey } = crypto.generateKeyPairSync("rsa", { modulusLength: 2048 });
		for (let i = 0; i < N; i++) {
			const big = new Uint8Array(SZ);       // hostile object numeric arg
			crypto.createSign("sha256").update("m").sign({ key: privateKey, saltLength: big });
			crypto.hkdfSync("sha256", "key", "salt", "info", big);
		}
		globalThis.__completed = N;
	`
	js, rt := newRuntime(t, spidermonkey.Config{MaxMemoryBytes: 256 << 20})
	if r, err := js.Eval(context.Background(), script); err != nil {
		t.Fatalf("Eval: %v", err)
	} else if r.Error != nil {
		t.Fatalf("script threw (leak exhausted the memory cap): %v", r.Error)
	}
	if err := rt.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalVal(t, js, "globalThis.__completed").Int(); got != 200 {
		t.Fatalf("completed %d iterations, want 200", got)
	}
}

// TestScalarStringArgObjectDoesNotLeak guards the same GC-root leak class for
// host ops that read a STRING parameter with Value.String(): reading an object
// receiver runs its guest toString but never releases the persistent root, so a
// JS wrapper that forwards a guest value raw (crypto key-export format/type,
// generateKeyPair type, createECDH curve, and http.request href — the last one
// outside crypto) pins the object on every call. Both cases below drive an
// object where a string is expected in a loop under a memory cap: with the roots
// freed the loop completes; if they leak the guest OOMs.
func TestScalarStringArgObjectDoesNotLeak(t *testing.T) {
	const script = `
		const crypto = require("crypto");
		const http = require("http");
		const N = 200;
		const SZ = 2 * 1024 * 1024;
		const priv = crypto.generateKeyPairSync("rsa", { modulusLength: 2048 }).privateKey;
		for (let i = 0; i < N; i++) {
			const big = new Uint8Array(SZ);
			try { priv.export({ format: big }); } catch (e) {}   // crypto_key_export format
			try { crypto.createECDH(big).generateKeys(); } catch (e) {} // crypto_ecdh curve
			http.request({ href: big }).on("error", () => {});   // http_client_req url (non-crypto)
		}
		globalThis.__completed = N;
	`
	js, rt := newRuntime(t, spidermonkey.Config{MaxMemoryBytes: 256 << 20})
	if r, err := js.Eval(context.Background(), script); err != nil {
		t.Fatalf("Eval: %v", err)
	} else if r.Error != nil {
		t.Fatalf("script threw (leak exhausted the memory cap): %v", r.Error)
	}
	if err := rt.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalVal(t, js, "globalThis.__completed").Int(); got != 200 {
		t.Fatalf("completed %d iterations, want 200", got)
	}
}
