package web

// compression.go: the host half of CompressionStream/DecompressionStream.
//
// These are WinterTC web APIs, so they belong here. They used to exist only in
// compat/nodejs (because that is where the zlib host op lived), which left a
// web-only embedding without them and scored zero on the whole WPT
// `compression` directory. The codec itself is shared —
// compat/internal/compress — so node:zlib and this are the same implementation.

import (
	"fmt"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/internal/compress"
)

// opMIMEType normalizes a media type the way the platform does everywhere it
// appears: parsed and serialized back, or "" when it does not parse. Blob,
// File, Request and Response all report their type in that form, and the
// mimesniff tests check every one of them against the same table.
func (w *Web) opMIMEType(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.ValueOf(""), nil
	}
	m, ok := parseMIMEType(args[0].String())
	if !ok {
		return spidermonkey.ValueOf(""), nil
	}
	return spidermonkey.ValueOf(m.String()), nil
}

// opCompress runs one buffer through a named codec ("gzip", "gunzip",
// "deflate", "inflate", "deflateRaw", "inflateRaw"). A codec failure — corrupt
// input to DecompressionStream, most often — comes back as an error object the
// guest turns into a TypeError on the stream, not as a host error.
func (w *Web) opCompress(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("compress: (method, data) required")
	}
	data, err := argBytes(args[1])
	if err != nil {
		return nil, err
	}
	out, err := compress.Run(args[0].String(), data)
	if err != nil {
		return spidermonkey.ValueOf(map[string]any{"error": err.Error()}), nil
	}
	u8, err := w.js.NewBytes(out)
	if err != nil {
		return nil, err
	}
	return u8, nil
}
