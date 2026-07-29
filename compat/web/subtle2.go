package web

// subtle2.go: the encryption half of crypto.subtle — AES-GCM/CBC/CTR
// encrypt/decrypt and AES key material, plus ECDH/HKDF/PBKDF2 deriveBits.
// This lifts the surface from the JWS-only set to JWE-capable.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/subtle"
	"fmt"
	"hash"
	"math/big"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/pbkdf2"
)

// maxSubtleKDFBytes bounds a guest-requested derived-key length so it can't
// drive an unbounded host allocation (a fatal Go OOM). maxSubtlePBKDF2Iter
// bounds the iteration count (uninterruptible host CPU guard).
const (
	maxSubtleKDFBytes   = 1 << 24 // 16 MiB
	maxSubtlePBKDF2Iter = 100_000_000
)

// hashNewByName reuses hashByName's crypto.Hash lookup (the hashes are
// registered by subtle.go's blank imports) and returns its constructor.
func hashNewByName(name string) (func() hash.Hash, error) {
	h, err := hashByName(name)
	if err != nil {
		return nil, err
	}
	return h.New, nil
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
		tagBytes = intArg(args[5]) / 8
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return subtleErr(err.Error()), nil
	}
	switch mode {
	case "AES-GCM":
		// WebCrypto permits ANY non-empty IV length and any of the seven tag
		// lengths. Go's stdlib offers each independently — NewGCMWithNonceSize
		// fixes the tag at 128 bits, NewGCMWithTagSize fixes the nonce at 96 —
		// and there is no constructor for both, which is why every combination
		// off the default pair used to be refused outright.
		//
		// They compose without a bespoke GCM: a short tag IS the full tag
		// truncated, and the ciphertext does not depend on the tag length at
		// all. So the AEAD is always built for the caller's nonce length, and
		// the tag is cut to size on the way out and checked by recomputation on
		// the way in.
		if len(iv) == 0 {
			return subtleErr("OperationError: AES-GCM IV must not be empty"), nil
		}
		switch tagBytes {
		case 4, 8, 12, 13, 14, 15, 16:
		default:
			return subtleErr("OperationError: AES-GCM tagLength must be 32, 64, 96, 104, 112, 120 or 128 bits"), nil
		}
		gcm, err := cipher.NewGCMWithNonceSize(block, len(iv))
		if err != nil {
			return subtleErr("OperationError: " + err.Error()), nil
		}
		if encrypt {
			sealed := gcm.Seal(nil, iv, data, aad)
			// sealed is ciphertext||tag16; keep only the requested tag bytes.
			return bytesValue(sealed[:len(data)+tagBytes]), nil
		}
		if len(data) < tagBytes {
			return subtleErr("OperationError: decryption failed"), nil
		}
		pt, ok := gcmOpenTruncated(gcm, iv, data, aad, tagBytes)
		if !ok {
			return subtleErr("OperationError: decryption failed"), nil
		}
		return bytesValue(pt), nil
	case "AES-CBC":
		bs := block.BlockSize()
		if len(iv) != bs {
			return subtleErr("OperationError: AES-CBC IV must be 16 bytes"), nil
		}
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
		if len(iv) != block.BlockSize() {
			return subtleErr("OperationError: AES-CTR counter block must be 16 bytes"), nil
		}
		ctrBits := 128
		if len(args) > 6 && !args[6].IsUndefined() {
			ctrBits = intArg(args[6])
		}
		if ctrBits <= 0 || ctrBits > 128 {
			return subtleErr("OperationError: AES-CTR length must be in 1..128"), nil
		}
		return bytesValue(aesCTR(block, iv, ctrBits, data)), nil
	}
	return subtleErr(fmt.Sprintf("unsupported AES mode %q", mode)), nil
}

// aesCTR runs AES in counter mode where only the low ctrBits of the 16-byte
// block form the counter (the WebCrypto AesCtrParams.length); the high bits are
// a fixed nonce. For the full 128-bit counter it delegates to the stdlib.
func aesCTR(block cipher.Block, counter []byte, ctrBits int, data []byte) []byte {
	if ctrBits >= 128 {
		out := make([]byte, len(data))
		cipher.NewCTR(block, counter).XORKeyStream(out, data)
		return out
	}
	bs := block.BlockSize()
	ctr := make([]byte, bs)
	copy(ctr, counter)
	ks := make([]byte, bs)
	out := make([]byte, len(data))
	for off := 0; off < len(data); off += bs {
		block.Encrypt(ks, ctr)
		n := len(data) - off
		if n > bs {
			n = bs
		}
		for i := 0; i < n; i++ {
			out[off+i] = data[off+i] ^ ks[i]
		}
		incrCounterBits(ctr, ctrBits) // increment only the low ctrBits, wrapping within them
	}
	return out
}

// incrCounterBits adds 1 to the low `bits` (big-endian, LSB-first) of ctr,
// wrapping within them and leaving the higher nonce bits untouched.
func incrCounterBits(ctr []byte, bits int) {
	for bit := 0; bit < bits; bit++ {
		idx := len(ctr) - 1 - bit/8
		mask := byte(1 << (uint(bit) % 8))
		ctr[idx] ^= mask
		if ctr[idx]&mask != 0 {
			return // 0 -> 1: no carry
		}
	}
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

// opECDHDerive(privHandle, pubHandle, bits) -> shared secret bytes.
//
// It takes handles rather than JWK because generateKey produces handles: the
// JWK-only form meant a generated ECDH key pair — the common case — could not
// derive anything at all.
func (s *subtleAPI) opECDHDerive(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("ecdh: (priv, pub, bits) required")
	}
	privKey, perr := s.get(args[0])
	pubKey, uerr := s.get(args[1])
	if perr != nil || uerr != nil || privKey.ecPriv == nil || pubKey.ecPub == nil {
		return subtleErr("InvalidAccessError: ECDH needs a private and a public EC key"), nil
	}
	if privKey.ecPriv.Curve != pubKey.ecPub.Curve {
		return subtleErr("InvalidAccessError: ECDH keys are on different curves"), nil
	}
	priv, err := privKey.ecPriv.ECDH()
	if err != nil {
		return subtleErr("InvalidAccessError: " + err.Error()), nil
	}
	pub, err := pubKey.ecPub.ECDH()
	if err != nil {
		return subtleErr("InvalidAccessError: " + err.Error()), nil
	}
	secret, err := priv.ECDH(pub)
	if err != nil {
		return subtleErr(err.Error()), nil
	}
	// A requested length longer than the shared secret is an OperationError in
	// WebCrypto, not a silently-shortened (weaker) key.
	bits := intArg(args[2])
	if bits > 0 {
		want := bits / 8
		if want > len(secret) {
			return subtleErr("OperationError: requested length exceeds the ECDH shared secret"), nil
		}
		secret = secret[:want]
	}
	return bytesValue(secret), nil
}

// ecdsaFromECDHPublic converts a crypto/ecdh point back to the ecdsa form the
// host key table stores, so raw and JWK imports produce the same kind of key.
func ecdsaFromECDHPublic(pt *ecdh.PublicKey, crv string) (*ecdsa.PublicKey, error) {
	b := pt.Bytes()
	if len(b) == 0 || b[0] != 4 {
		return nil, fmt.Errorf("expected an uncompressed EC point")
	}
	curve, err := curveByName(crv)
	if err != nil {
		return nil, err
	}
	half := (len(b) - 1) / 2
	return &ecdsa.PublicKey{
		Curve: curve,
		X:     new(big.Int).SetBytes(b[1 : 1+half]),
		Y:     new(big.Int).SetBytes(b[1+half:]),
	}, nil
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
	length := intArg(args[4]) / 8
	if length < 0 || length > maxSubtleKDFBytes {
		return subtleErr("OperationError: invalid derived-bits length"), nil
	}
	if length == 0 {
		return bytesValue(nil), nil
	}
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
	iter := intArg(args[3])
	// iterations < 1 would silently degrade to a one-round KDF (no stretching);
	// WebCrypto requires iterations >= 1.
	if iter < 1 || iter > maxSubtlePBKDF2Iter {
		return subtleErr("OperationError: PBKDF2 iterations out of range"), nil
	}
	length := intArg(args[4]) / 8
	if length < 0 || length > maxSubtleKDFBytes {
		return subtleErr("OperationError: invalid derived-bits length"), nil
	}
	// A zero-length derivation is a legal request for an empty key; Go's
	// pbkdf2 does not accept a zero key length, so answer it directly.
	if length == 0 {
		return bytesValue(nil), nil
	}
	return bytesValue(pbkdf2.Key(pw, salt, iter, length, newHash)), nil
}

func subtleErr(msg string) spidermonkey.Value {
	return spidermonkey.ValueOf(map[string]any{"__subtleError": true, "message": msg})
}

// opRSAOAEP(encrypt, keyHandle, hash, data, label) -> bytes. Uses the RSA key
// table from subtle.go (opRSAImport* store handles).
func (s *subtleAPI) opRSAOAEP(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("rsa-oaep: (encrypt, keyHandle, hash, data, label?) required")
	}
	encrypt := args[0].Bool()
	k, err := s.get(args[1])
	if err != nil {
		return subtleErr(err.Error()), nil
	}
	newHash, err := hashNewByName(args[2].String())
	if err != nil {
		return subtleErr(err.Error()), nil
	}
	data, err := argBytes(args[3])
	if err != nil {
		return nil, err
	}
	var label []byte
	if len(args) > 4 {
		label, _ = argBytes(args[4])
	}
	if encrypt {
		pub := k.rsaPub
		if pub == nil && k.rsaPriv != nil {
			pub = &k.rsaPriv.PublicKey
		}
		if pub == nil {
			return subtleErr("not an RSA key"), nil
		}
		ct, e := rsa.EncryptOAEP(newHash(), rand.Reader, pub, data, label)
		if e != nil {
			return subtleErr(e.Error()), nil
		}
		return bytesValue(ct), nil
	}
	if k.rsaPriv == nil {
		return subtleErr("decrypt needs an RSA private key"), nil
	}
	pt, e := rsa.DecryptOAEP(newHash(), rand.Reader, k.rsaPriv, data, label)
	if e != nil {
		return subtleErr("OperationError: decryption failed"), nil
	}
	return bytesValue(pt), nil
}

// -------------------------------------------------------------- AES-KW (RFC 3394)

var aesKWIV = []byte{0xA6, 0xA6, 0xA6, 0xA6, 0xA6, 0xA6, 0xA6, 0xA6}

// opAESKW wraps (encrypt=true) or unwraps key material with AES Key Wrap.
// args: (encrypt, kek, data).
func (s *subtleAPI) opAESKW(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("aes-kw: (encrypt, key, data) required")
	}
	kek, err := argBytes(args[1])
	if err != nil {
		return nil, err
	}
	data, err := argBytes(args[2])
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return subtleErr("OperationError: " + err.Error()), nil
	}
	wrap := aesKWWrap
	if !args[0].Bool() {
		wrap = aesKWUnwrap
	}
	out, err := wrap(block, data)
	if err != nil {
		return subtleErr("OperationError: " + err.Error()), nil
	}
	return bytesValue(out), nil
}

// aesKWXorT xors the 64-bit round counter t into the 8-byte big-endian A.
func aesKWXorT(a []byte, t uint64) {
	for k := 0; k < 8; k++ {
		a[7-k] ^= byte(t >> (8 * k))
	}
}

func aesKWWrap(block cipher.Block, plain []byte) ([]byte, error) {
	if len(plain) < 16 || len(plain)%8 != 0 {
		return nil, fmt.Errorf("AES-KW input must be a multiple of 8 bytes, at least 16")
	}
	n := len(plain) / 8
	a := append([]byte(nil), aesKWIV...)
	r := append([]byte(nil), plain...)
	var buf [16]byte
	for j := 0; j < 6; j++ {
		for i := 0; i < n; i++ {
			copy(buf[:8], a)
			copy(buf[8:], r[i*8:])
			block.Encrypt(buf[:], buf[:])
			copy(a, buf[:8])
			aesKWXorT(a, uint64(n*j+i+1))
			copy(r[i*8:i*8+8], buf[8:])
		}
	}
	return append(a, r...), nil
}

func aesKWUnwrap(block cipher.Block, ct []byte) ([]byte, error) {
	if len(ct) < 24 || len(ct)%8 != 0 {
		return nil, fmt.Errorf("AES-KW ciphertext must be a multiple of 8 bytes, at least 24")
	}
	n := len(ct)/8 - 1
	a := append([]byte(nil), ct[:8]...)
	r := append([]byte(nil), ct[8:]...)
	var buf [16]byte
	for j := 5; j >= 0; j-- {
		for i := n - 1; i >= 0; i-- {
			aesKWXorT(a, uint64(n*j+i+1))
			copy(buf[:8], a)
			copy(buf[8:], r[i*8:])
			block.Decrypt(buf[:], buf[:])
			copy(a, buf[:8])
			copy(r[i*8:i*8+8], buf[8:])
		}
	}
	if subtle.ConstantTimeCompare(a, aesKWIV) != 1 {
		return nil, fmt.Errorf("integrity check failed")
	}
	return r, nil
}

func (s *subtleAPI) ops2() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"subtle_aes_encrypt": s.opAESEncrypt,
		"subtle_aes_decrypt": s.opAESDecrypt,
		"subtle_aes_kw":      s.opAESKW,
		"subtle_ecdh":        s.opECDHDerive,

		"subtle_mlkem_generate":    s.opMLKEMGenerate,
		"subtle_mlkem_import":      s.opMLKEMImport,
		"subtle_mlkem_export":      s.opMLKEMExport,
		"subtle_mlkem_encapsulate": s.opMLKEMEncapsulate,
		"subtle_mlkem_decapsulate": s.opMLKEMDecapsulate,
		"subtle_hkdf":              s.opHKDFDerive,
		"subtle_pbkdf2":            s.opPBKDF2Derive,
		"subtle_rsa_oaep":          s.opRSAOAEP,
		"subtle_chacha":            s.opChaChaSeal,
		"subtle_kmac":              s.opKMAC,
		"subtle_aes_ocb":           s.opAESOCB,
		"subtle_cshake":            s.opCShake,
		"subtle_ed448_generate":    s.opEd448Generate,
		"subtle_ed448_import":      s.opEd448Import,
		"subtle_ed448_export":      s.opEd448Export,
		"subtle_ed448_sign":        s.opEd448Sign,
		"subtle_ed448_verify":      s.opEd448Verify,
		"subtle_x448_generate":     s.opX448Generate,
		"subtle_x448_import":       s.opX448Import,
		"subtle_x448_export":       s.opX448Export,
		"subtle_x448_derive":       s.opX448Derive,
	}
}

// gcmOpenTruncated verifies and decrypts a GCM message whose tag was cut to
// tagBytes. The tag length changes nothing about the ciphertext, so the work is
// to recover the plaintext and then recompute what the full tag would have
// been.
//
// The keystream comes out of the AEAD itself: sealing a run of zeros yields the
// keystream, because 0 XOR ks is ks. XOR it over the ciphertext for the
// plaintext, seal that for real, and compare — both the ciphertext (which must
// match, or the message was altered) and the leading tagBytes of the tag.
func gcmOpenTruncated(gcm cipher.AEAD, iv, data, aad []byte, tagBytes int) ([]byte, bool) {
	ct := data[:len(data)-tagBytes]
	given := data[len(data)-tagBytes:]
	if tagBytes == gcm.Overhead() {
		pt, err := gcm.Open(nil, iv, data, aad)
		return pt, err == nil
	}
	keystream := gcm.Seal(nil, iv, make([]byte, len(ct)), nil)[:len(ct)]
	pt := make([]byte, len(ct))
	for i := range ct {
		pt[i] = ct[i] ^ keystream[i]
	}
	sealed := gcm.Seal(nil, iv, pt, aad)
	if subtle.ConstantTimeCompare(sealed[:len(ct)], ct) != 1 {
		return nil, false
	}
	if subtle.ConstantTimeCompare(sealed[len(ct):len(ct)+tagBytes], given) != 1 {
		return nil, false
	}
	return pt, true
}
