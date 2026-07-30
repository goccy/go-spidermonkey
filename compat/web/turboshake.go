package web

// turboshake.go: TurboSHAKE and KangarooTwelve (RFC 9861).
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

import (
	"encoding/binary"
	"fmt"
	"math/bits"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
