package web

import (
	"bytes"
	"container/list"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/rsa"
	_ "crypto/sha1" // register hashes for crypto.Hash.New
	_ "crypto/sha256"
	_ "crypto/sha3"
	_ "crypto/sha512"
	"crypto/subtle"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/cloudflare/circl/dh/x448"
	mlkem512 "github.com/cloudflare/circl/kem/mlkem/mlkem512"
	"github.com/cloudflare/circl/sign"
	"github.com/cloudflare/circl/sign/ed448"
	"github.com/cloudflare/circl/sign/mldsa/mldsa44"
	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"github.com/cloudflare/circl/sign/mldsa/mldsa87"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/crypto/sha3"
	"hash"
	"math/big"
	"math/bits"
	"sync"
)

// ------------------------- the Go crypto primitives behind crypto.subtle
// (js/subtle.js).
// Keys live host-side in a handle table; the guest's CryptoKey carries only
// the handle. Byte outputs (digests, signatures, exported DER) return as
// plain arrays — data, not handles — so nothing stays pinned.

// maxRSAModulusBits bounds any caller-supplied RSA modulus. Every RSA operation
// (import Precompute/Validate, sign, verify, encrypt) runs an uninterruptible
// big-integer modexp on the shared host, so an oversized attacker-chosen modulus
// would pin a core (and, on the pooled cfworkers surface, a whole request slot)
// for minutes. 8192 covers every legitimate RSA key; larger is rejected.
const maxRSAModulusBits = 8192

// checkRSAModulus rejects a modulus above the cap before any modexp/Precompute.
func checkRSAModulus(n *big.Int) error {
	if n == nil || n.BitLen() > maxRSAModulusBits {
		return fmt.Errorf("RSA modulus too large (max %d bits)", maxRSAModulusBits)
	}
	return nil
}

type subtleKey struct {
	hmac    []byte
	rsaPriv *rsa.PrivateKey
	rsaPub  *rsa.PublicKey
	ecPriv  *ecdsa.PrivateKey
	ecPub   *ecdsa.PublicKey
	edPriv  ed25519.PrivateKey
	edPub   ed25519.PublicKey
	xPriv   *ecdh.PrivateKey // X25519
	xPub    *ecdh.PublicKey
	mlkem   *mlkemKey
	mldsa   *mldsaKey
	// The 448 curves come from CIRCL; the standard library has neither.
	ed448Priv ed448.PrivateKey
	ed448Pub  ed448.PublicKey
	x448Priv  *x448.Key
	x448Pub   *x448.Key
}

// maxSubtleKeys bounds the host key table. The table has no engine-driven free
// path (guest CryptoKeys carry only a numeric handle and FinalizationRegistry
// cleanup does not run in this build), so an unbounded table would leak one
// entry per HMAC/EC/RSA import on a long-lived pooled instance. LRU eviction
// keeps memory bounded: a key used every request (the "import once, cache"
// pattern) stays most-recently-used and is never evicted, while cold
// per-request-imported keys age out. Only a workload with more than this many
// DISTINCT, actively-used keys would evict a live one (degrading to the same
// "unknown key handle" error a miss already produces).
const maxSubtleKeys = 1024

type subtleAPI struct {
	mu    sync.Mutex
	keys  map[int64]*subtleKey
	elems map[int64]*list.Element // id -> its node in lru (front = most recently used)
	lru   *list.List              // values are int64 ids
}

func newSubtleAPI() *subtleAPI {
	return &subtleAPI{keys: map[int64]*subtleKey{}, elems: map[int64]*list.Element{}, lru: list.New()}
}

// randUint64 returns 8 crypto-random bytes as a uint64. crypto/rand.Read never
// fails on the platforms this runs on; on the theoretical error it falls back to
// a fixed value (the caller only uses it to pick an unused map key, retrying on
// collision, so correctness does not depend on the value).
func randUint64() uint64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return binary.LittleEndian.Uint64(b[:])
}

// put stores host-side key material (HMAC/RSA/ECDSA) under an UNGUESSABLE random
// handle. Handles are exposed to guest code, and globalThis.CryptoKey lets a
// guest forge a CryptoKey around any handle value; a random 63-bit id makes
// enumerating another (e.g. a pooled prior request's) key handle infeasible.
// Reset additionally drops all handles between pooled requests.
func (s *subtleAPI) put(k *subtleKey) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		// 52-bit id so it round-trips exactly through a JS float64 handle, yet is
		// still a 2^52 space that a guest cannot feasibly enumerate.
		id := int64(randUint64() & ((1 << 52) - 1))
		if id == 0 || s.keys[id] != nil {
			continue
		}
		s.keys[id] = k
		s.elems[id] = s.lru.PushFront(id)
		// Evict the least-recently-used key once over the cap.
		if len(s.keys) > maxSubtleKeys {
			if back := s.lru.Back(); back != nil {
				old := back.Value.(int64)
				s.lru.Remove(back)
				delete(s.keys, old)
				delete(s.elems, old)
			}
		}
		return id
	}
}

func (s *subtleAPI) get(v spidermonkey.Value) (*subtleKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := int64(v.Float())
	k, ok := s.keys[id]
	if !ok {
		return nil, fmt.Errorf("unknown key handle")
	}
	// Bump recency so an actively-used key is never evicted as "cold".
	if e := s.elems[id]; e != nil {
		s.lru.MoveToFront(e)
	}
	return k, nil
}

// reset drops all stored key material. A pooled instance (cfworkers) calls this
// between requests so one request's keys can't be addressed — via a forged
// CryptoKey handle — by the next request on the same instance.
func (s *subtleAPI) reset() {
	s.mu.Lock()
	s.keys = map[int64]*subtleKey{}
	s.elems = map[int64]*list.Element{}
	s.lru.Init()
	s.mu.Unlock()
}

// hashSum returns the digest of data under h (the h.New/Write/Sum idiom every
// sign/verify path shares).
func hashSum(h crypto.Hash, data []byte) []byte {
	hh := h.New()
	hh.Write(data)
	return hh.Sum(nil)
}

func hashByName(name string) (crypto.Hash, error) {
	switch name {
	case "SHA-1":
		return crypto.SHA1, nil
	case "SHA-256":
		return crypto.SHA256, nil
	case "SHA-384":
		return crypto.SHA384, nil
	case "SHA-512":
		return crypto.SHA512, nil
	// SHA-3 is a different sponge, not a longer SHA-2, and it is registered in
	// its own right — a caller asking for SHA3-256 does not want SHA-256.
	case "SHA3-256":
		return crypto.SHA3_256, nil
	case "SHA3-384":
		return crypto.SHA3_384, nil
	case "SHA3-512":
		return crypto.SHA3_512, nil
	}
	return 0, fmt.Errorf("unsupported hash %q", name)
}

func curveByName(name string) (elliptic.Curve, error) {
	switch name {
	case "P-256":
		return elliptic.P256(), nil
	case "P-384":
		return elliptic.P384(), nil
	case "P-521":
		return elliptic.P521(), nil
	}
	return nil, fmt.Errorf("unsupported curve %q", name)
}

func curveName(c elliptic.Curve) string {
	switch c {
	case elliptic.P256():
		return "P-256"
	case elliptic.P384():
		return "P-384"
	case elliptic.P521():
		return "P-521"
	}
	return ""
}

// argBytes reads a BufferSource argument through the bytes bridge.
func argBytes(v spidermonkey.Value) ([]byte, error) {
	o := v.Object()
	if o == nil {
		return nil, fmt.Errorf("expected a byte buffer argument")
	}
	defer o.Free()
	return o.Bytes()
}

// intArg reads a positional numeric argument, freeing its persistent root if
// the guest passed an object where a number is expected (e.g. an un-coerced
// tagLength / deriveBits length). Value.Int() returns 0 for an object receiver
// without releasing the root, which would otherwise pin the object, and its
// backing store, for the interpreter's life on every call.
func intArg(v spidermonkey.Value) int {
	if o := v.Object(); o != nil {
		o.Free()
	}
	return v.Int()
}

// bytesValue returns b as a plain array (data, not a handle).
func bytesValue(b []byte) spidermonkey.Value {
	ints := make([]int, len(b))
	for i, x := range b {
		ints[i] = int(x)
	}
	return spidermonkey.ValueOf(ints)
}

var b64u = base64.RawURLEncoding

type jwkDoc struct {
	Kty string `json:"kty"`
	Crv string `json:"crv,omitempty"`
	// Alg names the algorithm the key is for. An Ed JWK carries it; an X one does
	// not, and the member count is part of what the suite checks.
	Alg string `json:"alg,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
	D   string `json:"d,omitempty"`
	N   string `json:"n,omitempty"`
	E   string `json:"e,omitempty"`
	P   string `json:"p,omitempty"`
	Q   string `json:"q,omitempty"`
	Dp  string `json:"dp,omitempty"`
	Dq  string `json:"dq,omitempty"`
	Qi  string `json:"qi,omitempty"`
	K   string `json:"k,omitempty"`
	Ext bool   `json:"ext,omitempty"`
}

func b64uBig(s string) (*big.Int, error) {
	if s == "" {
		return nil, fmt.Errorf("missing JWK field")
	}
	b, err := b64u.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(b), nil
}

// bigB64u encodes n left-padded to size bytes (0 = minimal length).
func bigB64u(n *big.Int, size int) string {
	b := n.Bytes()
	if size > len(b) {
		b = append(make([]byte, size-len(b)), b...)
	}
	return b64u.EncodeToString(b)
}

// ------------------------------------------------------------------ digest

func (s *subtleAPI) opDigest(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("digest: (hash, data) required")
	}
	h, err := hashByName(args[0].String())
	if err != nil {
		return nil, err
	}
	data, err := argBytes(args[1])
	if err != nil {
		return nil, err
	}
	return bytesValue(hashSum(h, data)), nil
}

// -------------------------------------------------------------------- HMAC

func (s *subtleAPI) opHMACImport(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("hmac import: raw key required")
	}
	raw, err := argBytes(args[0])
	if err != nil {
		return nil, err
	}
	return spidermonkey.ValueOf(s.put(&subtleKey{hmac: raw})), nil
}

func (s *subtleAPI) opHMACExport(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	k, err := s.get(args[0])
	if err != nil {
		return nil, err
	}
	if k.hmac == nil {
		return nil, fmt.Errorf("not an HMAC key")
	}
	return bytesValue(k.hmac), nil
}

func (s *subtleAPI) hmacSum(hashName string, keyV, dataV spidermonkey.Value) ([]byte, error) {
	h, err := hashByName(hashName)
	if err != nil {
		return nil, err
	}
	k, err := s.get(keyV)
	if err != nil {
		return nil, err
	}
	if k.hmac == nil {
		return nil, fmt.Errorf("not an HMAC key")
	}
	data, err := argBytes(dataV)
	if err != nil {
		return nil, err
	}
	m := hmac.New(h.New, k.hmac)
	m.Write(data)
	return m.Sum(nil), nil
}

func (s *subtleAPI) opHMACSign(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("hmac sign: (hash, key, data) required")
	}
	sum, err := s.hmacSum(args[0].String(), args[1], args[2])
	if err != nil {
		return nil, err
	}
	return bytesValue(sum), nil
}

func (s *subtleAPI) opHMACVerify(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("hmac verify: (hash, key, sig, data) required")
	}
	sig, err := argBytes(args[2])
	if err != nil {
		return nil, err
	}
	sum, err := s.hmacSum(args[0].String(), args[1], args[3])
	if err != nil {
		return nil, err
	}
	return spidermonkey.ValueOf(hmac.Equal(sig, sum)), nil
}

// ------------------------------------------------------------------- ECDSA

func (s *subtleAPI) opECGenerate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("ec generate: curve required")
	}
	curve, err := curveByName(args[0].String())
	if err != nil {
		return nil, err
	}
	priv, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		return nil, err
	}
	return spidermonkey.ValueOf(map[string]any{
		"priv": s.put(&subtleKey{ecPriv: priv}),
		"pub":  s.put(&subtleKey{ecPub: &priv.PublicKey}),
	}), nil
}

// b64uLen is the byte length a base64url field decodes to, without decoding it
// twice.
func b64uLen(s string) int {
	n, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return -1
	}
	return len(n)
}

func (s *subtleAPI) opECImportJWK(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("ec import: jwk required")
	}
	// Malformed key MATERIAL is a DataError, not a generic failure: the caller
	// handed over something that does not decode as a key, and the suite (and
	// any caller branching on the name) needs to be told which kind of mistake
	// that was. Returning a bare Go error surfaced as a plain Error instead.
	var j jwkDoc
	if err := json.Unmarshal([]byte(args[0].String()), &j); err != nil {
		return subtleErr(errData, "bad JWK: "+err.Error()), nil
	}
	if j.Kty != "EC" {
		return subtleErr(errData, fmt.Sprintf("not an EC JWK (kty=%q)", j.Kty)), nil
	}
	curve, err := curveByName(j.Crv)
	if err != nil {
		return subtleErr(errData, err.Error()), nil
	}
	x, err := b64uBig(j.X)
	if err != nil {
		return subtleErr(errData, "bad JWK x: "+err.Error()), nil
	}
	y, err := b64uBig(j.Y)
	if err != nil {
		return subtleErr(errData, "bad JWK y: "+err.Error()), nil
	}
	// The coordinates must be exactly the curve's field width. A short or long
	// one is a different point, or none — never something to zero-extend.
	if size := (curve.Params().BitSize + 7) / 8; b64uLen(j.X) != size || b64uLen(j.Y) != size {
		return subtleErr(errData, "JWK coordinate length does not match the curve"), nil
	}
	pub := ecdsa.PublicKey{Curve: curve, X: x, Y: y}
	if !curve.IsOnCurve(x, y) {
		return subtleErr(errData, "JWK point is not on "+j.Crv), nil
	}
	if j.D == "" {
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{ecPub: &pub}), "type": "public", "crv": j.Crv,
		}), nil
	}
	d, err := b64uBig(j.D)
	if err != nil {
		return subtleErr(errData, "bad JWK d: "+err.Error()), nil
	}
	if size := (curve.Params().BitSize + 7) / 8; b64uLen(j.D) != size {
		return subtleErr(errData, "JWK private scalar length does not match the curve"), nil
	}
	priv := &ecdsa.PrivateKey{PublicKey: pub, D: d}
	return spidermonkey.ValueOf(map[string]any{
		"id": s.put(&subtleKey{ecPriv: priv}), "type": "private", "crv": j.Crv,
	}), nil
}

func (s *subtleAPI) opECImportDER(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("ec import: (format, der) required")
	}
	der, err := argBytes(args[1])
	if err != nil {
		return nil, err
	}
	switch args[0].String() {
	case "raw":
		// A raw EC key is a bare uncompressed point, so the curve cannot be
		// recovered from the bytes — the caller names it.
		if len(args) < 3 {
			return subtleErr(errData, "raw EC import needs a named curve"), nil
		}
		curveName := args[2].String()
		curve, cerr := ecdhCurve(curveName)
		if cerr != nil {
			return subtleErr(errData, cerr.Error()), nil
		}
		// A compressed point carries only the x coordinate and the parity of y,
		// behind a 0x02 or 0x03 tag. crypto/ecdh takes uncompressed points only,
		// so the point is decompressed and re-marshalled first — the alternative
		// is refusing a form the standard accepts.
		if len(der) > 0 && (der[0] == 0x02 || der[0] == 0x03) {
			ec, eerr := curveByName(curveName)
			if eerr != nil {
				return subtleErr(errData, eerr.Error()), nil
			}
			x, y := elliptic.UnmarshalCompressed(ec, der)
			if x == nil {
				return subtleErr(errData, "not a point on "+curveName), nil
			}
			der = elliptic.Marshal(ec, x, y)
		}
		pt, perr := curve.NewPublicKey(der)
		if perr != nil {
			return subtleErr(errData, perr.Error()), nil
		}
		pub, perr := ecdsaFromECDHPublic(pt, curveName)
		if perr != nil {
			return subtleErr(errData, perr.Error()), nil
		}
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{ecPub: pub}), "type": "public", "crv": curveName,
		}), nil
	case "pkcs8":
		key, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		priv, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return subtleErr(errData, "pkcs8 key is not EC"), nil
		}
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{ecPriv: priv}), "type": "private", "crv": curveName(priv.Curve),
		}), nil
	case "spki":
		key, err := x509.ParsePKIXPublicKey(der)
		if err != nil {
			// crypto/x509 takes uncompressed points only. A compressed one is a
			// legal SPKI, so it is read here instead of refused.
			pub, cerr := parseECSPKICompressed(der)
			if cerr != nil {
				return subtleErr(errData, err.Error()), nil
			}
			return spidermonkey.ValueOf(map[string]any{
				"id": s.put(&subtleKey{ecPub: pub}), "type": "public", "crv": curveName(pub.Curve),
			}), nil
		}
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return subtleErr(errData, "spki key is not EC"), nil
		}
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{ecPub: pub}), "type": "public", "crv": curveName(pub.Curve),
		}), nil
	}
	return subtleErr(errNotSupported, "unsupported EC key format"), nil
}

func (s *subtleAPI) opECExportJWK(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	k, err := s.get(args[0])
	if err != nil {
		return nil, err
	}
	var pub *ecdsa.PublicKey
	j := jwkDoc{Kty: "EC", Ext: true}
	switch {
	case k.ecPriv != nil:
		pub = &k.ecPriv.PublicKey
		size := (pub.Curve.Params().BitSize + 7) / 8
		j.D = bigB64u(k.ecPriv.D, size)
	case k.ecPub != nil:
		pub = k.ecPub
	default:
		return nil, fmt.Errorf("not an EC key")
	}
	size := (pub.Curve.Params().BitSize + 7) / 8
	j.Crv = curveName(pub.Curve)
	j.X = bigB64u(pub.X, size)
	j.Y = bigB64u(pub.Y, size)
	out, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}
	return spidermonkey.ValueOf(string(out)), nil
}

func (s *subtleAPI) opECExportDER(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("ec export: (format, key) required")
	}
	k, err := s.get(args[1])
	if err != nil {
		return nil, err
	}
	switch args[0].String() {
	case "raw":
		pub := k.ecPub
		if pub == nil && k.ecPriv != nil {
			pub = &k.ecPriv.PublicKey
		}
		if pub == nil {
			return subtleErr(errInvalidAccess, "raw export needs a public key"), nil
		}
		pt, perr := pub.ECDH()
		if perr != nil {
			return subtleErr(errOperation, perr.Error()), nil
		}
		return bytesValue(pt.Bytes()), nil
	case "pkcs8":
		if k.ecPriv == nil {
			return subtleErr(errInvalidAccess, "pkcs8 export needs a private key"), nil
		}
		der, err := x509.MarshalPKCS8PrivateKey(k.ecPriv)
		if err != nil {
			return nil, err
		}
		return bytesValue(der), nil
	case "spki":
		pub := k.ecPub
		if pub == nil && k.ecPriv != nil {
			pub = &k.ecPriv.PublicKey
		}
		if pub == nil {
			return nil, fmt.Errorf("not an EC key")
		}
		der, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			return nil, err
		}
		return bytesValue(der), nil
	}
	return subtleErr(errNotSupported, "unsupported EC export format"), nil
}

func (s *subtleAPI) opECSign(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("ec sign: (hash, key, data) required")
	}
	h, err := hashByName(args[0].String())
	if err != nil {
		return nil, err
	}
	k, err := s.get(args[1])
	if err != nil {
		return nil, err
	}
	if k.ecPriv == nil {
		return nil, fmt.Errorf("sign needs an EC private key")
	}
	data, err := argBytes(args[2])
	if err != nil {
		return nil, err
	}
	r, sv, err := ecdsa.Sign(rand.Reader, k.ecPriv, hashSum(h, data))
	if err != nil {
		return nil, err
	}
	// WebCrypto ECDSA signatures are raw r||s, each curve-size bytes.
	size := (k.ecPriv.Curve.Params().BitSize + 7) / 8
	sig := make([]byte, 2*size)
	r.FillBytes(sig[:size])
	sv.FillBytes(sig[size:])
	return bytesValue(sig), nil
}

func (s *subtleAPI) opECVerify(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("ec verify: (hash, key, sig, data) required")
	}
	h, err := hashByName(args[0].String())
	if err != nil {
		return nil, err
	}
	k, err := s.get(args[1])
	if err != nil {
		return nil, err
	}
	pub := k.ecPub
	if pub == nil && k.ecPriv != nil {
		pub = &k.ecPriv.PublicKey
	}
	if pub == nil {
		return nil, fmt.Errorf("verify needs an EC key")
	}
	sig, err := argBytes(args[2])
	if err != nil {
		return nil, err
	}
	data, err := argBytes(args[3])
	if err != nil {
		return nil, err
	}
	size := (pub.Curve.Params().BitSize + 7) / 8
	if len(sig) != 2*size {
		return spidermonkey.ValueOf(false), nil
	}
	r := new(big.Int).SetBytes(sig[:size])
	sv := new(big.Int).SetBytes(sig[size:])
	return spidermonkey.ValueOf(ecdsa.Verify(pub, hashSum(h, data), r, sv)), nil
}

// --------------------------------------------------------------------- RSA

func (s *subtleAPI) opRSAGenerate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("rsa generate: modulus length required")
	}
	bits := intArg(args[0])
	if bits < 1024 || bits > 8192 {
		return nil, fmt.Errorf("rsa generate: unsupported modulus length %d", bits)
	}
	priv, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return nil, err
	}
	return spidermonkey.ValueOf(map[string]any{
		"priv": s.put(&subtleKey{rsaPriv: priv}),
		"pub":  s.put(&subtleKey{rsaPub: &priv.PublicKey}),
	}), nil
}

func (s *subtleAPI) opRSAImportJWK(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("rsa import: jwk required")
	}
	var j jwkDoc
	if err := json.Unmarshal([]byte(args[0].String()), &j); err != nil {
		return nil, fmt.Errorf("bad JWK: %w", err)
	}
	if j.Kty != "RSA" {
		return nil, fmt.Errorf("not an RSA JWK (kty=%q)", j.Kty)
	}
	n, err := b64uBig(j.N)
	if err != nil {
		return nil, fmt.Errorf("bad JWK n: %w", err)
	}
	e, err := b64uBig(j.E)
	if err != nil {
		return nil, fmt.Errorf("bad JWK e: %w", err)
	}
	if err := checkRSAModulus(n); err != nil {
		return nil, err
	}
	// A public exponent must fit an int and be a sane odd value >= 3; a huge e
	// would both overflow int(e.Int64()) and drive an oversized modexp.
	if !e.IsInt64() || e.Int64() < 3 || e.BitLen() > 32 {
		return nil, fmt.Errorf("invalid RSA public exponent")
	}
	pub := rsa.PublicKey{N: n, E: int(e.Int64())}
	if j.D == "" {
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{rsaPub: &pub}), "type": "public", "bits": pub.N.BitLen(),
		}), nil
	}
	d, err := b64uBig(j.D)
	if err != nil {
		return nil, fmt.Errorf("bad JWK d: %w", err)
	}
	p, err := b64uBig(j.P)
	if err != nil {
		return nil, fmt.Errorf("bad JWK p: %w", err)
	}
	q, err := b64uBig(j.Q)
	if err != nil {
		return nil, fmt.Errorf("bad JWK q: %w", err)
	}
	// Precompute()/Validate() and later CRT operations run on d/p/q, NOT n, so a
	// JWK with a small n but huge d/p/q would bypass the modulus cap and pin a
	// core. Bound the private components too, BEFORE Precompute.
	for _, v := range []*big.Int{d, p, q} {
		if v.BitLen() > maxRSAModulusBits {
			return nil, fmt.Errorf("RSA private component too large (max %d bits)", maxRSAModulusBits)
		}
	}
	priv := &rsa.PrivateKey{PublicKey: pub, D: d, Primes: []*big.Int{p, q}}
	priv.Precompute()
	if err := priv.Validate(); err != nil {
		return nil, fmt.Errorf("invalid RSA JWK: %w", err)
	}
	return spidermonkey.ValueOf(map[string]any{
		"id": s.put(&subtleKey{rsaPriv: priv}), "type": "private", "bits": pub.N.BitLen(),
	}), nil
}

func (s *subtleAPI) opRSAImportDER(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("rsa import: (format, der) required")
	}
	der, err := argBytes(args[1])
	if err != nil {
		return nil, err
	}
	switch args[0].String() {
	case "pkcs8":
		key, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return nil, err
		}
		priv, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("pkcs8 key is not RSA")
		}
		if err := checkRSAModulus(priv.N); err != nil {
			return nil, err
		}
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{rsaPriv: priv}), "type": "private", "bits": priv.N.BitLen(),
		}), nil
	case "spki":
		key, err := x509.ParsePKIXPublicKey(der)
		if err != nil {
			return nil, err
		}
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("spki key is not RSA")
		}
		if err := checkRSAModulus(pub.N); err != nil {
			return nil, err
		}
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{rsaPub: pub}), "type": "public", "bits": pub.N.BitLen(),
		}), nil
	}
	return nil, fmt.Errorf("rsa import: unsupported format")
}

func (s *subtleAPI) opRSAExportJWK(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	k, err := s.get(args[0])
	if err != nil {
		return nil, err
	}
	j := jwkDoc{Kty: "RSA", Ext: true}
	switch {
	case k.rsaPriv != nil:
		priv := k.rsaPriv
		j.N = bigB64u(priv.N, 0)
		j.E = bigB64u(big.NewInt(int64(priv.E)), 0)
		j.D = bigB64u(priv.D, 0)
		j.P = bigB64u(priv.Primes[0], 0)
		j.Q = bigB64u(priv.Primes[1], 0)
		j.Dp = bigB64u(priv.Precomputed.Dp, 0)
		j.Dq = bigB64u(priv.Precomputed.Dq, 0)
		j.Qi = bigB64u(priv.Precomputed.Qinv, 0)
	case k.rsaPub != nil:
		j.N = bigB64u(k.rsaPub.N, 0)
		j.E = bigB64u(big.NewInt(int64(k.rsaPub.E)), 0)
	default:
		return nil, fmt.Errorf("not an RSA key")
	}
	out, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}
	return spidermonkey.ValueOf(string(out)), nil
}

func (s *subtleAPI) opRSAExportDER(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("rsa export: (format, key) required")
	}
	k, err := s.get(args[1])
	if err != nil {
		return nil, err
	}
	switch args[0].String() {
	case "pkcs8":
		if k.rsaPriv == nil {
			return nil, fmt.Errorf("pkcs8 export needs a private key")
		}
		der, err := x509.MarshalPKCS8PrivateKey(k.rsaPriv)
		if err != nil {
			return nil, err
		}
		return bytesValue(der), nil
	case "spki":
		pub := k.rsaPub
		if pub == nil && k.rsaPriv != nil {
			pub = &k.rsaPriv.PublicKey
		}
		if pub == nil {
			return nil, fmt.Errorf("not an RSA key")
		}
		der, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			return nil, err
		}
		return bytesValue(der), nil
	}
	return nil, fmt.Errorf("rsa export: unsupported format")
}

// rsaPSSOptions maps the WebCrypto saltLength: a negative value (the JS side's
// sentinel for "not provided") means the hash length; a positive value is used
// verbatim; a literal 0 can't be expressed by rsa.PSSOptions (its SaltLength 0
// is the PSSSaltLengthAuto sentinel, not a zero-length salt). Known limitation:
// rather than silently signing with an auto/hash-length salt — which would
// produce a signature a strict saltLength:0 verifier rejects — we fail loudly.
// (A few JOSE libraries pin saltLength:0; those must use a non-zero salt here.)
// rsaPSSOptions maps a WebCrypto saltLength onto rsa.PSSOptions. A negative
// value (the JS side's stand-in for "not given") means the default of a salt as
// long as the hash; a positive one is used as it is.
//
// A saltLength of ZERO cannot be expressed: rsa.PSSOptions overloads 0 as
// PSSSaltLengthAuto, and crypto/rsa exposes no way to ask for a genuinely empty
// salt. For VERIFICATION that is harmless — auto-detect accepts a signature made
// with any salt length, including none — so verifying works and only signing
// with an explicit zero cannot be done here. Producing one would mean a raw RSA
// private-key operation over math/big, whose Exp is documented as NOT
// cryptographically constant-time; a library that other code trusts with its
// keys should not contain that to gain a conformance case.
func rsaPSSOptions(saltLen int, h crypto.Hash, verifying bool) (*rsa.PSSOptions, error) {
	if saltLen == 0 {
		if !verifying {
			return nil, fmt.Errorf("RSA-PSS saltLength 0 is not supported for signing (crypto/rsa cannot express a zero-length salt); use a positive saltLength or the default")
		}
		return &rsa.PSSOptions{Hash: h, SaltLength: rsa.PSSSaltLengthAuto}, nil
	}
	opts := &rsa.PSSOptions{Hash: h, SaltLength: rsa.PSSSaltLengthEqualsHash}
	if saltLen > 0 {
		opts.SaltLength = saltLen
	}
	return opts, nil
}

func (s *subtleAPI) opRSASign(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 5 {
		return nil, fmt.Errorf("rsa sign: (scheme, hash, saltLen, key, data) required")
	}
	h, err := hashByName(args[1].String())
	if err != nil {
		return nil, err
	}
	k, err := s.get(args[3])
	if err != nil {
		return nil, err
	}
	if k.rsaPriv == nil {
		return nil, fmt.Errorf("sign needs an RSA private key")
	}
	data, err := argBytes(args[4])
	if err != nil {
		return nil, err
	}
	digest := hashSum(h, data)
	var sig []byte
	switch args[0].String() {
	case "pkcs1":
		sig, err = rsa.SignPKCS1v15(rand.Reader, k.rsaPriv, h, digest)
	case "pss":
		opts, oerr := rsaPSSOptions(intArg(args[2]), h, false)
		if oerr != nil {
			return subtleErr(errOperation, oerr.Error()), nil
		}
		sig, err = rsa.SignPSS(rand.Reader, k.rsaPriv, h, digest, opts)
	default:
		return nil, fmt.Errorf("rsa sign: unsupported scheme")
	}
	if err != nil {
		return nil, err
	}
	return bytesValue(sig), nil
}

func (s *subtleAPI) opRSAVerify(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 6 {
		return nil, fmt.Errorf("rsa verify: (scheme, hash, saltLen, key, sig, data) required")
	}
	h, err := hashByName(args[1].String())
	if err != nil {
		return nil, err
	}
	k, err := s.get(args[3])
	if err != nil {
		return nil, err
	}
	pub := k.rsaPub
	if pub == nil && k.rsaPriv != nil {
		pub = &k.rsaPriv.PublicKey
	}
	if pub == nil {
		return nil, fmt.Errorf("verify needs an RSA key")
	}
	sig, err := argBytes(args[4])
	if err != nil {
		return nil, err
	}
	data, err := argBytes(args[5])
	if err != nil {
		return nil, err
	}
	digest := hashSum(h, data)
	switch args[0].String() {
	case "pkcs1":
		return spidermonkey.ValueOf(rsa.VerifyPKCS1v15(pub, h, digest, sig) == nil), nil
	case "pss":
		opts, oerr := rsaPSSOptions(intArg(args[2]), h, true)
		if oerr != nil {
			return subtleErr(errOperation, oerr.Error()), nil
		}
		return spidermonkey.ValueOf(rsa.VerifyPSS(pub, h, digest, sig, opts) == nil), nil
	}
	return nil, fmt.Errorf("rsa verify: unsupported scheme")
}

// ----------------------------------------------------------------- Ed25519

func (s *subtleAPI) opEdGenerate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return spidermonkey.ValueOf(map[string]any{
		"priv": s.put(&subtleKey{edPriv: priv}),
		"pub":  s.put(&subtleKey{edPub: pub}),
	}), nil
}

func (s *subtleAPI) opEdImport(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return subtleErr(errData, "ed25519 import: (format, keyData) required"), nil
	}
	switch args[0].String() {
	case "jwk":
		var j jwkDoc
		if err := json.Unmarshal([]byte(args[1].String()), &j); err != nil {
			return subtleErr(errData, fmt.Sprintf("bad JWK: %v", err)), nil
		}
		if j.Kty != "OKP" || j.Crv != "Ed25519" {
			return subtleErr(errData, "not an Ed25519 JWK"), nil
		}
		// "alg" is optional, but if present it must be the algorithm's own name or
		// the JOSE name for the family. A near miss like "EDDSA" is a mistake, and
		// accepting it would mean importing a key under an algorithm nobody named.
		if j.Alg != "" && j.Alg != "Ed25519" && j.Alg != "EdDSA" {
			return subtleErr(errData, "JWK alg "+j.Alg+" is not Ed25519"), nil
		}
		// "x" is required for BOTH halves: a private OKP JWK carries the public key
		// beside the seed, and the two have to agree.
		x, err := b64u.DecodeString(j.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return subtleErr(errData, "bad JWK x"), nil
		}
		if j.D == "" {
			return spidermonkey.ValueOf(map[string]any{
				"id": s.put(&subtleKey{edPub: ed25519.PublicKey(x)}), "type": "public",
			}), nil
		}
		seed, err := b64u.DecodeString(j.D)
		if err != nil || len(seed) != ed25519.SeedSize {
			return subtleErr(errData, "bad JWK d"), nil
		}
		priv := ed25519.NewKeyFromSeed(seed)
		if !bytes.Equal(priv.Public().(ed25519.PublicKey), x) {
			return subtleErr(errData, "JWK x is not the public key of d"), nil
		}
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{edPriv: priv}), "type": "private",
		}), nil
	case "raw":
		x, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		if len(x) != ed25519.PublicKeySize {
			return subtleErr(errData, "bad raw Ed25519 public key"), nil
		}
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{edPub: ed25519.PublicKey(append([]byte(nil), x...))}), "type": "public",
		}), nil
	case "pkcs8":
		der, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		key, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		priv, ok := key.(ed25519.PrivateKey)
		if !ok {
			return subtleErr(errData, "pkcs8 key is not Ed25519"), nil
		}
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{edPriv: priv}), "type": "private",
		}), nil
	case "spki":
		der, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		key, err := x509.ParsePKIXPublicKey(der)
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		pub, ok := key.(ed25519.PublicKey)
		if !ok {
			return subtleErr(errData, "spki key is not Ed25519"), nil
		}
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{edPub: pub}), "type": "public",
		}), nil
	}
	return subtleErr(errData, "ed25519 import: unsupported format"), nil
}

func (s *subtleAPI) opEdExport(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("ed25519 export: (format, key) required")
	}
	k, err := s.get(args[1])
	if err != nil {
		return nil, err
	}
	pub := k.edPub
	if pub == nil && k.edPriv != nil {
		pub = k.edPriv.Public().(ed25519.PublicKey)
	}
	if pub == nil {
		return nil, fmt.Errorf("not an Ed25519 key")
	}
	switch args[0].String() {
	case "jwk":
		j := jwkDoc{Kty: "OKP", Crv: "Ed25519", Alg: "Ed25519", Ext: true, X: b64u.EncodeToString(pub)}
		if k.edPriv != nil {
			j.D = b64u.EncodeToString(k.edPriv.Seed())
		}
		out, err := json.Marshal(j)
		if err != nil {
			return nil, err
		}
		return spidermonkey.ValueOf(string(out)), nil
	case "raw", "raw-public":
		return bytesValue(pub), nil
	case "pkcs8":
		if k.edPriv == nil {
			return nil, fmt.Errorf("pkcs8 export needs a private key")
		}
		der, err := x509.MarshalPKCS8PrivateKey(k.edPriv)
		if err != nil {
			return nil, err
		}
		return bytesValue(der), nil
	case "spki":
		der, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			return nil, err
		}
		return bytesValue(der), nil
	}
	return nil, fmt.Errorf("ed25519 export: unsupported format")
}

func (s *subtleAPI) opEdSign(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("ed25519 sign: (key, data) required")
	}
	k, err := s.get(args[0])
	if err != nil {
		return nil, err
	}
	if k.edPriv == nil {
		return nil, fmt.Errorf("sign needs an Ed25519 private key")
	}
	data, err := argBytes(args[1])
	if err != nil {
		return nil, err
	}
	return bytesValue(ed25519.Sign(k.edPriv, data)), nil
}

func (s *subtleAPI) opEdVerify(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("ed25519 verify: (key, sig, data) required")
	}
	k, err := s.get(args[0])
	if err != nil {
		return nil, err
	}
	pub := k.edPub
	if pub == nil && k.edPriv != nil {
		pub = k.edPriv.Public().(ed25519.PublicKey)
	}
	if pub == nil {
		return nil, fmt.Errorf("verify needs an Ed25519 key")
	}
	sig, err := argBytes(args[1])
	if err != nil {
		return nil, err
	}
	data, err := argBytes(args[2])
	if err != nil {
		return nil, err
	}
	return spidermonkey.ValueOf(ed25519.Verify(pub, data, sig)), nil
}

// ops returns the host-function table js/subtle.js expects on __web_ops.
func (s *subtleAPI) ops() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"subtle_digest":          s.opDigest,
		"subtle_hmac_import":     s.opHMACImport,
		"subtle_hmac_export":     s.opHMACExport,
		"subtle_hmac_sign":       s.opHMACSign,
		"subtle_hmac_verify":     s.opHMACVerify,
		"subtle_ec_generate":     s.opECGenerate,
		"subtle_ec_import_jwk":   s.opECImportJWK,
		"subtle_ec_import_der":   s.opECImportDER,
		"subtle_ec_export_jwk":   s.opECExportJWK,
		"subtle_ec_export_der":   s.opECExportDER,
		"subtle_ec_sign":         s.opECSign,
		"subtle_ec_verify":       s.opECVerify,
		"subtle_rsa_generate":    s.opRSAGenerate,
		"subtle_rsa_import_jwk":  s.opRSAImportJWK,
		"subtle_rsa_import_der":  s.opRSAImportDER,
		"subtle_rsa_export_jwk":  s.opRSAExportJWK,
		"subtle_rsa_export_der":  s.opRSAExportDER,
		"subtle_rsa_sign":        s.opRSASign,
		"subtle_rsa_verify":      s.opRSAVerify,
		"subtle_x25519_generate": s.opX25519Generate,
		"subtle_x25519_import":   s.opX25519Import,
		"subtle_x25519_export":   s.opX25519Export,
		"subtle_x25519_derive":   s.opX25519Derive,
		"subtle_ed_generate":     s.opEdGenerate,
		"subtle_ed_import":       s.opEdImport,
		"subtle_ed_export":       s.opEdExport,
		"subtle_ed_sign":         s.opEdSign,
		"subtle_ed_verify":       s.opEdVerify,
	}
}

// ecCurveOIDs maps the named-curve OID an EC SubjectPublicKeyInfo carries in its
// algorithm parameters onto the curve.
var ecCurveOIDs = []struct {
	oid   asn1.ObjectIdentifier
	curve elliptic.Curve
}{
	{asn1.ObjectIdentifier{1, 2, 840, 10045, 3, 1, 7}, elliptic.P256()},
	{asn1.ObjectIdentifier{1, 3, 132, 0, 34}, elliptic.P384()},
	{asn1.ObjectIdentifier{1, 3, 132, 0, 35}, elliptic.P521()},
}

// parseECSPKICompressed reads an EC SubjectPublicKeyInfo whose point is
// compressed. It exists only because crypto/x509 rejects that form; the
// encoding is otherwise the one x509 already understands, so only the point
// needs different treatment.
func parseECSPKICompressed(der []byte) (*ecdsa.PublicKey, error) {
	var doc struct {
		Algorithm struct {
			Algorithm  asn1.ObjectIdentifier
			Parameters asn1.ObjectIdentifier
		}
		PublicKey asn1.BitString
	}
	if _, err := asn1.Unmarshal(der, &doc); err != nil {
		return nil, err
	}
	if !doc.Algorithm.Algorithm.Equal(asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}) {
		return nil, fmt.Errorf("spki key is not EC")
	}
	for _, c := range ecCurveOIDs {
		if !c.oid.Equal(doc.Algorithm.Parameters) {
			continue
		}
		x, y := elliptic.UnmarshalCompressed(c.curve, doc.PublicKey.Bytes)
		if x == nil {
			return nil, fmt.Errorf("not a point on the named curve")
		}
		return &ecdsa.PublicKey{Curve: c.curve, X: x, Y: y}, nil
	}
	return nil, fmt.Errorf("unsupported EC curve")
}

// ------------------------- the encryption half of crypto.subtle —
// AES-GCM/CBC/CTR
// encrypt/decrypt and AES key material, plus ECDH/HKDF/PBKDF2 deriveBits.
// This lifts the surface from the JWS-only set to JWE-capable.

// maxSubtleKDFBytes bounds a guest-requested derived-key length so it can't
// drive an unbounded host allocation (a fatal Go OOM). maxSubtlePBKDF2Iter
// bounds the iteration count (uninterruptible host CPU guard).
const (
	maxSubtleKDFBytes   = 1 << 24 // 16 MiB
	maxSubtlePBKDF2Iter = 100_000_000
)

// hashNewByName reuses hashByName's crypto.Hash lookup (the hashes are
// registered by this file's blank imports) and returns its constructor.
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
		return subtleErr(errOperation, err.Error()), nil
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
			return subtleErr(errOperation, "AES-GCM IV must not be empty"), nil
		}
		switch tagBytes {
		case 4, 8, 12, 13, 14, 15, 16:
		default:
			return subtleErr(errOperation, "AES-GCM tagLength must be 32, 64, 96, 104, 112, 120 or 128 bits"), nil
		}
		gcm, err := cipher.NewGCMWithNonceSize(block, len(iv))
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		if encrypt {
			sealed := gcm.Seal(nil, iv, data, aad)
			// sealed is ciphertext||tag16; keep only the requested tag bytes.
			return bytesValue(sealed[:len(data)+tagBytes]), nil
		}
		if len(data) < tagBytes {
			return subtleErr(errOperation, "decryption failed"), nil
		}
		pt, ok := gcmOpenTruncated(gcm, iv, data, aad, tagBytes)
		if !ok {
			return subtleErr(errOperation, "decryption failed"), nil
		}
		return bytesValue(pt), nil
	case "AES-CBC":
		bs := block.BlockSize()
		if len(iv) != bs {
			return subtleErr(errOperation, "AES-CBC IV must be 16 bytes"), nil
		}
		if encrypt {
			padded := pad7(data, bs)
			out := make([]byte, len(padded))
			cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
			return bytesValue(out), nil
		}
		if len(data) == 0 || len(data)%bs != 0 {
			return subtleErr(errOperation, "bad block size"), nil
		}
		out := make([]byte, len(data))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
		unpadded, err := unpad7(out, bs)
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		return bytesValue(unpadded), nil
	case "AES-CTR":
		if len(iv) != block.BlockSize() {
			return subtleErr(errOperation, "AES-CTR counter block must be 16 bytes"), nil
		}
		ctrBits := 128
		if len(args) > 6 && !args[6].IsUndefined() {
			ctrBits = intArg(args[6])
		}
		if ctrBits <= 0 || ctrBits > 128 {
			return subtleErr(errOperation, "AES-CTR length must be in 1..128"), nil
		}
		return bytesValue(aesCTR(block, iv, ctrBits, data)), nil
	}
	return subtleErr(errOperation, fmt.Sprintf("unsupported AES mode %q", mode)), nil
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
		return subtleErr(errInvalidAccess, "ECDH needs a private and a public EC key"), nil
	}
	if privKey.ecPriv.Curve != pubKey.ecPub.Curve {
		return subtleErr(errInvalidAccess, "ECDH keys are on different curves"), nil
	}
	priv, err := privKey.ecPriv.ECDH()
	if err != nil {
		return subtleErr(errInvalidAccess, err.Error()), nil
	}
	pub, err := pubKey.ecPub.ECDH()
	if err != nil {
		return subtleErr(errInvalidAccess, err.Error()), nil
	}
	secret, err := priv.ECDH(pub)
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	// A requested length longer than the shared secret is an OperationError in
	// WebCrypto, not a silently-shortened (weaker) key.
	bits := intArg(args[2])
	if bits > 0 {
		want := bits / 8
		if want > len(secret) {
			return subtleErr(errOperation, "requested length exceeds the ECDH shared secret"), nil
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
		return subtleErr(errOperation, err.Error()), nil
	}
	ikm, _ := argBytes(args[1])
	salt, _ := argBytes(args[2])
	info, _ := argBytes(args[3])
	length := intArg(args[4]) / 8
	if length < 0 || length > maxSubtleKDFBytes {
		return subtleErr(errOperation, "invalid derived-bits length"), nil
	}
	if length == 0 {
		return bytesValue(nil), nil
	}
	r := hkdf.New(newHash, ikm, salt, info)
	out := make([]byte, length)
	if _, err := r.Read(out); err != nil {
		return subtleErr(errOperation, err.Error()), nil
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
		return subtleErr(errOperation, err.Error()), nil
	}
	pw, _ := argBytes(args[1])
	salt, _ := argBytes(args[2])
	iter := intArg(args[3])
	// iterations < 1 would silently degrade to a one-round KDF (no stretching);
	// WebCrypto requires iterations >= 1.
	if iter < 1 || iter > maxSubtlePBKDF2Iter {
		return subtleErr(errOperation, "PBKDF2 iterations out of range"), nil
	}
	length := intArg(args[4]) / 8
	if length < 0 || length > maxSubtleKDFBytes {
		return subtleErr(errOperation, "invalid derived-bits length"), nil
	}
	// A zero-length derivation is a legal request for an empty key; Go's
	// pbkdf2 does not accept a zero key length, so answer it directly.
	if length == 0 {
		return bytesValue(nil), nil
	}
	return bytesValue(pbkdf2.Key(pw, salt, iter, length, newHash)), nil
}

// domError is the DOMException name the Web Crypto spec assigns to a failure.
// Which one is returned is part of the contract — the suite checks it on every
// rejected call — so it travels as its own field. It used to be prefixed onto
// the message and recovered by matching at the JS side, which meant any message
// beginning with a word ending in "Error" was silently read as a name.
type domError string

const (
	errData          domError = "DataError"
	errOperation     domError = "OperationError"
	errNotSupported  domError = "NotSupportedError"
	errInvalidAccess domError = "InvalidAccessError"
	errSyntax        domError = "SyntaxError"
)

func subtleErr(name domError, msg string) spidermonkey.Value {
	return spidermonkey.ValueOf(map[string]any{
		"__subtleError": true, "name": string(name), "message": msg,
	})
}

// opRSAOAEP(encrypt, keyHandle, hash, data, label) -> bytes. Uses the RSA key
// table above (opRSAImport* store handles).
func (s *subtleAPI) opRSAOAEP(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("rsa-oaep: (encrypt, keyHandle, hash, data, label?) required")
	}
	encrypt := args[0].Bool()
	k, err := s.get(args[1])
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	newHash, err := hashNewByName(args[2].String())
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
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
			return subtleErr(errOperation, "not an RSA key"), nil
		}
		ct, e := rsa.EncryptOAEP(newHash(), rand.Reader, pub, data, label)
		if e != nil {
			return subtleErr(errOperation, e.Error()), nil
		}
		return bytesValue(ct), nil
	}
	if k.rsaPriv == nil {
		return subtleErr(errOperation, "decrypt needs an RSA private key"), nil
	}
	pt, e := rsa.DecryptOAEP(newHash(), rand.Reader, k.rsaPriv, data, label)
	if e != nil {
		return subtleErr(errOperation, "decryption failed"), nil
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
		return subtleErr(errOperation, err.Error()), nil
	}
	wrap := aesKWWrap
	if !args[0].Bool() {
		wrap = aesKWUnwrap
	}
	out, err := wrap(block, data)
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
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
		"subtle_mldsa_generate":    s.opMLDSAGenerate,
		"subtle_mldsa_import":      s.opMLDSAImport,
		"subtle_mldsa_export":      s.opMLDSAExport,
		"subtle_mldsa_sign":        s.opMLDSASign,
		"subtle_mldsa_verify":      s.opMLDSAVerify,
		"subtle_hkdf":              s.opHKDFDerive,
		"subtle_pbkdf2":            s.opPBKDF2Derive,
		"subtle_rsa_oaep":          s.opRSAOAEP,
		"subtle_chacha":            s.opChaChaSeal,
		"subtle_kmac":              s.opKMAC,
		"subtle_aes_ocb":           s.opAESOCB,
		"subtle_turboshake":        s.opTurboSHAKE,
		"subtle_kangarootwelve":    s.opKangarooTwelve,
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

// ------------------------- the key encoding the post-quantum algorithms share.
//
// ML-KEM (FIPS 203) and ML-DSA (FIPS 204) are both "algorithm key pair" keys,
// and their DER is the same shape: an SPKI is the algorithm OID plus the public
// key in a BIT STRING, and a PKCS#8 is the OID plus the generation seed in a
// [0] choice inside the private-key OCTET STRING. crypto/x509 marshals neither
// yet, so both are hand-encoded here — they are small and fully specified, and
// the OID is the only thing that distinguishes one algorithm's key from
// another's, which is what lets an import refuse to reinterpret a key as the
// wrong parameter set.

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

// akpSPKI encodes a public key as SubjectPublicKeyInfo.
func akpSPKI(oid asn1.ObjectIdentifier, pub []byte) ([]byte, error) {
	return asn1.Marshal(spkiDoc{
		Algorithm: pkixAlgorithm{oid},
		PublicKey: asn1.BitString{Bytes: pub, BitLength: len(pub) * 8},
	})
}

// The tag the private-key OCTET STRING wraps its payload in. The two families
// differ only here: an AKP seed is the [0] alternative of a CHOICE, while an
// RFC 8410 curve key is a CurvePrivateKey, which is a nested OCTET STRING.
const (
	akpSeedTag   = 0x80
	curveSeedTag = 0x04
)

// akpPKCS8 encodes a seed as a PrivateKeyInfo.
func akpPKCS8(oid asn1.ObjectIdentifier, seed []byte) ([]byte, error) {
	return derPKCS8(oid, akpSeedTag, seed)
}

func derPKCS8(oid asn1.ObjectIdentifier, tag byte, seed []byte) ([]byte, error) {
	inner := append([]byte{tag, byte(len(seed))}, seed...)
	return asn1.Marshal(pkcs8Doc{Version: 0, Algorithm: pkixAlgorithm{oid}, PrivateKey: inner})
}

// akpParseSPKI reads back what akpSPKI wrote, reporting the OID so the caller
// can decide whether it names the parameter set that was asked for.
func akpParseSPKI(der []byte) (asn1.ObjectIdentifier, []byte, error) {
	var doc spkiDoc
	if _, err := asn1.Unmarshal(der, &doc); err != nil {
		return nil, nil, err
	}
	return doc.Algorithm.Algorithm, doc.PublicKey.Bytes, nil
}

// akpParsePKCS8 reads back what akpPKCS8 wrote. Only the seed alternative is
// accepted: the expanded-key and both-halves alternatives are legal DER but
// carry material this layer has no way to import.
func akpParsePKCS8(der []byte) (asn1.ObjectIdentifier, []byte, error) {
	return derParsePKCS8(der, akpSeedTag)
}

func derParsePKCS8(der []byte, tag byte) (asn1.ObjectIdentifier, []byte, error) {
	var doc pkcs8Doc
	if _, err := asn1.Unmarshal(der, &doc); err != nil {
		return nil, nil, err
	}
	b := doc.PrivateKey
	if len(b) < 2 || b[0] != tag || int(b[1]) != len(b)-2 {
		return nil, nil, fmt.Errorf("private key is not a seed")
	}
	return doc.Algorithm.Algorithm, b[2:], nil
}

// ------------------------- ChaCha20-Poly1305. It is a separate AEAD from
// the AES family — a fixed 256-bit key, a 96-bit nonce and a 128-bit tag, none
// of them negotiable — so it gets its own op rather than another mode inside
// the AES switch, where every one of those would have to be re-checked.

// opChaChaSeal(encrypt, key, nonce, data, aad) -> bytes | error.
func (s *subtleAPI) opChaChaSeal(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("chacha20-poly1305: (encrypt, key, nonce, data, aad?) required")
	}
	encrypt := args[0].Bool()
	key, err := argBytes(args[1])
	if err != nil {
		return nil, err
	}
	nonce, err := argBytes(args[2])
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
	if len(key) != chacha20poly1305.KeySize {
		return subtleErr(errOperation, "ChaCha20-Poly1305 key must be 256 bits"), nil
	}
	// The nonce length is fixed at 96 bits. Saying so is better than letting the
	// construction fail somewhere less legible.
	if len(nonce) != chacha20poly1305.NonceSize {
		return subtleErr(errOperation, "ChaCha20-Poly1305 nonce must be 96 bits"), nil
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	if encrypt {
		return bytesValue(aead.Seal(nil, nonce, data, aad)), nil
	}
	if len(data) < aead.Overhead() {
		return subtleErr(errOperation, "decryption failed"), nil
	}
	pt, err := aead.Open(nil, nonce, data, aad)
	if err != nil {
		return subtleErr(errOperation, "decryption failed"), nil
	}
	return bytesValue(pt), nil
}

// ------------------------- Ed448 signing and X448 key agreement.
//
// These are the 448-bit halves of the same pair the 25519 curves form, and the
// Web Crypto suite exercises them identically. Go's standard library has
// neither, so both come from CIRCL. The formats are the same shapes their
// 25519 siblings use, at the sizes this curve defines: a 57-byte Ed448 public
// key, a 56-byte X448 one.

// The RFC 8410 OIDs. crypto/x509 marshals the 25519 curves but knows neither of
// these, so their SPKI and PKCS#8 are encoded here — the same two shapes, with
// the seed inside a CurvePrivateKey (see the AKP section above).
var (
	oidEd448 = asn1.ObjectIdentifier{1, 3, 101, 113}
	oidX448  = asn1.ObjectIdentifier{1, 3, 101, 111}
)

// curveSPKI/curvePKCS8 and their parsers name the OID they expect, so importing
// an X448 key as Ed448 is refused rather than reinterpreted.
func curvePKCS8(oid asn1.ObjectIdentifier, seed []byte) ([]byte, error) {
	return derPKCS8(oid, curveSeedTag, seed)
}

func curveParsePKCS8(der []byte, want asn1.ObjectIdentifier, size int) ([]byte, error) {
	oid, seed, err := derParsePKCS8(der, curveSeedTag)
	if err != nil {
		return nil, err
	}
	if !oid.Equal(want) {
		return nil, fmt.Errorf("pkcs8 key is not %v", want)
	}
	if len(seed) != size {
		return nil, fmt.Errorf("private key must be %d bytes", size)
	}
	return seed, nil
}

func curveParseSPKI(der []byte, want asn1.ObjectIdentifier, size int) ([]byte, error) {
	oid, pub, err := akpParseSPKI(der)
	if err != nil {
		return nil, err
	}
	if !oid.Equal(want) {
		return nil, fmt.Errorf("spki key is not %v", want)
	}
	if len(pub) != size {
		return nil, fmt.Errorf("public key must be %d bytes", size)
	}
	return pub, nil
}

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
			return subtleErr(errOperation, err.Error()), nil
		}
		if len(raw) != ed448.PublicKeySize {
			return subtleErr(errData, fmt.Sprintf("Ed448 public key must be %d bytes", ed448.PublicKeySize)), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{ed448Pub: ed448.PublicKey(raw)})}), nil
	case "raw-seed":
		raw, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		if len(raw) != ed448.SeedSize {
			return subtleErr(errData, fmt.Sprintf("Ed448 seed must be %d bytes", ed448.SeedSize)), nil
		}
		priv := ed448.NewKeyFromSeed(raw)
		return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{ed448Priv: priv})}), nil
	case "spki":
		der, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		pub, err := curveParseSPKI(der, oidEd448, ed448.PublicKeySize)
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{ed448Pub: ed448.PublicKey(pub)})}), nil
	case "pkcs8":
		der, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		seed, err := curveParsePKCS8(der, oidEd448, ed448.SeedSize)
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{ed448Priv: ed448.NewKeyFromSeed(seed)})}), nil
	case "jwk":
		var jwk struct{ Kty, Crv, Alg, X, D string }
		if err := json.Unmarshal([]byte(args[1].String()), &jwk); err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		if jwk.Kty != "OKP" || jwk.Crv != "Ed448" {
			return subtleErr(errData, "not an Ed448 JWK"), nil
		}
		if jwk.Alg != "" && jwk.Alg != "Ed448" && jwk.Alg != "EdDSA" {
			return subtleErr(errData, "JWK alg "+jwk.Alg+" is not Ed448"), nil
		}
		// "x" is required for both halves, and must be the public key that "d"
		// derives; a JWK that disagrees with itself is not a key.
		pub, err := base64.RawURLEncoding.DecodeString(jwk.X)
		if err != nil || len(pub) != ed448.PublicKeySize {
			return subtleErr(errData, "bad Ed448 JWK x"), nil
		}
		if jwk.D != "" {
			seed, err := base64.RawURLEncoding.DecodeString(jwk.D)
			if err != nil || len(seed) != ed448.SeedSize {
				return subtleErr(errData, "bad Ed448 JWK d"), nil
			}
			priv := ed448.NewKeyFromSeed(seed)
			if !bytes.Equal(priv.Public().(ed448.PublicKey), pub) {
				return subtleErr(errData, "JWK x is not the public key of d"), nil
			}
			return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{ed448Priv: priv})}), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{ed448Pub: ed448.PublicKey(pub)})}), nil
	}
	return subtleErr(errNotSupported, "unsupported Ed448 key format"), nil
}

func (s *subtleAPI) opEd448Export(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("ed448 export: (format, handle) required")
	}
	k, err := s.get(args[1])
	if err != nil {
		return subtleErr(errInvalidAccess, "unknown key"), nil
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
			return subtleErr(errInvalidAccess, "raw-seed export needs a private key"), nil
		}
		return bytesValueOK(k.ed448Priv.Seed())
	case "spki":
		der, err := akpSPKI(oidEd448, pub)
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		return bytesValueOK(der)
	case "pkcs8":
		if k.ed448Priv == nil {
			return subtleErr(errInvalidAccess, "pkcs8 export needs a private key"), nil
		}
		der, err := curvePKCS8(oidEd448, k.ed448Priv.Seed())
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		return bytesValueOK(der)
	case "jwk":
		out := map[string]any{"kty": "OKP", "crv": "Ed448", "alg": "Ed448", "x": b64(pub)}
		if k.ed448Priv != nil {
			out["d"] = b64(k.ed448Priv.Seed())
		}
		return spidermonkey.ValueOf(out), nil
	}
	return subtleErr(errNotSupported, "unsupported Ed448 export format"), nil
}

func (s *subtleAPI) opEd448Sign(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("ed448 sign: (handle, data) required")
	}
	k, err := s.get(args[0])
	if err != nil || k.ed448Priv == nil {
		return subtleErr(errInvalidAccess, "not an Ed448 private key"), nil
	}
	msg, err := argBytes(args[1])
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
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
			return subtleErr(errOperation, err.Error()), nil
		}
		if len(raw) != x448.Size {
			return subtleErr(errData, fmt.Sprintf("X448 public key must be %d bytes", x448.Size)), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{x448Pub: asKey(raw)})}), nil
	case "raw-seed":
		raw, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		if len(raw) != x448.Size {
			return subtleErr(errData, fmt.Sprintf("X448 private key must be %d bytes", x448.Size)), nil
		}
		return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{x448Priv: asKey(raw)})}), nil
	case "spki":
		der, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		pub, err := curveParseSPKI(der, oidX448, x448.Size)
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{x448Pub: asKey(pub)})}), nil
	case "pkcs8":
		der, err := argBytes(args[1])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		d, err := curveParsePKCS8(der, oidX448, x448.Size)
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{x448Priv: asKey(d)})}), nil
	case "jwk":
		var jwk struct{ Kty, Crv, X, D string }
		if err := json.Unmarshal([]byte(args[1].String()), &jwk); err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		if jwk.Kty != "OKP" || jwk.Crv != "X448" {
			return subtleErr(errData, "not an X448 JWK"), nil
		}
		x, err := base64.RawURLEncoding.DecodeString(jwk.X)
		if err != nil || len(x) != x448.Size {
			return subtleErr(errData, "bad X448 JWK x"), nil
		}
		if jwk.D != "" {
			d, err := base64.RawURLEncoding.DecodeString(jwk.D)
			if err != nil || len(d) != x448.Size {
				return subtleErr(errData, "bad X448 JWK d"), nil
			}
			priv := asKey(d)
			var derived x448.Key
			x448.KeyGen(&derived, priv)
			if !bytes.Equal(derived[:], x) {
				return subtleErr(errData, "JWK x is not the public key of d"), nil
			}
			return spidermonkey.ValueOf(map[string]any{"priv": s.put(&subtleKey{x448Priv: priv})}), nil
		}
		return spidermonkey.ValueOf(map[string]any{"pub": s.put(&subtleKey{x448Pub: asKey(x)})}), nil
	}
	return subtleErr(errNotSupported, "unsupported X448 key format"), nil
}

func (s *subtleAPI) opX448Export(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("x448 export: (format, handle) required")
	}
	k, err := s.get(args[1])
	if err != nil {
		return subtleErr(errInvalidAccess, "unknown key"), nil
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
			return subtleErr(errInvalidAccess, "raw-seed export needs a private key"), nil
		}
		return bytesValueOK(k.x448Priv[:])
	case "spki":
		der, err := akpSPKI(oidX448, pub[:])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		return bytesValueOK(der)
	case "pkcs8":
		if k.x448Priv == nil {
			return subtleErr(errInvalidAccess, "pkcs8 export needs a private key"), nil
		}
		der, err := curvePKCS8(oidX448, k.x448Priv[:])
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		return bytesValueOK(der)
	case "jwk":
		out := map[string]any{"kty": "OKP", "crv": "X448", "x": b64(pub[:])}
		if k.x448Priv != nil {
			out["d"] = b64(k.x448Priv[:])
		}
		return spidermonkey.ValueOf(out), nil
	}
	return subtleErr(errNotSupported, "unsupported X448 export format"), nil
}

// opX448Derive(priv, pub, bits) -> shared secret.
func (s *subtleAPI) opX448Derive(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("x448 derive: (priv, pub) required")
	}
	priv, perr := s.get(args[0])
	pub, uerr := s.get(args[1])
	if perr != nil || uerr != nil || priv.x448Priv == nil || pub.x448Pub == nil {
		return subtleErr(errInvalidAccess, "X448 derive needs a private and a public key"), nil
	}
	var shared x448.Key
	// A low-order public key yields an all-zero secret, which the function
	// reports rather than returning: agreeing on zero is not agreement.
	if !x448.Shared(&shared, priv.x448Priv, pub.x448Pub) {
		return subtleErr(errOperation, "X448 produced an all-zero shared secret"), nil
	}
	out := shared[:]
	if len(args) > 2 && !args[2].IsUndefined() {
		bits := intArg(args[2])
		if bits > 0 {
			if bits%8 != 0 || bits/8 > len(out) {
				return subtleErr(errOperation, "X448 cannot produce that many bits"), nil
			}
			out = out[:bits/8]
		}
	}
	return bytesValueOK(out)
}

// ------------------------- KMAC128/KMAC256 (NIST SP 800-185).
//
// KMAC is cSHAKE with a prescribed way of feeding it: the key, the message and
// the requested output length each go in with a length prefix, so that no two
// different (key, message, length) triples can produce the same input stream.
// x/crypto/sha3 provides cSHAKE; what is missing is those encodings, which are
// small and exact enough to be worth having here rather than pulling in a
// dependency for.

// leftEncode/rightEncode write an integer as a byte string carrying its own
// length — leading for one, trailing for the other. That is what makes the
// concatenation below unambiguous.
func leftEncode(v uint64) []byte {
	buf := encodeBytes(v)
	return append([]byte{byte(len(buf))}, buf...)
}

func rightEncode(v uint64) []byte {
	buf := encodeBytes(v)
	return append(buf, byte(len(buf)))
}

// encodeBytes is the minimal big-endian representation, with zero taking one
// byte rather than none.
func encodeBytes(v uint64) []byte {
	if v == 0 {
		return []byte{0}
	}
	var b []byte
	for x := v; x > 0; x >>= 8 {
		b = append([]byte{byte(x)}, b...)
	}
	return b
}

// encodeString prefixes a byte string with its length IN BITS.
func encodeString(s []byte) []byte {
	return append(leftEncode(uint64(len(s))*8), s...)
}

// bytePad left-encodes the width, appends X, and zero-fills to a multiple of
// that width — so the key always occupies a whole number of sponge blocks and
// cannot run into the message.
func bytePad(x []byte, w int) []byte {
	out := append(leftEncode(uint64(w)), x...)
	if rem := len(out) % w; rem != 0 {
		out = append(out, make([]byte, w-rem)...)
	}
	return out
}

// kmac computes KMAC128/256 over (key, message, customization) for outLen
// bytes. The rate is the cSHAKE block size: 168 for the 128-bit variant, 136
// for the 256-bit one.
func kmac(bits int, key, msg, custom []byte, outLen int) ([]byte, error) {
	var h sha3.ShakeHash
	rate := 168
	switch bits {
	case 128:
		h = sha3.NewCShake128([]byte("KMAC"), custom)
	case 256:
		h = sha3.NewCShake256([]byte("KMAC"), custom)
		rate = 136
	default:
		return nil, fmt.Errorf("kmac: unsupported variant %d", bits)
	}
	if _, err := h.Write(bytePad(encodeString(key), rate)); err != nil {
		return nil, err
	}
	if _, err := h.Write(msg); err != nil {
		return nil, err
	}
	// The output length is part of the input: KMAC of the same message at two
	// lengths must not be a prefix of the other.
	if _, err := h.Write(rightEncode(uint64(outLen) * 8)); err != nil {
		return nil, err
	}
	out := make([]byte, outLen)
	if _, err := h.Read(out); err != nil {
		return nil, err
	}
	return out, nil
}

// opKMAC(bits, key, message, customization, outputBits) -> bytes | error.
func (s *subtleAPI) opKMAC(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 5 {
		return nil, fmt.Errorf("kmac: (bits, key, message, customization, outputBits) required")
	}
	bits := intArg(args[0])
	key, err := argBytes(args[1])
	if err != nil {
		return nil, err
	}
	msg, err := argBytes(args[2])
	if err != nil {
		return nil, err
	}
	custom, _ := argBytes(args[3])
	outBits := intArg(args[4])
	// Zero is a legal length: a zero-byte digest is what an extendable-output
	// function returns when asked for no output.
	if outBits < 0 || outBits%8 != 0 {
		return subtleErr(errOperation, "KMAC length must be a non-negative multiple of 8"), nil
	}
	if outBits/8 > maxSubtleKDFBytes {
		return subtleErr(errOperation, "KMAC length is too large"), nil
	}
	out, err := kmac(bits, key, msg, custom, outBits/8)
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	return bytesValue(out), nil
}

// opCShake(bits, data, customization, outputBits) -> bytes | error.
//
// cSHAKE is SHAKE with a domain-separation string, and with an empty one it IS
// SHAKE — which is what makes it usable as a plain extendable-output digest.
func (s *subtleAPI) opCShake(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("cshake: (bits, data, customization, outputBits) required")
	}
	bits := intArg(args[0])
	data, err := argBytes(args[1])
	if err != nil {
		return nil, err
	}
	custom, _ := argBytes(args[2])
	outBits := intArg(args[3])
	// Zero is a legal length: a zero-byte digest is what an extendable-output
	// function returns when asked for no output.
	if outBits < 0 || outBits%8 != 0 {
		return subtleErr(errOperation, "cSHAKE length must be a non-negative multiple of 8"), nil
	}
	if outBits/8 > maxSubtleKDFBytes {
		return subtleErr(errOperation, "cSHAKE length is too large"), nil
	}
	var h sha3.ShakeHash
	switch bits {
	case 128:
		h = sha3.NewCShake128(nil, custom)
	case 256:
		h = sha3.NewCShake256(nil, custom)
	default:
		return subtleErr(errOperation, "cSHAKE must be 128 or 256"), nil
	}
	if _, err := h.Write(data); err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	out := make([]byte, outBits/8)
	if _, err := h.Read(out); err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	return bytesValue(out), nil
}

// ------------------------- ML-DSA (FIPS 204), the post-quantum signature scheme.
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

// ------------------------- ML-KEM (FIPS 203), the post-quantum key
// encapsulation mechanism.
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
//
// The encoding itself is in the AKP section, shared with ML-DSA. What is
// ML-KEM's own is
// the mapping from OID back to parameter set: an import must not trust the
// caller's algorithm, so the key says which set it belongs to.

func mlkemByOID(oid asn1.ObjectIdentifier) (mlkemParams, error) {
	for _, p := range mlkemSets {
		if p.oid.Equal(oid) {
			return p, nil
		}
	}
	return mlkemParams{}, fmt.Errorf("not an ML-KEM key this build supports")
}

func mlkemParseSPKI(der []byte) (mlkemParams, []byte, error) {
	oid, pub, err := akpParseSPKI(der)
	if err != nil {
		return mlkemParams{}, nil, err
	}
	set, err := mlkemByOID(oid)
	return set, pub, err
}

func mlkemParsePKCS8(der []byte) (mlkemParams, []byte, error) {
	oid, seed, err := akpParsePKCS8(der)
	if err != nil {
		return mlkemParams{}, nil, err
	}
	set, err := mlkemByOID(oid)
	return set, seed, err
}

// -------------------------------------------------------------------- ops

// opMLKEMGenerate(name) -> {priv, pub} handles.
func (s *subtleAPI) opMLKEMGenerate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("mlkem generate: (name) required")
	}
	set, ok := mlkemSet(args[0].String())
	if !ok {
		return subtleErr(errNotSupported, "unsupported algorithm "+args[0].String()), nil
	}
	var k *mlkemKey
	if set.name == "ML-KEM-512" {
		// Generate from a fresh seed so the key can be exported as one, which is
		// the format the spec prefers for a private ML-KEM key.
		seed := make([]byte, mlkem512.KeySeedSize)
		if _, err := rand.Read(seed); err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		gk, err := mlkemFromSeed(set, seed)
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		k = gk
	} else if set.name == "ML-KEM-768" {
		dk, err := mlkem.GenerateKey768()
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		k = &mlkemKey{set: set, dk768: dk}
	} else {
		dk, err := mlkem.GenerateKey1024()
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		k = &mlkemKey{set: set, dk1k: dk}
	}
	pub, err := mlkemFromPublic(set, k.publicBytes())
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
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
		return subtleErr(errNotSupported, "unsupported algorithm "+declared), nil
	}
	format := args[1].String()

	// mismatch guards the two DER formats, which carry their own OID: importing
	// an ML-KEM-768 SPKI as ML-KEM-1024 is a DataError, not a silent reinterpret.
	finish := func(k *mlkemKey, err error) (spidermonkey.Value, error) {
		if err != nil {
			return subtleErr(errData, err.Error()), nil
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
			return subtleErr(errData, err.Error()), nil
		}
		return finish(mlkemFromSeed(set, b))
	case "raw-public":
		b, err := argBytes(args[2])
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		return finish(mlkemFromPublic(set, b))
	case "pkcs8":
		b, err := argBytes(args[2])
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		got, seed, err := mlkemParsePKCS8(b)
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		if got.name != set.name {
			return subtleErr(errData, "key is "+got.name+", not "+set.name), nil
		}
		return finish(mlkemFromSeed(set, seed))
	case "spki":
		b, err := argBytes(args[2])
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		got, pub, err := mlkemParseSPKI(b)
		if err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		if got.name != set.name {
			return subtleErr(errData, "key is "+got.name+", not "+set.name), nil
		}
		return finish(mlkemFromPublic(set, pub))
	case "jwk":
		var j struct{ Kty, Alg, Priv, Pub string }
		if err := json.Unmarshal([]byte(args[2].String()), &j); err != nil {
			return subtleErr(errData, err.Error()), nil
		}
		if j.Kty != "AKP" {
			return subtleErr(errData, "ML-KEM JWK must have kty AKP"), nil
		}
		if j.Alg != "" && j.Alg != set.name {
			return subtleErr(errData, "JWK alg is "+j.Alg+", not "+set.name), nil
		}
		if j.Priv != "" {
			seed, err := base64.RawURLEncoding.DecodeString(j.Priv)
			if err != nil {
				return subtleErr(errData, "bad JWK priv"), nil
			}
			return finish(mlkemFromSeed(set, seed))
		}
		pub, err := base64.RawURLEncoding.DecodeString(j.Pub)
		if err != nil {
			return subtleErr(errData, "bad JWK pub"), nil
		}
		return finish(mlkemFromPublic(set, pub))
	}
	return subtleErr(errNotSupported, "unsupported ML-KEM key format "+format), nil
}

// opMLKEMExport(format, handle) -> bytes, or a JWK object for "jwk".
func (s *subtleAPI) opMLKEMExport(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("mlkem export: (format, handle) required")
	}
	sk, err := s.get(args[1])
	if err != nil || sk.mlkem == nil {
		return subtleErr(errInvalidAccess, "not an ML-KEM key"), nil
	}
	k := sk.mlkem
	b64 := base64.RawURLEncoding.EncodeToString
	switch args[0].String() {
	case "raw-public":
		return bytesValueOK(k.publicBytes())
	case "raw-seed":
		if !k.private() {
			return subtleErr(errInvalidAccess, "raw-seed export needs a private key"), nil
		}
		return bytesValueOK(k.seed())
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
		der, err := akpPKCS8(k.set.oid, k.seed())
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		return bytesValueOK(der)
	case "jwk":
		out := map[string]any{"kty": "AKP", "alg": k.set.name, "pub": b64(k.publicBytes())}
		if k.private() {
			out["priv"] = b64(k.seed())
		}
		return spidermonkey.ValueOf(out), nil
	}
	return subtleErr(errNotSupported, "unsupported ML-KEM export format"), nil
}

// opMLKEMEncapsulate(handle) -> {sharedKey, ciphertext}.
func (s *subtleAPI) opMLKEMEncapsulate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("mlkem encapsulate: (handle) required")
	}
	sk, err := s.get(args[0])
	if err != nil || sk.mlkem == nil {
		return subtleErr(errInvalidAccess, "not an ML-KEM key"), nil
	}
	k := sk.mlkem
	// Encapsulation needs the public half, which a private handle can derive.
	pub, perr := mlkemFromPublic(k.set, k.publicBytes())
	if perr != nil {
		return subtleErr(errOperation, perr.Error()), nil
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
			return subtleErr(errOperation, rerr.Error()), nil
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
		return subtleErr(errInvalidAccess, "decapsulation needs an ML-KEM private key"), nil
	}
	ct, err := argBytes(args[1])
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	var shared []byte
	switch {
	case sk.mlkem.dk512 != nil:
		if len(ct) != mlkem512.CiphertextSize {
			return subtleErr(errOperation, fmt.Sprintf("ML-KEM-512 ciphertext must be %d bytes", mlkem512.CiphertextSize)), nil
		}
		shared = make([]byte, mlkem512.SharedKeySize)
		sk.mlkem.dk512.DecapsulateTo(shared, ct)
	case sk.mlkem.dk768 != nil:
		shared, err = sk.mlkem.dk768.Decapsulate(ct)
	default:
		shared, err = sk.mlkem.dk1k.Decapsulate(ct)
	}
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	return bytesValueOK(shared)
}

// ------------------------- AES-OCB (RFC 7253).
//
// OCB is an authenticated mode built from a block cipher and nothing else — no
// GHASH, no second pass — which is why the spec draft picks it up and why Go's
// standard library, which ships only GCM and CCM-adjacent modes, has no
// implementation to borrow. It is written out here against RFC 7253 and pinned
// to that document's own vectors.
//
// The shape: a per-block "offset" walks a Gray-code sequence of precomputed L
// values, each block is encrypted as E(P XOR Offset) XOR Offset, and the tag
// comes from the checksum of the plaintext plus a hash of the associated data.

const ocbBlock = 16

// ocbDouble is multiplication by x in GF(2^128): a left shift, and if the top
// bit fell off, a reduction by the field polynomial's low term.
func ocbDouble(s []byte) []byte {
	out := make([]byte, ocbBlock)
	carry := s[0] >> 7
	for i := 0; i < ocbBlock-1; i++ {
		out[i] = s[i]<<1 | s[i+1]>>7
	}
	out[ocbBlock-1] = s[ocbBlock-1] << 1
	// 0x87 is the low end of x^128 + x^7 + x^2 + x + 1.
	out[ocbBlock-1] ^= carry * 0x87
	return out
}

func xorBlock(dst, a, b []byte) {
	for i := range dst {
		dst[i] = a[i] ^ b[i]
	}
}

// ocbState holds what is derived from the key alone, so it can be computed once
// per operation rather than once per block.
type ocbState struct {
	block cipher.Block
	lStar []byte
	lDol  []byte
	l     [][]byte // l[i] = double^(i+1)(L_$)
}

func newOCB(key []byte) (*ocbState, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	s := &ocbState{block: block}
	s.lStar = make([]byte, ocbBlock)
	block.Encrypt(s.lStar, make([]byte, ocbBlock))
	s.lDol = ocbDouble(s.lStar)
	// Enough L values for any message this can be handed; more are grown below.
	s.l = append(s.l, ocbDouble(s.lDol))
	return s, nil
}

// lAt returns L_i, extending the table as needed. i is the number of trailing
// zeros of the block index, so it grows only logarithmically with the message.
func (s *ocbState) lAt(i int) []byte {
	for len(s.l) <= i {
		s.l = append(s.l, ocbDouble(s.l[len(s.l)-1]))
	}
	return s.l[i]
}

// hash is RFC 7253's HASH(K, A): the associated data folded into one block.
func (s *ocbState) hash(a []byte) []byte {
	sum := make([]byte, ocbBlock)
	offset := make([]byte, ocbBlock)
	tmp := make([]byte, ocbBlock)
	i := 1
	for len(a) >= ocbBlock {
		xorBlock(offset, offset, s.lAt(bits.TrailingZeros(uint(i))))
		xorBlock(tmp, a[:ocbBlock], offset)
		s.block.Encrypt(tmp, tmp)
		xorBlock(sum, sum, tmp)
		a = a[ocbBlock:]
		i++
	}
	if len(a) > 0 {
		xorBlock(offset, offset, s.lStar)
		padded := make([]byte, ocbBlock)
		copy(padded, a)
		padded[len(a)] = 0x80 // the 1 bit that makes padding unambiguous
		xorBlock(tmp, padded, offset)
		s.block.Encrypt(tmp, tmp)
		xorBlock(sum, sum, tmp)
	}
	return sum
}

// initOffset derives Offset_0 from the nonce, per RFC 7253 §4.2. The nonce is
// stretched and then bit-shifted by its own low 6 bits, which is what lets OCB
// accept a nonce shorter than a block without a separate derivation step.
func (s *ocbState) initOffset(nonce []byte, tagLen int) ([]byte, error) {
	if len(nonce) == 0 || len(nonce) > 15 {
		return nil, fmt.Errorf("nonce must be 1..15 bytes")
	}
	n := make([]byte, ocbBlock)
	n[0] = byte(((tagLen * 8) % 128) << 1)
	n[15-len(nonce)] |= 1
	copy(n[ocbBlock-len(nonce):], nonce)

	bottom := int(n[15] & 0x3f)
	n[15] &^= 0x3f
	ktop := make([]byte, ocbBlock)
	s.block.Encrypt(ktop, n)

	stretch := make([]byte, 24)
	copy(stretch, ktop)
	for i := 0; i < 8; i++ {
		stretch[16+i] = ktop[i] ^ ktop[i+1]
	}
	// Offset_0 is the 128 bits of Stretch starting at bit `bottom`.
	offset := make([]byte, ocbBlock)
	shift := bottom / 8
	rem := uint(bottom % 8)
	for i := 0; i < ocbBlock; i++ {
		hi := uint16(stretch[shift+i]) << 8
		lo := uint16(stretch[shift+i+1])
		offset[i] = byte((hi | lo) >> (8 - rem) >> 0 & 0xff)
		if rem == 0 {
			offset[i] = stretch[shift+i]
		}
	}
	return offset, nil
}

// seal encrypts and appends the tag; open verifies and strips it.
func (s *ocbState) seal(nonce, plaintext, aad []byte, tagLen int) ([]byte, error) {
	offset, err := s.initOffset(nonce, tagLen)
	if err != nil {
		return nil, err
	}
	checksum := make([]byte, ocbBlock)
	out := make([]byte, 0, len(plaintext)+tagLen)
	tmp := make([]byte, ocbBlock)
	p := plaintext
	i := 1
	for len(p) >= ocbBlock {
		xorBlock(offset, offset, s.lAt(bits.TrailingZeros(uint(i))))
		xorBlock(checksum, checksum, p[:ocbBlock])
		xorBlock(tmp, p[:ocbBlock], offset)
		s.block.Encrypt(tmp, tmp)
		xorBlock(tmp, tmp, offset)
		out = append(out, tmp...)
		p = p[ocbBlock:]
		i++
	}
	if len(p) > 0 {
		xorBlock(offset, offset, s.lStar)
		pad := make([]byte, ocbBlock)
		s.block.Encrypt(pad, offset)
		ct := make([]byte, len(p))
		for j := range p {
			ct[j] = p[j] ^ pad[j]
		}
		out = append(out, ct...)
		padded := make([]byte, ocbBlock)
		copy(padded, p)
		padded[len(p)] = 0x80
		xorBlock(checksum, checksum, padded)
	}
	tag := make([]byte, ocbBlock)
	xorBlock(tag, checksum, offset)
	xorBlock(tag, tag, s.lDol)
	s.block.Encrypt(tag, tag)
	xorBlock(tag, tag, s.hash(aad))
	return append(out, tag[:tagLen]...), nil
}

func (s *ocbState) open(nonce, ciphertext, aad []byte, tagLen int) ([]byte, error) {
	if len(ciphertext) < tagLen {
		return nil, fmt.Errorf("ciphertext shorter than the tag")
	}
	ct := ciphertext[:len(ciphertext)-tagLen]
	given := ciphertext[len(ciphertext)-tagLen:]
	offset, err := s.initOffset(nonce, tagLen)
	if err != nil {
		return nil, err
	}
	checksum := make([]byte, ocbBlock)
	out := make([]byte, 0, len(ct))
	tmp := make([]byte, ocbBlock)
	c := ct
	i := 1
	for len(c) >= ocbBlock {
		xorBlock(offset, offset, s.lAt(bits.TrailingZeros(uint(i))))
		xorBlock(tmp, c[:ocbBlock], offset)
		s.block.Decrypt(tmp, tmp)
		xorBlock(tmp, tmp, offset)
		out = append(out, tmp...)
		xorBlock(checksum, checksum, tmp)
		c = c[ocbBlock:]
		i++
	}
	if len(c) > 0 {
		xorBlock(offset, offset, s.lStar)
		pad := make([]byte, ocbBlock)
		s.block.Encrypt(pad, offset)
		pt := make([]byte, len(c))
		for j := range c {
			pt[j] = c[j] ^ pad[j]
		}
		out = append(out, pt...)
		padded := make([]byte, ocbBlock)
		copy(padded, pt)
		padded[len(pt)] = 0x80
		xorBlock(checksum, checksum, padded)
	}
	tag := make([]byte, ocbBlock)
	xorBlock(tag, checksum, offset)
	xorBlock(tag, tag, s.lDol)
	s.block.Encrypt(tag, tag)
	xorBlock(tag, tag, s.hash(aad))
	if subtle.ConstantTimeCompare(tag[:tagLen], given) != 1 {
		return nil, fmt.Errorf("authentication failed")
	}
	return out, nil
}

// opAESOCB(encrypt, key, nonce, data, aad, tagBits) -> bytes | error.
func (s *subtleAPI) opAESOCB(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("aes-ocb: (encrypt, key, nonce, data, aad?, tagBits?) required")
	}
	encrypt := args[0].Bool()
	key, err := argBytes(args[1])
	if err != nil {
		return nil, err
	}
	nonce, err := argBytes(args[2])
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
	if tagBytes < 8 || tagBytes > 16 {
		return subtleErr(errOperation, "AES-OCB tagLength must be 64..128 bits"), nil
	}
	st, err := newOCB(key)
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	if encrypt {
		out, err := st.seal(nonce, data, aad, tagBytes)
		if err != nil {
			return subtleErr(errOperation, err.Error()), nil
		}
		return bytesValue(out), nil
	}
	out, err := st.open(nonce, data, aad, tagBytes)
	if err != nil {
		return subtleErr(errOperation, "decryption failed"), nil
	}
	return bytesValue(out), nil
}

// ------------------------- TurboSHAKE and KangarooTwelve (RFC 9861).
//
// These are the two digest algorithms the Web Crypto suite exercises that no Go
// package here provides. They are Keccak with a REDUCED round count — twelve
// instead of twenty-four — which is why crypto/sha3 and x/crypto/sha3 cannot be
// used for them: both expose only the full permutation, and the round count is
// not a parameter. CIRCL has KangarooTwelve, but its TurboSHAKE is in an
// internal package and it has no KT256, so the permutation is written out here
// and both algorithms are built on it.
//
// Writing a permutation by hand is exactly the kind of thing that passes its own
// round-trip and still disagrees with every other implementation, so it is
// pinned two ways: to the RFC's own vectors (which the suite carries) and, for
// KT128, against CIRCL's independent implementation. See turboshake_test.go.

// keccakRC are the twenty-four Keccak round constants. Keccak-p[1600, n] uses
// the LAST n of them, so the twelve-round permutation starts at index 12.
var keccakRC = [24]uint64{
	0x0000000000000001, 0x0000000000008082, 0x800000000000808a, 0x8000000080008000,
	0x000000000000808b, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
	0x000000000000008a, 0x0000000000000088, 0x0000000080008009, 0x000000008000000a,
	0x000000008000808b, 0x800000000000008b, 0x8000000000008089, 0x8000000000008003,
	0x8000000000008002, 0x8000000000000080, 0x000000000000800a, 0x800000008000000a,
	0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
}

// keccakRho are the rotation offsets of the ρ step, indexed [x][y].
var keccakRho = [5][5]uint{
	{0, 36, 3, 41, 18},
	{1, 44, 10, 45, 2},
	{62, 6, 43, 15, 61},
	{28, 55, 25, 21, 56},
	{27, 20, 39, 8, 14},
}

// keccakP applies Keccak-p[1600, rounds] to a. Lanes are indexed x + 5y, which
// is the order the sponge reads and writes them in.
func keccakP(a *[25]uint64, rounds int) {
	for r := 24 - rounds; r < 24; r++ {
		// θ
		var c, d [5]uint64
		for x := 0; x < 5; x++ {
			c[x] = a[x] ^ a[x+5] ^ a[x+10] ^ a[x+15] ^ a[x+20]
		}
		for x := 0; x < 5; x++ {
			d[x] = c[(x+4)%5] ^ bits.RotateLeft64(c[(x+1)%5], 1)
		}
		for y := 0; y < 5; y++ {
			for x := 0; x < 5; x++ {
				a[x+5*y] ^= d[x]
			}
		}
		// ρ and π
		var b [25]uint64
		for y := 0; y < 5; y++ {
			for x := 0; x < 5; x++ {
				b[y+5*((2*x+3*y)%5)] = bits.RotateLeft64(a[x+5*y], int(keccakRho[x][y]))
			}
		}
		// χ
		for y := 0; y < 5; y++ {
			for x := 0; x < 5; x++ {
				a[x+5*y] = b[x+5*y] ^ (^b[(x+1)%5+5*y] & b[(x+2)%5+5*y])
			}
		}
		// ι
		a[0] ^= keccakRC[r]
	}
}

// turboSHAKE is the sponge: absorb msg at the given rate, pad with the domain
// separation byte, then squeeze outLen bytes. Twelve rounds is what makes it
// "turbo"; everything else is SHAKE.
func turboSHAKE(capacityBytes int, domain byte, msg []byte, outLen int) []byte {
	rate := 200 - capacityBytes
	var st [25]uint64
	var block [200]byte

	absorb := func(b []byte) {
		copy(block[:rate], b)
		for i := 0; i < rate; i += 8 {
			st[i/8] ^= binary.LittleEndian.Uint64(block[i : i+8])
		}
		keccakP(&st, 12)
	}
	for len(msg) >= rate {
		absorb(msg[:rate])
		msg = msg[rate:]
	}
	// Pad: the domain byte at the end of the message, 0x80 at the end of the
	// block. They land on the same byte when the message fills the rate but one.
	for i := range block {
		block[i] = 0
	}
	copy(block[:], msg)
	block[len(msg)] = domain
	block[rate-1] |= 0x80
	for i := 0; i < rate; i += 8 {
		st[i/8] ^= binary.LittleEndian.Uint64(block[i : i+8])
	}
	keccakP(&st, 12)

	out := make([]byte, 0, outLen)
	for len(out) < outLen {
		var buf [200]byte
		for i := 0; i < rate; i += 8 {
			binary.LittleEndian.PutUint64(buf[i:i+8], st[i/8])
		}
		n := rate
		if n > outLen-len(out) {
			n = outLen - len(out)
		}
		out = append(out, buf[:n]...)
		if len(out) < outLen {
			keccakP(&st, 12)
		}
	}
	return out
}

// lengthEncode is RFC 9861's right-hand length encoding: the value in
// big-endian with no leading zeros, followed by how many bytes that took.
func lengthEncode(x int) []byte {
	if x == 0 {
		return []byte{0x00}
	}
	var b []byte
	for v := x; v > 0; v >>= 8 {
		b = append([]byte{byte(v)}, b...)
	}
	return append(b, byte(len(b)))
}

// ktChunk is the block size of the tree, in bytes.
const ktChunk = 8192

// kangarooTwelve is RFC 9861's tree hash. A short input is one TurboSHAKE call;
// a long one is hashed in 8192-byte chunks whose chaining values are hashed
// again, which is what lets it use every core on a large message.
func kangarooTwelve(capacityBytes int, msg, custom []byte, outLen int) []byte {
	s := make([]byte, 0, len(msg)+len(custom)+9)
	s = append(s, msg...)
	s = append(s, custom...)
	s = append(s, lengthEncode(len(custom))...)

	if len(s) <= ktChunk {
		return turboSHAKE(capacityBytes, 0x07, s, outLen)
	}
	// A chaining value is as long as the capacity: 32 bytes for KT128, 64 for
	// KT256.
	cvLen := capacityBytes
	node := make([]byte, 0, ktChunk+64)
	node = append(node, s[:ktChunk]...)
	node = append(node, 0x03, 0, 0, 0, 0, 0, 0, 0)
	rest, n := s[ktChunk:], 0
	for len(rest) > 0 {
		take := ktChunk
		if take > len(rest) {
			take = len(rest)
		}
		node = append(node, turboSHAKE(capacityBytes, 0x0b, rest[:take], cvLen)...)
		rest = rest[take:]
		n++
	}
	node = append(node, lengthEncode(n)...)
	node = append(node, 0xff, 0xff)
	return turboSHAKE(capacityBytes, 0x06, node, outLen)
}

// -------------------------------------------------------------------- ops

// opTurboSHAKE(bits, domainSeparation, data, outputBits) -> digest bytes.
func (s *subtleAPI) opTurboSHAKE(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("turboshake: (bits, domain, data, outputBits) required")
	}
	strength := intArg(args[0])
	if strength != 128 && strength != 256 {
		return subtleErr(errNotSupported, "unsupported TurboSHAKE strength"), nil
	}
	domain := intArg(args[1])
	// The domain separation byte is a caller-visible parameter, and only the
	// range the RFC reserves for applications is allowed.
	if domain < 0x01 || domain > 0x7f {
		return subtleErr(errOperation, "TurboSHAKE domainSeparation must be between 0x01 and 0x7F"), nil
	}
	data, err := argBytes(args[2])
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	outBits := intArg(args[3])
	// Zero is a legal length: a zero-byte digest is what an extendable-output
	// function returns when asked for no output.
	if outBits < 0 || outBits%8 != 0 {
		return subtleErr(errOperation, "TurboSHAKE outputLength must be a non-negative multiple of 8"), nil
	}
	return bytesValueOK(turboSHAKE(strength/4, byte(domain), data, outBits/8))
}

// opKangarooTwelve(bits, data, customization, outputBits) -> digest bytes.
func (s *subtleAPI) opKangarooTwelve(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("kangarootwelve: (bits, data, customization, outputBits) required")
	}
	strength := intArg(args[0])
	if strength != 128 && strength != 256 {
		return subtleErr(errNotSupported, "unsupported KangarooTwelve strength"), nil
	}
	data, err := argBytes(args[1])
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	custom, err := argBytes(args[2])
	if err != nil {
		return subtleErr(errOperation, err.Error()), nil
	}
	outBits := intArg(args[3])
	// Zero is a legal length: a zero-byte digest is what an extendable-output
	// function returns when asked for no output.
	if outBits < 0 || outBits%8 != 0 {
		return subtleErr(errOperation, "outputLength must be a non-negative multiple of 8"), nil
	}
	return bytesValueOK(kangarooTwelve(strength/4, data, custom, outBits/8))
}

// ------------------------- the X25519 key agreement.
//
// It is the sibling of the Ed25519 signing key that was already here, and the
// only curve in the Web Crypto suite's key-agreement set that this runtime did
// not implement — every generateKey/deriveBits/import case for it failed as
// unsupported. Go's crypto/ecdh provides the primitive.

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
