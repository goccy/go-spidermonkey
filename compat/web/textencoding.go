package web

// textencoding.go: the legacy encodings TextDecoder is required to know.
//
// The guest decodes utf-8, utf-16le and windows-1252 itself, because those need
// no table. Everything else in the WHATWG encoding standard — Shift_JIS, GBK,
// Big5, EUC-KR, ISO-2022-JP, the whole ISO-8859 and windows-125x family,
// utf-16be — needs one, and the engine does not offer it: TextDecoder is a web
// API, so SpiderMonkey's ICU is not reachable from it even though Intl is fully
// built in. golang.org/x/text/encoding/htmlindex is exactly that table, keyed
// by the standard's own labels.

import (
	"fmt"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/transform"
)

// maxDecodeBytes bounds one decode so a hostile input cannot turn a small
// buffer into unbounded host allocation. Legacy encodings are at most 1 rune
// per byte, so the output is bounded by roughly 4x this.
const maxDecodeBytes = 64 << 20

// opTextEncodingName(label) -> the standard's canonical name for a label, or ""
// when the label names no encoding. The guest uses it both to validate a label
// (TextDecoder throws a RangeError for an unknown one) and to report
// `decoder.encoding`.
func (a *Web) opTextEncodingName(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("text_encoding_name: (label) required")
	}
	enc, err := htmlindex.Get(args[0].String())
	if err != nil {
		return spidermonkey.ValueOf(""), nil
	}
	name, err := htmlindex.Name(enc)
	if err != nil {
		return spidermonkey.ValueOf(""), nil
	}
	return spidermonkey.ValueOf(name), nil
}

// opTextDecode(label, bytes, fatal) -> the decoded string, or an error marker
// when `fatal` is set and the input is not valid in that encoding.
func (a *Web) opTextDecode(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("text_decode: (label, bytes, fatal?) required")
	}
	enc, err := htmlindex.Get(args[0].String())
	if err != nil {
		return spidermonkey.ValueOf(map[string]any{"error": "unsupported encoding " + args[0].String()}), nil
	}
	data, err := argBytes(args[1])
	if err != nil {
		return nil, err
	}
	if len(data) > maxDecodeBytes {
		return spidermonkey.ValueOf(map[string]any{"error": "input too large to decode"}), nil
	}
	fatal := len(args) > 2 && args[2].Bool()

	dec := enc.NewDecoder()
	if fatal {
		// The default decoder substitutes U+FFFD; a fatal TextDecoder must
		// report the malformed input instead.
		dec = &encoding.Decoder{Transformer: strictDecoder{enc.NewDecoder()}}
	}
	out, _, terr := transform.Bytes(dec, data)
	if terr != nil {
		return spidermonkey.ValueOf(map[string]any{"error": "the encoded data was not valid"}), nil
	}
	return spidermonkey.ValueOf(string(out)), nil
}

// strictDecoder turns the replacement character the table decoders emit for
// malformed input into a real error, which is what `fatal: true` means.
type strictDecoder struct{ inner *encoding.Decoder }

func (s strictDecoder) Reset() { s.inner.Reset() }

func (s strictDecoder) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	nDst, nSrc, err = s.inner.Transform(dst, src, atEOF)
	if err != nil {
		return nDst, nSrc, err
	}
	// U+FFFD is 0xEF 0xBF 0xBD in the UTF-8 the decoder produces. A source that
	// legitimately CONTAINS U+FFFD would be reported as malformed too; the
	// standard treats an encoded replacement character as an error in every
	// legacy encoding, so that is the same answer.
	for i := 0; i+2 < nDst; i++ {
		if dst[i] == 0xEF && dst[i+1] == 0xBF && dst[i+2] == 0xBD {
			return nDst, nSrc, fmt.Errorf("malformed input for the declared encoding")
		}
	}
	return nDst, nSrc, err
}
