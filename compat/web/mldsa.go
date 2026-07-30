package web

// mldsa.go: ML-DSA (FIPS 204), the post-quantum signature scheme.
//
// Go grows a crypto/mldsa in 1.27. Until that is a released toolchain this
// module cannot require it — go.mod would force every consumer onto an
// unreleased Go — so the primitive comes from CIRCL, which is already here for
// ML-KEM-512 and whose ML-DSA is validated against the ACVP vectors. Everything
// CIRCL touches is inside this file, so the swap to crypto/mldsa later is a
// one-file change.
//
// Unlike ML-KEM there is no per-parameter-set branching below this point: CIRCL
// answers all three sets through one sign.Scheme interface.

import (
	"crypto/rand"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/cloudflare/circl/sign"
	"github.com/cloudflare/circl/sign/mldsa/mldsa44"
	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"github.com/cloudflare/circl/sign/mldsa/mldsa87"
	spidermonkey "github.com/goccy/go-spidermonkey"
)

// mldsaParams is one ML-DSA parameter set: its registered Web Crypto name, the
// OID its DER carries, and the scheme that does the arithmetic.
type mldsaParams struct {
	name string
	oid  asn1.ObjectIdentifier
	sch  sign.Scheme
}

// The OID arc is 2.16.840.1.101.3.4.3.{17,18,19} for 44/65/87.
var mldsaSets = []mldsaParams{
	{"ML-DSA-44", asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 3, 17}, mldsa44.Scheme()},
	{"ML-DSA-65", asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 3, 18}, mldsa65.Scheme()},
	{"ML-DSA-87", asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 3, 19}, mldsa87.Scheme()},
}

// mldsaContextMax is the longest context string ML-DSA admits. CIRCL panics on
// a longer one, so the length is checked before the call rather than after.
const mldsaContextMax = 255

func mldsaSet(name string) (mldsaParams, bool) {
	for _, p := range mldsaSets {
		if p.name == name {
			return p, true
		}
	}
	return mldsaParams{}, false
}

func mldsaByOID(oid asn1.ObjectIdentifier) (mldsaParams, error) {
	for _, p := range mldsaSets {
		if p.oid.Equal(oid) {
			return p, nil
		}
	}
	return mldsaParams{}, fmt.Errorf("not an ML-DSA key")
}

// mldsaKey is one ML-DSA key. A private handle keeps the seed it was derived
// from: the seed is the only private form the spec exports, and deriving it
// back out of an expanded key is not possible.
type mldsaKey struct {
	set  mldsaParams
	priv sign.PrivateKey // nil for a public handle
	pub  sign.PublicKey
	seed []byte
}

func (k *mldsaKey) private() bool { return k.priv != nil }

func (k *mldsaKey) publicBytes() []byte {
	b, err := k.pub.MarshalBinary()
	if err != nil {
		return nil
	}
	return b
}

func mldsaFromSeed(set mldsaParams, seed []byte) (*mldsaKey, error) {
	if len(seed) != set.sch.SeedSize() {
		return nil, fmt.Errorf("%s seed must be %d bytes", set.name, set.sch.SeedSize())
	}
	// DeriveKey panics on a wrong-size seed, which the check above rules out.
	pub, priv := set.sch.DeriveKey(seed)
	return &mldsaKey{set: set, priv: priv, pub: pub, seed: append([]byte(nil), seed...)}, nil
}

func mldsaFromPublic(set mldsaParams, b []byte) (*mldsaKey, error) {
	if len(b) != set.sch.PublicKeySize() {
		return nil, fmt.Errorf("%s public key must be %d bytes", set.name, set.sch.PublicKeySize())
	}
	pub, err := set.sch.UnmarshalBinaryPublicKey(b)
	if err != nil {
		return nil, err
	}
	return &mldsaKey{set: set, pub: pub}, nil
}

// -------------------------------------------------------------------- ops

// opMLDSAGenerate(name) -> {priv, pub, name} handles.
func (s *subtleAPI) opMLDSAGenerate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("mldsa generate: (name) required")
	}
	set, ok := mldsaSet(args[0].String())
	if !ok {
		return subtleErr(errNotSupported, "unsupported algorithm "+args[0].String()), nil
	}
	// Generate through a seed rather than through the scheme's own GenerateKey,
	// so the key can be exported as one: raw-seed and pkcs8 both need it, and a
	// key generated any other way could not answer them.
	seed := make([]byte, set.sch.SeedSize())
	if _, err := rand.Read(seed); err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	k, err := mldsaFromSeed(set, seed)
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	pub, err := mldsaFromPublic(set, k.publicBytes())
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	return spidermonkey.ValueOf(map[string]any{
		"priv": s.put(&subtleKey{mldsa: k}),
		"pub":  s.put(&subtleKey{mldsa: pub}),
		"name": set.name,
	}), nil
}

// opMLDSAImport(name, format, data) -> {id, type, name}. `data` is a JSON
// string for jwk and bytes for every other format.
func (s *subtleAPI) opMLDSAImport(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("mldsa import: (name, format, data) required")
	}
	declared := args[0].String()
	set, ok := mldsaSet(declared)
	if !ok {
		return subtleErr(errNotSupported, "unsupported algorithm "+declared), nil
	}

	finish := func(k *mldsaKey, err error) (spidermonkey.Value, error) {
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		kind := "public"
		if k.private() {
			kind = "private"
		}
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{mldsa: k}), "type": kind, "name": k.set.name,
		}), nil
	}
	// The two DER formats carry their own OID, so importing an ML-DSA-44 SPKI as
	// ML-DSA-65 is a DataError rather than a silent reinterpret.
	matches := func(got mldsaParams) error {
		if got.name != set.name {
			return fmt.Errorf("key is %s, not %s", got.name, set.name)
		}
		return nil
	}

	format := args[1].String()
	switch format {
	case "raw-seed":
		b, err := argBytes(args[2])
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		return finish(mldsaFromSeed(set, b))
	case "raw-public":
		b, err := argBytes(args[2])
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		return finish(mldsaFromPublic(set, b))
	case "pkcs8", "spki":
		b, err := argBytes(args[2])
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		parse, load := akpParsePKCS8, mldsaFromSeed
		if format == "spki" {
			parse, load = akpParseSPKI, mldsaFromPublic
		}
		oid, material, err := parse(b)
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		got, err := mldsaByOID(oid)
		if err == nil {
			err = matches(got)
		}
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		return finish(load(set, material))
	case "jwk":
		var j struct{ Kty, Alg, Priv, Pub string }
		if err := json.Unmarshal([]byte(args[2].String()), &j); err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		if j.Kty != "AKP" {
			return subtleErr(errData, "ML-DSA JWK must have kty AKP"), nil
		}
		if j.Alg != "" && j.Alg != set.name {
			return subtleErr(errData, "JWK alg is "+j.Alg+", not "+set.name), nil
		}
		if j.Priv != "" {
			seed, err := base64.RawURLEncoding.DecodeString(j.Priv)
			if err != nil {
				return subtleErr(errData, "bad JWK priv"), nil
			}
			return finish(mldsaFromSeed(set, seed))
		}
		pub, err := base64.RawURLEncoding.DecodeString(j.Pub)
		if err != nil {
			return subtleErr(errData, "bad JWK pub"), nil
		}
		return finish(mldsaFromPublic(set, pub))
	}
	return subtleErr(errNotSupported, "unsupported ML-DSA key format "+format), nil
}

// opMLDSAExport(format, handle) -> bytes, or a JWK object for "jwk".
func (s *subtleAPI) opMLDSAExport(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("mldsa export: (format, handle) required")
	}
	sk, err := s.get(args[1])
	if err != nil || sk.mldsa == nil {
		return subtleErr(errInvalidAccess, "not an ML-DSA key"), nil
	}
	k := sk.mldsa
	b64 := base64.RawURLEncoding.EncodeToString
	switch args[0].String() {
	case "raw-public":
		return bytesValueOK(k.publicBytes())
	case "raw-seed":
		if !k.private() {
			return subtleErr(errInvalidAccess, "raw-seed export needs a private key"), nil
		}
		return bytesValueOK(k.seed)
	case "spki":
		der, err := akpSPKI(k.set.oid, k.publicBytes())
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		return bytesValueOK(der)
	case "pkcs8":
		if !k.private() {
			return subtleErr(errInvalidAccess, "pkcs8 export needs a private key"), nil
		}
		der, err := akpPKCS8(k.set.oid, k.seed)
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		return bytesValueOK(der)
	case "jwk":
		out := map[string]any{"kty": "AKP", "alg": k.set.name, "pub": b64(k.publicBytes())}
		if k.private() {
			out["priv"] = b64(k.seed)
		}
		return spidermonkey.ValueOf(out), nil
	}
	return subtleErr(errNotSupported, "unsupported ML-DSA export format"), nil
}

// opMLDSASign(handle, data, context) -> signature bytes.
func (s *subtleAPI) opMLDSASign(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("mldsa sign: (handle, data, context) required")
	}
	sk, err := s.get(args[0])
	if err != nil || sk.mldsa == nil || !sk.mldsa.private() {
		return subtleErr(errInvalidAccess, "signing needs an ML-DSA private key"), nil
	}
	msg, err := argBytes(args[1])
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	ctx, err := argBytes(args[2])
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	if len(ctx) > mldsaContextMax {
		return subtleErr(errOperation, fmt.Sprintf("ML-DSA context must be at most %d bytes", mldsaContextMax)), nil
	}
	k := sk.mldsa
	return bytesValueOK(k.set.sch.Sign(k.priv, msg, &sign.SignatureOpts{Context: string(ctx)}))
}

// opMLDSAVerify(handle, signature, data, context) -> bool.
func (s *subtleAPI) opMLDSAVerify(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("mldsa verify: (handle, signature, data, context) required")
	}
	sk, err := s.get(args[0])
	if err != nil || sk.mldsa == nil {
		return subtleErr(errInvalidAccess, "not an ML-DSA key"), nil
	}
	sig, err := argBytes(args[1])
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	msg, err := argBytes(args[2])
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	ctx, err := argBytes(args[3])
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	k := sk.mldsa
	// A signature of the wrong length, or a context too long to have produced
	// one, is a failed verification and not an error: verify reports false for
	// anything that is not a good signature.
	if len(sig) != k.set.sch.SignatureSize() || len(ctx) > mldsaContextMax {
		return spidermonkey.ValueOf(false), nil
	}
	return spidermonkey.ValueOf(k.set.sch.Verify(k.pub, msg, sig, &sign.SignatureOpts{Context: string(ctx)})), nil
}
