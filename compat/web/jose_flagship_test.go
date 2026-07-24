package web_test

// The compat/web flagship (docs/nodejs-compat-plan.md): the real, unmodified
// jose npm package signing and verifying JWTs on our WinterTC surface. Opt-in
// like the test262 suite: skipped unless examples/jose/node_modules exists
// (run `npm ci` in examples/jose).

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
	"github.com/goccy/go-spidermonkey/compat/web"
)

func newJoseJS(t *testing.T) *spidermonkey.JS {
	t.Helper()
	dir := filepath.Join("..", "..", "examples", "jose")
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "jose", "package.json")); err != nil {
		t.Skip("examples/jose/node_modules not installed; run `npm ci` in examples/jose")
	}
	js, err := spidermonkey.New(spidermonkey.Config{FS: os.DirFS(dir)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { js.Close() })
	w, err := web.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	js.SetModuleLoader(nodejs.ESMLoader)
	return js
}

func runJoseModule(t *testing.T, js *spidermonkey.JS, src string) {
	t.Helper()
	r, err := js.EvalModule(context.Background(), "flagship.js", src)
	if err != nil {
		t.Fatalf("EvalModule: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("module threw: %v", r.Error)
	}
	if got := evalString(t, js, `__r.err ?? ""`); got != "" {
		t.Fatalf("jose flow rejected: %s", got)
	}
}

const secretPhrase = "a-string-secret-at-least-256-bits-long!!"

func TestJoseFlagshipHS256(t *testing.T) {
	js := newJoseJS(t)

	runJoseModule(t, js, `
		import { SignJWT, jwtVerify } from "jose";
		globalThis.__r = {};
		await (async () => {
			const secret = new TextEncoder().encode(`+"`"+secretPhrase+"`"+`);
			const jwt = await new SignJWT({ role: "admin" })
				.setProtectedHeader({ alg: "HS256", typ: "JWT" })
				.setSubject("goccy")
				.setIssuedAt(1720000000)
				.sign(secret);
			__r.jwt = jwt;
			const { payload, protectedHeader } = await jwtVerify(jwt, secret);
			__r.sub = payload.sub;
			__r.role = payload.role;
			__r.alg = protectedHeader.alg;
			const parts = jwt.split(".");
			try {
				await jwtVerify(parts[0] + "." + parts[1] + "." + parts[2].slice(0, -4) + "AAAA", secret);
				__r.tampered = "verified";
			} catch (e) { __r.tampered = e.name || String(e); }
		})().catch(e => { __r.err = e instanceof Error ? e.name + ": " + e.message + "\n" + (e.stack || "") : String(e); });
	`)

	jwt := evalString(t, js, `__r.jwt`)
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt = %q", jwt)
	}
	if got := evalString(t, js, `__r.sub + "/" + __r.role + "/" + __r.alg`); got != "goccy/admin/HS256" {
		t.Errorf("verified claims = %s", got)
	}
	if got := evalString(t, js, `__r.tampered`); got != "JWSSignatureVerificationFailed" {
		t.Errorf("tampered verify = %s, want JWSSignatureVerificationFailed", got)
	}

	// Cross-check: Go recomputes the HS256 signature over jose's signing
	// input — byte-identical, or the whole stack is only self-consistent.
	m := hmac.New(sha256.New, []byte(secretPhrase))
	m.Write([]byte(parts[0] + "." + parts[1]))
	want := base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	if parts[2] != want {
		t.Errorf("signature = %s, want %s (Go reference)", parts[2], want)
	}
}

func TestJoseFlagshipES256(t *testing.T) {
	js := newJoseJS(t)

	runJoseModule(t, js, `
		import { SignJWT, jwtVerify, generateKeyPair, exportJWK } from "jose";
		globalThis.__r = {};
		await (async () => {
			const { publicKey, privateKey } = await generateKeyPair("ES256", { extractable: true });
			const jwt = await new SignJWT({ scope: "read" })
				.setProtectedHeader({ alg: "ES256" })
				.setSubject("goccy")
				.sign(privateKey);
			__r.parts = jwt.split(".").length;
			const { payload } = await jwtVerify(jwt, publicKey);
			__r.sub = payload.sub;
			__r.scope = payload.scope;
			const jwk = await exportJWK(publicKey);
			__r.kty = jwk.kty;
			__r.crv = jwk.crv;
			const { publicKey: otherPub } = await generateKeyPair("ES256");
			try {
				await jwtVerify(jwt, otherPub);
				__r.wrongKey = "verified";
			} catch (e) { __r.wrongKey = e.name || String(e); }
		})().catch(e => { __r.err = e instanceof Error ? e.name + ": " + e.message + "\n" + (e.stack || "") : String(e); });
	`)

	if got := eval(t, js, `__r.parts`).Int(); got != 3 {
		t.Errorf("jwt parts = %d", got)
	}
	if got := evalString(t, js, `__r.sub + "/" + __r.scope`); got != "goccy/read" {
		t.Errorf("verified claims = %s", got)
	}
	if got := evalString(t, js, `__r.kty + "/" + __r.crv`); got != "EC/P-256" {
		t.Errorf("exported JWK = %s", got)
	}
	if got := evalString(t, js, `__r.wrongKey`); got != "JWSSignatureVerificationFailed" {
		t.Errorf("wrong-key verify = %s", got)
	}
}
