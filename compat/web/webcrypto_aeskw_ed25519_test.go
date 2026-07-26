package web_test

// WebCrypto AES-KW (RFC 3394 key wrap) and Ed25519 (EdDSA) coverage. Both are
// anchored to fixed, published known-good vectors so a shared-bug round trip
// cannot pass silently: the AES-KW ciphertext is the RFC 3394 §4.1 vector, and
// the Ed25519 public key + empty-message signature are RFC 8032 Test 1.

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
