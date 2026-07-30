package web

// chacha.go: ChaCha20-Poly1305 for crypto.subtle. It is a separate AEAD from
// the AES family — a fixed 256-bit key, a 96-bit nonce and a 128-bit tag, none
// of them negotiable — so it gets its own op rather than another mode inside
// the AES switch, where every one of those would have to be re-checked.

import (
	"fmt"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"golang.org/x/crypto/chacha20poly1305"
)

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
