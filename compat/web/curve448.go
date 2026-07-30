package web

// curve448.go: Ed448 signing and X448 key agreement.
//
// These are the 448-bit halves of the same pair the 25519 curves form, and the
// Web Crypto suite exercises them identically. Go's standard library has
// neither, so both come from CIRCL. The formats are the same shapes their
// 25519 siblings use, at the sizes this curve defines: a 57-byte Ed448 public
// key, a 56-byte X448 one.

import (
	"bytes"
	"crypto/rand"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/cloudflare/circl/dh/x448"
	"github.com/cloudflare/circl/sign/ed448"
	spidermonkey "github.com/goccy/go-spidermonkey"
)

// The RFC 8410 OIDs. crypto/x509 marshals the 25519 curves but knows neither of
// these, so their SPKI and PKCS#8 are encoded here — the same two shapes, with
// the seed inside a CurvePrivateKey (see akp.go).
var (
	oidEd448 = asn1.ObjectIdentifier{1, 3, 101, 113}
	oidX448  = asn1.ObjectIdentifier{1, 3, 101, 111}
)

// curveSPKI/curvePKCS8 and their parsers name the OID they expect, so importing
// an X448 key as Ed448 is refused rather than reinterpreted.
func curvePKCS8(oid asn1.ObjectIdentifier, seed []byte) ([]byte, error) {
	return derPKCS8(oid, curveSeedTag, seed)
}

func curveParsePKCS8(der []byte, want asn1.ObjectIdentifier, size int) ([]byte, error) {
	oid, seed, err := derParsePKCS8(der, curveSeedTag)
	if err != nil {
		return nil, err
	}
	if !oid.Equal(want) {
		return nil, fmt.Errorf("pkcs8 key is not %v", want)
	}
	if len(seed) != size {
		return nil, fmt.Errorf("private key must be %d bytes", size)
	}
	return seed, nil
}

func curveParseSPKI(der []byte, want asn1.ObjectIdentifier, size int) ([]byte, error) {
	oid, pub, err := akpParseSPKI(der)
	if err != nil {
		return nil, err
	}
	if !oid.Equal(want) {
		return nil, fmt.Errorf("spki key is not %v", want)
	}
	if len(pub) != size {
		return nil, fmt.Errorf("public key must be %d bytes", size)
	}
	return pub, nil
}

// ---------------------------------------------------------------- Ed448

func (s *subtleAPI) opEd448Generate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	pub, priv, err := ed448.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return spidermonkey.ValueOf(map[string]any{
		"priv": s.put(&subtleKey{ed448Priv: priv}),
		"pub":  s.put(&subtleKey{ed448Pub: pub}),
	}), nil
}

func (s *subtleAPI) opEd448Import(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("ed448 import: (format, keyData) required")
	}
	switch args[0].String() {
	case "raw", "raw-public":
		raw, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		if len(raw) != ed448.PublicKeySize {
			return subtleErr(errData, fmt.Sprintf("Ed448 public key must be %d bytes", ed448.PublicKeySize)), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{ed448Pub: ed448.PublicKey(raw)})}), nil
	case "raw-seed":
		raw, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		if len(raw) != ed448.SeedSize {
			return subtleErr(errData, fmt.Sprintf("Ed448 seed must be %d bytes", ed448.SeedSize)), nil
		}
		priv := ed448.NewKeyFromSeed(raw)
		return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{ed448Priv: priv})}), nil
	case "spki":
		der, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		pub, err := curveParseSPKI(der, oidEd448, ed448.PublicKeySize)
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{ed448Pub: ed448.PublicKey(pub)})}), nil
	case "pkcs8":
		der, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		seed, err := curveParsePKCS8(der, oidEd448, ed448.SeedSize)
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{ed448Priv: ed448.NewKeyFromSeed(seed)})}), nil
	case "jwk":
		var jwk struct{ Kty, Crv, Alg, X, D string }
		if err := json.Unmarshal([]byte(args[1].String()), &jwk); err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		if jwk.Kty != "OKP" || jwk.Crv != "Ed448" {
			return subtleErr(errData, "not an Ed448 JWK"), nil
		}
		if jwk.Alg != "" && jwk.Alg != "Ed448" && jwk.Alg != "EdDSA" {
			return subtleErr(errData, "JWK alg "+jwk.Alg+" is not Ed448"), nil
		}
		// "x" is required for both halves, and must be the public key that "d"
		// derives; a JWK that disagrees with itself is not a key.
		pub, err := base64.RawURLEncoding.DecodeString(jwk.X)
		if err != nil || len(pub) != ed448.PublicKeySize {
			return subtleErr(errData, "bad Ed448 JWK x"), nil
		}
		if jwk.D != "" {
			seed, err := base64.RawURLEncoding.DecodeString(jwk.D)
			if err != nil || len(seed) != ed448.SeedSize {
				return subtleErr(errData, "bad Ed448 JWK d"), nil
			}
			priv := ed448.NewKeyFromSeed(seed)
			if !bytes.Equal(priv.Public().(ed448.PublicKey), pub) {
				return subtleErr(errData, "JWK x is not the public key of d"), nil
			}
			return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{ed448Priv: priv})}), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{ed448Pub: ed448.PublicKey(pub)})}), nil
	}
	return subtleErr(errNotSupported, "unsupported Ed448 key format"), nil
}

func (s *subtleAPI) opEd448Export(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("ed448 export: (format, handle) required")
	}
	k, err := s.get(args[1])
	if err != nil {
		return subtleErr(errInvalidAccess, "unknown key"), nil
	}
	b64 := base64.RawURLEncoding.EncodeToString
	pub := k.ed448Pub
	if k.ed448Priv != nil {
		pub = k.ed448Priv.Public().(ed448.PublicKey)
	}
	switch args[0].String() {
	case "raw", "raw-public":
		return bytesValueOK(pub)
	case "raw-seed":
		if k.ed448Priv == nil {
			return subtleErr(errInvalidAccess, "raw-seed export needs a private key"), nil
		}
		return bytesValueOK(k.ed448Priv.Seed())
	case "spki":
		der, err := akpSPKI(oidEd448, pub)
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		return bytesValueOK(der)
	case "pkcs8":
		if k.ed448Priv == nil {
			return subtleErr(errInvalidAccess, "pkcs8 export needs a private key"), nil
		}
		der, err := curvePKCS8(oidEd448, k.ed448Priv.Seed())
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		return bytesValueOK(der)
	case "jwk":
		out := map[string]any{"kty": "OKP", "crv": "Ed448", "alg": "Ed448", "x": b64(pub)}
		if k.ed448Priv != nil {
			out["d"] = b64(k.ed448Priv.Seed())
		}
		return spidermonkey.ValueOf(out), nil
	}
	return subtleErr(errNotSupported, "unsupported Ed448 export format"), nil
}

func (s *subtleAPI) opEd448Sign(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("ed448 sign: (handle, data) required")
	}
	k, err := s.get(args[0])
	if err != nil || k.ed448Priv == nil {
		return subtleErr(errInvalidAccess, "not an Ed448 private key"), nil
	}
	msg, err := argBytes(args[1])
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	// Ed448 carries a context string; Web Crypto uses the empty one.
	return bytesValueOK(ed448.Sign(k.ed448Priv, msg, ""))
}

func (s *subtleAPI) opEd448Verify(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("ed448 verify: (handle, signature, data) required")
	}
	k, err := s.get(args[0])
	if err != nil {
		return spidermonkey.ValueOf(false), nil
	}
	pub := k.ed448Pub
	if pub == nil && k.ed448Priv != nil {
		pub = k.ed448Priv.Public().(ed448.PublicKey)
	}
	sig, serr := argBytes(args[1])
	msg, merr := argBytes(args[2])
	if pub == nil || serr != nil || merr != nil {
		return spidermonkey.ValueOf(false), nil
	}
	return spidermonkey.ValueOf(ed448.Verify(pub, msg, sig, "")), nil
}

// ------------------------------------------------------------------ X448

func (s *subtleAPI) opX448Generate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	var priv, pub x448.Key
	if _, err := rand.Read(priv[:]); err != nil {
		return nil, err
	}
	x448.KeyGen(&pub, &priv)
	return spidermonkey.ValueOf(map[string]any{
		"priv": s.put(&subtleKey{x448Priv: &priv}),
		"pub":  s.put(&subtleKey{x448Pub: &pub}),
	}), nil
}

func (s *subtleAPI) opX448Import(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("x448 import: (format, keyData) required")
	}
	asKey := func(b []byte) *x448.Key {
		var k x448.Key
		copy(k[:], b)
		return &k
	}
	switch args[0].String() {
	case "raw", "raw-public":
		raw, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		if len(raw) != x448.Size {
			return subtleErr(errData, fmt.Sprintf("X448 public key must be %d bytes", x448.Size)), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{x448Pub: asKey(raw)})}), nil
	case "raw-seed":
		raw, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		if len(raw) != x448.Size {
			return subtleErr(errData, fmt.Sprintf("X448 private key must be %d bytes", x448.Size)), nil
		}
		return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{x448Priv: asKey(raw)})}), nil
	case "spki":
		der, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		pub, err := curveParseSPKI(der, oidX448, x448.Size)
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{x448Pub: asKey(pub)})}), nil
	case "pkcs8":
		der, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		d, err := curveParsePKCS8(der, oidX448, x448.Size)
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{x448Priv: asKey(d)})}), nil
	case "jwk":
		var jwk struct{ Kty, Crv, X, D string }
		if err := json.Unmarshal([]byte(args[1].String()), &jwk); err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		if jwk.Kty != "OKP" || jwk.Crv != "X448" {
			return subtleErr(errData, "not an X448 JWK"), nil
		}
		x, err := base64.RawURLEncoding.DecodeString(jwk.X)
		if err != nil || len(x) != x448.Size {
			return subtleErr(errData, "bad X448 JWK x"), nil
		}
		if jwk.D != "" {
			d, err := base64.RawURLEncoding.DecodeString(jwk.D)
			if err != nil || len(d) != x448.Size {
				return subtleErr(errData, "bad X448 JWK d"), nil
			}
			priv := asKey(d)
			var derived x448.Key
			x448.KeyGen(&derived, priv)
			if !bytes.Equal(derived[:], x) {
				return subtleErr(errData, "JWK x is not the public key of d"), nil
			}
			return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{x448Priv: priv})}), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{x448Pub: asKey(x)})}), nil
	}
	return subtleErr(errNotSupported, "unsupported X448 key format"), nil
}

func (s *subtleAPI) opX448Export(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("x448 export: (format, handle) required")
	}
	k, err := s.get(args[1])
	if err != nil {
		return subtleErr(errInvalidAccess, "unknown key"), nil
	}
	b64 := base64.RawURLEncoding.EncodeToString
	pub := k.x448Pub
	if pub == nil && k.x448Priv != nil {
		var p x448.Key
		x448.KeyGen(&p, k.x448Priv)
		pub = &p
	}
	switch args[0].String() {
	case "raw", "raw-public":
		return bytesValueOK(pub[:])
	case "raw-seed":
		if k.x448Priv == nil {
			return subtleErr(errInvalidAccess, "raw-seed export needs a private key"), nil
		}
		return bytesValueOK(k.x448Priv[:])
	case "spki":
		der, err := akpSPKI(oidX448, pub[:])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		return bytesValueOK(der)
	case "pkcs8":
		if k.x448Priv == nil {
			return subtleErr(errInvalidAccess, "pkcs8 export needs a private key"), nil
		}
		der, err := curvePKCS8(oidX448, k.x448Priv[:])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		return bytesValueOK(der)
	case "jwk":
		out := map[string]any{"kty": "OKP", "crv": "X448", "x": b64(pub[:])}
		if k.x448Priv != nil {
			out["d"] = b64(k.x448Priv[:])
		}
		return spidermonkey.ValueOf(out), nil
	}
	return subtleErr(errNotSupported, "unsupported X448 export format"), nil
}

// opX448Derive(priv, pub, bits) -> shared secret.
func (s *subtleAPI) opX448Derive(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("x448 derive: (priv, pub) required")
	}
	priv, perr := s.get(args[0])
	pub, uerr := s.get(args[1])
	if perr != nil || uerr != nil || priv.x448Priv == nil || pub.x448Pub == nil {
		return subtleErr(errInvalidAccess, "X448 derive needs a private and a public key"), nil
	}
	var shared x448.Key
	// A low-order public key yields an all-zero secret, which the function
	// reports rather than returning: agreeing on zero is not agreement.
	if !x448.Shared(&shared, priv.x448Priv, pub.x448Pub) {
		return subtleErr(errOperation, "X448 produced an all-zero shared secret"), nil
	}
	out := shared[:]
	if len(args) > 2 && !args[2].IsUndefined() {
		bits := intArg(args[2])
		if bits > 0 {
			if bits%8 != 0 || bits/8 > len(out) {
				return subtleErr(errOperation, "X448 cannot produce that many bits"), nil
			}
			out = out[:bits/8]
		}
	}
	return bytesValueOK(out)
}
