package web

import (
	"encoding/hex"
	"testing"
)

// TestKMACVectors pins KMAC against NIST SP 800-185's own samples — the same
// ones the Web Platform Tests carry. A MAC that is merely self-consistent is
// worthless; these fix it to the standard.
func TestKMACVectors(t *testing.T) {
	key, _ := hex.DecodeString("404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f")
	msg := []byte{0x00, 0x01, 0x02, 0x03}
	for _, tc := range []struct {
		name   string
		bits   int
		custom string
		msg    []byte
		want   string
	}{
		{"KMAC128 sample 1", 128, "", msg,
			"e5780b0d3ea6f7d3a429c5706aa43a00fadbd7d49628839e3187243f456ee14e"},
		{"KMAC128 sample 2", 128, "My Tagged Application", msg,
			"3b1fba963cd8b0b59e8c1a6d71888b7143651af8ba0a7070c0979e2811324aa5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := kmac(tc.bits, key, tc.msg, []byte(tc.custom), 32)
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(got) != tc.want {
				t.Errorf("kmac = %s, want %s", hex.EncodeToString(got), tc.want)
			}
		})
	}
}
