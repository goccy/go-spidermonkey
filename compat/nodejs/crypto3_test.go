package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func TestCryptoRSAEncrypt(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const crypto = require("crypto");
		globalThis.r = {};
		const { publicKey, privateKey } = crypto.generateKeyPairSync("rsa", { modulusLength: 2048 });
		const msg = Buffer.from("secret via RSA-OAEP");
		const ct = crypto.publicEncrypt({ key: publicKey, padding: 4, oaepHash: "sha256" }, msg);
		r.oaep = crypto.privateDecrypt({ key: privateKey, padding: 4, oaepHash: "sha256" }, ct).toString();
		const ct1 = crypto.publicEncrypt({ key: publicKey, padding: 1 }, msg);
		r.pkcs1 = crypto.privateDecrypt({ key: privateKey, padding: 1 }, ct1).toString();
	`)
	if got := evalStr(t, js, "r.oaep"); got != "secret via RSA-OAEP" {
		t.Errorf("OAEP = %q", got)
	}
	if got := evalStr(t, js, "r.pkcs1"); got != "secret via RSA-OAEP" {
		t.Errorf("PKCS1 = %q", got)
	}
}

func TestCryptoDiffieHellman(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const crypto = require("crypto");
		globalThis.r = {};
		const alice = crypto.createDiffieHellman(512);
		const bob = crypto.createDiffieHellman(alice.getPrime("hex"), alice.getGenerator("hex"));
		alice.generateKeys(); bob.generateKeys();
		const sa = alice.computeSecret(bob.getPublicKey()).toString("hex");
		const sb = bob.computeSecret(alice.getPublicKey()).toString("hex");
		r.match = sa === sb;
	`)
	if got := evalStr(t, js, "String(r.match)"); got != "true" {
		t.Error("DH secrets did not match")
	}
}

// TestCryptoDHModulusCapped verifies a caller-supplied Diffie-Hellman prime that
// is absurdly large is rejected rather than driving a multi-minute, uninterrupt-
// ible modexp that would pin the shared host process.
func TestCryptoDHModulusCapped(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const crypto = require("crypto");
		globalThis.r = {};
		// ~40000-bit prime (10000 hex 'f' digits) — far above maxDHModulusBits.
		const huge = "f".repeat(10000);
		try {
			const dh = crypto.createDiffieHellman(huge, "02");
			dh.generateKeys();
			r.threw = false;
		} catch (e) {
			r.threw = true;
		}
	`)
	if got := evalStr(t, js, "String(r.threw)"); got != "true" {
		t.Error("oversized DH modulus was not rejected")
	}
}

func TestCryptoChaCha(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const crypto = require("crypto");
		globalThis.r = {};
		const key = Buffer.alloc(32, 1), nonce = Buffer.alloc(12, 2);
		const c = crypto.createCipheriv("chacha20-poly1305", key, nonce);
		c.setAAD(Buffer.from("aad"));
		const ct = Buffer.concat([c.update(Buffer.from("chacha msg")), c.final()]);
		const tag = c.getAuthTag();
		const d = crypto.createDecipheriv("chacha20-poly1305", key, nonce);
		d.setAAD(Buffer.from("aad")); d.setAuthTag(tag);
		r.pt = Buffer.concat([d.update(ct), d.final()]).toString();
	`)
	if got := evalStr(t, js, "r.pt"); got != "chacha msg" {
		t.Errorf("chacha round trip = %q", got)
	}
}

func TestRSAPrivateEncryptPublicDecrypt(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const crypto = require("crypto");
		globalThis.r = {};
		const { publicKey, privateKey } = crypto.generateKeyPairSync("rsa", { modulusLength: 2048 });
		const msg = Buffer.from("sign-with-private");
		// The Node round-trip: privateEncrypt then publicDecrypt recovers it.
		const enc = crypto.privateEncrypt(privateKey, msg);
		r.recovered = crypto.publicDecrypt(publicKey, enc).toString();
		// And it must NOT be the same as public-encrypt (distinct primitive).
		r.distinct = enc.toString("hex") !== crypto.publicEncrypt({ key: publicKey, padding: 1 }, msg).toString("hex");
	`)
	if got := evalStr(t, js, `r.recovered`); got != "sign-with-private" {
		t.Fatalf("privateEncrypt/publicDecrypt round-trip = %q, want sign-with-private", got)
	}
	if evalStr(t, js, `String(r.distinct)`) != "true" {
		t.Fatalf("privateEncrypt produced the same output as publicEncrypt")
	}
}
