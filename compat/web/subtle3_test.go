package web_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
