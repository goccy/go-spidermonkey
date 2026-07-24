package web_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
