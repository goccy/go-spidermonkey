package nodejs

// nodecrypto2.go: the fuller node:crypto surface — symmetric ciphers
// (AES-GCM/CBC/CTR), sign/verify (RSA/ECDSA), key derivation (pbkdf2,
// scrypt, hkdf), Diffie-Hellman, and keypair generation. Keys and cipher
// state live host-side in handle tables; the JS side (extras.js) exposes the
// Cipheriv/Sign/etc. classes over these ops.

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"hash"
	"strings"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/crypto/scrypt"
)

func (rt *Runtime) crypto2Ops() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"crypto_cipher":      rt.opCipher,
		"crypto_sign":        rt.opSign,
		"crypto_verify":      rt.opVerify,
		"crypto_pbkdf2":      rt.opPBKDF2,
		"crypto_scrypt":      rt.opScrypt,
		"crypto_hkdf":        rt.opHKDF,
		"crypto_generatekey": rt.opGenerateKeyPair,
	}
}

// opCipher is a one-shot symmetric transform (the JS Cipheriv/Decipheriv
// classes buffer update() data and call this at final): encrypt returns
// {data, tag} (tag empty for non-GCM); decrypt returns {data} or an error
// object. args: (algorithm, key, iv, encrypt, data, aad, tag).
func (rt *Runtime) opCipher(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 5 {
		return nil, fmt.Errorf("crypto_cipher: (algorithm, key, iv, encrypt, data, aad?, tag?) required")
	}
	algo := args[0].String()
	key, _ := valueBytes(args[1])
	iv, _ := valueBytes(args[2])
	encrypt := args[3].Bool()
	data, _ := valueBytes(args[4])
	var aad, tag []byte
	if len(args) > 5 {
		aad, _ = valueBytes(args[5])
	}
	if len(args) > 6 {
		tag, _ = valueBytes(args[6])
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	switch {
	case strings.HasSuffix(algo, "-gcm"):
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return cryptoErr(err.Error()), nil
		}
		if encrypt {
			sealed := gcm.Seal(nil, iv, data, aad)
			ct := sealed[:len(sealed)-gcm.Overhead()]
			return rt.cipherResult(ct, sealed[len(sealed)-gcm.Overhead():])
		}
		pt, err := gcm.Open(nil, iv, append(append([]byte{}, data...), tag...), aad)
		if err != nil {
			return cryptoErr("Unsupported state or unable to authenticate data"), nil
		}
		return rt.cipherResult(pt, nil)
	case strings.HasSuffix(algo, "-ctr"):
		out := make([]byte, len(data))
		cipher.NewCTR(block, iv).XORKeyStream(out, data)
		return rt.cipherResult(out, nil)
	case strings.HasSuffix(algo, "-cbc"):
		bs := block.BlockSize()
		if encrypt {
			padded := pkcs7Pad(data, bs)
			out := make([]byte, len(padded))
			cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
			return rt.cipherResult(out, nil)
		}
		if len(data) == 0 || len(data)%bs != 0 {
			return cryptoErr("bad decrypt: input not block-aligned"), nil
		}
		out := make([]byte, len(data))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
		unpadded, err := pkcs7Unpad(out, bs)
		if err != nil {
			return cryptoErr(err.Error()), nil
		}
		return rt.cipherResult(unpadded, nil)
	}
	return cryptoErr(fmt.Sprintf("unsupported cipher %q", algo)), nil
}

// cipherResult returns {data: Uint8Array, tag: Uint8Array}. Both ride the
// bytes bridge and are tracked for release.
func (rt *Runtime) cipherResult(data, tag []byte) (spidermonkey.Value, error) {
	obj, err := rt.js.NewObject()
	if err != nil {
		return nil, err
	}
	dv, err := rt.js.NewBytes(data)
	if err != nil {
		return nil, err
	}
	defer dv.Free()
	if err := obj.Set("data", dv); err != nil {
		return nil, err
	}
	tv, err := rt.js.NewBytes(tag)
	if err != nil {
		return nil, err
	}
	defer tv.Free()
	if err := obj.Set("tag", tv); err != nil {
		return nil, err
	}
	return rt.trackReturn(obj), nil
}

func (rt *Runtime) bytesReturn(b []byte) (spidermonkey.Value, error) {
	u8, err := rt.js.NewBytes(b)
	if err != nil {
		return nil, err
	}
	return rt.trackReturn(u8), nil
}

func cryptoErr(msg string) spidermonkey.Value {
	return spidermonkey.ValueOf(map[string]any{"code": "ERR_CRYPTO", "message": msg})
}

func pkcs7Pad(data []byte, bs int) []byte {
	n := bs - len(data)%bs
	return append(data, bytes.Repeat([]byte{byte(n)}, n)...)
}

func pkcs7Unpad(data []byte, bs int) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("bad decrypt: empty")
	}
	n := int(data[len(data)-1])
	if n == 0 || n > bs || n > len(data) {
		return nil, fmt.Errorf("bad decrypt: invalid padding")
	}
	for _, b := range data[len(data)-n:] {
		if int(b) != n {
			return nil, fmt.Errorf("bad decrypt: invalid padding")
		}
	}
	return data[:len(data)-n], nil
}

// ---- sign / verify (PEM keys) ----

func hashForSign(name string) (crypto.Hash, hash.Hash, error) {
	switch name {
	case "sha256", "RSA-SHA256", "ecdsa-with-SHA256":
		h, _ := nodeHashByName("sha256")
		return h, h.New(), nil
	case "sha384":
		h, _ := nodeHashByName("sha384")
		return h, h.New(), nil
	case "sha512":
		h, _ := nodeHashByName("sha512")
		return h, h.New(), nil
	case "sha1":
		h, _ := nodeHashByName("sha1")
		return h, h.New(), nil
	}
	h, err := nodeHashByName(name)
	if err != nil {
		return 0, nil, err
	}
	return h, h.New(), nil
}

func parsePrivateKey(pemBytes []byte) (any, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

func parsePublicKey(pemBytes []byte) (any, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	if k, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		return k, nil
	}
	return x509.ParsePKCS1PublicKey(block.Bytes)
}

func (rt *Runtime) opSign(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("sign: (algorithm, key, data) required")
	}
	_, hh, err := hashForSign(args[0].String())
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	keyPEM, err := valueBytes(args[1])
	if err != nil {
		return nil, err
	}
	data, err := valueBytes(args[2])
	if err != nil {
		return nil, err
	}
	key, err := parsePrivateKey(keyPEM)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	hh.Write(data)
	digest := hh.Sum(nil)
	ch, _, _ := hashForSign(args[0].String())
	switch k := key.(type) {
	case *rsa.PrivateKey:
		sig, err := rsa.SignPKCS1v15(rand.Reader, k, ch, digest)
		if err != nil {
			return cryptoErr(err.Error()), nil
		}
		return rt.bytesReturn(sig)
	case *ecdsa.PrivateKey:
		sig, err := ecdsa.SignASN1(rand.Reader, k, digest)
		if err != nil {
			return cryptoErr(err.Error()), nil
		}
		return rt.bytesReturn(sig)
	}
	return cryptoErr("unsupported private key type"), nil
}

func (rt *Runtime) opVerify(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("verify: (algorithm, key, data, sig) required")
	}
	ch, hh, err := hashForSign(args[0].String())
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	keyPEM, err := valueBytes(args[1])
	if err != nil {
		return nil, err
	}
	data, err := valueBytes(args[2])
	if err != nil {
		return nil, err
	}
	sig, err := valueBytes(args[3])
	if err != nil {
		return nil, err
	}
	key, err := parsePublicKey(keyPEM)
	if err != nil {
		// Maybe a private key PEM was supplied (Node allows it).
		if pk, perr := parsePrivateKey(keyPEM); perr == nil {
			switch k := pk.(type) {
			case *rsa.PrivateKey:
				key = &k.PublicKey
			case *ecdsa.PrivateKey:
				key = &k.PublicKey
			}
		} else {
			return spidermonkey.ValueOf(false), nil
		}
	}
	hh.Write(data)
	digest := hh.Sum(nil)
	switch k := key.(type) {
	case *rsa.PublicKey:
		return spidermonkey.ValueOf(rsa.VerifyPKCS1v15(k, ch, digest, sig) == nil), nil
	case *ecdsa.PublicKey:
		return spidermonkey.ValueOf(ecdsa.VerifyASN1(k, digest, sig)), nil
	}
	return spidermonkey.ValueOf(false), nil
}

// ---- KDFs ----

func (rt *Runtime) opPBKDF2(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 5 {
		return nil, fmt.Errorf("pbkdf2: (password, salt, iterations, keylen, digest) required")
	}
	pw, _ := valueBytes(args[0])
	salt, _ := valueBytes(args[1])
	iter := args[2].Int()
	keylen := args[3].Int()
	h, err := nodeHashByName(args[4].String())
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	return rt.bytesReturn(pbkdf2.Key(pw, salt, iter, keylen, h.New))
}

func (rt *Runtime) opScrypt(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("scrypt: (password, salt, keylen, params) required")
	}
	pw, _ := valueBytes(args[0])
	salt, _ := valueBytes(args[1])
	keylen := args[2].Int()
	N, r, p := 16384, 8, 1
	if o := args[3].Object(); o != nil {
		defer o.Free()
		if v, _ := o.Get("N"); v != nil && !v.IsUndefined() {
			N = v.Int()
		}
		if v, _ := o.Get("r"); v != nil && !v.IsUndefined() {
			r = v.Int()
		}
		if v, _ := o.Get("p"); v != nil && !v.IsUndefined() {
			p = v.Int()
		}
	}
	out, err := scrypt.Key(pw, salt, N, r, p, keylen)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	return rt.bytesReturn(out)
}

func (rt *Runtime) opHKDF(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 5 {
		return nil, fmt.Errorf("hkdf: (digest, ikm, salt, info, keylen) required")
	}
	h, err := nodeHashByName(args[0].String())
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	ikm, _ := valueBytes(args[1])
	salt, _ := valueBytes(args[2])
	info, _ := valueBytes(args[3])
	keylen := args[4].Int()
	r := hkdf.New(h.New, ikm, salt, info)
	out := make([]byte, keylen)
	if _, err := r.Read(out); err != nil {
		return cryptoErr(err.Error()), nil
	}
	return rt.bytesReturn(out)
}

// opGenerateKeyPair(type, opts) -> {publicKey, privateKey} PEM strings.
func (rt *Runtime) opGenerateKeyPair(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("generateKeyPair: (type, options) required")
	}
	typ := args[0].String()
	opts := args[1].Object()
	var modulus int = 2048
	var curve string = "P-256"
	if opts != nil {
		defer opts.Free()
		if v, _ := opts.Get("modulusLength"); v != nil && !v.IsUndefined() {
			modulus = v.Int()
		}
		if v, _ := opts.Get("namedCurve"); v != nil && !v.IsUndefined() {
			curve = v.String()
		}
	}
	var pubDER, privDER []byte
	var err error
	switch typ {
	case "rsa":
		k, gerr := rsa.GenerateKey(rand.Reader, modulus)
		if gerr != nil {
			return cryptoErr(gerr.Error()), nil
		}
		privDER, _ = x509.MarshalPKCS8PrivateKey(k)
		pubDER, _ = x509.MarshalPKIXPublicKey(&k.PublicKey)
	case "ec":
		c, cerr := ecCurveByName(curve)
		if cerr != nil {
			return cryptoErr(cerr.Error()), nil
		}
		k, gerr := ecdsa.GenerateKey(c, rand.Reader)
		if gerr != nil {
			return cryptoErr(gerr.Error()), nil
		}
		privDER, _ = x509.MarshalPKCS8PrivateKey(k)
		pubDER, _ = x509.MarshalPKIXPublicKey(&k.PublicKey)
	default:
		return cryptoErr(fmt.Sprintf("unsupported key type %q", typ)), nil
	}
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	return spidermonkey.ValueOf(map[string]any{
		"publicKey":  string(pubPEM),
		"privateKey": string(privPEM),
	}), nil
}

func ecCurveByName(name string) (elliptic.Curve, error) {
	switch name {
	case "P-256", "prime256v1", "secp256r1":
		return elliptic.P256(), nil
	case "P-384", "secp384r1":
		return elliptic.P384(), nil
	case "P-521", "secp521r1":
		return elliptic.P521(), nil
	}
	return nil, fmt.Errorf("unsupported curve %q", name)
}

var _ hash.Hash
