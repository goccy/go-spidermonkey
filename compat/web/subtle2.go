package web

// subtle2.go: the encryption half of crypto.subtle — AES-GCM/CBC/CTR
// encrypt/decrypt and AES key material, plus ECDH/HKDF/PBKDF2 deriveBits.
// This lifts the surface from the JWS-only set to JWE-capable.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"hash"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/pbkdf2"
)

func jsonUnmarshalString(s string, v any) error { return json.Unmarshal([]byte(s), v) }

func hashNewByName(name string) (func() hash.Hash, error) {
	switch name {
	case "SHA-1":
		return sha1.New, nil
	case "SHA-256":
		return sha256.New, nil
	case "SHA-384":
		return sha512.New384, nil
	case "SHA-512":
		return sha512.New, nil
	}
	return nil, fmt.Errorf("unsupported hash %q", name)
}

// opAESEncrypt(mode, key, iv, data, aad, tagBits) -> bytes (ciphertext with
// tag appended for GCM) | error.
func (s *subtleAPI) opAESEncrypt(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	return s.aesRun(args, true)
}

// opAESDecrypt(mode, key, iv, data, aad, tagBits) -> plaintext | error.
func (s *subtleAPI) opAESDecrypt(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	return s.aesRun(args, false)
}

func (s *subtleAPI) aesRun(args []spidermonkey.Value, encrypt bool) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("aes: (mode, key, iv, data, aad?, tagBits?) required")
	}
	mode := args[0].String()
	key, err := argBytes(args[1])
	if err != nil {
		return nil, err
	}
	iv, err := argBytes(args[2])
	if err != nil {
		return nil, err
	}
	data, err := argBytes(args[3])
	if err != nil {
		return nil, err
	}
	var aad []byte
	if len(args) > 4 {
		aad, _ = argBytes(args[4])
	}
	tagBytes := 16
	if len(args) > 5 && !args[5].IsUndefined() {
		tagBytes = args[5].Int() / 8
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return subtleErr(err.Error()), nil
	}
	switch mode {
	case "AES-GCM":
		gcm, err := cipher.NewGCMWithTagSize(block, tagBytes)
		if err != nil {
			return subtleErr(err.Error()), nil
		}
		if encrypt {
			return bytesValue(gcm.Seal(nil, iv, data, aad)), nil
		}
		pt, err := gcm.Open(nil, iv, data, aad)
		if err != nil {
			return subtleErr("OperationError: decryption failed"), nil
		}
		return bytesValue(pt), nil
	case "AES-CBC":
		bs := block.BlockSize()
		if encrypt {
			padded := pad7(data, bs)
			out := make([]byte, len(padded))
			cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
			return bytesValue(out), nil
		}
		if len(data) == 0 || len(data)%bs != 0 {
			return subtleErr("OperationError: bad block size"), nil
		}
		out := make([]byte, len(data))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
		unpadded, err := unpad7(out, bs)
		if err != nil {
			return subtleErr("OperationError: " + err.Error()), nil
		}
		return bytesValue(unpadded), nil
	case "AES-CTR":
		out := make([]byte, len(data))
		cipher.NewCTR(block, iv).XORKeyStream(out, data)
		return bytesValue(out), nil
	}
	return subtleErr(fmt.Sprintf("unsupported AES mode %q", mode)), nil
}

func pad7(data []byte, bs int) []byte {
	n := bs - len(data)%bs
	out := make([]byte, len(data)+n)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(n)
	}
	return out
}

func unpad7(data []byte, bs int) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty")
	}
	n := int(data[len(data)-1])
	if n == 0 || n > bs || n > len(data) {
		return nil, fmt.Errorf("invalid padding")
	}
	for _, b := range data[len(data)-n:] {
		if int(b) != n {
			return nil, fmt.Errorf("invalid padding")
		}
	}
	return data[:len(data)-n], nil
}

// opECDHDerive(privJWK, pubJWK, bits) -> shared secret bytes. Keys arrive as
// JWK JSON so no host key table is needed.
func (s *subtleAPI) opECDHDerive(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("ecdh: (privJWK, pubJWK, bits) required")
	}
	priv, err := ecdhPrivFromJWK(args[0].String())
	if err != nil {
		return subtleErr(err.Error()), nil
	}
	pub, err := ecdhPubFromJWK(args[1].String())
	if err != nil {
		return subtleErr(err.Error()), nil
	}
	secret, err := priv.ECDH(pub)
	if err != nil {
		return subtleErr(err.Error()), nil
	}
	bits := args[2].Int()
	if bits > 0 && bits/8 < len(secret) {
		secret = secret[:bits/8]
	}
	return bytesValue(secret), nil
}

func ecdhCurve(crv string) (ecdh.Curve, error) {
	switch crv {
	case "P-256":
		return ecdh.P256(), nil
	case "P-384":
		return ecdh.P384(), nil
	case "P-521":
		return ecdh.P521(), nil
	}
	return nil, fmt.Errorf("unsupported ECDH curve %q", crv)
}

func ecdhPrivFromJWK(s string) (*ecdh.PrivateKey, error) {
	var j jwkDoc
	if err := jsonUnmarshalString(s, &j); err != nil {
		return nil, err
	}
	curve, err := ecdhCurve(j.Crv)
	if err != nil {
		return nil, err
	}
	d, err := b64u.DecodeString(j.D)
	if err != nil {
		return nil, fmt.Errorf("bad JWK d: %w", err)
	}
	return curve.NewPrivateKey(d)
}

func ecdhPubFromJWK(s string) (*ecdh.PublicKey, error) {
	var j jwkDoc
	if err := jsonUnmarshalString(s, &j); err != nil {
		return nil, err
	}
	curve, err := ecdhCurve(j.Crv)
	if err != nil {
		return nil, err
	}
	x, err := b64u.DecodeString(j.X)
	if err != nil {
		return nil, fmt.Errorf("bad JWK x: %w", err)
	}
	y, err := b64u.DecodeString(j.Y)
	if err != nil {
		return nil, fmt.Errorf("bad JWK y: %w", err)
	}
	// Uncompressed point: 0x04 || X || Y.
	point := append([]byte{0x04}, append(x, y...)...)
	return curve.NewPublicKey(point)
}

// opHKDFDerive(hash, ikm, salt, info, bits) -> bytes.
func (s *subtleAPI) opHKDFDerive(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 5 {
		return nil, fmt.Errorf("hkdf: (hash, ikm, salt, info, bits) required")
	}
	newHash, err := hashNewByName(args[0].String())
	if err != nil {
		return subtleErr(err.Error()), nil
	}
	ikm, _ := argBytes(args[1])
	salt, _ := argBytes(args[2])
	info, _ := argBytes(args[3])
	length := args[4].Int() / 8
	r := hkdf.New(newHash, ikm, salt, info)
	out := make([]byte, length)
	if _, err := r.Read(out); err != nil {
		return subtleErr(err.Error()), nil
	}
	return bytesValue(out), nil
}

// opPBKDF2Derive(hash, password, salt, iterations, bits) -> bytes.
func (s *subtleAPI) opPBKDF2Derive(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 5 {
		return nil, fmt.Errorf("pbkdf2: (hash, password, salt, iterations, bits) required")
	}
	newHash, err := hashNewByName(args[0].String())
	if err != nil {
		return subtleErr(err.Error()), nil
	}
	pw, _ := argBytes(args[1])
	salt, _ := argBytes(args[2])
	iter := args[3].Int()
	length := args[4].Int() / 8
	return bytesValue(pbkdf2.Key(pw, salt, iter, length, newHash)), nil
}

func subtleErr(msg string) spidermonkey.Value {
	return spidermonkey.ValueOf(map[string]any{"__subtleError": true, "message": msg})
}

func (s *subtleAPI) ops2() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"subtle_aes_encrypt": s.opAESEncrypt,
		"subtle_aes_decrypt": s.opAESDecrypt,
		"subtle_ecdh":        s.opECDHDerive,
		"subtle_hkdf":        s.opHKDFDerive,
		"subtle_pbkdf2":      s.opPBKDF2Derive,
	}
}
