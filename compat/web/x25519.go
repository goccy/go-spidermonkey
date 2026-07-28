package web

// x25519.go: the X25519 key agreement.
//
// It is the sibling of the Ed25519 signing key that was already here, and the
// only curve in the Web Crypto suite's key-agreement set that this runtime did
// not implement — every generateKey/deriveBits/import case for it failed as
// unsupported. Go's crypto/ecdh provides the primitive.

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// opX25519Generate returns a fresh X25519 key pair as host handles.
// bytesValueOK returns bytes as the plain array the guest expects.
func bytesValueOK(b []byte) (spidermonkey.Value, error) { return bytesValue(b), nil }

func (s *subtleAPI) opX25519Generate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return spidermonkey.ValueOf(map[string]any{
		"priv": s.put(&subtleKey{xPriv: priv}),
		"pub":  s.put(&subtleKey{xPub: priv.PublicKey()}),
	}), nil
}

// opX25519Import accepts the formats Web Crypto defines for this curve: "raw"
// (a 32-byte public key) and "jwk" (OKP with crv X25519).
func (s *subtleAPI) opX25519Import(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("x25519 import: (format, keyData) required")
	}
	switch args[0].String() {
	case "raw":
		raw, err := argBytes(args[1])
		if err != nil {
			return subtleErr(err.Error()), nil
		}
		pub, err := ecdh.X25519().NewPublicKey(raw)
		if err != nil {
			return subtleErr("DataError: " + err.Error()), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{xPub: pub})}), nil
	case "jwk":
		var jwk struct {
			Kty, Crv, X, D string
		}
		if err := json.Unmarshal([]byte(args[1].String()), &jwk); err != nil {
			return subtleErr("DataError: " + err.Error()), nil
		}
		if jwk.Kty != "OKP" || jwk.Crv != "X25519" {
			return subtleErr("DataError: not an X25519 JWK"), nil
		}
		if jwk.D != "" {
			d, err := base64.RawURLEncoding.DecodeString(jwk.D)
			if err != nil {
				return subtleErr("DataError: bad d"), nil
			}
			priv, err := ecdh.X25519().NewPrivateKey(d)
			if err != nil {
				return subtleErr("DataError: " + err.Error()), nil
			}
			return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{xPriv: priv})}), nil
		}
		x, err := base64.RawURLEncoding.DecodeString(jwk.X)
		if err != nil {
			return subtleErr("DataError: bad x"), nil
		}
		pub, err := ecdh.X25519().NewPublicKey(x)
		if err != nil {
			return subtleErr("DataError: " + err.Error()), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{xPub: pub})}), nil
	}
	return subtleErr("NotSupportedError: unsupported X25519 key format"), nil
}

// opX25519Export writes a key back out in "raw" (public only) or "jwk".
func (s *subtleAPI) opX25519Export(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("x25519 export: (format, handle) required")
	}
	k, kerr := s.get(args[1])
	if kerr != nil {
		return subtleErr("InvalidAccessError: " + kerr.Error()), nil
	}
	format := args[0].String()
	b64 := base64.RawURLEncoding.EncodeToString
	switch {
	case format == "raw" && k.xPub != nil:
		return bytesValueOK(k.xPub.Bytes())
	case format == "raw" && k.xPriv != nil:
		// "raw" of a private key is not exportable in Web Crypto.
		return subtleErr("InvalidAccessError: cannot export a private key as raw"), nil
	case format == "jwk" && k.xPriv != nil:
		return spidermonkey.ValueOf(map[string]any{
			"kty": "OKP", "crv": "X25519",
			"d": b64(k.xPriv.Bytes()),
			"x": b64(k.xPriv.PublicKey().Bytes()),
		}), nil
	case format == "jwk" && k.xPub != nil:
		return spidermonkey.ValueOf(map[string]any{
			"kty": "OKP", "crv": "X25519", "x": b64(k.xPub.Bytes()),
		}), nil
	}
	return subtleErr("NotSupportedError: unsupported X25519 export"), nil
}

// opX25519Derive computes the shared secret. A requested length longer than the
// secret is an OperationError, not a silently weaker key.
func (s *subtleAPI) opX25519Derive(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("x25519 derive: (priv, pub, bits) required")
	}
	privKey, perr := s.get(args[0])
	pubKey, uerr := s.get(args[1])
	if perr != nil || uerr != nil || privKey.xPriv == nil || pubKey.xPub == nil {
		return subtleErr("InvalidAccessError: X25519 derive needs a private and a public key"), nil
	}
	secret, err := privKey.xPriv.ECDH(pubKey.xPub)
	if err != nil {
		return subtleErr("OperationError: " + err.Error()), nil
	}
	if bits := intArg(args[2]); bits > 0 {
		want := bits / 8
		if want > len(secret) {
			return subtleErr("OperationError: requested length exceeds the X25519 shared secret"), nil
		}
		secret = secret[:want]
	}
	return bytesValueOK(secret)
}
