package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
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
