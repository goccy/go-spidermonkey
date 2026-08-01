package dom

import (
	"encoding/json"
	"strings"
	"testing"
)

// marshal without HTML escaping, so ">" reads as itself in expectations.
func marshalPlain(t *testing.T, v any) string {
	t.Helper()
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func TestSelectorParser(t *testing.T) {
	// Each case is a selector and the AST it must produce, stated as JSON —
	// the same encoding the guest matcher receives.
	cases := []struct {
		in   string
		want string
	}{
		{"div", `[[{"c":"","s":{"t":"div"}}]]`},
		{"DIV", `[[{"c":"","s":{"t":"div"}}]]`},
		{"*", `[[{"c":"","s":{"t":"*"}}]]`},
		{"#a.b", `[[{"c":"","s":{"i":"a","cl":["b"]}}]]`},
		{"a, b", `[[{"c":"","s":{"t":"a"}}],[{"c":"","s":{"t":"b"}}]]`},
		{"a b > c", `[[{"c":"","s":{"t":"a"}},{"c":" ","s":{"t":"b"}},{"c":">","s":{"t":"c"}}]]`},
		{"a + b ~ c", `[[{"c":"","s":{"t":"a"}},{"c":"+","s":{"t":"b"}},{"c":"~","s":{"t":"c"}}]]`},
		{"[x]", `[[{"c":"","s":{"a":[{"n":"x"}]}}]]`},
		{`[x="y z"]`, `[[{"c":"","s":{"a":[{"n":"x","op":"=","v":"y z"}]}}]]`},
		{"[x^=y i]", `[[{"c":"","s":{"a":[{"n":"x","op":"^=","v":"y","ci":true}]}}]]`},
		{":nth-child(2n+1)", `[[{"c":"","s":{"p":[{"n":"nth-child","ab":[2,1]}]}}]]`},
		{":nth-child(odd)", `[[{"c":"","s":{"p":[{"n":"nth-child","ab":[2,1]}]}}]]`},
		{":nth-child(-n+3)", `[[{"c":"","s":{"p":[{"n":"nth-child","ab":[-1,3]}]}}]]`},
		{":not(.a, #b)", `[[{"c":"","s":{"p":[{"n":"not","l":[[{"c":"","s":{"cl":["a"]}}],[{"c":"","s":{"i":"b"}}]]}]}}]]`},
		{"p:first-child", `[[{"c":"","s":{"t":"p","p":[{"n":"first-child"}]}}]]`},
	}
	for _, c := range cases {
		got, err := parseSelectorList(c.in)
		if err != nil {
			t.Errorf("parse(%q): %v", c.in, err)
			continue
		}
		if b := marshalPlain(t, got); b != c.want {
			t.Errorf("parse(%q)\n got %s\nwant %s", c.in, b, c.want)
		}
	}
}

func TestSelectorParserRejects(t *testing.T) {
	// An unknown pseudo-class or malformed selector must be a parse ERROR —
	// querySelector reports a SyntaxError — never a silent match-nothing.
	for _, in := range []string{
		"", "   ", ",", "a,", "a >", "[x", "[x=]", ":nth-child(x)",
		"::before", ":unknown-thing", "a{b}", ".", "#",
	} {
		if _, err := parseSelectorList(in); err == nil {
			t.Errorf("parse(%q) succeeded; a conforming querySelector must reject it", in)
		}
	}
}
