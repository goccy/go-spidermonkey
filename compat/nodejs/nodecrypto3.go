package nodejs

// nodecrypto3.go: the remaining node:crypto surface — RSA public/private
// encryption (OAEP + PKCS1), Diffie-Hellman, ChaCha20-Poly1305, and X.509
// certificate parsing.

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"golang.org/x/crypto/chacha20poly1305"
)

func (rt *Runtime) crypto3Ops() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"crypto_rsa_public":  rt.opRSAPublicEncrypt,
		"crypto_rsa_private": rt.opRSAPrivateDecrypt,
		"crypto_dh_generate": rt.opDHGenerate,
		"crypto_dh_compute":  rt.opDHCompute,
		"crypto_chacha":      rt.opChaCha,
		"crypto_x509":        rt.opX509Parse,
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
		h, herr := oaepHash("sha1")
		if len(args) > 3 && !args[3].IsUndefined() {
			h, herr = oaepHash(args[3].String())
		}
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
	switch padding {
	case "oaep":
		h, herr := oaepHash("sha1")
		if len(args) > 3 && !args[3].IsUndefined() {
			h, herr = oaepHash(args[3].String())
		}
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

func parseAnyRSAPublic(pemBytes []byte) (*rsa.PublicKey, error) {
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
		prime = p
	} else {
		bits := args[0].Int()
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

var _ = sha1.New
