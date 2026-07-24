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
