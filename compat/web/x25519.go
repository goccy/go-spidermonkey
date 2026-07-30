package web

// x25519.go: the X25519 key agreement.
//
// It is the sibling of the Ed25519 signing key that was already here, and the
// only curve in the Web Crypto suite's key-agreement set that this runtime did
// not implement — every generateKey/deriveBits/import case for it failed as
// unsupported. Go's crypto/ecdh provides the primitive.

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/x509"
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
// (a 32-byte public key), "spki", "pkcs8", and "jwk" (OKP with crv X25519).
func (s *subtleAPI) opX25519Import(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("x25519 import: (format, keyData) required")
	}
	switch args[0].String() {
	case "raw":
		raw, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		pub, err := ecdh.X25519().NewPublicKey(raw)
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{xPub: pub})}), nil
	case "spki":
		der, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		parsed, err := x509.ParsePKIXPublicKey(der)
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		pub, ok := parsed.(*ecdh.PublicKey)
		if !ok || pub.Curve() != ecdh.X25519() {
			return subtleErr(errData, "spki key is not X25519"), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{xPub: pub})}), nil
	case "pkcs8":
		der, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		parsed, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		priv, ok := parsed.(*ecdh.PrivateKey)
		if !ok || priv.Curve() != ecdh.X25519() {
			return subtleErr(errData, "pkcs8 key is not X25519"), nil
		}
		return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{xPriv: priv})}), nil
	case "jwk":
		var jwk struct {
			Kty, Crv, X, D string
		}
		if err := json.Unmarshal([]byte(args[1].String()), &jwk); err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		if jwk.Kty != "OKP" || jwk.Crv != "X25519" {
			return subtleErr(errData, "not an X25519 JWK"), nil
		}
		// "x" is required for both halves; a private JWK carries the public key
		// beside the scalar, and the two have to agree.
		x, err := base64.RawURLEncoding.DecodeString(jwk.X)
		if err != nil || len(x) == 0 {
			return subtleErr(errData, "bad x"), nil
		}
		if jwk.D != "" {
			d, err := base64.RawURLEncoding.DecodeString(jwk.D)
			if err != nil {
				return subtleErr(errData, "bad d"), nil
			}
			priv, err := ecdh.X25519().NewPrivateKey(d)
			if err != nil {
				return subtleErr(errData, err.Error()), nil
			}
			if !bytes.Equal(priv.PublicKey().Bytes(), x) {
				return subtleErr(errData, "JWK x is not the public key of d"), nil
			}
			return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{xPriv: priv})}), nil
		}
		pub, err := ecdh.X25519().NewPublicKey(x)
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{xPub: pub})}), nil
	}
	return subtleErr(errNotSupported, "unsupported X25519 key format"), nil
}

// opX25519Export writes a key back out as "raw" (public only), "spki",
// "pkcs8" (private only) or "jwk".
func (s *subtleAPI) opX25519Export(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("x25519 export: (format, handle) required")
	}
	k, kerr := s.get(args[1])
	if kerr != nil {
		return subtleErr(errInvalidAccess, kerr.Error()), nil
	}
	format := args[0].String()
	b64 := base64.RawURLEncoding.EncodeToString
	switch {
	case (format == "raw" || format == "raw-public") && k.xPub != nil:
		return bytesValueOK(k.xPub.Bytes())
	case (format == "raw" || format == "raw-public") && k.xPriv != nil:
		// "raw" of a private key is not exportable in Web Crypto.
		return subtleErr(errInvalidAccess, "cannot export a private key as raw"), nil
	case format == "spki":
		pub := k.xPub
		if pub == nil && k.xPriv != nil {
			pub = k.xPriv.PublicKey()
		}
		if pub == nil {
			break
		}
		der, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		return bytesValueOK(der)
	case format == "pkcs8" && k.xPriv != nil:
		der, err := x509.MarshalPKCS8PrivateKey(k.xPriv)
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		return bytesValueOK(der)
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
	return subtleErr(errNotSupported, "unsupported X25519 export"), nil
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
		return subtleErr(errInvalidAccess, "X25519 derive needs a private and a public key"), nil
	}
	secret, err := privKey.xPriv.ECDH(pubKey.xPub)
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	if bits := intArg(args[2]); bits > 0 {
		want := bits / 8
		if want > len(secret) {
			return subtleErr(errOperation, "requested length exceeds the X25519 shared secret"), nil
		}
		secret = secret[:want]
	}
	return bytesValueOK(secret)
}
