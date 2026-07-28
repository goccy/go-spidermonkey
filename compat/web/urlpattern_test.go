package web_test

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// URLPattern's syntax is path-to-regexp's, and the details that matter are the
// ones that are easy to get subtly wrong: the delimiter before a modified
// segment belongs to that segment, a repeated segment spans delimiters but
// excludes the first one from its group, and the component getters return the
// CANONICAL pattern rather than the text that was passed in.
func TestURLPattern(t *testing.T) {
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

	for _, tc := range []struct{ expr, want string }{
		// A named segment captures one path segment.
		{`new URLPattern({pathname:"/foo/:id"}).exec({pathname:"/foo/42"}).pathname.groups.id`, "42"},
		// The slash before an optional segment is optional with it.
		{`String(new URLPattern({pathname:"/foo/:bar?"}).test({pathname:"/foo"}))`, "true"},
		{`String(new URLPattern({pathname:"/foo/:bar?"}).test({pathname:"/foo/"}))`, "false"},
		// A repeated segment spans delimiters; the group excludes the leading one.
		{`new URLPattern({pathname:"/foo/:bar+"}).exec({pathname:"/foo/a/b"}).pathname.groups.bar`, "a/b"},
		{`String(new URLPattern({pathname:"/foo/:bar*"}).test({pathname:"/foo"}))`, "true"},
		// Canonical spelling: "(.*)" is "*", and braces around literal text vanish.
		{`new URLPattern({pathname:"/foo/(.*)"}).pathname`, "/foo/*"},
		{`new URLPattern({pathname:"/foo{/bar}"}).pathname`, "/foo/bar"},
		// A full-URL string pattern splits into components.
		{`new URLPattern("https://example.com/foo/:id").hostname`, "example.com"},
		{`new URLPattern("https://example.com/foo/:id").exec("https://example.com/foo/7").pathname.groups.id`, "7"},
		// An unspecified component matches anything.
		{`String(new URLPattern({protocol:"https"}).test("https://example.com/anything"))`, "true"},
		{`String(new URLPattern({protocol:"https"}).test("http://example.com/"))`, "false"},
		// A non-match is null, not a throw.
		{`String(new URLPattern({pathname:"/foo"}).exec({pathname:"/bar"}))`, "null"},
		{`String(new URLPattern({pathname:"/foo/(\\d+)"}).hasRegExpGroups)`, "true"},
		{`String(new URLPattern({pathname:"/foo/:id"}).hasRegExpGroups)`, "false"},
	} {
		r, err := js.Eval(context.Background(), `String(`+tc.expr+`)`)
		if err != nil {
			t.Errorf("%s: %v", tc.expr, err)
			continue
		}
		if r.Error != nil {
			t.Errorf("%s threw: %v", tc.expr, r.Error)
			continue
		}
		if got := r.Value.String(); got != tc.want {
			t.Errorf("%s\n got %q\nwant %q", tc.expr, got, tc.want)
		}
	}
}
