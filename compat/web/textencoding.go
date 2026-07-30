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
	"strings"
	"unicode/utf8"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
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

	// A single-byte encoding is decoded from its own table rather than through
	// the transform pipeline. Two reasons, and the first is correctness:
	// golang.org/x/text's ISO-8859 and windows-125x charmaps leave 0x80-0x9F
	// unassigned, where the Encoding Standard's index tables map every one of
	// them to the C1 control of the same value — the standard's null entries all
	// sit at 0xA1 and above. Decoding through the charmap therefore reported 499
	// perfectly valid bytes as malformed. The second reason is that byte-wise
	// decoding makes `fatal` exact: an unmapped byte IS the error, with no need
	// to infer one from a replacement character in the output.
	if cm, ok := enc.(*charmap.Charmap); ok {
		return decodeSingleByte(cm, data, fatal), nil
	}

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

// decodeSingleByte decodes with the standard's own single-byte rule: an ASCII
// byte is itself, and any other byte is its index code point — falling back to
// the C1 control for the 0x80-0x9F range the charmap leaves out.
func decodeSingleByte(cm *charmap.Charmap, data []byte, fatal bool) spidermonkey.Value {
	var b strings.Builder
	b.Grow(len(data))
	for _, c := range data {
		if c < 0x80 {
			b.WriteByte(c)
			continue
		}
		r := cm.DecodeByte(c)
		if r == utf8.RuneError {
			if c <= 0x9f {
				// Not unmapped: the standard's index has the C1 control here, and
				// only the charmap is missing it.
				b.WriteRune(rune(c))
				continue
			}
			if fatal {
				return spidermonkey.ValueOf(map[string]any{"error": "the encoded data was not valid"})
			}
			b.WriteRune(utf8.RuneError)
			continue
		}
		b.WriteRune(r)
	}
	return spidermonkey.ValueOf(b.String())
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
