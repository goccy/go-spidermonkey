package web

// subtle.go: the Go crypto primitives behind crypto.subtle (js/subtle.js).
// Keys live host-side in a handle table; the guest's CryptoKey carries only
// the handle. Byte outputs (digests, signatures, exported DER) return as
// plain arrays — data, not handles — so nothing stays pinned.

import (
	"container/list"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	_ "crypto/sha1" // register hashes for crypto.Hash.New
	_ "crypto/sha256"
	_ "crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"sync"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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

func (s *subtleAPI) opECImportJWK(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("ec import: jwk required")
	}
	var j jwkDoc
	if err := json.Unmarshal([]byte(args[0].String()), &j); err != nil {
		return nil, fmt.Errorf("bad JWK: %w", err)
	}
	if j.Kty != "EC" {
		return nil, fmt.Errorf("not an EC JWK (kty=%q)", j.Kty)
	}
	curve, err := curveByName(j.Crv)
	if err != nil {
		return nil, err
	}
	x, err := b64uBig(j.X)
	if err != nil {
		return nil, fmt.Errorf("bad JWK x: %w", err)
	}
	y, err := b64uBig(j.Y)
	if err != nil {
		return nil, fmt.Errorf("bad JWK y: %w", err)
	}
	pub := ecdsa.PublicKey{Curve: curve, X: x, Y: y}
	if !curve.IsOnCurve(x, y) {
		return nil, fmt.Errorf("JWK point is not on %s", j.Crv)
	}
	if j.D == "" {
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{ecPub: &pub}), "type": "public", "crv": j.Crv,
		}), nil
	}
	d, err := b64uBig(j.D)
	if err != nil {
		return nil, fmt.Errorf("bad JWK d: %w", err)
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
	case "pkcs8":
		key, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return nil, err
		}
		priv, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("pkcs8 key is not EC")
		}
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{ecPriv: priv}), "type": "private", "crv": curveName(priv.Curve),
		}), nil
	case "spki":
		key, err := x509.ParsePKIXPublicKey(der)
		if err != nil {
			return nil, err
		}
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("spki key is not EC")
		}
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{ecPub: pub}), "type": "public", "crv": curveName(pub.Curve),
		}), nil
	}
	return nil, fmt.Errorf("ec import: unsupported format")
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
	case "pkcs8":
		if k.ecPriv == nil {
			return nil, fmt.Errorf("pkcs8 export needs a private key")
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
	return nil, fmt.Errorf("ec export: unsupported format")
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
func rsaPSSOptions(saltLen int, h crypto.Hash) (*rsa.PSSOptions, error) {
	if saltLen == 0 {
		return nil, fmt.Errorf("RSA-PSS saltLength 0 is not supported (Go crypto/rsa cannot express a zero-length PSS salt); use a positive saltLength or the default")
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
		opts, oerr := rsaPSSOptions(intArg(args[2]), h)
		if oerr != nil {
			return subtleErr("OperationError: " + oerr.Error()), nil
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
		opts, oerr := rsaPSSOptions(intArg(args[2]), h)
		if oerr != nil {
			return subtleErr("OperationError: " + oerr.Error()), nil
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
		return nil, fmt.Errorf("ed25519 import: (format, keyData) required")
	}
	switch args[0].String() {
	case "jwk":
		var j jwkDoc
		if err := json.Unmarshal([]byte(args[1].String()), &j); err != nil {
			return nil, fmt.Errorf("bad JWK: %w", err)
		}
		if j.Kty != "OKP" || j.Crv != "Ed25519" {
			return nil, fmt.Errorf("not an Ed25519 JWK")
		}
		x, err := b64u.DecodeString(j.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("bad JWK x")
		}
		if j.D == "" {
			return spidermonkey.ValueOf(map[string]any{
				"id": s.put(&subtleKey{edPub: ed25519.PublicKey(x)}), "type": "public",
			}), nil
		}
		seed, err := b64u.DecodeString(j.D)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("bad JWK d")
		}
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{edPriv: ed25519.NewKeyFromSeed(seed)}), "type": "private",
		}), nil
	case "raw":
		x, err := argBytes(args[1])
		if err != nil {
			return nil, err
		}
		if len(x) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("bad raw Ed25519 public key")
		}
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{edPub: ed25519.PublicKey(append([]byte(nil), x...))}), "type": "public",
		}), nil
	case "pkcs8":
		der, err := argBytes(args[1])
		if err != nil {
			return nil, err
		}
		key, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return nil, err
		}
		priv, ok := key.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("pkcs8 key is not Ed25519")
		}
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{edPriv: priv}), "type": "private",
		}), nil
	case "spki":
		der, err := argBytes(args[1])
		if err != nil {
			return nil, err
		}
		key, err := x509.ParsePKIXPublicKey(der)
		if err != nil {
			return nil, err
		}
		pub, ok := key.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("spki key is not Ed25519")
		}
		return spidermonkey.ValueOf(map[string]any{
			"id": s.put(&subtleKey{edPub: pub}), "type": "public",
		}), nil
	}
	return nil, fmt.Errorf("ed25519 import: unsupported format")
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
		j := jwkDoc{Kty: "OKP", Crv: "Ed25519", Ext: true, X: b64u.EncodeToString(pub)}
		if k.edPriv != nil {
			j.D = b64u.EncodeToString(k.edPriv.Seed())
		}
		out, err := json.Marshal(j)
		if err != nil {
			return nil, err
		}
		return spidermonkey.ValueOf(string(out)), nil
	case "raw":
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
		"subtle_digest":         s.opDigest,
		"subtle_hmac_import":    s.opHMACImport,
		"subtle_hmac_export":    s.opHMACExport,
		"subtle_hmac_sign":      s.opHMACSign,
		"subtle_hmac_verify":    s.opHMACVerify,
		"subtle_ec_generate":    s.opECGenerate,
		"subtle_ec_import_jwk":  s.opECImportJWK,
		"subtle_ec_import_der":  s.opECImportDER,
		"subtle_ec_export_jwk":  s.opECExportJWK,
		"subtle_ec_export_der":  s.opECExportDER,
		"subtle_ec_sign":        s.opECSign,
		"subtle_ec_verify":      s.opECVerify,
		"subtle_rsa_generate":   s.opRSAGenerate,
		"subtle_rsa_import_jwk": s.opRSAImportJWK,
		"subtle_rsa_import_der": s.opRSAImportDER,
		"subtle_rsa_export_jwk": s.opRSAExportJWK,
		"subtle_rsa_export_der": s.opRSAExportDER,
		"subtle_rsa_sign":       s.opRSASign,
		"subtle_rsa_verify":     s.opRSAVerify,
		"subtle_ed_generate":    s.opEdGenerate,
		"subtle_ed_import":      s.opEdImport,
		"subtle_ed_export":      s.opEdExport,
		"subtle_ed_sign":        s.opEdSign,
		"subtle_ed_verify":      s.opEdVerify,
	}
}
