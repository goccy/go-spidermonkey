package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

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
