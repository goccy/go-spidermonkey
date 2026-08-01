package nodejs_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"testing"
)

// TestCipherKeyLengthValidated verifies createCipheriv rejects a key whose length
// doesn't match the named AES variant, rather than silently downgrading (e.g.
// aes-256-gcm with a 16-byte key quietly using AES-128).
func TestCipherKeyLengthValidated(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const crypto = require("crypto");
		globalThis.r = {};
		const chk = (name, fn) => { try { fn(); r[name] = "ok"; } catch (e) { r[name] = "threw"; } };
		const iv = Buffer.alloc(12);
		chk("mismatch", () => crypto.createCipheriv("aes-256-gcm", Buffer.alloc(16), iv).final());
		chk("match", () => crypto.createCipheriv("aes-256-gcm", Buffer.alloc(32), iv).final());
	`)
	if got := evalStr(t, js, "r.mismatch"); got != "threw" {
		t.Errorf("aes-256-gcm with 16-byte key = %q, want threw", got)
	}
	if got := evalStr(t, js, "r.match"); got != "ok" {
		t.Errorf("aes-256-gcm with 32-byte key = %q, want ok", got)
	}
}

// TestCryptoStateGuards verifies Hash/Cipher throw on reuse after finalization.
func TestCryptoStateGuards(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const crypto = require("crypto");
		globalThis.r = {};
		const chk = (name, fn) => { try { fn(); r[name] = "ok"; } catch { r[name] = "threw"; } };
		const h = crypto.createHash("sha256"); h.update("a"); h.digest("hex");
		chk("digest2", () => h.digest("hex"));
		chk("updateAfter", () => h.update("b"));
		const c = crypto.createCipheriv("aes-256-gcm", Buffer.alloc(32), Buffer.alloc(12));
		c.update("A"); c.final();
		chk("final2", () => c.final());
		chk("updateAfterFinal", () => c.update("B"));
		const c2 = crypto.createCipheriv("aes-256-gcm", Buffer.alloc(32), Buffer.alloc(12));
		chk("tagBeforeFinal", () => c2.getAuthTag());
	`)
	for _, name := range []string{"digest2", "updateAfter", "final2", "updateAfterFinal", "tagBeforeFinal"} {
		if got := evalStr(t, js, "r."+name); got != "threw" {
			t.Errorf("%s = %q, want threw", name, got)
		}
	}
}

// TestRandomIntDegenerate verifies crypto.randomInt throws on a degenerate range.
func TestRandomIntDegenerate(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const crypto = require("crypto");
		globalThis.r = {};
		const chk = (name, fn) => { try { const v = fn(); r[name] = Number.isInteger(v) ? "ok:" + v : "bad:" + v; } catch { r[name] = "threw"; } };
		chk("zero", () => crypto.randomInt(0));
		chk("neg", () => crypto.randomInt(-5));
		chk("eq", () => crypto.randomInt(5, 5));
		chk("rev", () => crypto.randomInt(5, 3));
		chk("valid", () => crypto.randomInt(1, 2));
	`)
	for _, name := range []string{"zero", "neg", "eq", "rev"} {
		if got := evalStr(t, js, "r."+name); got != "threw" {
			t.Errorf("randomInt %s = %q, want threw", name, got)
		}
	}
	if got := evalStr(t, js, "r.valid"); got != "ok:1" {
		t.Errorf("randomInt(1,2) = %q, want ok:1", got)
	}
}

// TestCryptoVerifyUnsupportedDigestThrows verifies createVerify().verify() with
// an unsupported digest THROWS rather than returning a truthy error object — a
// truthy return would make `if (verify.verify(...))` accept any signature.
func TestCryptoVerifyUnsupportedDigestThrows(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const crypto = require("crypto");
		const { privateKey, publicKey } = crypto.generateKeyPairSync("ec", { namedCurve: "P-256" });
		const sig = crypto.createSign("sha256").update("data").sign(privateKey);
		try {
			crypto.createVerify("ripemd160").update("data").verify(publicKey, sig);
			r.out = "no-throw";
		} catch { r.out = "threw"; }
		// A supported digest still verifies correctly.
		r.ok = crypto.createVerify("sha256").update("data").verify(publicKey, sig);
	`)
	if got := evalStr(t, js, `r.out`); got != "threw" {
		t.Errorf("verify with unsupported digest = %q, want threw (signature-bypass risk)", got)
	}
	if got := evalStr(t, js, `String(r.ok)`); got != "true" {
		t.Errorf("verify with supported digest = %q, want true", got)
	}
}

// TestGenerateKeyPairPassphraseRejected verifies requesting an encrypted private
// key (cipher/passphrase) is rejected rather than silently returning plaintext.
func TestGenerateKeyPairPassphraseRejected(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const crypto = require("crypto");
		try {
			crypto.generateKeyPairSync("rsa", {
				modulusLength: 2048,
				privateKeyEncoding: { type: "pkcs8", format: "pem", cipher: "aes-256-cbc", passphrase: "secret" },
			});
			r.out = "no-throw";
		} catch { r.out = "threw"; }
	`)
	if got := evalStr(t, js, `r.out`); got != "threw" {
		t.Errorf("encrypted-key request = %q, want threw (silent plaintext key)", got)
	}
}

// TestScryptCostAlias verifies the cost/blockSize/parallelization aliases are
// honored (equal to N/r/p) rather than silently dropped to the weak defaults.
func TestScryptCostAlias(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const crypto = require("crypto");
		const a = crypto.scryptSync("pw", "salt", 32, { cost: 1024, blockSize: 8, parallelization: 1 }).toString("hex");
		const b = crypto.scryptSync("pw", "salt", 32, { N: 1024, r: 8, p: 1 }).toString("hex");
		const def = crypto.scryptSync("pw", "salt", 32).toString("hex");
		r.aliasMatches = a === b;
		r.aliasDiffersFromDefault = a !== def;
	`)
	if got := evalStr(t, js, `String(r.aliasMatches)`); got != "true" {
		t.Errorf("cost alias != N: %q", got)
	}
	if got := evalStr(t, js, `String(r.aliasDiffersFromDefault)`); got != "true" {
		t.Errorf("cost alias silently used the default N: %q", got)
	}
}

// TestRandomFillOffsetSizeAndLarge verifies randomFillSync honors offset/size
// (leaving surrounding bytes untouched) and handles buffers larger than 64 KiB.
func TestRandomFillOffsetSizeAndLarge(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const crypto = require("crypto");
		const buf = Buffer.alloc(10, 0);
		crypto.randomFillSync(buf, 3, 4);
		r.prefixZero = buf[0] === 0 && buf[1] === 0 && buf[2] === 0;
		r.suffixZero = buf[7] === 0 && buf[8] === 0 && buf[9] === 0;
		r.filledNonzero = (buf[3] | buf[4] | buf[5] | buf[6]) !== 0;
		try { crypto.randomFillSync(Buffer.alloc(100000)); r.large = "ok"; }
		catch (e) { r.large = "throw:" + e.message; }
	`)
	for _, c := range []struct{ expr, want, msg string }{
		{`String(r.prefixZero)`, "true", "bytes before offset were clobbered"},
		{`String(r.suffixZero)`, "true", "bytes after offset+size were clobbered"},
		{`String(r.filledNonzero)`, "true", "target window was not filled"},
		{`r.large`, "ok", "buffer >64KiB threw"},
	} {
		if got := evalStr(t, js, c.expr); got != c.want {
			t.Errorf("%s = %q, want %q", c.msg, got, c.want)
		}
	}
}

// TestRSAPSSSignVerify verifies RSA-PSS padding is actually used: a PSS signature
// verifies under PSS and does NOT verify under the default PKCS#1 v1.5 (proving
// the padding option isn't silently downgraded).
func TestRSAPSSSignVerify(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const crypto = require("crypto");
		const { privateKey, publicKey } = crypto.generateKeyPairSync("rsa", { modulusLength: 2048 });
		const pss = crypto.constants.RSA_PKCS1_PSS_PADDING;
		const sig = crypto.createSign("sha256").update("hello").sign({ key: privateKey, padding: pss });
		r.pssVerify = crypto.createVerify("sha256").update("hello").verify({ key: publicKey, padding: pss }, sig);
		r.crossVerify = crypto.createVerify("sha256").update("hello").verify(publicKey, sig);
	`)
	if got := evalStr(t, js, `String(r.pssVerify)`); got != "true" {
		t.Errorf("PSS sign/verify roundtrip = %q, want true", got)
	}
	if got := evalStr(t, js, `String(r.crossVerify)`); got != "false" {
		t.Errorf("PSS signature verified as PKCS1v15 = %q, want false (padding silently downgraded)", got)
	}
}

// TestRandomFillElementOffset verifies randomFillSync interprets offset/size in
// ELEMENT units for a multi-byte TypedArray (Node semantics), so filling element
// 1 of a Uint32Array touches bytes 4-7, not byte 1.
func TestRandomFillElementOffset(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const crypto = require("crypto");
		const u = new Uint32Array(4); // 16 zero bytes
		crypto.randomFillSync(u, 1, 1); // element 1 => bytes 4-7
		const b = new Uint8Array(u.buffer);
		r.elem0Zero = (b[0] | b[1] | b[2] | b[3]) === 0;
		r.elem1Filled = (b[4] | b[5] | b[6] | b[7]) !== 0;
		r.tailZero = (b[8] | b[9] | b[10] | b[11] | b[12] | b[13] | b[14] | b[15]) === 0;
	`)
	for _, c := range []struct{ expr, msg string }{
		{`String(r.elem0Zero)`, "element 0 was clobbered (byte vs element offset)"},
		{`String(r.elem1Filled)`, "element 1 was not filled"},
		{`String(r.tailZero)`, "elements past offset+size were clobbered"},
	} {
		if got := evalStr(t, js, c.expr); got != "true" {
			t.Errorf("%s (got %q)", c.msg, got)
		}
	}
}

// TestDiffieHellmanBufferPrime verifies createDiffieHellman accepts a Buffer
// prime (a peer's getPrime()) so two parties can reconstruct the same group.
func TestDiffieHellmanBufferPrime(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const crypto = require("crypto");
		const alice = crypto.createDiffieHellman(512);
		const prime = alice.getPrime();       // Buffer, no encoding
		const gen = alice.getGenerator();
		try {
			const bob = crypto.createDiffieHellman(prime, gen);
			alice.generateKeys(); bob.generateKeys();
			const aSecret = alice.computeSecret(bob.getPublicKey()).toString("hex");
			const bSecret = bob.computeSecret(alice.getPublicKey()).toString("hex");
			r.match = aSecret === bSecret && aSecret.length > 0;
		} catch (e) { r.err = e.message; }
	`)
	if got := evalStr(t, js, `String(r.err ?? "")`); got != "" {
		t.Fatalf("createDiffieHellman(Buffer prime) threw: %s", got)
	}
	if got := evalStr(t, js, `String(r.match)`); got != "true" {
		t.Errorf("DH shared secret from Buffer-prime group = %q, want true", got)
	}
}

func TestCryptoCipherGCMCrossCheck(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const crypto = require("crypto");
		globalThis.r = {};
		const key = Buffer.alloc(32, 7);   // 32 bytes of 0x07
		const iv = Buffer.alloc(12, 3);    // 12 bytes of 0x03
		const c = crypto.createCipheriv("aes-256-gcm", key, iv);
		c.setAAD(Buffer.from("header"));
		let ct = Buffer.concat([c.update(Buffer.from("secret message"), "utf8"), c.final()]);
		r.ct = ct.toString("hex");
		r.tag = c.getAuthTag().toString("hex");

		const d = crypto.createDecipheriv("aes-256-gcm", key, iv);
		d.setAAD(Buffer.from("header"));
		d.setAuthTag(Buffer.from(r.tag, "hex"));
		r.pt = Buffer.concat([d.update(ct), d.final()]).toString("utf8");

		// tampered tag -> throw
		try {
			const d2 = crypto.createDecipheriv("aes-256-gcm", key, iv);
			d2.setAAD(Buffer.from("header"));
			d2.setAuthTag(Buffer.alloc(16, 0));
			d2.update(ct); d2.final();
			r.tamper = "no-throw";
		} catch (e) { r.tamper = "threw"; }
	`)

	if got := evalStr(t, js, `r.pt`); got != "secret message" {
		t.Errorf("decrypt round trip = %q", got)
	}
	if got := evalStr(t, js, `r.tamper`); got != "threw" {
		t.Errorf("tampered tag should throw, got %q", got)
	}

	// Cross-check ciphertext+tag against Go's AES-GCM.
	key := bytes.Repeat([]byte{7}, 32)
	iv := bytes.Repeat([]byte{3}, 12)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	sealed := gcm.Seal(nil, iv, []byte("secret message"), []byte("header"))
	wantCT := hex.EncodeToString(sealed[:len(sealed)-16])
	wantTag := hex.EncodeToString(sealed[len(sealed)-16:])
	if got := evalStr(t, js, `r.ct`); got != wantCT {
		t.Errorf("gcm ct = %s, want %s (Go)", got, wantCT)
	}
	if got := evalStr(t, js, `r.tag`); got != wantTag {
		t.Errorf("gcm tag = %s, want %s (Go)", got, wantTag)
	}
}

func TestCryptoCipherCBCandCTR(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const crypto = require("crypto");
		globalThis.r = {};
		const key = Buffer.alloc(16, 1);
		const iv = Buffer.alloc(16, 2);
		for (const algo of ["aes-128-cbc", "aes-128-ctr"]) {
			const msg = "the quick brown fox jumps"; // 25 bytes (not block-aligned)
			const c = crypto.createCipheriv(algo, key, iv);
			const ct = Buffer.concat([c.update(msg, "utf8"), c.final()]);
			const d = crypto.createDecipheriv(algo, key, iv);
			const pt = Buffer.concat([d.update(ct), d.final()]).toString("utf8");
			r[algo] = pt === msg ? "ok" : "MISMATCH:" + pt;
		}
	`)
	for _, algo := range []string{"aes-128-cbc", "aes-128-ctr"} {
		if got := evalStr(t, js, `r["`+algo+`"]`); got != "ok" {
			t.Errorf("%s round trip = %s", algo, got)
		}
	}
}

func TestCryptoSignVerify(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const crypto = require("crypto");
		globalThis.r = {};
		for (const type of ["rsa", "ec"]) {
			// PEM encodings requested explicitly: without encodings Node (and
			// now this runtime) returns KeyObjects, not strings.
			const enc = {
				publicKeyEncoding: { type: "spki", format: "pem" },
				privateKeyEncoding: { type: "pkcs8", format: "pem" },
			};
			const opts = type === "rsa" ? { modulusLength: 2048, ...enc } : { namedCurve: "P-256", ...enc };
			const { publicKey, privateKey } = crypto.generateKeyPairSync(type, opts);
			r[type + "Pub"] = publicKey.includes("BEGIN PUBLIC KEY");
			const data = Buffer.from("sign me");
			const sig = crypto.createSign("sha256").update(data).sign(privateKey);
			r[type + "Verify"] = crypto.createVerify("sha256").update(data).verify(publicKey, sig);
			const bad = crypto.createVerify("sha256").update(Buffer.from("other")).verify(publicKey, sig);
			r[type + "Bad"] = bad;
		}
	`)
	for _, type_ := range []string{"rsa", "ec"} {
		if !evalVal(t, js, `r["`+type_+`Pub"]`).Bool() {
			t.Errorf("%s public key not PEM", type_)
		}
		if !evalVal(t, js, `r["`+type_+`Verify"]`).Bool() {
			t.Errorf("%s verify(valid) = false", type_)
		}
		if evalVal(t, js, `r["`+type_+`Bad"]`).Bool() {
			t.Errorf("%s verify(tampered) = true", type_)
		}
	}
}

func TestCryptoKDFs(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const crypto = require("crypto");
		globalThis.r = {};
		// PBKDF2 test vector (RFC 6070-style, sha256).
		r.pbkdf2 = crypto.pbkdf2Sync("password", "salt", 1, 32, "sha256").toString("hex");
		r.scrypt = crypto.scryptSync("password", "salt", 32).toString("hex");
		r.scryptLen = crypto.scryptSync("pw", "salt", 64).length;
		const hkdf = Buffer.from(crypto.hkdfSync("sha256", "ikm", "salt", "info", 42));
		r.hkdfLen = hkdf.length;
		// Determinism.
		r.deterministic = crypto.pbkdf2Sync("p", "s", 100, 16, "sha256").toString("hex")
			=== crypto.pbkdf2Sync("p", "s", 100, 16, "sha256").toString("hex");
	`)
	// PBKDF2-HMAC-SHA256, password="password", salt="salt", c=1, dkLen=32.
	const wantPBKDF2 = "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"
	if got := evalStr(t, js, `r.pbkdf2`); got != wantPBKDF2 {
		t.Errorf("pbkdf2 = %s, want %s", got, wantPBKDF2)
	}
	if got := evalVal(t, js, `r.scryptLen`).Int(); got != 64 {
		t.Errorf("scrypt keylen = %d", got)
	}
	if got := evalVal(t, js, `r.hkdfLen`).Int(); got != 42 {
		t.Errorf("hkdf length = %d", got)
	}
	if !evalVal(t, js, `r.deterministic`).Bool() {
		t.Error("pbkdf2 not deterministic")
	}
}

func TestCryptoRandom(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const crypto = require("crypto");
		globalThis.r = {};
		const a = crypto.randomBytes(32);
		r.len = a.length;
		r.isBuf = Buffer.isBuffer(a);
		r.distinct = crypto.randomBytes(16).toString("hex") !== crypto.randomBytes(16).toString("hex");
		const ints = new Set();
		for (let i = 0; i < 50; i++) ints.add(crypto.randomInt(0, 10));
		r.inRange = [...ints].every((n) => n >= 0 && n < 10);
	`)
	if got := evalVal(t, js, `r.len`).Int(); got != 32 {
		t.Errorf("randomBytes length = %d", got)
	}
	for _, k := range []string{"r.isBuf", "r.distinct", "r.inRange"} {
		if !evalVal(t, js, k).Bool() {
			t.Errorf("%s = false", k)
		}
	}
}

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

// node:crypto KeyObject.export({format:'jwk'}), crypto.createECDH, and the
// X25519 one-shot crypto.diffieHellman. The ECDH secrets are checked for
// agreement between both parties (a shared-bug generate/compute cannot yield
// matching-yet-wrong secrets from independently generated key pairs), and the
// JWK shapes are checked against Node's documented field sets.

func TestKeyObjectExportJWK(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const crypto = require("crypto");
		globalThis.__r = {};

		// EC (P-256): private JWK carries kty/crv/x/y/d; the derived public JWK drops d.
		const ec = crypto.generateKeyPairSync("ec", { namedCurve: "P-256" });
		const ecPriv = ec.privateKey.export({ format: "jwk" });
		const ecPub = ec.publicKey.export({ format: "jwk" });
		__r.ec = [ecPriv.kty, ecPriv.crv, typeof ecPriv.x, typeof ecPriv.y, typeof ecPriv.d, typeof ecPub.d].join("/");
		// x/y match between the private key and its public half.
		__r.ecXYmatch = ecPriv.x === ecPub.x && ecPriv.y === ecPub.y;

		// RSA: private JWK carries the full CRT set; public JWK is just n/e.
		const rsa = crypto.generateKeyPairSync("rsa", { modulusLength: 2048 });
		const rp = rsa.privateKey.export({ format: "jwk" });
		__r.rsa = [rp.kty, typeof rp.n, typeof rp.e, typeof rp.d, typeof rp.p, typeof rp.q, typeof rp.dp, typeof rp.dq, typeof rp.qi].join("/");
		const rpub = rsa.publicKey.export({ format: "jwk" });
		__r.rsaPub = [rpub.kty, typeof rpub.n, typeof rpub.e, typeof rpub.d].join("/");

		// Ed25519: OKP with x (+ d on the private key).
		const ed = crypto.generateKeyPairSync("ed25519");
		const edp = ed.privateKey.export({ format: "jwk" });
		__r.ed = [edp.kty, edp.crv, typeof edp.x, typeof edp.d].join("/");

		// Secret key: oct with base64url k.
		const sk = crypto.createSecretKey(Buffer.from("0123456789abcdef"));
		const skj = sk.export({ format: "jwk" });
		__r.oct = skj.kty + "/" + skj.k;
	`)

	if got := evalStr(t, js, `__r.ec`); got != "EC/P-256/string/string/string/undefined" {
		t.Errorf("EC JWK shape = %s", got)
	}
	if got := evalStr(t, js, `String(__r.ecXYmatch)`); got != "true" {
		t.Error("EC private/public JWK x,y disagree")
	}
	if got := evalStr(t, js, `__r.rsa`); got != "RSA/string/string/string/string/string/string/string/string" {
		t.Errorf("RSA private JWK shape = %s", got)
	}
	if got := evalStr(t, js, `__r.rsaPub`); got != "RSA/string/string/undefined" {
		t.Errorf("RSA public JWK shape = %s", got)
	}
	if got := evalStr(t, js, `__r.ed`); got != "OKP/Ed25519/string/string" {
		t.Errorf("Ed25519 JWK shape = %s", got)
	}
	// "0123456789abcdef" base64url-encoded.
	if got := evalStr(t, js, `__r.oct`); got != "oct/MDEyMzQ1Njc4OWFiY2RlZg" {
		t.Errorf("oct JWK = %s", got)
	}
}

func TestCreateECDHRoundTrip(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const crypto = require("crypto");
		globalThis.__r = {};
		for (const curve of ["prime256v1", "secp384r1", "secp521r1"]) {
			const alice = crypto.createECDH(curve);
			const bob = crypto.createECDH(curve);
			const aPub = alice.generateKeys();
			const bPub = bob.generateKeys();
			const aSecret = alice.computeSecret(bPub).toString("hex");
			const bSecret = bob.computeSecret(aPub).toString("hex");
			__r[curve] = aSecret === bSecret && aSecret.length > 0;
			// The public key is the uncompressed 0x04||X||Y point Node uses.
			__r[curve + "_pt"] = aPub[0] === 0x04;
		}
	`)

	for _, c := range []string{"prime256v1", "secp384r1", "secp521r1"} {
		if got := evalStr(t, js, `String(__r["`+c+`"])`); got != "true" {
			t.Errorf("createECDH(%q) secrets did not agree", c)
		}
		if got := evalStr(t, js, `String(__r["`+c+`_pt"])`); got != "true" {
			t.Errorf("createECDH(%q) public key is not an uncompressed point", c)
		}
	}
}

func TestDiffieHellmanX25519(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const crypto = require("crypto");
		globalThis.__r = {};
		const alice = crypto.generateKeyPairSync("x25519");
		const bob = crypto.generateKeyPairSync("x25519");
		const aSecret = crypto.diffieHellman({ privateKey: alice.privateKey, publicKey: bob.publicKey }).toString("hex");
		const bSecret = crypto.diffieHellman({ privateKey: bob.privateKey, publicKey: alice.publicKey }).toString("hex");
		__r.match = aSecret === bSecret;
		__r.len = Buffer.from(aSecret, "hex").length;
		// The X25519 KeyObject also exports as an OKP JWK.
		const jwk = alice.privateKey.export({ format: "jwk" });
		__r.jwk = [jwk.kty, jwk.crv, typeof jwk.x, typeof jwk.d].join("/");
	`)

	if got := evalStr(t, js, `String(__r.match)`); got != "true" {
		t.Error("X25519 diffieHellman secrets did not agree")
	}
	if got := evalStr(t, js, `String(__r.len)`); got != "32" {
		t.Errorf("X25519 shared secret length = %s, want 32", got)
	}
	if got := evalStr(t, js, `__r.jwk`); got != "OKP/X25519/string/string" {
		t.Errorf("X25519 JWK shape = %s", got)
	}
}

// node:crypto KeyObject family: createSecretKey/createPublicKey/
// createPrivateKey, the crypto.sign/crypto.verify one-shots (including
// Ed25519 with algorithm null), generateKeyPairSync encoding handling
// (KeyObjects with no encodings, DER Buffers, real PKCS#1), and KeyObjects
// being accepted everywhere a raw key is (the jsonwebtoken v9 pattern).

func TestCreateSecretKeyObjectAndHmacAcceptance(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `{
		const crypto = require("crypto");
		globalThis.r = {};
		const sk = crypto.createSecretKey(Buffer.from("hmac-secret"));
		r.type = sk.type;
		r.size = sk.symmetricKeySize;
		r.isKeyObject = sk instanceof crypto.KeyObject;
		r.exportRound = sk.export().toString() === "hmac-secret";
		// A secret KeyObject must be accepted wherever a raw hmac key is.
		r.hmacMatch = crypto.createHmac("sha256", sk).update("msg").digest("hex")
			=== crypto.createHmac("sha256", "hmac-secret").update("msg").digest("hex");
		// createSecretKey(string, encoding).
		const skHex = crypto.createSecretKey("6162", "hex");
		r.encMatch = skHex.export().toString() === "ab";
		// And in a cipher.
		const key = crypto.createSecretKey(Buffer.alloc(16, 1));
		const iv = Buffer.alloc(16, 2);
		const c = crypto.createCipheriv("aes-128-cbc", key, iv);
		const ct = Buffer.concat([c.update("block cipher via KeyObject"), c.final()]);
		const d = crypto.createDecipheriv("aes-128-cbc", Buffer.alloc(16, 1), iv);
		r.cipherRound = Buffer.concat([d.update(ct), d.final()]).toString() === "block cipher via KeyObject";
	}`)
	for expr, want := range map[string]string{
		"r.type":                "secret",
		"String(r.size)":        "11",
		"String(r.isKeyObject)": "true",
		"String(r.exportRound)": "true",
		"String(r.hmacMatch)":   "true",
		"String(r.encMatch)":    "true",
		"String(r.cipherRound)": "true",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestGenerateKeyPairReturnsKeyObjectsWithoutEncodings(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `{
		const crypto = require("crypto");
		globalThis.r = {};
		const kp = crypto.generateKeyPairSync("rsa", { modulusLength: 2048 });
		r.pubIsKeyObject = kp.publicKey instanceof crypto.KeyObject;
		r.privIsKeyObject = kp.privateKey instanceof crypto.KeyObject;
		r.pubType = kp.publicKey.type;
		r.privType = kp.privateKey.type;
		r.pubAsym = kp.publicKey.asymmetricKeyType;
		r.privAsym = kp.privateKey.asymmetricKeyType;
		// export() works: pem gives a string, der a Buffer (DER SEQUENCE).
		const pubPem = kp.publicKey.export({ type: "spki", format: "pem" });
		r.pubPemOk = typeof pubPem === "string" && pubPem.includes("BEGIN PUBLIC KEY");
		const privDer = kp.privateKey.export({ type: "pkcs8", format: "der" });
		r.privDerOk = Buffer.isBuffer(privDer) && privDer[0] === 0x30;
		const privPkcs1 = kp.privateKey.export({ type: "pkcs1", format: "pem" });
		r.privPkcs1Ok = privPkcs1.includes("BEGIN RSA PRIVATE KEY");
		// KeyObjects work directly in createSign/createVerify.
		const sig = crypto.createSign("sha256").update("payload").sign(kp.privateKey);
		r.signVerify = crypto.createVerify("sha256").update("payload").verify(kp.publicKey, sig);
		// createPublicKey derives the public key from a private KeyObject.
		const derived = crypto.createPublicKey(kp.privateKey);
		r.derivedType = derived.type;
		r.derivedVerify = crypto.createVerify("sha256").update("payload").verify(derived, sig);
		// ec KeyObjects report their type too.
		const ec = crypto.generateKeyPairSync("ec", { namedCurve: "P-256" });
		r.ecAsym = ec.publicKey.asymmetricKeyType;
	}`)
	for expr, want := range map[string]string{
		"String(r.pubIsKeyObject)":  "true",
		"String(r.privIsKeyObject)": "true",
		"r.pubType":                 "public",
		"r.privType":                "private",
		"r.pubAsym":                 "rsa",
		"r.privAsym":                "rsa",
		"String(r.pubPemOk)":        "true",
		"String(r.privDerOk)":       "true",
		"String(r.privPkcs1Ok)":     "true",
		"String(r.signVerify)":      "true",
		"r.derivedType":             "public",
		"String(r.derivedVerify)":   "true",
		"r.ecAsym":                  "ec",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// format:'der' must yield Buffers with real DER, and type:'pkcs1' real PKCS#1
// structures (parsed host-side with Go's x509 to prove the label matches the
// bytes — a PKCS#8 body under a PKCS#1 label must fail this).
func TestGenerateKeyPairHonorsEncodings(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `{
		const crypto = require("crypto");
		globalThis.r = {};
		const der = crypto.generateKeyPairSync("ec", {
			namedCurve: "P-256",
			publicKeyEncoding: { type: "spki", format: "der" },
			privateKeyEncoding: { type: "pkcs8", format: "der" },
		});
		r.derPubIsBuf = Buffer.isBuffer(der.publicKey);
		r.derPrivIsBuf = Buffer.isBuffer(der.privateKey);
		r.derPubHex = der.publicKey.toString("hex");
		r.derPrivHex = der.privateKey.toString("hex");
		// DER re-imports through createPublicKey/createPrivateKey.
		r.reimportPub = crypto.createPublicKey({ key: der.publicKey, format: "der", type: "spki" }).type;
		r.reimportPriv = crypto.createPrivateKey({ key: der.privateKey, format: "der", type: "pkcs8" }).type;

		const p1 = crypto.generateKeyPairSync("rsa", {
			modulusLength: 2048,
			publicKeyEncoding: { type: "pkcs1", format: "pem" },
			privateKeyEncoding: { type: "pkcs1", format: "pem" },
		});
		r.p1PubLabel = p1.publicKey.startsWith("-----BEGIN RSA PUBLIC KEY-----");
		r.p1PrivLabel = p1.privateKey.startsWith("-----BEGIN RSA PRIVATE KEY-----");
		r.p1Priv = p1.privateKey;
		r.p1Pub = p1.publicKey;
		// The PKCS#1 PEM signs/verifies through the normal paths.
		const sig = crypto.createSign("sha256").update("x").sign(p1.privateKey);
		r.p1Works = crypto.createVerify("sha256").update("x").verify(p1.publicKey, sig);
	}`)
	for expr, want := range map[string]string{
		"String(r.derPubIsBuf)":  "true",
		"String(r.derPrivIsBuf)": "true",
		"r.reimportPub":          "public",
		"r.reimportPriv":         "private",
		"String(r.p1PubLabel)":   "true",
		"String(r.p1PrivLabel)":  "true",
		"String(r.p1Works)":      "true",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
	// Host-side proof the labels match the bytes.
	p1Priv := evalStr(t, js, `r.p1Priv`)
	block, _ := pem.Decode([]byte(p1Priv))
	if block == nil || block.Type != "RSA PRIVATE KEY" {
		t.Fatalf("pkcs1 private PEM block = %v, want RSA PRIVATE KEY", block)
	}
	if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
		t.Errorf("pkcs1-labeled private key is not real PKCS#1: %v", err)
	}
	p1Pub := evalStr(t, js, `r.p1Pub`)
	if block, _ = pem.Decode([]byte(p1Pub)); block == nil || block.Type != "RSA PUBLIC KEY" {
		t.Fatalf("pkcs1 public PEM block missing/mislabeled")
	}
	if _, err := x509.ParsePKCS1PublicKey(block.Bytes); err != nil {
		t.Errorf("pkcs1-labeled public key is not real RSAPublicKey DER: %v", err)
	}
}

func TestSignVerifyOneShotsAndEd25519(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `{
		const crypto = require("crypto");
		globalThis.r = {};
		// Hash-named one-shots over RSA and EC KeyObjects.
		const rsa = crypto.generateKeyPairSync("rsa", { modulusLength: 2048 });
		const data = Buffer.from("one-shot payload");
		const sig = crypto.sign("sha256", data, rsa.privateKey);
		r.rsaOneShot = crypto.verify("sha256", data, rsa.publicKey, sig);
		r.rsaOneShotBad = crypto.verify("sha256", Buffer.from("tampered"), rsa.publicKey, sig);
		// One-shots also take plain PEM strings.
		const pemPriv = rsa.privateKey.export({ type: "pkcs8", format: "pem" });
		const pemPub = rsa.publicKey.export({ type: "spki", format: "pem" });
		r.pemOneShot = crypto.verify("sha256", data, pemPub, crypto.sign("sha256", data, pemPriv));

		// Ed25519: algorithm is null, the message is signed directly.
		const ed = crypto.generateKeyPairSync("ed25519");
		r.edAsym = ed.publicKey.asymmetricKeyType;
		const edSig = crypto.sign(null, data, ed.privateKey);
		r.edSigLen = edSig.length;
		r.edVerify = crypto.verify(null, data, ed.publicKey, edSig);
		r.edVerifyBad = crypto.verify(null, Buffer.from("other"), ed.publicKey, edSig);
		// Ed25519 KeyObjects export/re-import.
		const edPem = ed.privateKey.export({ type: "pkcs8", format: "pem" });
		r.edReimport = crypto.verify(null, data, ed.publicKey, crypto.sign(null, data, crypto.createPrivateKey(edPem)));

		// x25519 generates too (KeyObjects with the right type).
		const x = crypto.generateKeyPairSync("x25519");
		r.xAsym = x.publicKey.asymmetricKeyType;
	}`)
	for expr, want := range map[string]string{
		"String(r.rsaOneShot)":    "true",
		"String(r.rsaOneShotBad)": "false",
		"String(r.pemOneShot)":    "true",
		"r.edAsym":                "ed25519",
		"String(r.edSigLen)":      "64",
		"String(r.edVerify)":      "true",
		"String(r.edVerifyBad)":   "false",
		"String(r.edReimport)":    "true",
		"r.xAsym":                 "x25519",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// The jsonwebtoken v9 pattern: createSecretKey/createPrivateKey on every
// sign/verify, with the resulting KeyObjects fed to createHmac/createSign/
// createVerify and the one-shots.
func TestKeyObjectsAcceptedLikeRawKeys(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `{
		const crypto = require("crypto");
		globalThis.r = {};
		// HS256-style: secret KeyObject per operation.
		const hs = (key) => crypto.createHmac("sha256", crypto.createSecretKey(Buffer.from(key))).update("h.p").digest("base64url");
		r.hsStable = hs("jwt-secret") === crypto.createHmac("sha256", "jwt-secret").update("h.p").digest("base64url");

		// RS256-style: createPrivateKey(pem) per sign, createPublicKey per verify.
		const kp = crypto.generateKeyPairSync("rsa", {
			modulusLength: 2048,
			publicKeyEncoding: { type: "spki", format: "pem" },
			privateKeyEncoding: { type: "pkcs8", format: "pem" },
		});
		const signingInput = Buffer.from("header.payload");
		const sig = crypto.createSign("RSA-SHA256").update(signingInput).sign(crypto.createPrivateKey(kp.privateKey));
		r.rsVerify = crypto.createVerify("RSA-SHA256").update(signingInput).verify(crypto.createPublicKey(kp.publicKey), sig);
		// createPrivateKey/createPublicKey accept KeyObjects (jwt passes them through).
		const asObj = crypto.createPrivateKey(crypto.createPrivateKey(kp.privateKey));
		r.doubleWrap = asObj.type === "private" && asObj.asymmetricKeyType === "rsa";
		// { key: KeyObject } option-bag form.
		const sig2 = crypto.sign("sha256", signingInput, { key: crypto.createPrivateKey(kp.privateKey) });
		r.bagVerify = crypto.verify("sha256", signingInput, { key: crypto.createPublicKey(kp.publicKey) }, sig2);
		// publicEncrypt/privateDecrypt take KeyObjects too.
		const ct = crypto.publicEncrypt({ key: crypto.createPublicKey(kp.publicKey), padding: 4 }, Buffer.from("boxed"));
		r.rsaEnc = crypto.privateDecrypt({ key: crypto.createPrivateKey(kp.privateKey), padding: 4 }, ct).toString();
	}`)
	for expr, want := range map[string]string{
		"String(r.hsStable)":   "true",
		"String(r.rsVerify)":   "true",
		"String(r.doubleWrap)": "true",
		"String(r.bagVerify)":  "true",
		"r.rsaEnc":             "boxed",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// TestCryptoNumericArgObjectDoesNotLeak guards against a guest-triggerable
// unbounded memory leak: several node:crypto host ops read a numeric parameter
// with Value.Int(), which returns 0 for an object receiver WITHOUT releasing
// the persistent GC root minted for it at decode. Their JS wrappers forward the
// parameter raw, so a guest that passes an object (e.g. a large Uint8Array)
// where a number is expected pins that object — and its backing store — for the
// interpreter's life on every call. Under a memory cap this exhausts memory.
//
// The case uses a SUCCESS-path parameter — saltLength, which PKCS#1 v1.5 sign
// ignores — so passing an object is silently accepted and leaks with no
// guest-visible error, accumulating across the loop. Each iteration allocates
// and drops a fresh large array used as the hostile numeric argument. With the
// roots freed, the arrays are collectable and every iteration completes; if they
// leak the guest OOMs long before the loop ends and the Eval errors.
//
// hkdfSync's keylen was a second site here and is now unreachable: the wrapper
// rejects a non-number before the op sees it, which is the stronger fix — the
// leak cannot be reached through the public API at all. The loop asserts that
// rejection so the guarantee is not quietly lost if the check is removed.
func TestCryptoNumericArgObjectDoesNotLeak(t *testing.T) {
	const script = `
		const crypto = require("crypto");
		const N = 200;              // 200 * 2 MiB per site = 400 MiB pinned if leaked
		const SZ = 2 * 1024 * 1024; // >> the 256 MiB cap when accumulated
		const { privateKey } = crypto.generateKeyPairSync("rsa", { modulusLength: 2048 });
		let rejected = 0;
		for (let i = 0; i < N; i++) {
			const big = new Uint8Array(SZ);       // hostile object numeric arg
			crypto.createSign("sha256").update("m").sign({ key: privateKey, saltLength: big });
			try { crypto.hkdfSync("sha256", "key", "salt", "info", big); }
			catch (e) { if (e.code === "ERR_INVALID_ARG_TYPE") rejected++; }
		}
		if (rejected !== N) throw new Error("hkdfSync accepted an object keylen " + (N - rejected) + " times");
		globalThis.__completed = N;
	`
	js, rt := newRuntime(t, spidermonkey.Config{MaxMemoryBytes: 256 << 20})
	if r, err := js.Eval(context.Background(), script); err != nil {
		t.Fatalf("Eval: %v", err)
	} else if r.Error != nil {
		t.Fatalf("script threw (leak exhausted the memory cap): %v", r.Error)
	}
	if err := rt.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalVal(t, js, "globalThis.__completed").Int(); got != 200 {
		t.Fatalf("completed %d iterations, want 200", got)
	}
}

// TestScalarStringArgObjectDoesNotLeak guards the same GC-root leak class for
// host ops that read a STRING parameter with Value.String(): reading an object
// receiver runs its guest toString but never releases the persistent root, so a
// JS wrapper that forwards a guest value raw (crypto key-export format/type,
// generateKeyPair type, createECDH curve, and http.request href — the last one
// outside crypto) pins the object on every call. Both cases below drive an
// object where a string is expected in a loop under a memory cap: with the roots
// freed the loop completes; if they leak the guest OOMs.
func TestScalarStringArgObjectDoesNotLeak(t *testing.T) {
	const script = `
		const crypto = require("crypto");
		const http = require("http");
		const N = 200;
		const SZ = 2 * 1024 * 1024;
		const priv = crypto.generateKeyPairSync("rsa", { modulusLength: 2048 }).privateKey;
		for (let i = 0; i < N; i++) {
			const big = new Uint8Array(SZ);
			try { priv.export({ format: big }); } catch (e) {}   // crypto_key_export format
			try { crypto.createECDH(big).generateKeys(); } catch (e) {} // crypto_ecdh curve
			http.request({ href: big }).on("error", () => {});   // http_client_req url (non-crypto)
		}
		globalThis.__completed = N;
	`
	js, rt := newRuntime(t, spidermonkey.Config{MaxMemoryBytes: 256 << 20})
	if r, err := js.Eval(context.Background(), script); err != nil {
		t.Fatalf("Eval: %v", err)
	} else if r.Error != nil {
		t.Fatalf("script threw (leak exhausted the memory cap): %v", r.Error)
	}
	if err := rt.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalVal(t, js, "globalThis.__completed").Int(); got != 200 {
		t.Fatalf("completed %d iterations, want 200", got)
	}
}

// cipher.setAutoPadding(false) used to be a silent no-op: encrypt still
// appended a PKCS#7 block and decrypt still stripped one. Real semantics for
// CBC: with autopadding off, encrypt requires block-aligned input (else the
// Node-style 'data not multiple of block length' error) and produces exactly
// input-length ciphertext; decrypt returns the raw plaintext including the
// padding bytes. Stream modes (CTR/GCM) ignore the flag, as in Node.

func TestCipherSetAutoPaddingFalseCBC(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `{
		const crypto = require("crypto");
		globalThis.r = {};
		const key = Buffer.alloc(16, 1);
		const iv = Buffer.alloc(16, 2);
		const block = Buffer.from("exactly 16 bytes"); // one AES block

		// Encrypt, autopadding off, aligned input: exactly one block out.
		const c = crypto.createCipheriv("aes-128-cbc", key, iv).setAutoPadding(false);
		const ct = Buffer.concat([c.update(block), c.final()]);
		r.ctLen = ct.length;
		r.ctHex = ct.toString("hex");

		// Encrypt, autopadding off, misaligned input: throws like Node.
		try {
			const bad = crypto.createCipheriv("aes-128-cbc", key, iv);
			bad.setAutoPadding(false);
			bad.update(Buffer.from("15 bytes only!!"));
			bad.final();
			r.misaligned = "no-throw";
		} catch (e) { r.misaligned = e.message; }

		// Decrypt, autopadding off: the PKCS#7 padding bytes come back raw.
		const enc = crypto.createCipheriv("aes-128-cbc", key, iv); // default: pads
		const padded = Buffer.concat([enc.update(block), enc.final()]);
		r.paddedLen = padded.length; // 32: data block + full padding block
		const dec = crypto.createDecipheriv("aes-128-cbc", key, iv).setAutoPadding(false);
		const raw = Buffer.concat([dec.update(padded), dec.final()]);
		r.rawLen = raw.length;
		r.rawText = raw.subarray(0, 16).toString();
		r.padBytes = raw.subarray(16).every((b) => b === 0x10);

		// Round trip with autopadding off on both sides.
		const dec2 = crypto.createDecipheriv("aes-128-cbc", key, iv).setAutoPadding(false);
		r.noPadRound = Buffer.concat([dec2.update(ct), dec2.final()]).toString();

		// setAutoPadding() with no argument means true (Node default).
		const c3 = crypto.createCipheriv("aes-128-cbc", key, iv).setAutoPadding();
		r.reset = Buffer.concat([c3.update(block), c3.final()]).length; // padded: 32

		// Stream mode ignores the flag: CTR with a misaligned length still works.
		const ctr = crypto.createCipheriv("aes-128-ctr", key, iv).setAutoPadding(false);
		const ctrCt = Buffer.concat([ctr.update(Buffer.from("15 bytes only!!")), ctr.final()]);
		const ctrDec = crypto.createDecipheriv("aes-128-ctr", key, iv).setAutoPadding(false);
		r.ctr = Buffer.concat([ctrDec.update(ctrCt), ctrDec.final()]).toString();
	}`)

	// Cross-check the unpadded ciphertext against Go's raw CBC.
	key := bytes.Repeat([]byte{1}, 16)
	iv := bytes.Repeat([]byte{2}, 16)
	block, _ := aes.NewCipher(key)
	want := make([]byte, 16)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(want, []byte("exactly 16 bytes"))
	if got := evalStr(t, js, `r.ctHex`); got != hex.EncodeToString(want) {
		t.Errorf("no-pad ciphertext = %s, want %s (Go CBC)", got, hex.EncodeToString(want))
	}
	for expr, want := range map[string]string{
		"String(r.ctLen)":     "16", // no PKCS#7 block appended
		"r.misaligned":        "data not multiple of block length",
		"String(r.paddedLen)": "32",
		"String(r.rawLen)":    "32", // padding NOT stripped
		"r.rawText":           "exactly 16 bytes",
		"String(r.padBytes)":  "true", // 16 bytes of 0x10 kept
		"r.noPadRound":        "exactly 16 bytes",
		"String(r.reset)":     "32", // setAutoPadding() re-enables padding
		"r.ctr":               "15 bytes only!!",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}
