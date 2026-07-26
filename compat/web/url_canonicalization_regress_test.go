package web_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestURLPercentEncoding verifies the WHATWG basic URL parser percent-encodes
// components on output per their encode sets and never double-encodes an
// already-valid %XX escape.
func TestURLPercentEncoding(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	for _, tc := range [][2]string{
		{`new URL("http://h/a b").href`, "http://h/a%20b"},
		{`new URL("http://h/a b").pathname`, "/a%20b"},
		{`new URL("http://h/p?a=b c#d e").href`, "http://h/p?a=b%20c#d%20e"},
		{`new URL("http://h/a<b>{c}").pathname`, "/a%3Cb%3E%7Bc%7D"},
		// Valid %XX escapes pass through untouched (no double-encoding).
		{`new URL("http://h/%7Efoo").href`, "http://h/%7Efoo"},
		{`new URL("http://h/p?q=%20x").search`, "?q=%20x"},
		// Userinfo has the larger encode set.
		{`new URL("http://u ser:p@h/").username`, "u%20ser"},
		{`new URL("http://u^s:p|w@h/").username + "|" + new URL("http://u^s:p;w@h/").password`, "u%5Es|p%3Bw"},
		{`new URL("http://u ser:p@h/").href`, "http://u%20ser:p@h/"},
		// Non-ASCII is percent-encoded in path/query/fragment.
		{`new URL("http://h/caf\u00e9").pathname`, "/caf%C3%A9"},
	} {
		if got := evalString(t, js, tc[0]); got != tc[1] {
			t.Errorf("%s = %q, want %q", tc[0], got, tc[1])
		}
	}
}

// TestURLControlCharStripping verifies ASCII tab/CR/LF are removed anywhere in
// the input and C0-control/space is trimmed from both ends before parsing.
func TestURLControlCharStripping(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	for _, tc := range [][2]string{
		{`new URL("http://h/a\tb\nc").pathname`, "/abc"},
		{`new URL("ht\ttp://h/x").href`, "http://h/x"},
		{`new URL("  http://h/x  ").href`, "http://h/x"},
		{`new URL("\u0000\u0001http://h/x\u0000").href`, "http://h/x"},
	} {
		if got := evalString(t, js, tc[0]); got != tc[1] {
			t.Errorf("%s = %q, want %q", tc[0], got, tc[1])
		}
	}
}

// TestURLBackslashIsSlashForSpecialSchemes verifies "\" acts as "/" in special
// scheme URLs — both as the authority terminator (the classic host-spoofing
// vector) and inside paths — while non-special schemes keep it literal.
func TestURLBackslashIsSlashForSpecialSchemes(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	for _, tc := range [][2]string{
		{`new URL("http://h\\evil.com/x").hostname`, "h"},
		{`new URL("http://h\\evil.com/x").pathname`, "/evil.com/x"},
		{`new URL("https://h/a\\b").pathname`, "/a/b"},
		{`new URL("\\d", "http://h/a/b").href`, "http://h/d"},
		{`new URL("\\\\other.io\\p", "https://h/a").href`, "https://other.io/p"},
		// Non-special schemes keep the backslash literal ("\" is not in the
		// WHATWG path percent-encode set).
		{`new URL("git-x://h/a\\b").pathname`, `/a\b`},
	} {
		if got := evalString(t, js, tc[0]); got != tc[1] {
			t.Errorf("%s = %q, want %q", tc[0], got, tc[1])
		}
	}
}

// TestURLIPv4Normalization verifies dotted-decimal canonicalization of hosts
// that end in a number: leading zeros (octal), hex parts, and fewer than four
// parts all normalize; out-of-range addresses are invalid URLs.
func TestURLIPv4Normalization(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	for _, tc := range [][2]string{
		{`new URL("http://192.168.000.001/").hostname`, "192.168.0.1"},
		{`new URL("http://0x7f.1/").hostname`, "127.0.0.1"},
		{`new URL("http://127.1/").hostname`, "127.0.0.1"},
		{`new URL("http://2130706433/").hostname`, "127.0.0.1"},
		{`new URL("http://0x7f000001/").hostname`, "127.0.0.1"},
	} {
		if got := evalString(t, js, tc[0]); got != tc[1] {
			t.Errorf("%s = %q, want %q", tc[0], got, tc[1])
		}
	}
	for _, bad := range []string{
		`new URL("http://999.1.1.1/")`,   // part > 255
		`new URL("http://1.2.3.4.5/")`,   // 5 parts, ends in a number
		`new URL("http://1.2.3.4444/")`,  // last part out of range
		`new URL("http://0x100000000/")`, // whole address out of range
	} {
		if got := evalString(t, js, `(() => { try { `+bad+`; return "no-throw"; } catch (e) { return e instanceof TypeError ? "TypeError" : "other"; } })()`); got != "TypeError" {
			t.Errorf("%s: got %q, want TypeError", bad, got)
		}
	}
}

// TestURLHostPunycode verifies non-ASCII hostnames come out IDNA-encoded
// (lowercase + NFC + RFC 3492 punycode) in hostname/href.
func TestURLHostPunycode(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	for _, tc := range [][2]string{
		{`new URL("http://ma\u00f1ana.com/").hostname`, "xn--maana-pta.com"},
		{`new URL("http://MA\u00d1ANA.com/x").href`, "http://xn--maana-pta.com/x"},
		// NFC: "n" + combining tilde composes to U+00F1 first.
		{`new URL("http://man\u0303ana.com/").hostname`, "xn--maana-pta.com"},
		{`new URL("http://B\u00dcCHER.de/").hostname`, "xn--bcher-kva.de"},
		{`new URL("HTTP://EXAMPLE.COM:80/").href`, "http://example.com/"},
	} {
		if got := evalString(t, js, tc[0]); got != tc[1] {
			t.Errorf("%s = %q, want %q", tc[0], got, tc[1])
		}
	}
}

// TestURLPathNormalization verifies "." and ".." segments (including %2e
// escapes) resolve while empty segments are preserved.
func TestURLPathNormalization(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	for _, tc := range [][2]string{
		{`new URL("http://h/a/./b/../c").pathname`, "/a/c"},
		{`new URL("http://h/a/%2e%2e/b").pathname`, "/b"},
		{`new URL("http://h/a/%2E/b").pathname`, "/a/b"},
		{`new URL("http://h//x").pathname`, "//x"},
		{`new URL("http://h/a/..").pathname`, "/"},
		{`new URL("http://h/a/b/.").pathname`, "/a/b/"},
	} {
		if got := evalString(t, js, tc[0]); got != tc[1] {
			t.Errorf("%s = %q, want %q", tc[0], got, tc[1])
		}
	}
}

// TestURLFileHostParsing verifies file: URLs keep the WHATWG file-host rules:
// "file:///p" has an EMPTY host (the third slash starts the path, unlike
// http's ignore-extra-slashes rule) and a "localhost" host normalizes to "".
func TestURLFileHostParsing(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	for _, tc := range [][2]string{
		{`new URL("file:///tmp/x").hostname + "|" + new URL("file:///tmp/x").pathname`, "|/tmp/x"},
		{`new URL("file://localhost/tmp/x").href`, "file:///tmp/x"},
		{`new URL("file://host.example/x").hostname`, "host.example"},
		{`new URL("file:/tmp/x").href`, "file:///tmp/x"},
		{`new URL("http:///x").hostname`, "x"}, // http DOES skip extra slashes
	} {
		if got := evalString(t, js, tc[0]); got != tc[1] {
			t.Errorf("%s = %q, want %q", tc[0], got, tc[1])
		}
	}
}

// TestURLSettersCanonicalize verifies the component setters run through the
// same encode/parse pipeline as the constructor.
func TestURLSettersCanonicalize(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	for _, tc := range [][2]string{
		{`(() => { const u = new URL("http://h/"); u.pathname = "/a b"; return u.href; })()`, "http://h/a%20b"},
		{`(() => { const u = new URL("http://h/"); u.pathname = "a\\b"; return u.pathname; })()`, "/a/b"},
		{`(() => { const u = new URL("http://h/"); u.search = "a=b c"; return u.search; })()`, "?a=b%20c"},
		{`(() => { const u = new URL("http://h/"); u.hash = "d e"; return u.hash; })()`, "#d%20e"},
		{`(() => { const u = new URL("http://h/"); u.username = "u u"; return u.href; })()`, "http://u%20u@h/"},
		{`(() => { const u = new URL("http://h/"); u.hostname = "192.168.000.001"; return u.hostname; })()`, "192.168.0.1"},
		{`(() => { const u = new URL("http://h/"); u.hostname = "MA\u00d1ANA.com"; return u.hostname; })()`, "xn--maana-pta.com"},
		// Invalid host assignment is ignored (WHATWG setter semantics).
		{`(() => { const u = new URL("http://h/"); u.hostname = "999.1.1.1"; return u.hostname; })()`, "h"},
		{`(() => { const u = new URL("http://h:8080/"); u.host = "G.com:443"; return u.host; })()`, "g.com:443"},
		{`(() => { const u = new URL("HTTPS://h/"); u.protocol = "HTTP"; return u.href; })()`, "http://h/"},
		// pathname setter must not treat "?"/"#" as terminators.
		{`(() => { const u = new URL("http://h/"); u.pathname = "/a?b#c"; return u.pathname; })()`, "/a%3Fb%23c"},
	} {
		if got := evalString(t, js, tc[0]); got != tc[1] {
			t.Errorf("%s = %q, want %q", tc[0], got, tc[1])
		}
	}
}

// TestURLSearchParamsStayLinkedAfterEncoding verifies searchParams round-trips
// still work with the encoding pipeline in place.
func TestURLSearchParamsStayLinkedAfterEncoding(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	if got := evalString(t, js, `
		const u = new URL("http://h/p?a=b c");
		const before = u.searchParams.get("a");
		u.searchParams.set("a", "x y");
		[before, u.searchParams.get("a"), u.href].join("|")
	`); got != "b c|x y|http://h/p?a=x+y" {
		t.Errorf("searchParams round-trip = %q", got)
	}
}
