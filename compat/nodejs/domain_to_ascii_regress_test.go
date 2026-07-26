package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestDomainToASCIIPunycode verifies url.domainToASCII performs real IDNA
// encoding (lowercase + NFC + RFC 3492 punycode) and returns "" for clearly
// invalid domains; domainToUnicode decodes the other direction.
func TestDomainToASCIIPunycode(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const url = require("url");
		globalThis.r = {};
		r.manana = url.domainToASCII("mañana.com");
		r.upper = url.domainToASCII("MAÑANA.COM");
		// NFC first: "n" + combining tilde composes to U+00F1.
		r.nfc = url.domainToASCII("mañana.com");
		r.ascii = url.domainToASCII("EXAMPLE.com");
		r.bucher = url.domainToASCII("bücher.de");
		r.emptyLabel = JSON.stringify(url.domainToASCII("a..b"));
		r.space = JSON.stringify(url.domainToASCII("mañ ana.com"));
		r.longLabel = JSON.stringify(url.domainToASCII("a".repeat(64) + ".com"));
		r.trailingDot = url.domainToASCII("example.com.");
		r.unicode = url.domainToUnicode("xn--maana-pta.com");
		r.unicodePlain = url.domainToUnicode("example.com");
	`)
	for _, tc := range [][2]string{
		{`r.manana`, "xn--maana-pta.com"},
		{`r.upper`, "xn--maana-pta.com"},
		{`r.nfc`, "xn--maana-pta.com"},
		{`r.ascii`, "example.com"},
		{`r.bucher`, "xn--bcher-kva.de"},
		{`r.emptyLabel`, `""`},
		{`r.space`, `""`},
		{`r.longLabel`, `""`},
		{`r.trailingDot`, "example.com."},
		{`r.unicode`, "mañana.com"},
		{`r.unicodePlain`, "example.com"},
	} {
		if got := evalStr(t, js, tc[0]); got != tc[1] {
			t.Errorf("%s = %q, want %q", tc[0], got, tc[1])
		}
	}
}
