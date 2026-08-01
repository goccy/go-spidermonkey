package nodejs

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	_ "crypto/md5" // register the hashes crypto.Hash.New needs
	"crypto/rand"
	"crypto/rsa"
	_ "crypto/sha1" // register SHA-1 for crypto.SHA1.New (OAEP)
	"crypto/sha256"
	_ "crypto/sha256"
	_ "crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/crypto/scrypt"
	"hash"
	"math/big"
	"strings"
)

// ------------------------- the hash/hmac primitives behind node:crypto
// (js/extras.js).
// Node uses OpenSSL-style lowercase algorithm names.

func nodeHashByName(name string) (crypto.Hash, error) {
	switch name {
	case "md5":
		return crypto.MD5, nil
	case "sha1":
		return crypto.SHA1, nil
	case "sha224":
		return crypto.SHA224, nil
	case "sha256":
		return crypto.SHA256, nil
	case "sha384":
		return crypto.SHA384, nil
	case "sha512":
		return crypto.SHA512, nil
	case "sha512-224", "sha512_224":
		return crypto.SHA512_224, nil
	case "sha512-256", "sha512_256":
		return crypto.SHA512_256, nil
	}
	return 0, fmt.Errorf("unsupported digest %q", name)
}

func (rt *Runtime) opCryptoHash(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("crypto_hash: (algorithm, data) required")
	}
	h, err := nodeHashByName(args[0].String())
	if err != nil {
		return nil, err
	}
	data, err := valueBytes(args[1])
	if err != nil {
		return nil, err
	}
	hh := h.New()
	hh.Write(data)
	return byteArrayValue(hh.Sum(nil)), nil
}

func (rt *Runtime) opCryptoHMAC(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("crypto_hmac: (algorithm, key, data) required")
	}
	h, err := nodeHashByName(args[0].String())
	if err != nil {
		return nil, err
	}
	key, err := valueBytes(args[1])
	if err != nil {
		return nil, err
	}
	data, err := valueBytes(args[2])
	if err != nil {
		return nil, err
	}
	m := hmac.New(h.New, key)
	m.Write(data)
	return byteArrayValue(m.Sum(nil)), nil
}

// valueBytes reads a BufferSource argument (freeing the arg pin) or falls
// back to the value's string bytes.
func valueBytes(v spidermonkey.Value) ([]byte, error) {
	if o := v.Object(); o != nil {
		defer o.Free()
		return o.Bytes()
	}
	return []byte(v.String()), nil
}

// byteArrayValue returns b as a plain array (data, not a handle).
func byteArrayValue(b []byte) spidermonkey.Value {
	ints := make([]int, len(b))
	for i, x := range b {
		ints[i] = int(x)
	}
	return spidermonkey.ValueOf(ints)
}

// ------------------------- the fuller node:crypto surface — symmetric ciphers
// (AES-GCM/CBC/CTR), sign/verify (RSA/ECDSA), key derivation (pbkdf2,
// scrypt, hkdf), Diffie-Hellman, and keypair generation. Keys and cipher
// state live host-side in handle tables; the JS side (extras.js) exposes the
// Cipheriv/Sign/etc. classes over these ops.

// KDF guardrails: a guest-supplied output length or scrypt cost must not drive
// an unbounded host allocation (a Go OOM is fatal and un-recoverable). These
// caps are generous relative to any real key-derivation use.
const (
	maxKDFBytes   = 1 << 24        // 16 MiB derived-key ceiling
	maxScryptMem  = 32 << 20       // 32 MiB, matching Node's default scrypt maxmem
	maxPBKDF2Iter = 100_000_000    // iteration ceiling (uninterruptible host CPU guard)
	maxScryptOps  = int64(1) << 26 // N*r*p work ceiling (~67M), a CPU guard on top of the memory caps
)

func (rt *Runtime) crypto2Ops() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"crypto_cipher":        rt.opCipher,
		"crypto_sign":          rt.opSign,
		"crypto_verify":        rt.opVerify,
		"crypto_pbkdf2":        rt.opPBKDF2,
		"crypto_scrypt":        rt.opScrypt,
		"crypto_hkdf":          rt.opHKDF,
		"crypto_generatekey":   rt.opGenerateKeyPair,
		"crypto_key_parse":     rt.opKeyParse,
		"crypto_key_export":    rt.opKeyExport,
		"crypto_ecdh_generate": rt.opECDHGenerate,
		"crypto_ecdh_compute":  rt.opECDHCompute,
		"crypto_dh_keyobject":  rt.opDHKeyObject,
	}
}

// aesKeyLen returns the key length (bytes) implied by an "aes-<bits>-<mode>"
// algorithm name, and whether the name is a recognized AES variant.
func aesKeyLen(algo string) (int, bool) {
	switch {
	case strings.HasPrefix(algo, "aes-128-"):
		return 16, true
	case strings.HasPrefix(algo, "aes-192-"):
		return 24, true
	case strings.HasPrefix(algo, "aes-256-"):
		return 32, true
	}
	return 0, false
}

// opCipher is a one-shot symmetric transform (the JS Cipheriv/Decipheriv
// classes buffer update() data and call this at final): encrypt returns
// {data, tag} (tag empty for non-GCM); decrypt returns {data} or an error
// object. args: (algorithm, key, iv, encrypt, data, aad, tag, autoPadding).
func (rt *Runtime) opCipher(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 5 {
		return nil, fmt.Errorf("crypto_cipher: (algorithm, key, iv, encrypt, data, aad?, tag?, autoPadding?) required")
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
	// setAutoPadding(false) (block modes only; stream modes ignore it, as in
	// Node). Default: on.
	autoPad := true
	if len(args) > 7 && !args[7].IsUndefined() {
		autoPad = args[7].Bool()
	}

	// The named algorithm's key size must match the actual key. aes.NewCipher
	// picks AES-128/192/256 from len(key) ALONE, so without this check
	// createCipheriv("aes-256-gcm", key16, iv) would silently encrypt with
	// AES-128 (a downgrade / interop break); Node rejects the mismatch.
	if want, ok := aesKeyLen(algo); ok && len(key) != want {
		return cryptoErr("Invalid key length"), nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	switch {
	case strings.HasSuffix(algo, "-gcm"):
		// The IV length is caller-controlled; NewGCMWithNonceSize accepts any
		// non-empty length, whereas Seal/Open on a fixed-nonce AEAD PANIC on a
		// mismatch. Node defaults to 12 but allows a configured IV length.
		if len(iv) == 0 {
			return cryptoErr("Invalid IV length"), nil
		}
		gcm, err := cipher.NewGCMWithNonceSize(block, len(iv))
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
		if len(iv) != block.BlockSize() {
			return cryptoErr("Invalid IV length"), nil
		}
		out := make([]byte, len(data))
		cipher.NewCTR(block, iv).XORKeyStream(out, data)
		return rt.cipherResult(out, nil)
	case strings.HasSuffix(algo, "-cbc"):
		bs := block.BlockSize()
		if len(iv) != bs {
			return cryptoErr("Invalid IV length"), nil
		}
		if encrypt {
			if !autoPad {
				// Node: with autopadding off the caller supplies whole blocks
				// and gets exactly input-length ciphertext (no PKCS#7 block).
				if len(data)%bs != 0 {
					return cryptoErr("data not multiple of block length"), nil
				}
				out := make([]byte, len(data))
				cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, data)
				return rt.cipherResult(out, nil)
			}
			padded := pkcs7Pad(data, bs)
			out := make([]byte, len(padded))
			cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
			return rt.cipherResult(out, nil)
		}
		if !autoPad && len(data) == 0 {
			// Zero blocks of unpadded ciphertext decrypt to zero bytes.
			return rt.cipherResult(nil, nil)
		}
		if len(data) == 0 || len(data)%bs != 0 {
			return cryptoErr("bad decrypt: input not block-aligned"), nil
		}
		out := make([]byte, len(data))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
		if !autoPad {
			// Autopadding off: hand back the raw plaintext INCLUDING the
			// padding bytes — Node leaves stripping to the caller.
			return rt.cipherResult(out, nil)
		}
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

// pssSaltLength maps Node's RSA_PSS_SALTLEN_* sentinels to Go's rsa salt-length
// convention: -1 (DIGEST) → equals-hash, -2 (MAX_SIGN/AUTO) → auto, else the
// explicit byte count.
func pssSaltLength(n int) int {
	switch {
	case n == -1:
		return rsa.PSSSaltLengthEqualsHash
	case n < 0:
		return rsa.PSSSaltLengthAuto
	default:
		return n
	}
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

// noDigestAlgo reports whether the guest passed algorithm null/undefined
// (crypto.sign(null, ...) — the Ed25519 one-shot form; the JS side normalizes
// null to "").
func noDigestAlgo(s string) bool {
	return s == "" || s == "null" || s == "undefined" || s == "ed25519"
}

func (rt *Runtime) opSign(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("sign: (algorithm, key, data) required")
	}
	algo := args[0].String()
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
	// Ed25519 signs the message directly — Node requires algorithm null (no
	// pre-hash) for it, and conversely a null algorithm only works for Ed25519.
	if ek, ok := key.(ed25519.PrivateKey); ok {
		if !noDigestAlgo(algo) {
			return cryptoErr("ed25519 does not support a digest algorithm"), nil
		}
		return rt.bytesReturn(ed25519.Sign(ek, data))
	}
	if noDigestAlgo(algo) {
		return cryptoErr("a digest algorithm is required for this key type"), nil
	}
	_, hh, err := hashForSign(algo)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	hh.Write(data)
	digest := hh.Sum(nil)
	ch, _, _ := hashForSign(algo)
	// Optional trailing (padding, saltLength). Default padding 1 = PKCS#1 v1.5;
	// 6 = RSA-PSS. saltLength -2 (auto/max) is the Node default for PSS.
	padding, saltLen := 1, -2
	if len(args) > 3 && !args[3].IsUndefined() {
		padding = intArg(args[3])
	}
	if len(args) > 4 && !args[4].IsUndefined() {
		saltLen = intArg(args[4])
	}
	switch k := key.(type) {
	case *rsa.PrivateKey:
		if err := checkRSAModulus(k.N); err != nil {
			return cryptoErr(err.Error()), nil
		}
		if padding == 6 {
			sig, err := rsa.SignPSS(rand.Reader, k, ch, digest, &rsa.PSSOptions{SaltLength: pssSaltLength(saltLen), Hash: ch})
			if err != nil {
				return cryptoErr(err.Error()), nil
			}
			return rt.bytesReturn(sig)
		}
		if padding != 1 {
			return cryptoErr(fmt.Sprintf("unsupported RSA padding %d", padding)), nil
		}
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
	algo := args[0].String()
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
			case ed25519.PrivateKey:
				key = k.Public()
			}
		} else {
			return spidermonkey.ValueOf(false), nil
		}
	}
	// Ed25519 verifies the message directly (algorithm null, no pre-hash).
	if ek, ok := key.(ed25519.PublicKey); ok {
		if !noDigestAlgo(algo) {
			return cryptoErr("ed25519 does not support a digest algorithm"), nil
		}
		return spidermonkey.ValueOf(ed25519.Verify(ek, data, sig)), nil
	}
	if noDigestAlgo(algo) {
		return cryptoErr("a digest algorithm is required for this key type"), nil
	}
	ch, hh, err := hashForSign(algo)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	hh.Write(data)
	digest := hh.Sum(nil)
	padding, saltLen := 1, -2
	if len(args) > 4 && !args[4].IsUndefined() {
		padding = intArg(args[4])
	}
	if len(args) > 5 && !args[5].IsUndefined() {
		saltLen = intArg(args[5])
	}
	switch k := key.(type) {
	case *rsa.PublicKey:
		if err := checkRSAModulus(k.N); err != nil {
			return spidermonkey.ValueOf(false), nil // reject oversized modulus (DoS guard) rather than run the modexp
		}
		if padding == 6 {
			return spidermonkey.ValueOf(rsa.VerifyPSS(k, ch, digest, sig, &rsa.PSSOptions{SaltLength: pssSaltLength(saltLen), Hash: ch}) == nil), nil
		}
		if padding != 1 {
			return cryptoErr(fmt.Sprintf("unsupported RSA padding %d", padding)), nil
		}
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
	iter := intArg(args[2])
	if iter < 1 || iter > maxPBKDF2Iter {
		return cryptoErr("iterations out of range"), nil
	}
	keylen := intArg(args[3])
	if keylen < 0 || keylen > maxKDFBytes {
		return cryptoErr("invalid key length"), nil
	}
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
	keylen := intArg(args[2])
	N, r, p := 16384, 8, 1
	if o := args[3].Object(); o != nil {
		defer o.Free()
		// Node accepts both the short names (N/r/p) and the long aliases
		// (cost/blockSize/parallelization); the alias wins when both are set,
		// matching Node. Silently ignoring the aliases would hand a caller who
		// deliberately raised the work factor the much weaker defaults.
		getInt := func(names ...string) (int, bool) {
			for _, n := range names {
				if v, ok := optScalar(o, n); ok {
					return v.Int(), true
				}
			}
			return 0, false
		}
		if v, ok := getInt("cost", "N"); ok {
			N = v
		}
		if v, ok := getInt("blockSize", "r"); ok {
			r = v
		}
		if v, ok := getInt("parallelization", "p"); ok {
			p = v
		}
	}
	// Bound cost/output before scrypt.Key allocates 128*N*r bytes.
	if keylen < 0 || keylen > maxKDFBytes {
		return cryptoErr("invalid key length"), nil
	}
	if N <= 1 || N&(N-1) != 0 || r <= 0 || p <= 0 {
		return cryptoErr("invalid scrypt parameters"), nil
	}
	// scrypt.Key allocates 128*N*r (the V array) AND 128*r*p (the B buffer, via
	// pbkdf2) — bound BOTH, or a huge p (with tiny N,r) still OOMs the host.
	if int64(128)*int64(N)*int64(r) > maxScryptMem || int64(128)*int64(r)*int64(p) > maxScryptMem {
		return cryptoErr("scrypt parameters exceed the memory limit"), nil
	}
	// The memory caps still permit a huge N*r*p work factor (e.g. N=p=262144,
	// r=1) that pins a core for minutes; bound the CPU cost too.
	if int64(N)*int64(r)*int64(p) > maxScryptOps {
		return cryptoErr("scrypt parameters exceed the cost limit"), nil
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
	keylen := intArg(args[4])
	if keylen < 0 || keylen > maxKDFBytes {
		return cryptoErr("invalid key length"), nil
	}
	r := hkdf.New(h.New, ikm, salt, info)
	out := make([]byte, keylen)
	if _, err := r.Read(out); err != nil {
		return cryptoErr(err.Error()), nil
	}
	return rt.bytesReturn(out)
}

// ---- key material helpers (KeyObject / generateKeyPair encodings) ----

// asymmetricKeyTypeOf names a parsed key the way Node's
// KeyObject.asymmetricKeyType does.
func asymmetricKeyTypeOf(key any) string {
	switch key.(type) {
	case *rsa.PrivateKey, *rsa.PublicKey:
		return "rsa"
	case *ecdsa.PrivateKey, *ecdsa.PublicKey:
		return "ec"
	case ed25519.PrivateKey, ed25519.PublicKey:
		return "ed25519"
	case *ecdh.PrivateKey, *ecdh.PublicKey:
		return "x25519"
	}
	return ""
}

// publicKeyOf derives the public half of a private key.
func publicKeyOf(priv any) any {
	switch k := priv.(type) {
	case *rsa.PrivateKey:
		return &k.PublicKey
	case *ecdsa.PrivateKey:
		return &k.PublicKey
	case ed25519.PrivateKey:
		return k.Public()
	case *ecdh.PrivateKey:
		return k.PublicKey()
	}
	return nil
}

// marshalPrivateKeyDER serializes a private key as the requested DER
// structure and returns the matching PEM label — the label MUST match the
// structure ('pkcs1' means a real RSAPrivateKey inside 'RSA PRIVATE KEY',
// never a mislabeled PKCS#8).
func marshalPrivateKeyDER(key any, typ string) ([]byte, string, error) {
	switch typ {
	case "pkcs8", "":
		der, err := x509.MarshalPKCS8PrivateKey(key)
		return der, "PRIVATE KEY", err
	case "pkcs1":
		rk, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, "", fmt.Errorf("pkcs1 encoding requires an RSA key")
		}
		return x509.MarshalPKCS1PrivateKey(rk), "RSA PRIVATE KEY", nil
	case "sec1":
		ek, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, "", fmt.Errorf("sec1 encoding requires an EC key")
		}
		der, err := x509.MarshalECPrivateKey(ek)
		return der, "EC PRIVATE KEY", err
	}
	return nil, "", fmt.Errorf("unsupported private key encoding type %q", typ)
}

// marshalPublicKeyDER is marshalPrivateKeyDER's public-key counterpart
// (spki default, pkcs1 for RSA).
func marshalPublicKeyDER(key any, typ string) ([]byte, string, error) {
	switch typ {
	case "spki", "":
		der, err := x509.MarshalPKIXPublicKey(key)
		return der, "PUBLIC KEY", err
	case "pkcs1":
		rk, ok := key.(*rsa.PublicKey)
		if !ok {
			return nil, "", fmt.Errorf("pkcs1 encoding requires an RSA key")
		}
		return x509.MarshalPKCS1PublicKey(rk), "RSA PUBLIC KEY", nil
	}
	return nil, "", fmt.Errorf("unsupported public key encoding type %q", typ)
}

// parseAnyKey parses PEM key material as a private key first, then as a
// public key, reporting which it was.
func parseAnyKey(pemBytes []byte) (key any, isPrivate bool, err error) {
	if k, perr := parsePrivateKey(pemBytes); perr == nil {
		return k, true, nil
	}
	k, perr := parsePublicKey(pemBytes)
	if perr != nil {
		return nil, false, perr
	}
	return k, false, nil
}

// opKeyParse(pem) -> {keyType, asymmetricKeyType, privatePem?, publicPem} —
// the host half of createPublicKey/createPrivateKey: canonical PKCS#8/SPKI
// PEMs plus the metadata KeyObject exposes.
func (rt *Runtime) opKeyParse(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("key_parse: key material required")
	}
	raw, err := valueBytes(args[0])
	if err != nil {
		return nil, err
	}
	key, isPrivate, perr := parseAnyKey(raw)
	if perr != nil {
		return cryptoErr(perr.Error()), nil
	}
	out := map[string]any{
		"asymmetricKeyType": asymmetricKeyTypeOf(key),
	}
	pub := key
	if isPrivate {
		out["keyType"] = "private"
		privDER, _, merr := marshalPrivateKeyDER(key, "pkcs8")
		if merr != nil {
			return cryptoErr(merr.Error()), nil
		}
		out["privatePem"] = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}))
		pub = publicKeyOf(key)
		if pub == nil {
			return cryptoErr("cannot derive public key"), nil
		}
	} else {
		out["keyType"] = "public"
	}
	pubDER, _, merr := marshalPublicKeyDER(pub, "spki")
	if merr != nil {
		return cryptoErr(merr.Error()), nil
	}
	out["publicPem"] = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	return spidermonkey.ValueOf(out), nil
}

// opKeyExport(pem, wantPrivate, format, type) -> PEM string (format "pem") or
// DER bytes (format "der"). Exporting the public side of a private key
// derives it (Node's createPublicKey(privateKey) path).
func (rt *Runtime) opKeyExport(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("key_export: (key, wantPrivate, format, type) required")
	}
	raw, err := valueBytes(args[0])
	if err != nil {
		return nil, err
	}
	wantPrivate := args[1].Bool()
	format := strArg(args[2])
	typ := strArg(args[3])
	key, isPrivate, perr := parseAnyKey(raw)
	if perr != nil {
		return cryptoErr(perr.Error()), nil
	}
	if format == "jwk" {
		target := key
		if !wantPrivate && isPrivate {
			target = publicKeyOf(key)
		}
		jwk, jerr := keyToJWK(target, wantPrivate)
		if jerr != nil {
			return cryptoErr(jerr.Error()), nil
		}
		return spidermonkey.ValueOf(jwk), nil
	}
	var der []byte
	var label string
	var merr error
	if wantPrivate {
		if !isPrivate {
			return cryptoErr("cannot export a private key from public key material"), nil
		}
		der, label, merr = marshalPrivateKeyDER(key, typ)
	} else {
		pub := key
		if isPrivate {
			if pub = publicKeyOf(key); pub == nil {
				return cryptoErr("cannot derive public key"), nil
			}
		}
		der, label, merr = marshalPublicKeyDER(pub, typ)
	}
	if merr != nil {
		return cryptoErr(merr.Error()), nil
	}
	if format == "der" {
		return rt.bytesReturn(der)
	}
	return spidermonkey.ValueOf(string(pem.EncodeToMemory(&pem.Block{Type: label, Bytes: der}))), nil
}

// keyEncodingOpts reads a publicKeyEncoding/privateKeyEncoding option object:
// present, requested DER structure ('pkcs1'/'pkcs8'/'sec1'/'spki') and format
// ('pem'/'der').
func keyEncodingOpts(opts *spidermonkey.Object, name string) (present bool, typ, format string) {
	v, _ := opts.Get(name)
	if v == nil || v.IsUndefined() {
		return false, "", ""
	}
	o := v.Object()
	if o == nil {
		return false, "", ""
	}
	defer o.Free()
	if tv, ok := optScalar(o, "type"); ok {
		typ = tv.String()
	}
	if fv, ok := optScalar(o, "format"); ok {
		format = fv.String()
	}
	return true, typ, format
}

// opGenerateKeyPair(type, opts) -> {keyType, publicKey, privateKey,
// publicIsDer, privateIsDer, hasPublicEncoding, hasPrivateEncoding}.
// publicKey/privateKey are PEM strings, or base64 DER when *IsDer (the JS
// side turns those into Buffers). Without an encoding the canonical
// SPKI/PKCS#8 PEM is returned and the JS side wraps it in a KeyObject.
func (rt *Runtime) opGenerateKeyPair(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("generateKeyPair: (type, options) required")
	}
	typ := strArg(args[0])
	opts := args[1].Object()
	var modulus int = 2048
	var curve string = "P-256"
	hasPubEnc, pubType, pubFormat := false, "", ""
	hasPrivEnc, privType, privFormat := false, "", ""
	if opts != nil {
		defer opts.Free()
		if v, ok := optScalar(opts, "modulusLength"); ok {
			modulus = v.Int()
		}
		if v, ok := optScalar(opts, "namedCurve"); ok {
			curve = v.String()
		}
		hasPubEnc, pubType, pubFormat = keyEncodingOpts(opts, "publicKeyEncoding")
		hasPrivEnc, privType, privFormat = keyEncodingOpts(opts, "privateKeyEncoding")
		// A caller who asks for an encrypted private key (cipher+passphrase) must
		// NOT silently receive a plaintext one — that would defeat encryption at
		// rest. PKCS#8 PBES2 output isn't produced here yet, so reject the request
		// explicitly instead of ignoring the options.
		if pke, _ := opts.Get("privateKeyEncoding"); pke != nil {
			if o := pke.Object(); o != nil {
				hasCipher := optPresent(o, "cipher")
				hasPass := optPresent(o, "passphrase")
				o.Free()
				if hasCipher || hasPass {
					return cryptoErr("encrypted private key output (cipher/passphrase) is not supported"), nil
				}
			}
		}
	}
	var priv, pub any
	switch typ {
	case "rsa":
		// Bound the modulus: an unchecked huge value makes rsa.GenerateKey spin
		// on billion-bit prime search, wedging the host. (Matches the subtle
		// side's 1024..8192 range.)
		if modulus < 1024 || modulus > 8192 {
			return cryptoErr("unsupported RSA modulus length"), nil
		}
		k, gerr := rsa.GenerateKey(rand.Reader, modulus)
		if gerr != nil {
			return cryptoErr(gerr.Error()), nil
		}
		priv, pub = k, &k.PublicKey
	case "ec":
		c, cerr := ecCurveByName(curve)
		if cerr != nil {
			return cryptoErr(cerr.Error()), nil
		}
		k, gerr := ecdsa.GenerateKey(c, rand.Reader)
		if gerr != nil {
			return cryptoErr(gerr.Error()), nil
		}
		priv, pub = k, &k.PublicKey
	case "ed25519":
		pk, sk, gerr := ed25519.GenerateKey(rand.Reader)
		if gerr != nil {
			return cryptoErr(gerr.Error()), nil
		}
		priv, pub = sk, pk
	case "x25519":
		k, gerr := ecdh.X25519().GenerateKey(rand.Reader)
		if gerr != nil {
			return cryptoErr(gerr.Error()), nil
		}
		priv, pub = k, k.PublicKey()
	default:
		return cryptoErr(fmt.Sprintf("unsupported key type %q", typ)), nil
	}

	pubDER, pubLabel, merr := marshalPublicKeyDER(pub, pubType)
	if merr != nil {
		return cryptoErr(merr.Error()), nil
	}
	privDER, privLabel, merr := marshalPrivateKeyDER(priv, privType)
	if merr != nil {
		return cryptoErr(merr.Error()), nil
	}
	out := map[string]any{
		"keyType":            typ,
		"hasPublicEncoding":  hasPubEnc,
		"hasPrivateEncoding": hasPrivEnc,
		"publicIsDer":        false,
		"privateIsDer":       false,
	}
	if hasPubEnc && pubFormat == "der" {
		out["publicKey"] = base64.StdEncoding.EncodeToString(pubDER)
		out["publicIsDer"] = true
	} else {
		out["publicKey"] = string(pem.EncodeToMemory(&pem.Block{Type: pubLabel, Bytes: pubDER}))
	}
	if hasPrivEnc && privFormat == "der" {
		out["privateKey"] = base64.StdEncoding.EncodeToString(privDER)
		out["privateIsDer"] = true
	} else {
		out["privateKey"] = string(pem.EncodeToMemory(&pem.Block{Type: privLabel, Bytes: privDER}))
	}
	return spidermonkey.ValueOf(out), nil
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

// ---- JWK export (KeyObject.export({format:'jwk'})) ----

func jwkB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwkBigFixed left-pads n to size bytes (EC coordinates are fixed-width).
func jwkBigFixed(n *big.Int, size int) string {
	b := n.Bytes()
	if size > len(b) {
		b = append(make([]byte, size-len(b)), b...)
	}
	return jwkB64(b)
}

func jwkCurveName(c elliptic.Curve) (string, error) {
	switch c {
	case elliptic.P256():
		return "P-256", nil
	case elliptic.P384():
		return "P-384", nil
	case elliptic.P521():
		return "P-521", nil
	}
	return "", fmt.Errorf("unsupported EC curve for JWK")
}

// keyToJWK builds a Node-shaped JWK map from a parsed key. includePrivate adds
// the private components (RSA d/p/q/dp/dq/qi, EC/OKP d).
func keyToJWK(key any, includePrivate bool) (map[string]any, error) {
	switch k := key.(type) {
	case *rsa.PublicKey:
		return map[string]any{"kty": "RSA", "n": jwkB64(k.N.Bytes()), "e": jwkB64(big.NewInt(int64(k.E)).Bytes())}, nil
	case *rsa.PrivateKey:
		m := map[string]any{"kty": "RSA", "n": jwkB64(k.N.Bytes()), "e": jwkB64(big.NewInt(int64(k.E)).Bytes())}
		if includePrivate {
			k.Precompute()
			m["d"], m["p"], m["q"] = jwkB64(k.D.Bytes()), jwkB64(k.Primes[0].Bytes()), jwkB64(k.Primes[1].Bytes())
			m["dp"], m["dq"], m["qi"] = jwkB64(k.Precomputed.Dp.Bytes()), jwkB64(k.Precomputed.Dq.Bytes()), jwkB64(k.Precomputed.Qinv.Bytes())
		}
		return m, nil
	case *ecdsa.PublicKey:
		crv, err := jwkCurveName(k.Curve)
		if err != nil {
			return nil, err
		}
		size := (k.Curve.Params().BitSize + 7) / 8
		return map[string]any{"kty": "EC", "crv": crv, "x": jwkBigFixed(k.X, size), "y": jwkBigFixed(k.Y, size)}, nil
	case *ecdsa.PrivateKey:
		crv, err := jwkCurveName(k.Curve)
		if err != nil {
			return nil, err
		}
		size := (k.Curve.Params().BitSize + 7) / 8
		m := map[string]any{"kty": "EC", "crv": crv, "x": jwkBigFixed(k.X, size), "y": jwkBigFixed(k.Y, size)}
		if includePrivate {
			m["d"] = jwkBigFixed(k.D, size)
		}
		return m, nil
	case ed25519.PublicKey:
		return map[string]any{"kty": "OKP", "crv": "Ed25519", "x": jwkB64(k)}, nil
	case ed25519.PrivateKey:
		m := map[string]any{"kty": "OKP", "crv": "Ed25519", "x": jwkB64(k.Public().(ed25519.PublicKey))}
		if includePrivate {
			m["d"] = jwkB64(k.Seed())
		}
		return m, nil
	case *ecdh.PublicKey:
		return map[string]any{"kty": "OKP", "crv": "X25519", "x": jwkB64(k.Bytes())}, nil
	case *ecdh.PrivateKey:
		m := map[string]any{"kty": "OKP", "crv": "X25519", "x": jwkB64(k.PublicKey().Bytes())}
		if includePrivate {
			m["d"] = jwkB64(k.Bytes())
		}
		return m, nil
	}
	return nil, fmt.Errorf("unsupported key type for JWK export")
}

// ---- ECDH (createECDH) and the X25519 one-shot (crypto.diffieHellman) ----

func ecdhCurveNode(name string) (ecdh.Curve, error) {
	switch name {
	case "P-256", "prime256v1", "secp256r1":
		return ecdh.P256(), nil
	case "P-384", "secp384r1":
		return ecdh.P384(), nil
	case "P-521", "secp521r1":
		return ecdh.P521(), nil
	}
	return nil, fmt.Errorf("unsupported ECDH curve %q", name)
}

func (rt *Runtime) opECDHGenerate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("ecdh_generate: curve required")
	}
	c, err := ecdhCurveNode(strArg(args[0]))
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	k, err := c.GenerateKey(rand.Reader)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	return spidermonkey.ValueOf(map[string]any{
		"priv": hex.EncodeToString(k.Bytes()),
		"pub":  hex.EncodeToString(k.PublicKey().Bytes()),
	}), nil
}

func (rt *Runtime) opECDHCompute(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("ecdh_compute: (curve, priv, otherPub) required")
	}
	c, err := ecdhCurveNode(strArg(args[0]))
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	privRaw, err := hex.DecodeString(args[1].String())
	if err != nil {
		return cryptoErr("bad private key hex"), nil
	}
	pubRaw, err := hex.DecodeString(args[2].String())
	if err != nil {
		return cryptoErr("bad public key hex"), nil
	}
	priv, err := c.NewPrivateKey(privRaw)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	pub, err := c.NewPublicKey(pubRaw)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	secret, err := priv.ECDH(pub)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	return spidermonkey.ValueOf(hex.EncodeToString(secret)), nil
}

// opDHKeyObject computes the X25519 (or NIST-ECDH) shared secret from two
// KeyObjects' PEM material — crypto.diffieHellman({privateKey, publicKey}).
func (rt *Runtime) opDHKeyObject(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("dh_keyobject: (privatePem, publicPem) required")
	}
	privPEM, err := valueBytes(args[0])
	if err != nil {
		return nil, err
	}
	pubPEM, err := valueBytes(args[1])
	if err != nil {
		return nil, err
	}
	pk, err := parsePrivateKey(privPEM)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	xpriv, ok := pk.(*ecdh.PrivateKey)
	if !ok {
		return cryptoErr("private key is not an X25519/ECDH key"), nil
	}
	pubAny, err := parsePublicKey(pubPEM)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	xpub, ok := pubAny.(*ecdh.PublicKey)
	if !ok {
		return cryptoErr("public key is not an X25519/ECDH key"), nil
	}
	secret, err := xpriv.ECDH(xpub)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	return rt.bytesReturn(secret)
}

// ------------------------- the remaining node:crypto surface — RSA public/private
// encryption (OAEP + PKCS1), Diffie-Hellman, ChaCha20-Poly1305, and X.509
// certificate parsing.

// maxDHModulusBits bounds any Diffie-Hellman modulus (generated or caller-
// supplied). It comfortably covers standard MODP groups up to 8192 bits while
// rejecting the pathologically large primes that would turn a single modexp
// into a multi-minute, uninterruptible CPU hog on the shared host process.
const maxDHModulusBits = 8192

func (rt *Runtime) crypto3Ops() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"crypto_rsa_public":          rt.opRSAPublicEncrypt,
		"crypto_rsa_private":         rt.opRSAPrivateDecrypt,
		"crypto_rsa_private_encrypt": rt.opRSAPrivateEncrypt,
		"crypto_rsa_public_decrypt":  rt.opRSAPublicDecrypt,
		"crypto_dh_generate":         rt.opDHGenerate,
		"crypto_dh_compute":          rt.opDHCompute,
		"crypto_chacha":              rt.opChaCha,
		"crypto_x509":                rt.opX509Parse,
	}
}

func oaepHash(name string) (crypto.Hash, error) {
	switch name {
	case "sha1", "":
		return crypto.SHA1, nil
	case "sha256":
		return crypto.SHA256, nil
	}
	return 0, fmt.Errorf("unsupported OAEP hash %q", name)
}

// oaepHashArg picks the OAEP hash from args[idx] (defaulting to SHA-1 when the
// argument is absent or undefined), matching Node's oaepHash option.
func oaepHashArg(args []spidermonkey.Value, idx int) (crypto.Hash, error) {
	if len(args) > idx && !args[idx].IsUndefined() {
		return oaepHash(args[idx].String())
	}
	return oaepHash("sha1")
}

// opRSAPublicEncrypt(keyPEM, data, padding, oaepHash) -> ciphertext.
// padding: "oaep" | "pkcs1".
func (rt *Runtime) opRSAPublicEncrypt(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("rsa_public: (keyPEM, data, padding, hash?) required")
	}
	keyPEM, _ := valueBytes(args[0])
	data, _ := valueBytes(args[1])
	padding := args[2].String()
	pub, err := parseAnyRSAPublic(keyPEM)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	switch padding {
	case "oaep":
		h, herr := oaepHashArg(args, 3)
		if herr != nil {
			return cryptoErr(herr.Error()), nil
		}
		ct, e := rsa.EncryptOAEP(h.New(), rand.Reader, pub, data, nil)
		if e != nil {
			return cryptoErr(e.Error()), nil
		}
		return rt.bytesReturn(ct)
	case "pkcs1":
		ct, e := rsa.EncryptPKCS1v15(rand.Reader, pub, data)
		if e != nil {
			return cryptoErr(e.Error()), nil
		}
		return rt.bytesReturn(ct)
	}
	return cryptoErr("unsupported padding"), nil
}

func (rt *Runtime) opRSAPrivateDecrypt(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("rsa_private: (keyPEM, data, padding, hash?) required")
	}
	keyPEM, _ := valueBytes(args[0])
	data, _ := valueBytes(args[1])
	padding := args[2].String()
	key, err := parsePrivateKey(keyPEM)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	priv, ok := key.(*rsa.PrivateKey)
	if !ok {
		return cryptoErr("not an RSA private key"), nil
	}
	if err := checkRSAModulus(priv.N); err != nil {
		return cryptoErr(err.Error()), nil
	}
	switch padding {
	case "oaep":
		h, herr := oaepHashArg(args, 3)
		if herr != nil {
			return cryptoErr(herr.Error()), nil
		}
		pt, e := rsa.DecryptOAEP(h.New(), rand.Reader, priv, data, nil)
		if e != nil {
			return cryptoErr(e.Error()), nil
		}
		return rt.bytesReturn(pt)
	case "pkcs1":
		pt, e := rsa.DecryptPKCS1v15(rand.Reader, priv, data)
		if e != nil {
			return cryptoErr(e.Error()), nil
		}
		return rt.bytesReturn(pt)
	}
	return cryptoErr("unsupported padding"), nil
}

// opRSAPrivateEncrypt(keyPEM, data) -> RSA private-key operation with PKCS#1
// type-1 padding (Node's crypto.privateEncrypt, RSA_PKCS1_PADDING). SignPKCS1v15
// with hash 0 signs the data directly with type-1 padding and no DigestInfo,
// which is exactly the private encrypt primitive.
func (rt *Runtime) opRSAPrivateEncrypt(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("rsa_private_encrypt: (keyPEM, data) required")
	}
	keyPEM, _ := valueBytes(args[0])
	data, _ := valueBytes(args[1])
	key, err := parsePrivateKey(keyPEM)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	priv, ok := key.(*rsa.PrivateKey)
	if !ok {
		return cryptoErr("not an RSA private key"), nil
	}
	if err := checkRSAModulus(priv.N); err != nil {
		return cryptoErr(err.Error()), nil
	}
	out, e := rsa.SignPKCS1v15(rand.Reader, priv, crypto.Hash(0), data)
	if e != nil {
		return cryptoErr(e.Error()), nil
	}
	return rt.bytesReturn(out)
}

// opRSAPublicDecrypt(keyPEM, data) -> recover the message from a
// privateEncrypt (Node's crypto.publicDecrypt): the raw public-key operation
// c^e mod n, then strip PKCS#1 type-1 padding (00 01 FF..FF 00 M).
func (rt *Runtime) opRSAPublicDecrypt(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("rsa_public_decrypt: (keyPEM, data) required")
	}
	keyPEM, _ := valueBytes(args[0])
	data, _ := valueBytes(args[1])
	pub, err := parseAnyRSAPublic(keyPEM)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	k := (pub.N.BitLen() + 7) / 8
	c := new(big.Int).SetBytes(data)
	if c.Cmp(pub.N) >= 0 {
		return cryptoErr("decryption error"), nil
	}
	em := new(big.Int).Exp(c, big.NewInt(int64(pub.E)), pub.N).FillBytes(make([]byte, k))
	// PKCS#1 type-1: 0x00 0x01 (0xFF)+ 0x00 M, with at least 8 padding bytes.
	if len(em) < 11 || em[0] != 0x00 || em[1] != 0x01 {
		return cryptoErr("decryption error"), nil
	}
	i := 2
	for i < len(em) && em[i] == 0xFF {
		i++
	}
	if i < 10 || i >= len(em) || em[i] != 0x00 {
		return cryptoErr("decryption error"), nil
	}
	return rt.bytesReturn(em[i+1:])
}

// maxRSAModulusBits bounds any caller-supplied RSA modulus. Every RSA operation
// is an uninterruptible big-integer modexp on the shared host, so an oversized
// attacker-chosen modulus would pin a core for minutes; 8192 covers every
// legitimate key. checkRSAModulus enforces it before any modexp runs.
const maxRSAModulusBits = 8192

func checkRSAModulus(n *big.Int) error {
	if n == nil || n.BitLen() > maxRSAModulusBits {
		return fmt.Errorf("RSA modulus too large (max %d bits)", maxRSAModulusBits)
	}
	return nil
}

func parseAnyRSAPublic(pemBytes []byte) (*rsa.PublicKey, error) {
	pub, err := parseAnyRSAPublicUncapped(pemBytes)
	if err != nil {
		return nil, err
	}
	if err := checkRSAModulus(pub.N); err != nil {
		return nil, err
	}
	return pub, nil
}

func parseAnyRSAPublicUncapped(pemBytes []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	if k, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PublicKey); ok {
			return rk, nil
		}
	}
	if k, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		return k, nil
	}
	// A private-key PEM: derive its public part (Node allows publicEncrypt
	// with a private key).
	if pk, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := pk.(*rsa.PrivateKey); ok {
			return &rk.PublicKey, nil
		}
	}
	if pk, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return &pk.PublicKey, nil
	}
	return nil, fmt.Errorf("could not parse RSA public key")
}

// opDHGenerate(primeLen|primeHex, generator) -> {prime, generator, priv, pub}
// (all hex). Modular-exponentiation DH over a fresh safe-ish prime.
func (rt *Runtime) opDHGenerate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("dh_generate: (bits|primeHex, generator?) required")
	}
	var prime *big.Int
	if args[0].IsObject() || len(args[0].String()) > 12 {
		// primeHex provided
		p, ok := new(big.Int).SetString(args[0].String(), 16)
		if !ok {
			return cryptoErr("bad prime hex"), nil
		}
		// A caller-supplied prime bypasses the numeric-bits cap below, so bound it
		// here too: g^priv mod p is an uninterruptible host modexp, and a huge p
		// (hundreds of kbits) would pin the shared host process for minutes.
		if p.BitLen() < 512 || p.BitLen() > maxDHModulusBits {
			return cryptoErr("unsupported DH modulus"), nil
		}
		prime = p
	} else {
		bits := intArg(args[0])
		if bits < 512 || bits > 4096 {
			return cryptoErr("unsupported DH modulus"), nil
		}
		// A SAFE prime p = 2q+1 (q prime) with generator 2 generates a
		// large-order subgroup, so the shared secret cannot collapse into a
		// small subgroup — what Node's createDiffieHellman produces. A plain
		// random prime can have smooth order and leak the secret.
		p, err := generateSafePrime(bits)
		if err != nil {
			return cryptoErr(err.Error()), nil
		}
		prime = p
	}
	g := big.NewInt(2)
	if len(args) > 1 && !args[1].IsUndefined() {
		if gv, ok := new(big.Int).SetString(args[1].String(), 16); ok {
			g = gv
		}
	}
	priv, err := rand.Int(rand.Reader, new(big.Int).Sub(prime, big.NewInt(2)))
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	priv.Add(priv, big.NewInt(1))
	pub := new(big.Int).Exp(g, priv, prime)
	// Even-length hex (byte-aligned) so Buffer.from(hex) round-trips without
	// dropping a trailing nibble.
	return spidermonkey.ValueOf(map[string]any{
		"prime": evenHex(prime), "generator": evenHex(g),
		"priv": evenHex(priv), "pub": evenHex(pub),
	}), nil
}

func evenHex(n *big.Int) string { return hex.EncodeToString(n.Bytes()) }

// generateSafePrime returns a prime p of the requested bit length such that
// (p-1)/2 is also prime (a "safe prime"), so a generator of 2 spans a
// large-order subgroup. It samples a prime q of bits-1 length and tests
// p = 2q+1, which is the standard construction.
func generateSafePrime(bits int) (*big.Int, error) {
	one := big.NewInt(1)
	for {
		q, err := rand.Prime(rand.Reader, bits-1)
		if err != nil {
			return nil, err
		}
		p := new(big.Int).Lsh(q, 1) // 2q
		p.Add(p, one)               // 2q+1
		if p.BitLen() == bits && p.ProbablyPrime(20) {
			return p, nil
		}
	}
}

// opDHCompute(primeHex, privHex, otherPubHex) -> secret hex.
func (rt *Runtime) opDHCompute(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("dh_compute: (prime, priv, otherPub) required")
	}
	prime, ok1 := new(big.Int).SetString(args[0].String(), 16)
	priv, ok2 := new(big.Int).SetString(args[1].String(), 16)
	otherPub, ok3 := new(big.Int).SetString(args[2].String(), 16)
	if !ok1 || !ok2 || !ok3 {
		return cryptoErr("bad DH hex value"), nil
	}
	// Bound the modulus: otherPub^priv mod p is an uninterruptible host modexp, so
	// an oversized prime would pin the shared host process for minutes.
	if prime.BitLen() < 512 || prime.BitLen() > maxDHModulusBits {
		return cryptoErr("unsupported DH modulus"), nil
	}
	// Reject a peer public key outside (1, p-1): the endpoints 0, 1 and p-1
	// force the shared secret to a known constant (a small-subgroup / invalid
	// key confinement attack).
	one := big.NewInt(1)
	pMinus1 := new(big.Int).Sub(prime, one)
	if otherPub.Cmp(one) <= 0 || otherPub.Cmp(pMinus1) >= 0 {
		return cryptoErr("invalid DH public key"), nil
	}
	secret := new(big.Int).Exp(otherPub, priv, prime)
	return spidermonkey.ValueOf(evenHex(secret)), nil
}

// opChaCha(encrypt, key, nonce, data, aad) -> {data, tag} | plaintext.
func (rt *Runtime) opChaCha(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("chacha: (encrypt, key, nonce, data, aad?) required")
	}
	encrypt := args[0].Bool()
	key, _ := valueBytes(args[1])
	nonce, _ := valueBytes(args[2])
	data, _ := valueBytes(args[3])
	var aad, tag []byte
	if len(args) > 4 {
		aad, _ = valueBytes(args[4])
	}
	if len(args) > 5 {
		tag, _ = valueBytes(args[5])
	}
	if len(nonce) != chacha20poly1305.NonceSize {
		return cryptoErr("Invalid IV length"), nil
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	if encrypt {
		sealed := aead.Seal(nil, nonce, data, aad)
		return rt.cipherResult(sealed[:len(sealed)-16], sealed[len(sealed)-16:])
	}
	pt, err := aead.Open(nil, nonce, append(append([]byte{}, data...), tag...), aad)
	if err != nil {
		return cryptoErr("unable to authenticate data"), nil
	}
	return rt.cipherResult(pt, nil)
}

// opX509Parse(pem) -> {subject, issuer, validFrom, validTo, serialNumber,
// fingerprint256, publicKeyPEM} | error.
func (rt *Runtime) opX509Parse(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("x509: pem required")
	}
	raw, _ := valueBytes(args[0])
	block, _ := pem.Decode(raw)
	der := raw
	if block != nil {
		der = block.Bytes
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return cryptoErr(err.Error()), nil
	}
	fp := sha256.Sum256(cert.Raw)
	fpHex := ""
	for i, b := range fp {
		if i > 0 {
			fpHex += ":"
		}
		fpHex += fmt.Sprintf("%02X", b)
	}
	pubDER, _ := x509.MarshalPKIXPublicKey(cert.PublicKey)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	return spidermonkey.ValueOf(map[string]any{
		"subject":        cert.Subject.String(),
		"issuer":         cert.Issuer.String(),
		"validFrom":      cert.NotBefore.UTC().Format("Jan _2 15:04:05 2006 MST"),
		"validTo":        cert.NotAfter.UTC().Format("Jan _2 15:04:05 2006 MST"),
		"serialNumber":   cert.SerialNumber.Text(16),
		"fingerprint256": fpHex,
		"publicKey":      string(pubPEM),
		"ca":             cert.IsCA,
	}), nil
}
