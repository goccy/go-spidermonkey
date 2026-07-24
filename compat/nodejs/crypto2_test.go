package nodejs_test

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
			const opts = type === "rsa" ? { modulusLength: 2048 } : { namedCurve: "P-256" };
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
