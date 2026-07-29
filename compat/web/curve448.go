package web

// curve448.go: Ed448 signing and X448 key agreement.
//
// These are the 448-bit halves of the same pair the 25519 curves form, and the
// Web Crypto suite exercises them identically. Go's standard library has
// neither, so both come from CIRCL. The formats are the same shapes their
// 25519 siblings use, at the sizes this curve defines: a 57-byte Ed448 public
// key, a 56-byte X448 one.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/cloudflare/circl/dh/x448"
	"github.com/cloudflare/circl/sign/ed448"
	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
			return subtleErr(err.Error()), nil
		}
		if len(raw) != ed448.PublicKeySize {
			return subtleErr(fmt.Sprintf("DataError: Ed448 public key must be %d bytes", ed448.PublicKeySize)), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{ed448Pub: ed448.PublicKey(raw)})}), nil
	case "raw-seed":
		raw, err := argBytes(args[1])
		if err != nil {
			return subtleErr(err.Error()), nil
		}
		if len(raw) != ed448.SeedSize {
			return subtleErr(fmt.Sprintf("DataError: Ed448 seed must be %d bytes", ed448.SeedSize)), nil
		}
		priv := ed448.NewKeyFromSeed(raw)
		return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{ed448Priv: priv})}), nil
	case "jwk":
		var jwk struct{ Kty, Crv, X, D string }
		if err := json.Unmarshal([]byte(args[1].String()), &jwk); err != nil {
			return subtleErr("DataError: " + err.Error()), nil
		}
		if jwk.Kty != "OKP" || jwk.Crv != "Ed448" {
			return subtleErr("DataError: not an Ed448 JWK"), nil
		}
		if jwk.D != "" {
			seed, err := base64.RawURLEncoding.DecodeString(jwk.D)
			if err != nil || len(seed) != ed448.SeedSize {
				return subtleErr("DataError: bad Ed448 JWK d"), nil
			}
			return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{ed448Priv: ed448.NewKeyFromSeed(seed)})}), nil
		}
		pub, err := base64.RawURLEncoding.DecodeString(jwk.X)
		if err != nil || len(pub) != ed448.PublicKeySize {
			return subtleErr("DataError: bad Ed448 JWK x"), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{ed448Pub: ed448.PublicKey(pub)})}), nil
	}
	return subtleErr("NotSupportedError: unsupported Ed448 key format"), nil
}

func (s *subtleAPI) opEd448Export(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("ed448 export: (format, handle) required")
	}
	k, err := s.get(args[1])
	if err != nil {
		return subtleErr("InvalidAccessError: unknown key"), nil
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
			return subtleErr("InvalidAccessError: raw-seed export needs a private key"), nil
		}
		return bytesValueOK(k.ed448Priv.Seed())
	case "jwk":
		out := map[string]any{"kty": "OKP", "crv": "Ed448", "x": b64(pub)}
		if k.ed448Priv != nil {
			out["d"] = b64(k.ed448Priv.Seed())
		}
		return spidermonkey.ValueOf(out), nil
	}
	return subtleErr("NotSupportedError: unsupported Ed448 export format"), nil
}

func (s *subtleAPI) opEd448Sign(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("ed448 sign: (handle, data) required")
	}
	k, err := s.get(args[0])
	if err != nil || k.ed448Priv == nil {
		return subtleErr("InvalidAccessError: not an Ed448 private key"), nil
	}
	msg, err := argBytes(args[1])
	if err != nil {
		return subtleErr(err.Error()), nil
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
			return subtleErr(err.Error()), nil
		}
		if len(raw) != x448.Size {
			return subtleErr(fmt.Sprintf("DataError: X448 public key must be %d bytes", x448.Size)), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{x448Pub: asKey(raw)})}), nil
	case "raw-seed":
		raw, err := argBytes(args[1])
		if err != nil {
			return subtleErr(err.Error()), nil
		}
		if len(raw) != x448.Size {
			return subtleErr(fmt.Sprintf("DataError: X448 private key must be %d bytes", x448.Size)), nil
		}
		return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{x448Priv: asKey(raw)})}), nil
	case "jwk":
		var jwk struct{ Kty, Crv, X, D string }
		if err := json.Unmarshal([]byte(args[1].String()), &jwk); err != nil {
			return subtleErr("DataError: " + err.Error()), nil
		}
		if jwk.Kty != "OKP" || jwk.Crv != "X448" {
			return subtleErr("DataError: not an X448 JWK"), nil
		}
		if jwk.D != "" {
			d, err := base64.RawURLEncoding.DecodeString(jwk.D)
			if err != nil || len(d) != x448.Size {
				return subtleErr("DataError: bad X448 JWK d"), nil
			}
			return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{x448Priv: asKey(d)})}), nil
		}
		x, err := base64.RawURLEncoding.DecodeString(jwk.X)
		if err != nil || len(x) != x448.Size {
			return subtleErr("DataError: bad X448 JWK x"), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{x448Pub: asKey(x)})}), nil
	}
	return subtleErr("NotSupportedError: unsupported X448 key format"), nil
}

func (s *subtleAPI) opX448Export(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("x448 export: (format, handle) required")
	}
	k, err := s.get(args[1])
	if err != nil {
		return subtleErr("InvalidAccessError: unknown key"), nil
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
			return subtleErr("InvalidAccessError: raw-seed export needs a private key"), nil
		}
		return bytesValueOK(k.x448Priv[:])
	case "jwk":
		out := map[string]any{"kty": "OKP", "crv": "X448", "x": b64(pub[:])}
		if k.x448Priv != nil {
			out["d"] = b64(k.x448Priv[:])
		}
		return spidermonkey.ValueOf(out), nil
	}
	return subtleErr("NotSupportedError: unsupported X448 export format"), nil
}

// opX448Derive(priv, pub, bits) -> shared secret.
func (s *subtleAPI) opX448Derive(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("x448 derive: (priv, pub) required")
	}
	priv, perr := s.get(args[0])
	pub, uerr := s.get(args[1])
	if perr != nil || uerr != nil || priv.x448Priv == nil || pub.x448Pub == nil {
		return subtleErr("InvalidAccessError: X448 derive needs a private and a public key"), nil
	}
	var shared x448.Key
	// A low-order public key yields an all-zero secret, which the function
	// reports rather than returning: agreeing on zero is not agreement.
	if !x448.Shared(&shared, priv.x448Priv, pub.x448Pub) {
		return subtleErr("OperationError: X448 produced an all-zero shared secret"), nil
	}
	out := shared[:]
	if len(args) > 2 && !args[2].IsUndefined() {
		bits := intArg(args[2])
		if bits > 0 {
			if bits%8 != 0 || bits/8 > len(out) {
				return subtleErr("OperationError: X448 cannot produce that many bits"), nil
			}
			out = out[:bits/8]
		}
	}
	return bytesValueOK(out)
}
