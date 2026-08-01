package nodejs_test

import (
	spidermonkey "github.com/goccy/go-spidermonkey"
	"testing"
)

// TestURLRejectsOutOfRangePort verifies new URL() throws for a port > 65535.
func TestURLRejectsOutOfRangePort(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		try { new URL("http://h:99999/"); r.threw = false; } catch { r.threw = true; }
		r.canParse = (typeof URL.canParse === "function") ? URL.canParse("http://h:99999") : "n/a";
		r.valid = new URL("http://h:8080/").port;
	`)
	if !evalVal(t, js, "r.threw").Bool() {
		t.Error("new URL with port 99999 did not throw")
	}
	if got := evalStr(t, js, "String(r.valid)"); got != "8080" {
		t.Errorf("valid port = %q, want 8080", got)
	}
}

// TestURLResolvePathOnlyBase verifies url.resolve() with a scheme-less base
// returns a path-only result, not one carrying the internal placeholder host.
func TestURLResolvePathOnlyBase(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const url = require("url");
		r.pathBase = url.resolve("/one/two/three", "four");
		r.absBase = url.resolve("http://ex.com/a/b", "c");
	`)
	if got := evalStr(t, js, `r.pathBase`); got != "/one/two/four" {
		t.Errorf("resolve path-only base = %q, want /one/two/four", got)
	}
	if got := evalStr(t, js, `r.absBase`); got != "http://ex.com/a/c" {
		t.Errorf("resolve absolute base = %q, want http://ex.com/a/c", got)
	}
}

// TestLegacyURLParseNetworkPath verifies Node's slashesDenoteHost contract:
// a leading "//" denotes a host ONLY when the third argument says so (or a
// scheme precedes it) — url.parse("//evil.com/x") is a plain path.
func TestLegacyURLParseNetworkPath(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const url = require("url");
		globalThis.r = {};
		const u1 = url.parse("//evil.com/x");
		r.plain = [u1.pathname, String(u1.host), String(u1.hostname), String(u1.protocol), String(u1.slashes), u1.href].join("|");
		const u2 = url.parse("//evil.com/x", false, true);
		r.denoted = [u2.pathname, u2.host, u2.hostname, String(u2.protocol), String(u2.slashes), u2.href].join("|");
		const u3 = url.parse("//evil.com/a?q=1", true);
		r.query = [u3.pathname, String(u3.host), u3.query.q].join("|");
	`)
	if got := evalStr(t, js, `r.plain`); got != "//evil.com/x|null|null|null|null|//evil.com/x" {
		t.Errorf("parse(\"//evil.com/x\") = %s", got)
	}
	if got := evalStr(t, js, `r.denoted`); got != "/x|evil.com|evil.com|null|true|//evil.com/x" {
		t.Errorf("parse(\"//evil.com/x\", false, true) = %s", got)
	}
	if got := evalStr(t, js, `r.query`); got != "//evil.com/a|null|1" {
		t.Errorf("parse with query = %s", got)
	}
}

// TestLegacyURLParsePercentEncoding verifies url.parse reflects the WHATWG
// percent-encoding pipeline when it delegates to URL.
func TestLegacyURLParsePercentEncoding(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const url = require("url");
		globalThis.r = {};
		const u = url.parse("http://x.com/a b");
		r.enc = [u.pathname, u.path, u.href].join("|");
		// auth comes back decoded in the legacy API.
		r.auth = String(url.parse("http://u%20ser:pw@h/").auth);
	`)
	if got := evalStr(t, js, `r.enc`); got != "/a%20b|/a%20b|http://x.com/a%20b" {
		t.Errorf("percent-encoding = %s", got)
	}
	if got := evalStr(t, js, `r.auth`); got != "u ser:pw" {
		t.Errorf("auth = %q", got)
	}
}

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

// TestPathToFileURLEncoding verifies pathToFileURL percent-encodes the
// characters that would otherwise be misparsed in a URL ('#', '%', '?',
// spaces, controls) so every path round-trips through fileURLToPath.
func TestPathToFileURLEncoding(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const { pathToFileURL, fileURLToPath } = require("url");
		globalThis.r = {};
		r.hash = pathToFileURL("/tmp/a#b.txt").href;
		r.percent = pathToFileURL("/tmp/100%.txt").href;
		r.space = pathToFileURL("/tmp/a b.txt").href;
		r.question = pathToFileURL("/tmp/a?b.txt").href;
		r.newline = pathToFileURL("/tmp/a\nb").href;
		const roundTrips = [];
		for (const p of ["/tmp/a#b.txt", "/tmp/100%.txt", "/tmp/a b.txt", "/tmp/a?b.txt", "/tmp/a\tb\nc", "/tmp/%41.txt"]) {
			roundTrips.push(fileURLToPath(pathToFileURL(p)) === p);
		}
		r.roundTrips = roundTrips.join(",");
	`)
	for _, tc := range [][2]string{
		{`r.hash`, "file:///tmp/a%23b.txt"},
		{`r.percent`, "file:///tmp/100%25.txt"},
		{`r.space`, "file:///tmp/a%20b.txt"},
		{`r.question`, "file:///tmp/a%3Fb.txt"},
		{`r.newline`, "file:///tmp/a%0Ab"},
		{`r.roundTrips`, "true,true,true,true,true,true"},
	} {
		if got := evalStr(t, js, tc[0]); got != tc[1] {
			t.Errorf("%s = %q, want %q", tc[0], got, tc[1])
		}
	}
}

// TestFileURLToPathValidation verifies fileURLToPath rejects encoded '/'
// (ERR_INVALID_FILE_URL_PATH), non-file schemes (ERR_INVALID_URL_SCHEME),
// and decodes percent-escapes exactly once.
func TestFileURLToPathValidation(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const { fileURLToPath } = require("url");
		globalThis.r = {};
		const code = (fn) => { try { fn(); return "no-throw"; } catch (e) { return (e instanceof TypeError ? "TypeError:" : "other:") + (e.code || ""); } };
		r.encodedSlashUpper = code(() => fileURLToPath("file:///tmp/a%2Fb"));
		r.encodedSlashLower = code(() => fileURLToPath("file:///tmp/a%2fb"));
		r.httpScheme = code(() => fileURLToPath("http://h/x"));
		r.badHost = code(() => fileURLToPath("file://example.com/x"));
		r.localhost = fileURLToPath("file://localhost/tmp/x");
		r.decodeOnce = fileURLToPath("file:///tmp/%2520");
		r.plain = fileURLToPath("file:///tmp/ok.txt");
	`)
	for _, tc := range [][2]string{
		{`r.encodedSlashUpper`, "TypeError:ERR_INVALID_FILE_URL_PATH"},
		{`r.encodedSlashLower`, "TypeError:ERR_INVALID_FILE_URL_PATH"},
		{`r.httpScheme`, "TypeError:ERR_INVALID_URL_SCHEME"},
		{`r.badHost`, "TypeError:ERR_INVALID_FILE_URL_HOST"},
		{`r.localhost`, "/tmp/x"},
		// %2520 decodes ONCE to %20, not to a space.
		{`r.decodeOnce`, "/tmp/%20"},
		{`r.plain`, "/tmp/ok.txt"},
	} {
		if got := evalStr(t, js, tc[0]); got != tc[1] {
			t.Errorf("%s = %q, want %q", tc[0], got, tc[1])
		}
	}
}
