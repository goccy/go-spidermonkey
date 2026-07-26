package web_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// URL protocol setter follows the WHATWG scheme-state override rules: the
// change is silently refused when the current scheme is file: with an empty
// host (http URLs require a host we don't have), when switching between
// special and non-special schemes, and when the new scheme is file: but the
// URL carries credentials or a port. Regression: file:///tmp/x with
// u.protocol = "http:" produced a corrupt href.
func TestURLProtocolSetterSchemeOverrideRules(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	eval(t, js, `
		globalThis.__c = {};
		{
			// file: with an EMPTY host cannot change scheme.
			const u = new URL("file:///tmp/x");
			u.protocol = "http:";
			__c.fileEmptyHost = u.href;
		}
		{
			// file: WITH a host may switch to another special scheme.
			const u = new URL("file://host/x");
			u.protocol = "http:";
			__c.fileWithHost = u.href;
		}
		{
			// special <-> non-special is still refused.
			const u = new URL("http://example.com/");
			u.protocol = "foo:";
			__c.specialToNon = u.href;
		}
		{
			// http <-> https (both special) still allowed.
			const u = new URL("http://example.com/");
			u.protocol = "https:";
			__c.httpToHttps = u.href;
		}
		{
			// credentials block a switch TO file:.
			const u = new URL("http://user:pass@example.com/p");
			u.protocol = "file:";
			__c.credsToFile = u.href;
		}
		{
			// a non-null port blocks a switch TO file:.
			const u = new URL("http://example.com:8080/p");
			u.protocol = "file:";
			__c.portToFile = u.href;
		}
	`)
	for expr, want := range map[string]string{
		"__c.fileEmptyHost": "file:///tmp/x",
		"__c.fileWithHost":  "http://host/x",
		"__c.specialToNon":  "http://example.com/",
		"__c.httpToHttps":   "https://example.com/",
		"__c.credsToFile":   "http://user:pass@example.com/p",
		"__c.portToFile":    "http://example.com:8080/p",
	} {
		if got := evalString(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// file:-scheme relative references resolve against a file: base per the
// WHATWG file/file-slash states: "file:x" against "file:///dir/y" is
// file:///dir/x (base directory + relative path), "file:/x" is rooted, and
// a bare "file:" keeps the base path (and query when none is supplied).
// Regression: the base path was ignored, yielding file:///x.
func TestURLFileSchemeRelativeReference(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	eval(t, js, `
		globalThis.__c = {};
		__c.rel = new URL("file:x", "file:///dir/y").href;
		__c.relNested = new URL("file:sub/z", "file:///a/b/c").href;
		__c.relDot = new URL("file:../up.txt", "file:///a/b/c").href;
		__c.rooted = new URL("file:/abs", "file:///dir/y").href;
		__c.bare = new URL("file:", "file:///dir/y?q=1").href;
		__c.queryOnly = new URL("file:?fresh=1", "file:///dir/y?q=1").href;
		__c.hostKept = new URL("file:x", "file://host/dir/y").href;
		__c.authority = new URL("file://other/p", "file:///dir/y").href;
		__c.noBase = new URL("file:x").href;
		__c.schemeless = new URL("z.txt", "file:///dir/y").href;
	`)
	for expr, want := range map[string]string{
		"__c.rel":        "file:///dir/x",
		"__c.relNested":  "file:///a/b/sub/z",
		"__c.relDot":     "file:///a/up.txt",
		"__c.rooted":     "file:///abs",
		"__c.bare":       "file:///dir/y?q=1",
		"__c.queryOnly":  "file:///dir/y?fresh=1",
		"__c.hostKept":   "file://host/dir/x",
		"__c.authority":  "file://other/p",
		"__c.noBase":     "file:///x",
		"__c.schemeless": "file:///dir/z.txt",
	} {
		if got := evalString(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}
