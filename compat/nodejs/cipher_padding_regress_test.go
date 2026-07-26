package nodejs_test

// cipher.setAutoPadding(false) used to be a silent no-op: encrypt still
// appended a PKCS#7 block and decrypt still stripped one. Real semantics for
// CBC: with autopadding off, encrypt requires block-aligned input (else the
// Node-style 'data not multiple of block length' error) and produces exactly
// input-length ciphertext; decrypt returns the raw plaintext including the
// padding bytes. Stream modes (CTR/GCM) ignore the flag, as in Node.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func TestCipherSetAutoPaddingFalseCBC(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `{
		const crypto = require("crypto");
		globalThis.r = {};
		const key = Buffer.alloc(16, 1);
		const iv = Buffer.alloc(16, 2);
		const block = Buffer.from("exactly 16 bytes"); // one AES block

		// Encrypt, autopadding off, aligned input: exactly one block out.
		const c = crypto.createCipheriv("aes-128-cbc", key, iv).setAutoPadding(false);
		const ct = Buffer.concat([c.update(block), c.final()]);
		r.ctLen = ct.length;
		r.ctHex = ct.toString("hex");

		// Encrypt, autopadding off, misaligned input: throws like Node.
		try {
			const bad = crypto.createCipheriv("aes-128-cbc", key, iv);
			bad.setAutoPadding(false);
			bad.update(Buffer.from("15 bytes only!!"));
			bad.final();
			r.misaligned = "no-throw";
		} catch (e) { r.misaligned = e.message; }

		// Decrypt, autopadding off: the PKCS#7 padding bytes come back raw.
		const enc = crypto.createCipheriv("aes-128-cbc", key, iv); // default: pads
		const padded = Buffer.concat([enc.update(block), enc.final()]);
		r.paddedLen = padded.length; // 32: data block + full padding block
		const dec = crypto.createDecipheriv("aes-128-cbc", key, iv).setAutoPadding(false);
		const raw = Buffer.concat([dec.update(padded), dec.final()]);
		r.rawLen = raw.length;
		r.rawText = raw.subarray(0, 16).toString();
		r.padBytes = raw.subarray(16).every((b) => b === 0x10);

		// Round trip with autopadding off on both sides.
		const dec2 = crypto.createDecipheriv("aes-128-cbc", key, iv).setAutoPadding(false);
		r.noPadRound = Buffer.concat([dec2.update(ct), dec2.final()]).toString();

		// setAutoPadding() with no argument means true (Node default).
		const c3 = crypto.createCipheriv("aes-128-cbc", key, iv).setAutoPadding();
		r.reset = Buffer.concat([c3.update(block), c3.final()]).length; // padded: 32

		// Stream mode ignores the flag: CTR with a misaligned length still works.
		const ctr = crypto.createCipheriv("aes-128-ctr", key, iv).setAutoPadding(false);
		const ctrCt = Buffer.concat([ctr.update(Buffer.from("15 bytes only!!")), ctr.final()]);
		const ctrDec = crypto.createDecipheriv("aes-128-ctr", key, iv).setAutoPadding(false);
		r.ctr = Buffer.concat([ctrDec.update(ctrCt), ctrDec.final()]).toString();
	}`)

	// Cross-check the unpadded ciphertext against Go's raw CBC.
	key := bytes.Repeat([]byte{1}, 16)
	iv := bytes.Repeat([]byte{2}, 16)
	block, _ := aes.NewCipher(key)
	want := make([]byte, 16)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(want, []byte("exactly 16 bytes"))
	if got := evalStr(t, js, `r.ctHex`); got != hex.EncodeToString(want) {
		t.Errorf("no-pad ciphertext = %s, want %s (Go CBC)", got, hex.EncodeToString(want))
	}
	for expr, want := range map[string]string{
		"String(r.ctLen)":     "16", // no PKCS#7 block appended
		"r.misaligned":        "data not multiple of block length",
		"String(r.paddedLen)": "32",
		"String(r.rawLen)":    "32", // padding NOT stripped
		"r.rawText":           "exactly 16 bytes",
		"String(r.padBytes)":  "true", // 16 bytes of 0x10 kept
		"r.noPadRound":        "exactly 16 bytes",
		"String(r.reset)":     "32", // setAutoPadding() re-enables padding
		"r.ctr":               "15 bytes only!!",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}
