package web_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// TextDecoder is required to know every encoding in the WHATWG standard, not
// just the three that need no table. Shift_JIS, GBK, Big5, EUC-KR, ISO-2022-JP,
// the ISO-8859 and windows-125x families and utf-16be all threw a RangeError
// here, which is indistinguishable from a typo in the label.
func TestTextDecoderLegacyEncodings(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	w, err := web.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer w.Close()

	got := evalString(t, js, `(() => {
		const out = [];
		const dec = (label, bytes, opts) =>
			new TextDecoder(label, opts).decode(new Uint8Array(bytes));

		// Shift_JIS: 0x93 0xFA 0x96 0x7B is 日本.
		out.push("sjis:" + dec("shift_jis", [0x93, 0xFA, 0x96, 0x7B]));
		// GBK: 0xD6 0xD0 0xCE 0xC4 is 中文.
		out.push("gbk:" + dec("gbk", [0xD6, 0xD0, 0xCE, 0xC4]));
		// Big5: 0xA4 0xA4 0xA4 0xE5 is 中文.
		out.push("big5:" + dec("big5", [0xA4, 0xA4, 0xA4, 0xE5]));
		// EUC-KR: 0xC7 0xD1 0xB1 0xDB is 한글.
		out.push("euckr:" + dec("euc-kr", [0xC7, 0xD1, 0xB1, 0xDB]));
		// utf-16be, which differs from the built-in utf-16le only in byte order.
		out.push("be:" + dec("utf-16be", [0x00, 0x41, 0x00, 0x42]));
		// windows-1251 (Cyrillic): 0xCF 0xF0 0xE8 is При.
		out.push("cp1251:" + dec("windows-1251", [0xCF, 0xF0, 0xE8]));

		// The canonical name is reported, whatever label was used.
		out.push("name:" + new TextDecoder("SJIS").encoding);
		out.push("name2:" + new TextDecoder("csbig5").encoding);

		// A label that names no encoding is still a RangeError.
		try { new TextDecoder("no-such-encoding"); out.push("bogus:NO-THROW"); }
		catch (e) { out.push("bogus:" + e.name); }

		// fatal:true reports malformed input instead of substituting U+FFFD.
		try { dec("shift_jis", [0x93], { fatal: true }); out.push("fatal:NO-THROW"); }
		catch (e) { out.push("fatal:" + e.name); }
		out.push("lenient:" + (dec("shift_jis", [0x93]).length > 0 ? "replaced" : "EMPTY"));

		return out.join(" | ");
	})()`)

	want := "sjis:日本 | gbk:中文 | big5:中文 | euckr:한글 | be:AB | cp1251:При | " +
		"name:shift_jis | name2:big5 | bogus:RangeError | fatal:TypeError | lenient:replaced"
	if got != want {
		t.Errorf("legacy decoding =\n %s\nwant\n %s", got, want)
	}
}
