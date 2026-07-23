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
			const payload = b64u(enc.encode(JSON.stringify({ sub: "goccy", iat: 1720000000 })));
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
