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
