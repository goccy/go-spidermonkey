package web

// The vectors below are RFC 9861's own, extracted from the Web Platform Tests
// that carry them rather than transcribed by hand — a permutation written from a
// specification will happily agree with itself while disagreeing with every
// other implementation, and only real vectors catch that. KT128 is additionally
// cross-checked against CIRCL, which is an independent implementation.

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/cloudflare/circl/xof/k12"
)

// ptn is the RFC's input pattern: the bytes 00 01 02 .. FA repeating.
func ptn(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

type xofVector struct {
	strength  int // 128 or 256
	input     int // -1 for the empty input, otherwise ptn(input)
	outBits   int
	want      string
	domain    int // TurboSHAKE only; 0 means the default 0x1F
	customLen int // KangarooTwelve only; -1 for none
}

func (v xofVector) msg() []byte {
	if v.input < 0 {
		return nil
	}
	return ptn(v.input)
}

var turboSHAKEVectors = []xofVector{
	{strength: 128, input: -1, outBits: 256, want: "1e415f1c5983aff2169217277d17bb538cd945a397ddec541f1ce41af2c1b74c", domain: 0},
	{strength: 128, input: -1, outBits: 512, want: "1e415f1c5983aff2169217277d17bb538cd945a397ddec541f1ce41af2c1b74c3e8ccae2a4dae56c84a04c2385c03c15e8193bdf58737363321691c05462c8df", domain: 0},
	{strength: 128, input: 1, outBits: 256, want: "55cedd6f60af7bb29a4042ae832ef3f58db7299f893ebb9247247d856958daa9", domain: 0},
	{strength: 128, input: 17, outBits: 256, want: "9c97d036a3bac819db70ede0ca554ec6e4c2a1a4ffbfd9ec269ca6a111161233", domain: 0},
	{strength: 256, input: -1, outBits: 512, want: "367a329dafea871c7802ec67f905ae13c57695dc2c6663c61035f59a18f8e7db11edc0e12e91ea60eb6b32df06dd7f002fbafabb6e13ec1cc20d995547600db0", domain: 0},
	{strength: 256, input: 1, outBits: 512, want: "3e1712f928f8eaf1054632b2aa0a246ed8b0c378728f60bc970410155c28820e90cc90d8a3006aa2372c5c5ea176b0682bf22bae7467ac94f74d43d39b0482e2", domain: 0},
	{strength: 256, input: 17, outBits: 512, want: "b3bab0300e6a191fbe6137939835923578794ea54843f5011090fa2f3780a9e5cb22c59d78b40a0fbff9e672c0fbe0970bd2c845091c6044d687054da5d8e9c7", domain: 0},
}

var kangarooTwelveVectors = []xofVector{
	{strength: 128, input: -1, outBits: 256, want: "1ac2d450fc3b4205d19da7bfca1b37513c0803577ac7167f06fe2ce1f0ef39e5", customLen: -1},
	{strength: 128, input: -1, outBits: 512, want: "1ac2d450fc3b4205d19da7bfca1b37513c0803577ac7167f06fe2ce1f0ef39e54269c056b8c82e48276038b6d292966cc07a3d4645272e31ff38508139eb0a71", customLen: -1},
	{strength: 128, input: 1, outBits: 256, want: "2bda92450e8b147f8a7cb629e784a058efca7cf7d8218e02d345dfaa65244a1f", customLen: -1},
	{strength: 128, input: 17, outBits: 256, want: "6bf75fa2239198db4772e36478f8e19b0f371205f6a9a93a273f51df37122888", customLen: -1},
	{strength: 128, input: -1, outBits: 256, want: "fab658db63e94a246188bf7af69a133045f46ee984c56e3c3328caaf1aa1a583", customLen: 1},
	{strength: 128, input: 8191, outBits: 256, want: "1b577636f723643e990cc7d6a659837436fd6a103626600eb8301cd1dbe553d6", customLen: -1},
	{strength: 128, input: 8192, outBits: 256, want: "48f256f6772f9edfb6a8b661ec92dc93b95ebd05a08a17b39ae3490870c926c3", customLen: -1},
	{strength: 128, input: 8192, outBits: 256, want: "3ed12f70fb05ddb58689510ab3e4d23c6c6033849aa01e1d8c220a297fedcd0b", customLen: 8189},
	{strength: 128, input: 8192, outBits: 256, want: "6a7c1b6a5cd0d8c9ca943a4a216cc64604559a2ea45f78570a15253d67ba00ae", customLen: 8190},
	{strength: 256, input: -1, outBits: 512, want: "b23d2e9cea9f4904e02bec06817fc10ce38ce8e93ef4c89e6537076af8646404e3e8b68107b8833a5d30490aa33482353fd4adc7148ecb782855003aaebde4a9", customLen: -1},
	{strength: 256, input: -1, outBits: 1024, want: "b23d2e9cea9f4904e02bec06817fc10ce38ce8e93ef4c89e6537076af8646404e3e8b68107b8833a5d30490aa33482353fd4adc7148ecb782855003aaebde4a9b0925319d8ea1e121a609821ec19efea89e6d08daee1662b69c840289f188ba860f55760b61f82114c030c97e5178449608ccd2cd2d919fc7829ff69931ac4d0", customLen: -1},
	{strength: 256, input: 1, outBits: 512, want: "0d005a194085360217128cf17f91e1f71314efa5564539d444912e3437efa17f82db6f6ffe76e781eaa068bce01f2bbf81eacb983d7230f2fb02834a21b1ddd0", customLen: -1},
	{strength: 256, input: 17, outBits: 512, want: "1ba3c02b1fc514474f06c8979978a9056c8483f4a1b63d0dccefe3a28a2f323e1cdcca40ebf006ac76ef0397152346837b1277d3e7faa9c9653b19075098527b", customLen: -1},
	{strength: 256, input: -1, outBits: 512, want: "9280f5cc39b54a5a594ec63de0bb99371e4609d44bf845c2f5b8c316d72b159811f748f23e3fabbe5c3226ec96c62186df2d33e9df74c5069ceecbb4dd10eff6", customLen: 1},
	{strength: 256, input: 8191, outBits: 512, want: "3081434d93a4108d8d8a3305b89682cebedc7ca4ea8a3ce869fbb73cbe4a58eef6f24de38ffc170514c70e7ab2d01f03812616e863d769afb3753193ba045b20", customLen: -1},
	{strength: 256, input: 8192, outBits: 512, want: "c6ee8e2ad3200c018ac87aaa031cdac22121b412d07dc6e0dccbb53423747e9a1c18834d99df596cf0cf4b8dfafb7bf02d139d0c9035725adc1a01b7230a41fa", customLen: -1},
	{strength: 256, input: 8192, outBits: 512, want: "74e47879f10a9c5d11bd2da7e194fe57e86378bf3c3f7448eff3c576a0f18c5caae0999979512090a7f348af4260d4de3c37f1ecaf8d2c2c96c1d16c64b12496", customLen: 8189},
	{strength: 256, input: 8192, outBits: 512, want: "f4b5908b929ffe01e0f79ec2f21243d41a396b2e7303a6af1d6399cd6c7a0a2dd7c4f607e8277f9c9b1cb4ab9ddc59d4b92d1fc7558441f1832c3279a4241b8b", customLen: 8190},
}

func TestTurboSHAKEVectors(t *testing.T) {
	for _, v := range turboSHAKEVectors {
		domain := byte(0x1f)
		if v.domain != 0 {
			domain = byte(v.domain)
		}
		got := turboSHAKE(v.strength/4, domain, v.msg(), v.outBits/8)
		if hex.EncodeToString(got) != v.want {
			t.Errorf("TurboSHAKE%d(ptn(%d), %d bits, D=%#x) = %s, want %s",
				v.strength, v.input, v.outBits, domain, hex.EncodeToString(got), v.want)
		}
	}
}

func TestKangarooTwelveVectors(t *testing.T) {
	for _, v := range kangarooTwelveVectors {
		var custom []byte
		if v.customLen > 0 {
			custom = ptn(v.customLen)
		}
		got := kangarooTwelve(v.strength/4, v.msg(), custom, v.outBits/8)
		if hex.EncodeToString(got) != v.want {
			t.Errorf("KT%d(ptn(%d), %d bits, C=ptn(%d)) = %s, want %s",
				v.strength, v.input, v.outBits, v.customLen, hex.EncodeToString(got), v.want)
		}
	}
}

// TestKangarooTwelveAgainstCIRCL crosses the tree hash with an independent
// implementation at lengths that straddle the 8192-byte chunk boundary, which is
// where a tree hash and a plain sponge stop agreeing.
func TestKangarooTwelveAgainstCIRCL(t *testing.T) {
	for _, n := range []int{0, 1, 8191, 8192, 8193, 16384, 16385, 40000} {
		for _, clen := range []int{0, 7, 100} {
			msg, custom := ptn(n), ptn(clen)
			want := make([]byte, 64)
			h := k12.NewDraft10(custom)
			h.Write(msg)
			h.Read(want)

			got := kangarooTwelve(32, msg, custom, 64)
			if !bytes.Equal(got, want) {
				t.Fatalf("KT128(ptn(%d), C=ptn(%d)):\n got %x\nwant %x", n, clen, got, want)
			}
		}
	}
}
