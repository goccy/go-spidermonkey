package web_test

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
