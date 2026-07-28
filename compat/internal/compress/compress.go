// Package compress holds the one-shot compression codecs both compat layers
// need: node:zlib on one side and the WHATWG CompressionStream on the other.
//
// It lives here rather than in either layer because CompressionStream is a WEB
// api — putting the codec in compat/nodejs left compat/web without it, and the
// Web Platform Tests scored zero on the whole `compression` directory as a
// result.
package compress

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"

	"github.com/andybalholm/brotli"
)

// MaxOutput caps decompressed output so a small "zip bomb" can't expand to
// gigabytes on the host heap before it reaches the (capped) guest memory.
// MaxOutput is that cap, exported because the streaming decompressor in
// compat/nodejs enforces the same ceiling incrementally.
const MaxOutput = 256 << 20 // 256 MiB

// readCapped reads r but errors past MaxOutput instead of allocating without
// bound.
func readCapped(r io.Reader) ([]byte, error) {
	out, err := io.ReadAll(io.LimitReader(r, MaxOutput+1))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > MaxOutput {
		return nil, fmt.Errorf("decompressed output exceeds %d bytes", MaxOutput)
	}
	return out, nil
}

func Run(method string, data []byte) ([]byte, error) {
	switch method {
	case "gzip":
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case "gunzip":
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return readCapped(r)
	case "unzip":
		// Node's unzip auto-detects the wrapper: gzip magic (0x1f 0x8b) -> gzip,
		// otherwise a zlib (deflate) stream. A plain deflate response body must
		// decompress here too, not fail as it did when unzip only did gzip.
		if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
			r, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				return nil, err
			}
			return readCapped(r)
		}
		r, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return readCapped(r)
	case "deflate":
		var buf bytes.Buffer
		w := zlib.NewWriter(&buf)
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case "inflate":
		r, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return readCapped(r)
	case "deflateRaw":
		var buf bytes.Buffer
		w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case "inflateRaw":
		r := flate.NewReader(bytes.NewReader(data))
		defer r.Close()
		return readCapped(r)
	case "brotliCompress":
		var buf bytes.Buffer
		w := brotli.NewWriter(&buf)
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case "brotliDecompress":
		return readCapped(brotli.NewReader(bytes.NewReader(data)))
	}
	return nil, fmt.Errorf("unsupported zlib method %q", method)
}
