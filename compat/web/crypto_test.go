package web_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
	"math/big"
	"strings"
	"testing"
)

// runAsync evaluates src (an async IIFE writing into globalThis.__c) and
// fails the test on any recorded rejection. Eval drains microtasks, so the
// async chain completes before it returns (subtle ops are synchronous
// underneath).
func runAsync(t *testing.T, js *spidermonkey.JS, src string) {
	t.Helper()
	eval(t, js, `globalThis.__c = {};`+src)
	if got := evalString(t, js, `__c.err ?? ""`); got != "" {
		t.Fatalf("async chain rejected: %s", got)
	}
}

func TestSubtleDigest(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	runAsync(t, js, `
		(async () => {
			const d = await crypto.subtle.digest("SHA-256", new TextEncoder().encode("abc"));
			__c.hex = [...new Uint8Array(d)].map(b => b.toString(16).padStart(2, "0")).join("");
			__c.isBuf = d instanceof ArrayBuffer;
		})().catch(e => { __c.err = String(e.stack || e); });
	`)
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := evalString(t, js, `__c.hex`); got != want {
		t.Errorf("SHA-256(abc) = %s, want %s", got, want)
	}
	if !eval(t, js, `__c.isBuf`).Bool() {
		t.Error("digest did not resolve to an ArrayBuffer")
	}
}

func TestSubtleHMACCrossCheckWithGo(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	runAsync(t, js, `
		(async () => {
			const raw = new Uint8Array(32).map((_, i) => i + 1);
			const key = await crypto.subtle.importKey("raw", raw, { name: "HMAC", hash: "SHA-256" }, true, ["sign", "verify"]);
			const data = new TextEncoder().encode("header.payload");
			const sig = await crypto.subtle.sign("HMAC", key, data);
			__c.sigHex = [...new Uint8Array(sig)].map(b => b.toString(16).padStart(2, "0")).join("");
			__c.ok = await crypto.subtle.verify("HMAC", key, sig, data);
			const tampered = new Uint8Array(sig.slice(0)); // copy — a bare view would corrupt sig itself
			tampered[0] ^= 1;
			__c.tampered = await crypto.subtle.verify("HMAC", key, tampered, data);
			const jwk = await crypto.subtle.exportKey("jwk", key);
			__c.jwkKty = jwk.kty;
			const key2 = await crypto.subtle.importKey("jwk", jwk, { name: "HMAC", hash: "SHA-256" }, false, ["verify"]);
			__c.reimported = await crypto.subtle.verify("HMAC", key2, sig, data);
		})().catch(e => { __c.err = String(e.stack || e); });
	`)

	// The exact same HMAC computed by Go must match byte-for-byte.
	rawKey := make([]byte, 32)
	for i := range rawKey {
		rawKey[i] = byte(i + 1)
	}
	m := hmac.New(sha256.New, rawKey)
	m.Write([]byte("header.payload"))
	if got, want := evalString(t, js, `__c.sigHex`), hex.EncodeToString(m.Sum(nil)); got != want {
		t.Errorf("HMAC = %s, want %s (Go reference)", got, want)
	}
	if !eval(t, js, `__c.ok`).Bool() {
		t.Error("verify(valid) = false")
	}
	if eval(t, js, `__c.tampered`).Bool() {
		t.Error("verify(tampered) = true")
	}
	if got := evalString(t, js, `__c.jwkKty`); got != "oct" {
		t.Errorf("exported jwk kty = %s", got)
	}
	if !eval(t, js, `__c.reimported`).Bool() {
		t.Error("JWK round-tripped key failed to verify")
	}
}

func TestSubtleHS256JWT(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	// A complete JWS flow — the shape jose performs under the hood.
	runAsync(t, js, `
		const b64u = (u8) => btoa(String.fromCharCode(...u8)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
		(async () => {
			const enc = new TextEncoder();
			const key = await crypto.subtle.generateKey({ name: "HMAC", hash: "SHA-256" }, true, ["sign", "verify"]);
			const header = b64u(enc.encode(JSON.stringify({ alg: "HS256", typ: "JWT" })));
			const payload = b64u(enc.encode(JSON.stringify({ sub: "alice", iat: 1720000000 })));
			const input = enc.encode(header + "." + payload);
			const sig = await crypto.subtle.sign("HMAC", key, input);
			const jwt = header + "." + payload + "." + b64u(new Uint8Array(sig));
			__c.parts = jwt.split(".").length;
			const [h, p, sg] = jwt.split(".");
			const sigBytes = Uint8Array.from(atob(sg.replace(/-/g, "+").replace(/_/g, "/")), c => c.charCodeAt(0));
			__c.verified = await crypto.subtle.verify("HMAC", key, sigBytes, enc.encode(h + "." + p));
			__c.badPayload = await crypto.subtle.verify("HMAC", key, sigBytes, enc.encode(h + ".tampered"));
		})().catch(e => { __c.err = String(e.stack || e); });
	`)
	if got := eval(t, js, `__c.parts`).Int(); got != 3 {
		t.Errorf("JWT has %d parts", got)
	}
	if !eval(t, js, `__c.verified`).Bool() {
		t.Error("JWT signature did not verify")
	}
	if eval(t, js, `__c.badPayload`).Bool() {
		t.Error("tampered JWT verified")
	}
}

func TestSubtleECDSA(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	runAsync(t, js, `
		(async () => {
			const { privateKey, publicKey } = await crypto.subtle.generateKey(
				{ name: "ECDSA", namedCurve: "P-256" }, true, ["sign", "verify"]);
			__c.privType = privateKey.type;
			__c.algName = privateKey.algorithm.name + "/" + privateKey.algorithm.namedCurve;
			const data = new TextEncoder().encode("es256 payload");
			const sig = await crypto.subtle.sign({ name: "ECDSA", hash: "SHA-256" }, privateKey, data);
			__c.sigLen = sig.byteLength;
			__c.ok = await crypto.subtle.verify({ name: "ECDSA", hash: "SHA-256" }, publicKey, sig, data);
			const bad = new Uint8Array(sig.slice(0)); bad[3] ^= 0xff;
			__c.bad = await crypto.subtle.verify({ name: "ECDSA", hash: "SHA-256" }, publicKey, bad, data);
			// JWK round trip of the public key.
			const jwk = await crypto.subtle.exportKey("jwk", publicKey);
			__c.crv = jwk.crv;
			const pub2 = await crypto.subtle.importKey("jwk", jwk, { name: "ECDSA", namedCurve: "P-256" }, false, ["verify"]);
			__c.viaJwk = await crypto.subtle.verify({ name: "ECDSA", hash: "SHA-256" }, pub2, sig, data);
			// SPKI round trip.
			const spki = await crypto.subtle.exportKey("spki", publicKey);
			const pub3 = await crypto.subtle.importKey("spki", spki, { name: "ECDSA", namedCurve: "P-256" }, false, ["verify"]);
			__c.viaSpki = await crypto.subtle.verify({ name: "ECDSA", hash: "SHA-256" }, pub3, sig, data);
		})().catch(e => { __c.err = String(e.stack || e); });
	`)
	if got := evalString(t, js, `__c.privType`); got != "private" {
		t.Errorf("privateKey.type = %s", got)
	}
	if got := evalString(t, js, `__c.algName`); got != "ECDSA/P-256" {
		t.Errorf("algorithm = %s", got)
	}
	if got := eval(t, js, `__c.sigLen`).Int(); got != 64 {
		t.Errorf("P-256 signature length = %d, want 64 (raw r||s)", got)
	}
	for name, want := range map[string]bool{"ok": true, "bad": false, "viaJwk": true, "viaSpki": true} {
		if got := eval(t, js, `__c.`+name).Bool(); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if got := evalString(t, js, `__c.crv`); got != "P-256" {
		t.Errorf("jwk crv = %s", got)
	}
}

func TestSubtleECDSACrossCheckWithGo(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	runAsync(t, js, `
		(async () => {
			const { privateKey, publicKey } = await crypto.subtle.generateKey(
				{ name: "ECDSA", namedCurve: "P-256" }, true, ["sign", "verify"]);
			const data = new TextEncoder().encode("cross-check me");
			const sig = await crypto.subtle.sign({ name: "ECDSA", hash: "SHA-256" }, privateKey, data);
			__c.sigHex = [...new Uint8Array(sig)].map(b => b.toString(16).padStart(2, "0")).join("");
			__c.jwk = JSON.stringify(await crypto.subtle.exportKey("jwk", publicKey));
		})().catch(e => { __c.err = String(e.stack || e); });
	`)

	var jwk struct{ X, Y string }
	if err := json.Unmarshal([]byte(evalString(t, js, `__c.jwk`)), &jwk); err != nil {
		t.Fatal(err)
	}
	xb, err := base64.RawURLEncoding.DecodeString(jwk.X)
	if err != nil {
		t.Fatal(err)
	}
	yb, err := base64.RawURLEncoding.DecodeString(jwk.Y)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := hex.DecodeString(evalString(t, js, `__c.sigHex`))
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != 64 {
		t.Fatalf("signature length = %d", len(sig))
	}
	pub := ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(xb),
		Y:     new(big.Int).SetBytes(yb),
	}
	digest := sha256.Sum256([]byte("cross-check me"))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&pub, digest[:], r, s) {
		t.Fatal("Go could not verify the guest's ECDSA signature")
	}
}

func TestSubtleRSA(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	runAsync(t, js, `
		(async () => {
			const data = new TextEncoder().encode("rs256 payload");
			for (const [name, tag] of [["RSASSA-PKCS1-v1_5", "pkcs1"], ["RSA-PSS", "pss"]]) {
				const alg = name === "RSA-PSS" ? { name, saltLength: 32 } : { name };
				const { privateKey, publicKey } = await crypto.subtle.generateKey(
					{ name, hash: "SHA-256", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]) },
					true, ["sign", "verify"]);
				const sig = await crypto.subtle.sign(alg, privateKey, data);
				__c[tag + "_len"] = sig.byteLength;
				__c[tag + "_ok"] = await crypto.subtle.verify(alg, publicKey, sig, data);
				const bad = new Uint8Array(sig.slice(0)); bad[0] ^= 1;
				__c[tag + "_bad"] = await crypto.subtle.verify(alg, publicKey, bad, data);
			}
		})().catch(e => { __c.err = String(e.stack || e); });
	`)
	for _, tag := range []string{"pkcs1", "pss"} {
		if got := eval(t, js, fmt.Sprintf(`__c.%s_len`, tag)).Int(); got != 256 {
			t.Errorf("%s signature length = %d, want 256", tag, got)
		}
		if !eval(t, js, fmt.Sprintf(`__c.%s_ok`, tag)).Bool() {
			t.Errorf("%s verify(valid) = false", tag)
		}
		if eval(t, js, fmt.Sprintf(`__c.%s_bad`, tag)).Bool() {
			t.Errorf("%s verify(tampered) = true", tag)
		}
	}
}

func TestSubtleUsageEnforcement(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	runAsync(t, js, `
		(async () => {
			const key = await crypto.subtle.importKey("raw", new Uint8Array(32),
				{ name: "HMAC", hash: "SHA-256" }, false, ["verify"]);
			try {
				await crypto.subtle.sign("HMAC", key, new Uint8Array(1));
				__c.signErr = "no-throw";
			} catch (e) { __c.signErr = e.name; }
			try {
				await crypto.subtle.exportKey("raw", key);
				__c.exportErr = "no-throw";
			} catch (e) { __c.exportErr = e.name; }
		})().catch(e => { __c.err = String(e.stack || e); });
	`)
	if got := evalString(t, js, `__c.signErr`); got != "InvalidAccessError" {
		t.Errorf("sign without usage: %s", got)
	}
	if got := evalString(t, js, `__c.exportErr`); got != "InvalidAccessError" {
		t.Errorf("export non-extractable: %s", got)
	}
}

func TestHeadersRequestResponse(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	if got := evalString(t, js, `
		const h = new Headers({ "Content-Type": "text/plain" });
		h.append("X-Multi", "a");
		h.append("X-Multi", "b");
		h.set("Authorization", "Bearer t");
		[h.get("content-type"), h.get("x-multi"), h.has("AUTHORIZATION"), [...h.keys()].join("+")].join("|")
	`); got != "text/plain|a, b|true|authorization+content-type+x-multi" {
		t.Errorf("headers = %s", got)
	}

	runAsync(t, js, `
		(async () => {
			const req = new Request("https://api.example/items?limit=2", {
				method: "POST",
				headers: { "content-type": "application/json" },
				body: JSON.stringify({ name: "x" }),
			});
			__c.reqInfo = [req.method, new URL(req.url).pathname, (await req.json()).name].join("|");

			const res = Response.json({ ok: 1 }, { status: 201 });
			__c.resInfo = [res.status, res.ok, res.headers.get("content-type"), (await res.json()).ok].join("|");

			const streamed = new Response("chunked body");
			const reader = streamed.body.getReader();
			const chunks = [];
			for (;;) {
				const { value, done } = await reader.read();
				if (done) break;
				chunks.push(...value);
			}
			__c.streamText = new TextDecoder().decode(new Uint8Array(chunks));
		})().catch(e => { __c.err = String(e.stack || e); });
	`)
	if got := evalString(t, js, `__c.reqInfo`); got != "POST|/items|x" {
		t.Errorf("request = %s", got)
	}
	if got := evalString(t, js, `__c.resInfo`); got != "201|true|application/json|1" {
		t.Errorf("response = %s", got)
	}
	if got := evalString(t, js, `__c.streamText`); got != "chunked body" {
		t.Errorf("streamed body = %q", got)
	}
	_ = strings.TrimSpace("")
}

// TestSubtleKeyTableBounded verifies the host key table is LRU-bounded: a cold
// (unused) key is eventually evicted under import pressure, while a key used on
// every iteration (the "import once, cache" pattern) stays hot and keeps working.
// This guards the fix for the unbounded host-memory leak that persisting keys
// across requests would otherwise cause (no engine-driven free path).
func TestSubtleKeyTableBounded(t *testing.T) {
	js, w := newWeb(t, spidermonkey.Config{})
	eval(t, js, `
		globalThis.__r = {};
		(async () => {
			const enc = new TextEncoder();
			const mk = (name) => crypto.subtle.importKey("raw", enc.encode(name),
				{ name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
			const sign = (k) => crypto.subtle.sign("HMAC", k, enc.encode("m"));

			const hot = await mk("hot");
			const cold = await mk("cold"); // keep a live JS ref, but never use it again
			await sign(cold); // works now

			// Import more than the table cap (1024) worth of throwaway keys while
			// keeping the hot key most-recently-used. The cold key ages out.
			for (let i = 0; i < 1200; i++) {
				await mk("throwaway-" + i);
				await sign(hot);
			}

			try { await sign(cold); __r.coldEvicted = false; }
			catch { __r.coldEvicted = true; }
			const sig = await sign(hot);
			__r.hotOk = new Uint8Array(sig).length === 32;
		})().catch(e => { __r.err = String(e && e.message || e); });
	`)
	drainWeb(t, w)
	if got := evalString(t, js, `__r.err ?? ""`); got != "" {
		t.Fatalf("threw: %s", got)
	}
	if got := evalString(t, js, `String(__r.hotOk)`); got != "true" {
		t.Errorf("actively-used key was evicted under table pressure (got %q)", got)
	}
	if got := evalString(t, js, `String(__r.coldEvicted)`); got != "true" {
		t.Errorf("cold key was NOT evicted — key table is not bounded (got %q)", got)
	}
}

// TestAESJWKExportAlg verifies exported AES JWK `alg` reflects the key SIZE and
// mode (A128GCM/A256CBC/…), not a hardcoded A256GCM.
func TestAESJWKExportAlg(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const mk = async (name, length) => {
				const k = await crypto.subtle.generateKey({ name, length }, true, name === "AES-CTR" ? ["encrypt"] : ["encrypt", "decrypt"]);
				return (await crypto.subtle.exportKey("jwk", k)).alg;
			};
			__c.gcm128 = await mk("AES-GCM", 128);
			__c.gcm256 = await mk("AES-GCM", 256);
			__c.cbc256 = await mk("AES-CBC", 256);
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.err ?? ""`); got != "" {
		t.Fatalf("unexpected error: %s", got)
	}
	for expr, want := range map[string]string{
		"__c.gcm128": "A128GCM", "__c.gcm256": "A256GCM", "__c.cbc256": "A256CBC",
	} {
		if got := evalString(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// TestSubtleRSAPrivateComponentCapped verifies importing a JWK with a small n but
// huge p/q is rejected (the Precompute DoS the modulus cap alone missed).
func TestSubtleRSAPrivateComponentCapped(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const huge = "_".repeat(6000); // ~36 kbit base64url value
			try {
				await crypto.subtle.importKey("jwk",
					{ kty: "RSA", n: "sXchDaQ", e: "AQAB", d: huge, p: huge, q: huge, dp: "AA", dq: "AA", qi: "AA" },
					{ name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" }, false, ["sign"]);
				__c.rsa = "no-throw";
			} catch { __c.rsa = "rejected"; }
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.rsa`); got != "rejected" {
		t.Errorf("oversized RSA private component = %q, want rejected", got)
	}
}

// crypto.subtle.deriveKey to an HMAC key WITHOUT an explicit length must
// default to the hash's block size (512 bits for SHA-256, 1024 for SHA-512),
// per WebCrypto's "get key length" operation — NOT a flat 256. Regression: the
// derived key was truncated to 256 bits, producing MAC material incompatible
// with every compliant engine (Node/browsers). We cross-check by deriving the
// same bits explicitly and confirming the signature matches only at the
// spec-correct length.
func TestDeriveKeyHMACDefaultLength(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const enc = new TextEncoder();
			const base = await crypto.subtle.importKey(
				"raw", enc.encode("password"), "PBKDF2", false, ["deriveKey", "deriveBits"]);
			const salt = enc.encode("salt");
			const kdf = { name: "PBKDF2", salt, iterations: 1000, hash: "SHA-256" };

			// Derive an HMAC/SHA-256 key with NO length -> spec default 512 bits.
			const mac = await crypto.subtle.deriveKey(
				kdf, base, { name: "HMAC", hash: "SHA-256" }, true, ["sign"]);
			const exported = new Uint8Array(await crypto.subtle.exportKey("raw", mac));
			__c.macLen = exported.length;

			// Independently derive 512 bits and import as HMAC; signatures must match.
			const bits512 = await crypto.subtle.deriveBits(kdf, base, 512);
			const mac512 = await crypto.subtle.importKey(
				"raw", bits512, { name: "HMAC", hash: "SHA-256" }, true, ["sign"]);
			const data = enc.encode("message");
			const sigA = new Uint8Array(await crypto.subtle.sign("HMAC", mac, data));
			const sigB = new Uint8Array(await crypto.subtle.sign("HMAC", mac512, data));
			__c.match = sigA.length === sigB.length && sigA.every((b, i) => b === sigB[i]);
		})().catch(e => { __c.err = String(e.stack || e); });
	`)
	if got := eval(t, js, `__c.macLen`).Int(); got != 64 {
		t.Errorf("derived HMAC key length = %d bytes, want 64 (512 bits)", got)
	}
	if !eval(t, js, `__c.match`).Bool() {
		t.Error("deriveKey(HMAC, no length) did not match an explicit 512-bit derivation")
	}
}

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

// ML-DSA is the post-quantum signature scheme of FIPS 204. The known-answer
// vectors live in the Web Platform Tests; what this pins is what those do not
// reach — that every key format round-trips (the DER is hand-encoded here), that
// a signature is bound to the context string it was made under, and that a
// tampered signature is rejected rather than merely reported differently.
func TestMLDSA(t *testing.T) {
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
			const msg = new TextEncoder().encode("post-quantum");
			for (const name of ["ML-DSA-44", "ML-DSA-65", "ML-DSA-87"]) {
				const kp = await crypto.subtle.generateKey({ name }, true, ["sign", "verify"]);
				const sig = await crypto.subtle.sign({ name }, kp.privateKey, msg);
				const ok = await crypto.subtle.verify({ name }, kp.publicKey, sig, msg);
				out.push(name + ":sign:" + sig.byteLength + ":" + (ok ? "ok" : "BAD"));

				// A flipped bit must not verify.
				const bad = new Uint8Array(sig.slice(0));
				bad[0] ^= 1;
				out.push(name + ":tampered:" + (await crypto.subtle.verify({ name }, kp.publicKey, bad, msg) ? "ACCEPTED" : "rejected"));

				// The context string is part of what is signed: a signature made
				// under one context does not verify under another, or under none.
				const ctx = new TextEncoder().encode("ctx");
				const csig = await crypto.subtle.sign({ name, context: ctx }, kp.privateKey, msg);
				const inCtx = await crypto.subtle.verify({ name, context: ctx }, kp.publicKey, csig, msg);
				const noCtx = await crypto.subtle.verify({ name }, kp.publicKey, csig, msg);
				out.push(name + ":context:" + (inCtx ? "ok" : "BAD") + ":" + (noCtx ? "LEAKED" : "bound"));

				// Every format round-trips, and an imported key still produces or
				// checks signatures the original pair agrees with.
				for (const [fmt, key] of [["raw-seed", kp.privateKey], ["pkcs8", kp.privateKey],
					["raw-public", kp.publicKey], ["spki", kp.publicKey], ["jwk", kp.privateKey]]) {
					const data = await crypto.subtle.exportKey(fmt, key);
					const usages = key.type === "private" ? ["sign"] : ["verify"];
					const again = await crypto.subtle.importKey(fmt, data, { name }, true, usages);
					let good = again.algorithm.name === name && again.type === key.type;
					if (again.type === "private") {
						const s = await crypto.subtle.sign({ name }, again, msg);
						good = good && await crypto.subtle.verify({ name }, kp.publicKey, s, msg);
					} else {
						good = good && await crypto.subtle.verify({ name }, again, sig, msg);
					}
					out.push(name + ":" + fmt + ":" + (good ? "ok" : "BAD"));
				}
			}

			// A usage ML-DSA cannot have is a SyntaxError, as for any algorithm.
			try { await crypto.subtle.generateKey({ name: "ML-DSA-44" }, true, ["encrypt"]); out.push("usage:NO-THROW"); }
			catch (e) { out.push("usage:" + e.name); }

			globalThis.__r = out.join(" | ");
		})().catch((e) => { globalThis.__r = "THREW " + e.name + ": " + e.message + " after " + globalThis.__out.join(" | "); });
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
	// The signature sizes are each parameter set's own, and are spelled out so a
	// key silently built for the wrong set cannot pass.
	line := func(name, size string) string {
		return name + ":sign:" + size + ":ok | " + name + ":tampered:rejected | " + name + ":context:ok:bound | " +
			name + ":raw-seed:ok | " + name + ":pkcs8:ok | " + name + ":raw-public:ok | " + name + ":spki:ok | " + name + ":jwk:ok | "
	}
	want := line("ML-DSA-44", "2420") + line("ML-DSA-65", "3309") + line("ML-DSA-87", "4627") + "usage:SyntaxError"
	if got := v.Value.String(); got != want {
		t.Errorf("ML-DSA =\n %s\nwant\n %s", got, want)
	}
}

// ML-KEM is the post-quantum key encapsulation mechanism of FIPS 203, and the
// four operations it adds to crypto.subtle (encapsulate/decapsulate, each in a
// bits and a key form) exist nowhere else in the Web Crypto surface. The test
// pins the property that makes it a KEM — both sides reach the same secret —
// plus every key format, since those are hand-encoded DER here.
func TestMLKEM(t *testing.T) {
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
			for (const name of ["ML-KEM-512", "ML-KEM-768", "ML-KEM-1024"]) {
				const kp = await crypto.subtle.generateKey({ name }, true,
					["encapsulateBits", "decapsulateBits", "encapsulateKey", "decapsulateKey"]);

				// The KEM property: what the sender encapsulates is what the
				// holder of the private key decapsulates.
				const enc = await crypto.subtle.encapsulateBits({ name }, kp.publicKey);
				const back = await crypto.subtle.decapsulateBits({ name }, kp.privateKey, enc.ciphertext);
				const same = new Uint8Array(enc.sharedKey).join() === new Uint8Array(back).join();
				out.push(name + ":bits:" + enc.sharedKey.byteLength + "/" + enc.ciphertext.byteLength + ":" + (same ? "agree" : "DIFFER"));

				// The key form wraps the same secret as an AES key.
				const ek = await crypto.subtle.encapsulateKey({ name }, kp.publicKey,
					{ name: "AES-GCM", length: 256 }, true, ["encrypt", "decrypt"]);
				const dk = await crypto.subtle.decapsulateKey({ name }, kp.privateKey, ek.ciphertext,
					{ name: "AES-GCM", length: 256 }, true, ["encrypt", "decrypt"]);
				const a = new Uint8Array(await crypto.subtle.exportKey("raw", ek.sharedKey));
				const b = new Uint8Array(await crypto.subtle.exportKey("raw", dk));
				out.push(name + ":key:" + ek.sharedKey.algorithm.name + ":" + (a.join() === b.join() ? "agree" : "DIFFER"));

				// Every format round-trips, and an imported key still decapsulates
				// what the original public key encapsulated.
				for (const [fmt, key] of [["raw-seed", kp.privateKey], ["pkcs8", kp.privateKey],
					["raw-public", kp.publicKey], ["spki", kp.publicKey], ["jwk", kp.privateKey]]) {
					const data = await crypto.subtle.exportKey(fmt, key);
					const usages = key.type === "private" ? ["decapsulateBits"] : ["encapsulateBits"];
					const again = await crypto.subtle.importKey(fmt, data, { name }, true, usages);
					let ok = again.algorithm.name === name && again.type === key.type;
					if (again.type === "private") {
						const s = await crypto.subtle.decapsulateBits({ name }, again, enc.ciphertext);
						ok = ok && new Uint8Array(s).join() === new Uint8Array(enc.sharedKey).join();
					}
					out.push(name + ":" + fmt + ":" + (ok ? "ok" : "BAD"));
				}
			}



			// A usage ML-KEM cannot have is a SyntaxError, as for any algorithm.
			try { await crypto.subtle.generateKey({ name: "ML-KEM-768" }, true, ["sign"]); out.push("usage:NO-THROW"); }
			catch (e) { out.push("usage:" + e.name); }

			globalThis.__r = out.join(" | ");
		})().catch((e) => { globalThis.__r = "THREW " + e.name + ": " + e.message + " after " + globalThis.__out.join(" | "); });
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
	// All three parameter sets round-trip. 512 comes from CIRCL, the other two
	// from crypto/mlkem, and the shapes below are each set's own key sizes.
	want := "ML-KEM-512:bits:32/768:agree | ML-KEM-512:key:AES-GCM:agree | " +
		"ML-KEM-512:raw-seed:ok | ML-KEM-512:pkcs8:ok | ML-KEM-512:raw-public:ok | ML-KEM-512:spki:ok | ML-KEM-512:jwk:ok | " +
		"ML-KEM-768:bits:32/1088:agree | ML-KEM-768:key:AES-GCM:agree | " +
		"ML-KEM-768:raw-seed:ok | ML-KEM-768:pkcs8:ok | ML-KEM-768:raw-public:ok | ML-KEM-768:spki:ok | ML-KEM-768:jwk:ok | " +
		"ML-KEM-1024:bits:32/1568:agree | ML-KEM-1024:key:AES-GCM:agree | " +
		"ML-KEM-1024:raw-seed:ok | ML-KEM-1024:pkcs8:ok | ML-KEM-1024:raw-public:ok | ML-KEM-1024:spki:ok | ML-KEM-1024:jwk:ok | " +
		"usage:SyntaxError"
	if got := v.Value.String(); got != want {
		t.Errorf("ML-KEM =\n %s\nwant\n %s", got, want)
	}
}

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

// WebCrypto AES-KW (RFC 3394 key wrap) and Ed25519 (EdDSA) coverage. Both are
// anchored to fixed, published known-good vectors so a shared-bug round trip
// cannot pass silently: the AES-KW ciphertext is the RFC 3394 §4.1 vector, and
// the Ed25519 public key + empty-message signature are RFC 8032 Test 1.

func TestSubtleAESKeyWrapRFC3394(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	// RFC 3394 §4.1: wrap 128 bits of key data with a 128-bit KEK.
	runAsync(t, js, `
		(async () => {
			const hex = (s) => Uint8Array.from(s.match(/../g).map((b) => parseInt(b, 16)));
			const toHex = (u) => [...new Uint8Array(u)].map((b) => b.toString(16).padStart(2, "0")).join("");
			const kekBytes = hex("000102030405060708090A0B0C0D0E0F");
			const keyData = hex("00112233445566778899AABBCCDDEEFF");
			const kek = await crypto.subtle.importKey("raw", kekBytes, { name: "AES-KW" }, false, ["wrapKey", "unwrapKey"]);
			const toWrap = await crypto.subtle.importKey("raw", keyData, { name: "AES-CBC" }, true, ["encrypt", "decrypt"]);
			const wrapped = await crypto.subtle.wrapKey("raw", toWrap, kek, { name: "AES-KW" });
			__c.wrapped = toHex(wrapped);
			const unwrapped = await crypto.subtle.unwrapKey("raw", wrapped, kek, { name: "AES-KW" }, { name: "AES-CBC" }, true, ["encrypt", "decrypt"]);
			__c.unwrapped = toHex(await crypto.subtle.exportKey("raw", unwrapped));
			// A corrupted wrap must fail the integrity check.
			const bad = new Uint8Array(wrapped); bad[0] ^= 1;
			try { await crypto.subtle.unwrapKey("raw", bad, kek, { name: "AES-KW" }, { name: "AES-CBC" }, true, ["encrypt"]); __c.tamper = "no-throw"; }
			catch (e) { __c.tamper = e.name; }
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)

	const wantCT = "1fa68b0a8112b447aef34bd8fb5a7b829d3e862371d2cfe5"
	if got := evalString(t, js, `__c.wrapped`); got != wantCT {
		t.Errorf("AES-KW ciphertext = %s, want %s (RFC 3394 §4.1)", got, wantCT)
	}
	if got := evalString(t, js, `__c.unwrapped`); got != "00112233445566778899aabbccddeeff" {
		t.Errorf("AES-KW unwrap = %s, want the original key data", got)
	}
	if got := evalString(t, js, `__c.tamper`); got != "OperationError" {
		t.Errorf("corrupted unwrap = %s, want OperationError", got)
	}
}

func TestSubtleEd25519RFC8032(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	// RFC 8032 Test 1: verify the published empty-message signature under the
	// published public key. This is an independent vector — no key we generated.
	runAsync(t, js, `
		(async () => {
			const hex = (s) => Uint8Array.from(s.match(/../g).map((b) => parseInt(b, 16)));
			const pub = hex("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a");
			const sig = hex("e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b");
			const key = await crypto.subtle.importKey("raw", pub, { name: "Ed25519" }, true, ["verify"]);
			__c.rfcVerify = await crypto.subtle.verify({ name: "Ed25519" }, key, sig, new Uint8Array(0));
			__c.rfcTamper = await crypto.subtle.verify({ name: "Ed25519" }, key, sig, new TextEncoder().encode("x"));

			// generate / sign / verify round trip, anchored by the RFC-proven verify.
			const kp = await crypto.subtle.generateKey({ name: "Ed25519" }, true, ["sign", "verify"]);
			const msg = new TextEncoder().encode("EdDSA over WinterTC");
			const s = await crypto.subtle.sign({ name: "Ed25519" }, kp.privateKey, msg);
			__c.ok = await crypto.subtle.verify({ name: "Ed25519" }, kp.publicKey, s, msg);
			const other = await crypto.subtle.generateKey({ name: "Ed25519" }, true, ["sign", "verify"]);
			__c.wrongKey = await crypto.subtle.verify({ name: "Ed25519" }, other.publicKey, s, msg);

			// JWK export/import round trip (jose's EdDSA path).
			const jwk = await crypto.subtle.exportKey("jwk", kp.privateKey);
			__c.kty = jwk.kty; __c.crv = jwk.crv; __c.hasD = typeof jwk.d === "string";
			const reimported = await crypto.subtle.importKey("jwk", jwk, { name: "Ed25519" }, true, ["sign"]);
			const s2 = await crypto.subtle.sign({ name: "Ed25519" }, reimported, msg);
			__c.jwkRoundTrip = await crypto.subtle.verify({ name: "Ed25519" }, kp.publicKey, s2, msg);
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)

	if got := evalString(t, js, `String(__c.rfcVerify)`); got != "true" {
		t.Error("RFC 8032 Test 1 signature did not verify")
	}
	if got := evalString(t, js, `String(__c.rfcTamper)`); got != "false" {
		t.Error("RFC 8032 signature verified over the wrong message")
	}
	if got := evalString(t, js, `String(__c.ok)`); got != "true" {
		t.Error("generated Ed25519 sign/verify round trip failed")
	}
	if got := evalString(t, js, `String(__c.wrongKey)`); got != "false" {
		t.Error("Ed25519 signature verified under the wrong key")
	}
	if got := evalString(t, js, `__c.kty + "/" + __c.crv + "/" + __c.hasD`); got != "OKP/Ed25519/true" {
		t.Errorf("exported Ed25519 private JWK = %s, want OKP/Ed25519/true", got)
	}
	if got := evalString(t, js, `String(__c.jwkRoundTrip)`); got != "true" {
		t.Error("Ed25519 JWK export/import round trip failed")
	}
}

// These pin the crypto-input hardening: a malformed parameter must surface as a
// catchable OperationError, never a Go panic that would tear down the host
// (the AES IV cases previously panicked in crypto/cipher), and a weak KDF
// parameter must be rejected rather than silently accepted.
func TestSubtleCryptoInputHardening(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	runAsync(t, js, `
		(async () => {
			const raw = new Uint8Array(32).fill(7);
			const gcmKey = await crypto.subtle.importKey("raw", raw, { name: "AES-GCM" }, false, ["encrypt", "decrypt"]);
			const data = new TextEncoder().encode("hello");

			// AES-GCM with a non-96-bit IV is legal per spec; must round-trip.
			const iv16 = new Uint8Array(16).fill(3);
			const ct = await crypto.subtle.encrypt({ name: "AES-GCM", iv: iv16 }, gcmKey, data);
			const pt = await crypto.subtle.decrypt({ name: "AES-GCM", iv: iv16 }, gcmKey, ct);
			__c.gcm16 = new TextDecoder().decode(pt);

			// AES-CBC with a wrong-length IV must throw, not panic.
			const cbcKey = await crypto.subtle.importKey("raw", raw, { name: "AES-CBC" }, false, ["encrypt"]);
			try { await crypto.subtle.encrypt({ name: "AES-CBC", iv: new Uint8Array(12) }, cbcKey, data); __c.cbcBad = "no-throw"; }
			catch (e) { __c.cbcBad = e.name; }

			// PBKDF2 with 0 iterations must be rejected (no silent 1-round KDF).
			const pw = await crypto.subtle.importKey("raw", new TextEncoder().encode("pw"), { name: "PBKDF2" }, false, ["deriveBits"]);
			try {
				await crypto.subtle.deriveBits({ name: "PBKDF2", hash: "SHA-256", salt: new Uint8Array(8), iterations: 0 }, pw, 256);
				__c.pbkdf2 = "no-throw";
			} catch (e) { __c.pbkdf2 = e.name; }
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)

	if got := evalString(t, js, `__c.err ?? ""`); got != "" {
		t.Fatalf("unexpected error: %s", got)
	}
	if got := evalString(t, js, `__c.gcm16`); got != "hello" {
		t.Errorf("AES-GCM with 16-byte IV round trip = %q, want hello", got)
	}
	if got := evalString(t, js, `__c.cbcBad`); got != "OperationError" {
		t.Errorf("AES-CBC wrong IV = %q, want OperationError", got)
	}
	if got := evalString(t, js, `__c.pbkdf2`); got != "OperationError" {
		t.Errorf("PBKDF2 iterations=0 = %q, want OperationError", got)
	}
}

// TestSubtleRSAModulusCapped verifies importing a JWK with an absurdly large RSA
// modulus is rejected, so a guest can't drive an uninterruptible multi-minute
// modexp (via Precompute/verify) that would pin the shared host.
func TestSubtleRSAModulusCapped(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			// A ~40000-bit modulus: 5000 base64url 'A' chars ≈ 30000 bits of value
			// with a leading set byte pushes it well past the 8192-bit cap.
			const n = "_".repeat(6000); // base64url; decodes to ~36000 bits, all 1s
			try {
				await crypto.subtle.importKey("jwk",
					{ kty: "RSA", n, e: "AQAB" },
					{ name: "RSA-PSS", hash: "SHA-256" }, false, ["verify"]);
				__c.rsa = "no-throw";
			} catch (e) { __c.rsa = "rejected"; }
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.err ?? ""`); got != "" {
		t.Fatalf("unexpected error: %s", got)
	}
	if got := evalString(t, js, `__c.rsa`); got != "rejected" {
		t.Errorf("oversized RSA modulus import = %q, want rejected", got)
	}
}

// deriveKey must work with a base key imported with ONLY ["deriveKey"] usage
// (the canonical PBKDF2 password pattern); it must not also require deriveBits.
func TestDeriveKeyWithDeriveKeyUsageOnly(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const pw = await crypto.subtle.importKey("raw", new TextEncoder().encode("pw"),
				{ name: "PBKDF2" }, false, ["deriveKey"]); // deriveKey only
			const key = await crypto.subtle.deriveKey(
				{ name: "PBKDF2", hash: "SHA-256", salt: new Uint8Array(16), iterations: 1000 },
				pw, { name: "AES-GCM", length: 256 }, false, ["encrypt", "decrypt"]);
			__c.type = key && key.type;
		})().catch((e) => { __c.err = String(e && (e.name + ": " + e.message) || e); });
	`)
	if got := evalString(t, js, `__c.err ?? ""`); got != "" {
		t.Fatalf("deriveKey with deriveKey-only usage failed: %s", got)
	}
	if got := evalString(t, js, `__c.type`); got != "secret" {
		t.Fatalf("derived key type = %q, want secret", got)
	}
}

// A non-extractable AES key must not expose its raw bytes through any property;
// exportKey stays the only (gated) way out.
func TestNonExtractableKeyMaterialHidden(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const k = await crypto.subtle.generateKey({ name: "AES-GCM", length: 256 }, false, ["encrypt", "decrypt"]);
			__c.rawProp = (k._raw === undefined) ? "hidden" : "LEAKED";
			try { await crypto.subtle.exportKey("raw", k); __c.exp = "no-throw"; }
			catch (e) { __c.exp = e.name; }
			// The key must still WORK for its usage (material reachable internally).
			const iv = new Uint8Array(12);
			const ct = await crypto.subtle.encrypt({ name: "AES-GCM", iv }, k, new TextEncoder().encode("x"));
			__c.works = ct.byteLength > 0;
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.err ?? ""`); got != "" {
		t.Fatalf("unexpected: %s", got)
	}
	if got := evalString(t, js, `__c.rawProp`); got != "hidden" {
		t.Fatalf("non-extractable key raw material = %q, want hidden", got)
	}
	if got := evalString(t, js, `__c.exp`); got != "InvalidAccessError" {
		t.Fatalf("exportKey on non-extractable = %q, want InvalidAccessError", got)
	}
	if evalString(t, js, `String(__c.works)`) != "true" {
		t.Fatalf("non-extractable key stopped working for its own usage")
	}
}

// AES-CTR with a partial counter length (WebCrypto AesCtrParams.length) must
// round-trip (encrypt then decrypt yields the original), across a block
// boundary so the counter actually increments.
func TestSubtleAESCTRPartialCounterRoundTrip(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	runAsync(t, js, `
		(async () => {
			const raw = new Uint8Array(32).fill(5);
			const key = await crypto.subtle.importKey("raw", raw, { name: "AES-CTR" }, false, ["encrypt", "decrypt"]);
			const counter = new Uint8Array(16); counter[15] = 1;
			const data = new TextEncoder().encode("a".repeat(40)); // > 2 blocks
			const params = { name: "AES-CTR", counter, length: 64 };
			const ct = await crypto.subtle.encrypt(params, key, data);
			const pt = await crypto.subtle.decrypt(params, key, ct);
			__c.roundtrip = new TextDecoder().decode(pt) === "a".repeat(40);
			__c.changed = new Uint8Array(ct)[0] !== data[0];
		})().catch((e) => { __c.err = String(e && (e.name + ": " + e.message) || e); });
	`)
	if got := evalString(t, js, `__c.err ?? ""`); got != "" {
		t.Fatalf("AES-CTR length=64 failed: %s", got)
	}
	if evalString(t, js, `String(__c.roundtrip)`) != "true" {
		t.Fatalf("AES-CTR partial-counter did not round-trip")
	}
	if evalString(t, js, `String(__c.changed)`) != "true" {
		t.Fatalf("AES-CTR produced no ciphertext change")
	}
}

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

func TestSubtleAESGCM(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	runAsync(t, js, `
		(async () => {
			const raw = new Uint8Array(32).fill(9);
			const key = await crypto.subtle.importKey("raw", raw, { name: "AES-GCM" }, false, ["encrypt", "decrypt"]);
			const iv = new Uint8Array(12).fill(5);
			const data = new TextEncoder().encode("secret payload");
			const ct = new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv }, key, data));
			__c.ctHex = [...ct].map((b) => b.toString(16).padStart(2, "0")).join("");
			const pt = await crypto.subtle.decrypt({ name: "AES-GCM", iv }, key, ct);
			__c.pt = new TextDecoder().decode(pt);
			// wrong IV -> reject
			try { await crypto.subtle.decrypt({ name: "AES-GCM", iv: new Uint8Array(12) }, key, ct); __c.bad = "no-throw"; }
			catch (e) { __c.bad = e.name; }
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.pt`); got != "secret payload" {
		t.Errorf("AES-GCM round trip = %q", got)
	}
	if got := evalString(t, js, `__c.bad`); got != "OperationError" {
		t.Errorf("wrong IV should throw OperationError, got %q", got)
	}
	// Cross-check ciphertext+tag against Go.
	key := make([]byte, 32)
	for i := range key {
		key[i] = 9
	}
	iv := make([]byte, 12)
	for i := range iv {
		iv[i] = 5
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	want := hex.EncodeToString(gcm.Seal(nil, iv, []byte("secret payload"), nil))
	if got := evalString(t, js, `__c.ctHex`); got != want {
		t.Errorf("AES-GCM ct = %s, want %s (Go)", got, want)
	}
}

func TestSubtleAESGenerate(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	runAsync(t, js, `
		(async () => {
			const key = await crypto.subtle.generateKey({ name: "AES-CBC", length: 256 }, true, ["encrypt", "decrypt"]);
			__c.type = key.type;
			__c.algo = key.algorithm.name + "/" + key.algorithm.length;
			const exported = new Uint8Array(await crypto.subtle.exportKey("raw", key));
			__c.keyLen = exported.length;
			const iv = crypto.getRandomValues(new Uint8Array(16));
			const data = new TextEncoder().encode("cbc message here");
			const ct = await crypto.subtle.encrypt({ name: "AES-CBC", iv }, key, data);
			const pt = await crypto.subtle.decrypt({ name: "AES-CBC", iv }, key, ct);
			__c.round = new TextDecoder().decode(pt);
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.type`); got != "secret" {
		t.Errorf("generated key type = %q", got)
	}
	if got := evalString(t, js, `__c.algo`); got != "AES-CBC/256" {
		t.Errorf("algorithm = %q", got)
	}
	if got := evalString(t, js, `__c.keyLen`); got != "32" {
		t.Errorf("exported key length = %q, want 32", got)
	}
	if got := evalString(t, js, `__c.round`); got != "cbc message here" {
		t.Errorf("AES-CBC round trip = %q", got)
	}
}

func TestSubtleECDHDerive(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	runAsync(t, js, `
		(async () => {
			const alice = await crypto.subtle.generateKey({ name: "ECDH", namedCurve: "P-256" }, true, ["deriveBits", "deriveKey"]);
			const bob = await crypto.subtle.generateKey({ name: "ECDH", namedCurve: "P-256" }, true, ["deriveBits", "deriveKey"]);
			// Both sides derive the same shared secret.
			const secretA = new Uint8Array(await crypto.subtle.deriveBits({ name: "ECDH", public: bob.publicKey }, alice.privateKey, 256));
			const secretB = new Uint8Array(await crypto.subtle.deriveBits({ name: "ECDH", public: alice.publicKey }, bob.privateKey, 256));
			__c.match = secretA.length === 32 && secretA.every((b, i) => b === secretB[i]);
			// deriveKey into an AES-GCM key that actually works.
			const aesKey = await crypto.subtle.deriveKey(
				{ name: "ECDH", public: bob.publicKey }, alice.privateKey,
				{ name: "AES-GCM", length: 256 }, false, ["encrypt", "decrypt"]);
			const iv = crypto.getRandomValues(new Uint8Array(12));
			const ct = await crypto.subtle.encrypt({ name: "AES-GCM", iv }, aesKey, new TextEncoder().encode("hi"));
			__c.derived = new TextDecoder().decode(await crypto.subtle.decrypt({ name: "AES-GCM", iv }, aesKey, ct));
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.err ?? ""`); got != "" {
		t.Fatalf("ECDH derive rejected: %s", got)
	}
	if got := evalString(t, js, `String(__c.match)`); got != "true" {
		t.Error("ECDH shared secrets did not match")
	}
	if got := evalString(t, js, `__c.derived`); got != "hi" {
		t.Errorf("deriveKey AES round trip = %q", got)
	}
}

func TestSubtleHKDFandPBKDF2(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	runAsync(t, js, `
		(async () => {
			// HKDF
			const ikm = await crypto.subtle.importKey("raw", new TextEncoder().encode("input key material"), "HKDF", false, ["deriveBits"]);
			const bits = new Uint8Array(await crypto.subtle.deriveBits(
				{ name: "HKDF", hash: "SHA-256", salt: new Uint8Array(0), info: new Uint8Array(0) }, ikm, 256));
			__c.hkdfLen = bits.length;
			// Deterministic.
			const bits2 = new Uint8Array(await crypto.subtle.deriveBits(
				{ name: "HKDF", hash: "SHA-256", salt: new Uint8Array(0), info: new Uint8Array(0) }, ikm, 256));
			__c.hkdfDet = bits.every((b, i) => b === bits2[i]);

			// PBKDF2
			const pw = await crypto.subtle.importKey("raw", new TextEncoder().encode("password"), "PBKDF2", false, ["deriveBits"]);
			const dk = new Uint8Array(await crypto.subtle.deriveBits(
				{ name: "PBKDF2", hash: "SHA-256", salt: new TextEncoder().encode("salt"), iterations: 1 }, pw, 256));
			__c.pbkdf2Hex = [...dk].map((b) => b.toString(16).padStart(2, "0")).join("");
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `String(__c.hkdfLen)`); got != "32" {
		t.Errorf("HKDF length = %q", got)
	}
	if got := evalString(t, js, `String(__c.hkdfDet)`); got != "true" {
		t.Error("HKDF not deterministic")
	}
	// PBKDF2-HMAC-SHA256, "password"/"salt"/1 iter/32 bytes — same vector as
	// the node:crypto test.
	const want = "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"
	if got := evalString(t, js, `__c.pbkdf2Hex`); got != want {
		t.Errorf("PBKDF2 = %s, want %s", got, want)
	}
}

func TestSubtleRSAOAEP(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	runAsync(t, js, `
		(async () => {
			const { publicKey, privateKey } = await crypto.subtle.generateKey(
				{ name: "RSA-OAEP", modulusLength: 2048, publicExponent: new Uint8Array([1,0,1]), hash: "SHA-256" },
				true, ["encrypt", "decrypt"]);
			const data = new TextEncoder().encode("subtle oaep message");
			const ct = await crypto.subtle.encrypt({ name: "RSA-OAEP" }, publicKey, data);
			const pt = await crypto.subtle.decrypt({ name: "RSA-OAEP" }, privateKey, ct);
			__c.round = new TextDecoder().decode(pt);
			// wrong ciphertext -> reject
			try {
				const bad = new Uint8Array(ct); bad[0] ^= 0xff;
				await crypto.subtle.decrypt({ name: "RSA-OAEP" }, privateKey, bad);
				__c.bad = "no-throw";
			} catch (e) { __c.bad = e.name; }
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.err ?? ""`); got != "" {
		t.Fatalf("RSA-OAEP rejected: %s", got)
	}
	if got := evalString(t, js, `__c.round`); got != "subtle oaep message" {
		t.Errorf("RSA-OAEP round trip = %q", got)
	}
	if got := evalString(t, js, `__c.bad`); got != "OperationError" {
		t.Errorf("corrupt ciphertext should throw OperationError, got %q", got)
	}
}

func TestSubtleWrapKey(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	runAsync(t, js, `
		(async () => {
			// Wrap an AES key with AES-GCM, then unwrap and use it.
			const aesKey = await crypto.subtle.generateKey({ name: "AES-GCM", length: 256 }, true, ["encrypt", "decrypt"]);
			const wrapKey = await crypto.subtle.generateKey({ name: "AES-GCM", length: 256 }, true, ["wrapKey", "unwrapKey"]);
			const iv = crypto.getRandomValues(new Uint8Array(12));
			const wrapped = await crypto.subtle.wrapKey("raw", aesKey, wrapKey, { name: "AES-GCM", iv });
			const unwrapped = await crypto.subtle.unwrapKey("raw", wrapped, wrapKey, { name: "AES-GCM", iv }, { name: "AES-GCM" }, true, ["encrypt", "decrypt"]);
			// The unwrapped key must equal the original (compare raw material).
			const a = new Uint8Array(await crypto.subtle.exportKey("raw", aesKey));
			const b = new Uint8Array(await crypto.subtle.exportKey("raw", unwrapped));
			__c.same = a.length === b.length && a.every((x, i) => x === b[i]);
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	if got := evalString(t, js, `__c.err ?? ""`); got != "" {
		t.Fatalf("wrapKey rejected: %s", got)
	}
	if got := evalString(t, js, `String(__c.same)`); got != "true" {
		t.Error("unwrapped key material differs from original")
	}
}
