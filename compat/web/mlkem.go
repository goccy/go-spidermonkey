package web

// mlkem.go: ML-KEM (FIPS 203), the post-quantum key encapsulation mechanism.
//
// It is the largest single algorithm still missing from Web Crypto here: the
// suite's ML-KEM files are ~1,600 subtests, every one of which failed as
// "unsupported algorithm". Go's crypto/mlkem provides the primitive for the
// ML-KEM-768 and ML-KEM-1024 parameter sets. It has no ML-KEM-512 — the set is
// deprecated for new use but still registered, and the suite exercises it — so
// that one comes from CIRCL, whose ML-KEM is validated against the ACVP
// vectors.
//
// The key formats are hand-encoded DER because crypto/x509 does not marshal
// ML-KEM keys yet. Both are small and fully specified: an SPKI is the
// algorithm OID plus the encapsulation key in a BIT STRING, and a PKCS#8 is
// the OID plus the 64-byte seed in a [0] choice inside the private-key OCTET
// STRING.

import (
	"crypto/mlkem"
	"crypto/rand"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"fmt"

	mlkem512 "github.com/cloudflare/circl/kem/mlkem/mlkem512"
	spidermonkey "github.com/goccy/go-spidermonkey"
)

// mlkemParams is one ML-KEM parameter set, as far as this layer needs it.
type mlkemParams struct {
	name string
	oid  asn1.ObjectIdentifier
}

// The OID arc is 2.16.840.1.101.3.4.4.{1,2,3} for 512/768/1024.
var mlkemSets = []mlkemParams{
	{"ML-KEM-512", asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 4, 1}},
	{"ML-KEM-768", asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 4, 2}},
	{"ML-KEM-1024", asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 4, 3}},
}

func mlkemSet(name string) (mlkemParams, bool) {
	for _, p := range mlkemSets {
		if p.name == name {
			return p, true
		}
	}
	return mlkemParams{}, false
}

// mlkemKey holds whichever parameter set a handle was created with. Only one
// of the four pointers is ever set.
type mlkemKey struct {
	set   mlkemParams
	dk768 *mlkem.DecapsulationKey768
	ek768 *mlkem.EncapsulationKey768
	dk1k  *mlkem.DecapsulationKey1024
	ek1k  *mlkem.EncapsulationKey1024
	// The 512 set keeps its seed alongside the key: CIRCL derives from a seed
	// but does not hand it back, and raw-seed export needs it.
	dk512   *mlkem512.PrivateKey
	ek512   *mlkem512.PublicKey
	seed512 []byte
}

func (k *mlkemKey) private() bool { return k.dk768 != nil || k.dk1k != nil || k.dk512 != nil }

// seed returns the 64-byte seed a private key was built from.
func (k *mlkemKey) seed() []byte {
	switch {
	case k.dk768 != nil:
		return k.dk768.Bytes()
	case k.dk1k != nil:
		return k.dk1k.Bytes()
	case k.dk512 != nil:
		return k.seed512
	}
	return nil
}

// publicBytes returns the encapsulation key, deriving it from the private key
// when that is what the handle holds.
func (k *mlkemKey) publicBytes() []byte {
	switch {
	case k.dk768 != nil:
		return k.dk768.EncapsulationKey().Bytes()
	case k.dk1k != nil:
		return k.dk1k.EncapsulationKey().Bytes()
	case k.ek768 != nil:
		return k.ek768.Bytes()
	case k.ek1k != nil:
		return k.ek1k.Bytes()
	case k.dk512 != nil:
		buf := make([]byte, mlkem512.PublicKeySize)
		k.dk512.Public().(*mlkem512.PublicKey).Pack(buf)
		return buf
	case k.ek512 != nil:
		buf := make([]byte, mlkem512.PublicKeySize)
		k.ek512.Pack(buf)
		return buf
	}
	return nil
}

func mlkemFromSeed(set mlkemParams, seed []byte) (*mlkemKey, error) {
	if set.name == "ML-KEM-512" {
		if len(seed) != mlkem512.KeySeedSize {
			return nil, fmt.Errorf("ML-KEM-512 seed must be %d bytes", mlkem512.KeySeedSize)
		}
		_, sk := mlkem512.NewKeyFromSeed(seed)
		return &mlkemKey{set: set, dk512: sk, seed512: append([]byte(nil), seed...)}, nil
	}
	if set.name == "ML-KEM-768" {
		dk, err := mlkem.NewDecapsulationKey768(seed)
		if err != nil {
			return nil, err
		}
		return &mlkemKey{set: set, dk768: dk}, nil
	}
	dk, err := mlkem.NewDecapsulationKey1024(seed)
	if err != nil {
		return nil, err
	}
	return &mlkemKey{set: set, dk1k: dk}, nil
}

func mlkemFromPublic(set mlkemParams, b []byte) (*mlkemKey, error) {
	if set.name == "ML-KEM-512" {
		if len(b) != mlkem512.PublicKeySize {
			return nil, fmt.Errorf("ML-KEM-512 encapsulation key must be %d bytes", mlkem512.PublicKeySize)
		}
		pk := new(mlkem512.PublicKey)
		if err := pk.Unpack(b); err != nil {
			return nil, err
		}
		return &mlkemKey{set: set, ek512: pk}, nil
	}
	if set.name == "ML-KEM-768" {
		ek, err := mlkem.NewEncapsulationKey768(b)
		if err != nil {
			return nil, err
		}
		return &mlkemKey{set: set, ek768: ek}, nil
	}
	ek, err := mlkem.NewEncapsulationKey1024(b)
	if err != nil {
		return nil, err
	}
	return &mlkemKey{set: set, ek1k: ek}, nil
}

// -------------------------------------------------------------------- DER

type pkixAlgorithm struct {
	Algorithm asn1.ObjectIdentifier
}

type spkiDoc struct {
	Algorithm pkixAlgorithm
	PublicKey asn1.BitString
}

type pkcs8Doc struct {
	Version    int
	Algorithm  pkixAlgorithm
	PrivateKey []byte // an OCTET STRING holding the [0] seed choice
}

// mlkemSPKI encodes an encapsulation key as SubjectPublicKeyInfo.
func mlkemSPKI(set mlkemParams, pub []byte) ([]byte, error) {
	return asn1.Marshal(spkiDoc{
		Algorithm: pkixAlgorithm{set.oid},
		PublicKey: asn1.BitString{Bytes: pub, BitLength: len(pub) * 8},
	})
}

// mlkemPKCS8 encodes a seed as a PrivateKeyInfo. The inner value is the
// ML-KEM private-key CHOICE, whose seed alternative is context tag 0.
func mlkemPKCS8(set mlkemParams, seed []byte) ([]byte, error) {
	inner := append([]byte{0x80, byte(len(seed))}, seed...)
	return asn1.Marshal(pkcs8Doc{Version: 0, Algorithm: pkixAlgorithm{set.oid}, PrivateKey: inner})
}

// mlkemParseSPKI reads back what mlkemSPKI wrote, and reports the parameter set
// the OID names so an import does not have to trust the caller's algorithm.
func mlkemParseSPKI(der []byte) (mlkemParams, []byte, error) {
	var doc spkiDoc
	if _, err := asn1.Unmarshal(der, &doc); err != nil {
		return mlkemParams{}, nil, err
	}
	for _, p := range mlkemSets {
		if p.oid.Equal(doc.Algorithm.Algorithm) {
			return p, doc.PublicKey.Bytes, nil
		}
	}
	return mlkemParams{}, nil, fmt.Errorf("not an ML-KEM key this build supports")
}

func mlkemParsePKCS8(der []byte) (mlkemParams, []byte, error) {
	var doc pkcs8Doc
	if _, err := asn1.Unmarshal(der, &doc); err != nil {
		return mlkemParams{}, nil, err
	}
	var set mlkemParams
	found := false
	for _, p := range mlkemSets {
		if p.oid.Equal(doc.Algorithm.Algorithm) {
			set, found = p, true
		}
	}
	if !found {
		return mlkemParams{}, nil, fmt.Errorf("not an ML-KEM key this build supports")
	}
	// The seed alternative: [0] IMPLICIT OCTET STRING (64 bytes).
	b := doc.PrivateKey
	if len(b) < 2 || b[0] != 0x80 || int(b[1]) != len(b)-2 {
		return mlkemParams{}, nil, fmt.Errorf("ML-KEM private key is not a seed")
	}
	return set, b[2:], nil
}

// -------------------------------------------------------------------- ops

// opMLKEMGenerate(name) -> {priv, pub} handles.
func (s *subtleAPI) opMLKEMGenerate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("mlkem generate: (name) required")
	}
	set, ok := mlkemSet(args[0].String())
	if !ok {
		return subtleErr("NotSupportedError: unsupported algorithm " + args[0].String()), nil
	}
	var k *mlkemKey
	if set.name == "ML-KEM-512" {
		// Generate from a fresh seed so the key can be exported as one, which is
		// the format the spec prefers for a private ML-KEM key.
		seed := make([]byte, mlkem512.KeySeedSize)
		if _, err := rand.Read(seed); err != nil {
			return subtleErr("OperationError: " + err.Error()), nil
		}
		gk, err := mlkemFromSeed(set, seed)
		if err != nil {
			return subtleErr("OperationError: " + err.Error()), nil
		}
		k = gk
	} else if set.name == "ML-KEM-768" {
		dk, err := mlkem.GenerateKey768()
		if err != nil {
			return subtleErr("OperationError: " + err.Error()), nil
		}
		k = &mlkemKey{set: set, dk768: dk}
	} else {
		dk, err := mlkem.GenerateKey1024()
		if err != nil {
			return subtleErr("OperationError: " + err.Error()), nil
		}
		k = &mlkemKey{set: set, dk1k: dk}
	}
	pub, err := mlkemFromPublic(set, k.publicBytes())
	if err != nil {
		return subtleErr("OperationError: " + err.Error()), nil
	}
	return spidermonkey.ValueOf(map[string]any{
		"priv": s.put(&subtleKey{mlkem: k}),
		"pub":  s.put(&subtleKey{mlkem: pub}),
		"name": set.name,
	}), nil
}

// opMLKEMImport(name, format, data) -> {id, type, name}. `data` is a JSON
// string for jwk and bytes for every other format.
func (s *subtleAPI) opMLKEMImport(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("mlkem import: (name, format, data) required")
	}
	declared := args[0].String()
	set, ok := mlkemSet(declared)
	if !ok {
		return subtleErr("NotSupportedError: unsupported algorithm " + declared), nil
	}
	format := args[1].String()

	// mismatch guards the two DER formats, which carry their own OID: importing
	// an ML-KEM-768 SPKI as ML-KEM-1024 is a DataError, not a silent reinterpret.
	finish := func(k *mlkemKey, err error) (spidermonkey.Value, error) {
		if err != nil {
			return subtleErr("DataError: " + err.Error()), nil
		}
		kind := "public"
		if k.private() {
			kind = "private"
		}
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{mlkem: k}), "type": kind, "name": k.set.name,
		}), nil
	}

	switch format {
	case "raw-seed":
		b, err := argBytes(args[2])
		if err != nil {
			return subtleErr("DataError: " + err.Error()), nil
		}
		return finish(mlkemFromSeed(set, b))
	case "raw-public":
		b, err := argBytes(args[2])
		if err != nil {
			return subtleErr("DataError: " + err.Error()), nil
		}
		return finish(mlkemFromPublic(set, b))
	case "pkcs8":
		b, err := argBytes(args[2])
		if err != nil {
			return subtleErr("DataError: " + err.Error()), nil
		}
		got, seed, err := mlkemParsePKCS8(b)
		if err != nil {
			return subtleErr("DataError: " + err.Error()), nil
		}
		if got.name != set.name {
			return subtleErr("DataError: key is " + got.name + ", not " + set.name), nil
		}
		return finish(mlkemFromSeed(set, seed))
	case "spki":
		b, err := argBytes(args[2])
		if err != nil {
			return subtleErr("DataError: " + err.Error()), nil
		}
		got, pub, err := mlkemParseSPKI(b)
		if err != nil {
			return subtleErr("DataError: " + err.Error()), nil
		}
		if got.name != set.name {
			return subtleErr("DataError: key is " + got.name + ", not " + set.name), nil
		}
		return finish(mlkemFromPublic(set, pub))
	case "jwk":
		var j struct{ Kty, Alg, Priv, Pub string }
		if err := json.Unmarshal([]byte(args[2].String()), &j); err != nil {
			return subtleErr("DataError: " + err.Error()), nil
		}
		if j.Kty != "AKP" {
			return subtleErr("DataError: ML-KEM JWK must have kty AKP"), nil
		}
		if j.Alg != "" && j.Alg != set.name {
			return subtleErr("DataError: JWK alg is " + j.Alg + ", not " + set.name), nil
		}
		if j.Priv != "" {
			seed, err := base64.RawURLEncoding.DecodeString(j.Priv)
			if err != nil {
				return subtleErr("DataError: bad JWK priv"), nil
			}
			return finish(mlkemFromSeed(set, seed))
		}
		pub, err := base64.RawURLEncoding.DecodeString(j.Pub)
		if err != nil {
			return subtleErr("DataError: bad JWK pub"), nil
		}
		return finish(mlkemFromPublic(set, pub))
	}
	return subtleErr("NotSupportedError: unsupported ML-KEM key format " + format), nil
}

// opMLKEMExport(format, handle) -> bytes, or a JWK object for "jwk".
func (s *subtleAPI) opMLKEMExport(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("mlkem export: (format, handle) required")
	}
	sk, err := s.get(args[1])
	if err != nil || sk.mlkem == nil {
		return subtleErr("InvalidAccessError: not an ML-KEM key"), nil
	}
	k := sk.mlkem
	b64 := base64.RawURLEncoding.EncodeToString
	switch args[0].String() {
	case "raw-public":
		return bytesValueOK(k.publicBytes())
	case "raw-seed":
		if !k.private() {
			return subtleErr("InvalidAccessError: raw-seed export needs a private key"), nil
		}
		return bytesValueOK(k.seed())
	case "spki":
		der, err := mlkemSPKI(k.set, k.publicBytes())
		if err != nil {
			return subtleErr("OperationError: " + err.Error()), nil
		}
		return bytesValueOK(der)
	case "pkcs8":
		if !k.private() {
			return subtleErr("InvalidAccessError: pkcs8 export needs a private key"), nil
		}
		der, err := mlkemPKCS8(k.set, k.seed())
		if err != nil {
			return subtleErr("OperationError: " + err.Error()), nil
		}
		return bytesValueOK(der)
	case "jwk":
		out := map[string]any{"kty": "AKP", "alg": k.set.name, "pub": b64(k.publicBytes())}
		if k.private() {
			out["priv"] = b64(k.seed())
		}
		return spidermonkey.ValueOf(out), nil
	}
	return subtleErr("NotSupportedError: unsupported ML-KEM export format"), nil
}

// opMLKEMEncapsulate(handle) -> {sharedKey, ciphertext}.
func (s *subtleAPI) opMLKEMEncapsulate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("mlkem encapsulate: (handle) required")
	}
	sk, err := s.get(args[0])
	if err != nil || sk.mlkem == nil {
		return subtleErr("InvalidAccessError: not an ML-KEM key"), nil
	}
	k := sk.mlkem
	// Encapsulation needs the public half, which a private handle can derive.
	pub, perr := mlkemFromPublic(k.set, k.publicBytes())
	if perr != nil {
		return subtleErr("OperationError: " + perr.Error()), nil
	}
	var shared, ct []byte
	switch {
	case pub.ek512 != nil:
		// CIRCL writes into caller-provided buffers rather than returning them,
		// and takes its own encapsulation randomness.
		ct = make([]byte, mlkem512.CiphertextSize)
		shared = make([]byte, mlkem512.SharedKeySize)
		seed := make([]byte, mlkem512.EncapsulationSeedSize)
		if _, rerr := rand.Read(seed); rerr != nil {
			return subtleErr("OperationError: " + rerr.Error()), nil
		}
		pub.ek512.EncapsulateTo(ct, shared, seed)
	case pub.ek768 != nil:
		shared, ct = pub.ek768.Encapsulate()
	default:
		shared, ct = pub.ek1k.Encapsulate()
	}
	// One flat buffer, shared key first: a byte array nested inside a map does
	// not survive the value bridge, and the split point is fixed at 32 bytes.
	return bytesValueOK(append(append([]byte(nil), shared...), ct...))
}

// opMLKEMDecapsulate(handle, ciphertext) -> shared key bytes.
func (s *subtleAPI) opMLKEMDecapsulate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("mlkem decapsulate: (handle, ciphertext) required")
	}
	sk, err := s.get(args[0])
	if err != nil || sk.mlkem == nil || !sk.mlkem.private() {
		return subtleErr("InvalidAccessError: decapsulation needs an ML-KEM private key"), nil
	}
	ct, err := argBytes(args[1])
	if err != nil {
		return subtleErr("OperationError: " + err.Error()), nil
	}
	var shared []byte
	switch {
	case sk.mlkem.dk512 != nil:
		if len(ct) != mlkem512.CiphertextSize {
			return subtleErr("OperationError: ML-KEM-512 ciphertext must be %d bytes"), nil
		}
		shared = make([]byte, mlkem512.SharedKeySize)
		sk.mlkem.dk512.DecapsulateTo(shared, ct)
	case sk.mlkem.dk768 != nil:
		shared, err = sk.mlkem.dk768.Decapsulate(ct)
	default:
		shared, err = sk.mlkem.dk1k.Decapsulate(ct)
	}
	if err != nil {
		return subtleErr("OperationError: " + err.Error()), nil
	}
	return bytesValueOK(shared)
}
