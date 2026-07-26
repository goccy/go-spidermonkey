package nodejs_test

// node:crypto KeyObject.export({format:'jwk'}), crypto.createECDH, and the
// X25519 one-shot crypto.diffieHellman. The ECDH secrets are checked for
// agreement between both parties (a shared-bug generate/compute cannot yield
// matching-yet-wrong secrets from independently generated key pairs), and the
// JWK shapes are checked against Node's documented field sets.

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
