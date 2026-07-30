package web

// kmac.go: KMAC128/KMAC256 (NIST SP 800-185) for crypto.subtle.
//
// KMAC is cSHAKE with a prescribed way of feeding it: the key, the message and
// the requested output length each go in with a length prefix, so that no two
// different (key, message, length) triples can produce the same input stream.
// x/crypto/sha3 provides cSHAKE; what is missing is those encodings, which are
// small and exact enough to be worth having here rather than pulling in a
// dependency for.

import (
	"fmt"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"golang.org/x/crypto/sha3"
)

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
