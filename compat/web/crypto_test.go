package web_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
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
