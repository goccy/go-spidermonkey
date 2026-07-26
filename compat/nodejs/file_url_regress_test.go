package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
