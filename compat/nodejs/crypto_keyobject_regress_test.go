package nodejs_test

// node:crypto KeyObject family: createSecretKey/createPublicKey/
// createPrivateKey, the crypto.sign/crypto.verify one-shots (including
// Ed25519 with algorithm null), generateKeyPairSync encoding handling
// (KeyObjects with no encodings, DER Buffers, real PKCS#1), and KeyObjects
// being accepted everywhere a raw key is (the jsonwebtoken v9 pattern).

import (
	"crypto/x509"
	"encoding/pem"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
