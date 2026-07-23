package nodejs

// nodecrypto.go: the hash/hmac primitives behind node:crypto (js/extras.js).
// Node uses OpenSSL-style lowercase algorithm names.

import (
	"crypto"
	"crypto/hmac"
	_ "crypto/md5" // register the hashes crypto.Hash.New needs
	_ "crypto/sha1"
	_ "crypto/sha256"
	_ "crypto/sha512"
	"fmt"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func nodeHashByName(name string) (crypto.Hash, error) {
	switch name {
	case "md5":
		return crypto.MD5, nil
	case "sha1":
		return crypto.SHA1, nil
	case "sha256":
		return crypto.SHA256, nil
	case "sha384":
		return crypto.SHA384, nil
	case "sha512":
		return crypto.SHA512, nil
	}
	return 0, fmt.Errorf("unsupported digest %q", name)
}

func (rt *Runtime) opCryptoHash(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("crypto_hash: (algorithm, data) required")
	}
	h, err := nodeHashByName(args[0].String())
	if err != nil {
		return nil, err
	}
	data, err := valueBytes(args[1])
	if err != nil {
		return nil, err
	}
	hh := h.New()
	hh.Write(data)
	return byteArrayValue(hh.Sum(nil)), nil
}

func (rt *Runtime) opCryptoHMAC(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("crypto_hmac: (algorithm, key, data) required")
	}
	h, err := nodeHashByName(args[0].String())
	if err != nil {
		return nil, err
	}
	key, err := valueBytes(args[1])
	if err != nil {
		return nil, err
	}
	data, err := valueBytes(args[2])
	if err != nil {
		return nil, err
	}
	m := hmac.New(h.New, key)
	m.Write(data)
	return byteArrayValue(m.Sum(nil)), nil
}

// valueBytes reads a BufferSource argument (freeing the arg pin) or falls
// back to the value's string bytes.
func valueBytes(v spidermonkey.Value) ([]byte, error) {
	if o := v.Object(); o != nil {
		defer o.Free()
		return o.Bytes()
	}
	return []byte(v.String()), nil
}

// byteArrayValue returns b as a plain array (data, not a handle).
func byteArrayValue(b []byte) spidermonkey.Value {
	ints := make([]int, len(b))
	for i, x := range b {
		ints[i] = int(x)
	}
	return spidermonkey.ValueOf(ints)
}
