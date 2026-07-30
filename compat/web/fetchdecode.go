package web

// fetchdecode.go: decoding a response's Content-Encoding.
//
// fetch decodes the content codings a response declares before the body reaches
// the caller — `res.text()` on a gzipped resource gives the text, not the gzip.
// net/http does this for gzip only, and only when IT chose to ask for gzip; a
// server that compresses without being asked, or that uses br or zstd, hands
// back bytes nothing unwraps. So the decoding is here, as a RoundTripper above
// the cache: a cache hit is decoded too, and what the cache stores stays the
// bytes the origin actually sent.
//
// A corrupt body must FAIL rather than truncate. Every reader below reports its
// own error, and because the decode is streaming that error arrives at the read
// that hits the damage — which is what the caller's body promise rejects with.

import (
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// decodingTransport unwraps the content codings of every response it passes on.
type decodingTransport struct{ next http.RoundTripper }

func (t *decodingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Announce what can be decoded. Without this a server has no reason to
	// compress at all, and net/http's own transparent gzip only happens when it
	// added the header itself — which it will not, now that one is present.
	out := req.Clone(req.Context())
	if out.Header.Get("Accept-Encoding") == "" {
		out.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	}
	resp, err := t.next.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	return decodeBody(resp), nil
}

// decodeBody replaces the body with a decoded one when the response declares a
// coding this runtime understands. A coding it does not understand is left
// alone: the bytes are then what the caller asked for, and pretending otherwise
// would be worse than handing them over.
func decodeBody(resp *http.Response) *http.Response {
	coding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if coding == "" || coding == "identity" {
		return resp
	}
	// A list of codings is applied in order, so it is undone in reverse.
	parts := strings.Split(coding, ",")
	body := resp.Body
	decoded := false
	for i := len(parts) - 1; i >= 0; i-- {
		next, ok := decodeReader(strings.TrimSpace(parts[i]), body)
		if !ok {
			// An unknown coding stops the unwrapping: anything further out was
			// applied on top of it and cannot be reached through it either.
			break
		}
		body = next
		decoded = true
	}
	if !decoded {
		return resp
	}
	resp.Body = body
	// The declared length described the ENCODED bytes and no longer describes
	// anything the caller can see, so it goes — as does the coding itself, which
	// has been undone.
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return resp
}

// decodeReader wraps r in the decoder for one coding, reporting whether the
// coding is one this runtime knows.
//
// The decoders are all LAZY about their headers: a gzip.Reader validates its
// header on construction, which would mean a corrupt body failed at
// RoundTrip time rather than at the read — and a body error has to reach the
// caller as a rejected body promise, not as a failed fetch. So construction is
// deferred to the first Read.
func decodeReader(coding string, r io.ReadCloser) (io.ReadCloser, bool) {
	switch coding {
	case "gzip", "x-gzip":
		return &lazyDecoder{src: r, open: func(in io.Reader) (io.Reader, error) { return gzip.NewReader(in) }}, true
	case "deflate":
		return &lazyDecoder{src: r, open: func(in io.Reader) (io.Reader, error) { return zlib.NewReader(in) }}, true
	case "br":
		return &lazyDecoder{src: r, open: func(in io.Reader) (io.Reader, error) { return brotli.NewReader(in), nil }}, true
	case "zstd":
		return &lazyDecoder{src: r, open: func(in io.Reader) (io.Reader, error) {
			// Window sizes above the default are refused rather than allocated:
			// a response can otherwise name a window that costs the host hundreds
			// of megabytes per body. The suite checks exactly that refusal.
			d, err := zstd.NewReader(in, zstd.WithDecoderMaxWindow(1<<23), zstd.WithDecoderConcurrency(1))
			if err != nil {
				return nil, err
			}
			return d.IOReadCloser(), nil
		}}, true
	}
	return nil, false
}

// lazyDecoder defers building its decoder until the first Read, so a malformed
// stream is reported to whoever reads the body rather than to whoever made the
// request.
type lazyDecoder struct {
	src  io.ReadCloser
	open func(io.Reader) (io.Reader, error)
	r    io.Reader
	err  error
}

func (d *lazyDecoder) Read(p []byte) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	if d.r == nil {
		r, err := d.open(d.src)
		if err != nil {
			d.err = err
			return 0, err
		}
		d.r = r
	}
	n, err := d.r.Read(p)
	if err != nil && err != io.EOF {
		d.err = err
	}
	return n, err
}

func (d *lazyDecoder) Close() error {
	if c, ok := d.r.(io.Closer); ok {
		_ = c.Close()
	}
	return d.src.Close()
}
