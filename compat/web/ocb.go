package web

// ocb.go: AES-OCB (RFC 7253) for crypto.subtle.
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

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"fmt"
	"math/bits"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
		return subtleErr("OperationError: AES-OCB tagLength must be 64..128 bits"), nil
	}
	st, err := newOCB(key)
	if err != nil {
		return subtleErr("OperationError: " + err.Error()), nil
	}
	if encrypt {
		out, err := st.seal(nonce, data, aad, tagBytes)
		if err != nil {
			return subtleErr("OperationError: " + err.Error()), nil
		}
		return bytesValue(out), nil
	}
	out, err := st.open(nonce, data, aad, tagBytes)
	if err != nil {
		return subtleErr("OperationError: decryption failed"), nil
	}
	return bytesValue(out), nil
}
