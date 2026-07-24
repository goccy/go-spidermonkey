package nodejs

// zlib.go: the compression primitives behind node:zlib and the web
// Compression/DecompressionStream. One-shot buffer transforms over Go's
// compress/* plus a pure-Go brotli; the JS side wraps them as sync functions,
// callback functions, and Transform streams.

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"

	"github.com/andybalholm/brotli"
	spidermonkey "github.com/goccy/go-spidermonkey"
)

func (rt *Runtime) zlibOps() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"zlib_transform": rt.opZlibTransform,
	}
}

// opZlibTransform(method, data) -> Uint8Array | {code,message}. method is
// "gzip"/"gunzip"/"deflate"/"inflate"/"deflateRaw"/"inflateRaw"/
// "brotliCompress"/"brotliDecompress".
func (rt *Runtime) opZlibTransform(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("zlib_transform: (method, data) required")
	}
	method := args[0].String()
	data, err := valueBytes(args[1])
	if err != nil {
		return nil, err
	}
	out, err := zlibRun(method, data)
	if err != nil {
		return spidermonkey.ValueOf(map[string]any{"code": "Z_DATA_ERROR", "message": err.Error()}), nil
	}
	u8, err := rt.js.NewBytes(out)
	if err != nil {
		return nil, err
	}
	return rt.trackReturn(u8), nil
}

// maxZlibOutput caps decompressed output so a small "zip bomb" can't expand to
// gigabytes on the host heap before it reaches the (capped) guest memory.
const maxZlibOutput = 256 << 20 // 256 MiB

// readCapped reads r but errors past maxZlibOutput instead of allocating without
// bound.
func readCapped(r io.Reader) ([]byte, error) {
	out, err := io.ReadAll(io.LimitReader(r, maxZlibOutput+1))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > maxZlibOutput {
		return nil, fmt.Errorf("decompressed output exceeds %d bytes", maxZlibOutput)
	}
	return out, nil
}

func zlibRun(method string, data []byte) ([]byte, error) {
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
	case "gunzip", "unzip":
		r, err := gzip.NewReader(bytes.NewReader(data))
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
