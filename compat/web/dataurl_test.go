package web

import "testing"

// The Content-Type of a data: URL is the type PARSED and SERIALIZED BACK
// (mimesniff §4.4-4.5), not the raw text before the comma: that is what
// lowercases it, normalizes whitespace around parameters and drops malformed
// ones. The cases below are the shapes fetch/data-urls' processing tests use.
func TestParseDataURL(t *testing.T) {
	for _, tc := range []struct {
		in, mime, body string
	}{
		{"data:,", "text/plain;charset=US-ASCII", ""},
		{"data:,X", "text/plain;charset=US-ASCII", "X"},
		{"data:text/html,X", "text/html", "X"},
		{"data:TEXT/HTML,X", "text/html", "X"},
		{"data:text/html ;charset=gbk,X", "text/html;charset=gbk", "X"},
		{"data:text/html; charset=gbk,X", "text/html;charset=gbk", "X"},
		{"data:;base64,WA", "text/plain;charset=US-ASCII", "X"},
		{"data:image/gif;base64,R0lGODdh", "image/gif", "GIF87a"},
		// The MIME part is NOT percent-decoded (only the body is), per the data:
		// URL processor, so this stays literal.
		{"data:text/plain;charset=%22gbk%22,X", "text/plain;charset=%22gbk%22", "X"},
		// A type that does not parse falls back to the default rather than being
		// passed through.
		{"data:nonsense,X", "text/plain;charset=US-ASCII", "X"},
		// The FIRST comma separates; later ones are content.
		{"data:text/plain,a,b", "text/plain", "a,b"},
	} {
		mime, body, err := parseDataURL(tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if mime != tc.mime || string(body) != tc.body {
			t.Errorf("%q\n got mime=%q body=%q\nwant mime=%q body=%q",
				tc.in, mime, string(body), tc.mime, tc.body)
		}
	}
}

func TestParseMIMEType(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"text/html", "text/html"},
		{"TEXT/HTML", "text/html"},
		{"text/html ;charset=gbk", "text/html;charset=gbk"},
		{"text/html; charset=gbk", "text/html;charset=gbk"},
		{`text/html;charset="gbk"`, "text/html;charset=gbk"},
		{"text/html;charset=gbk;charset=big5", "text/html;charset=gbk"}, // first wins
		{"text/html;charset", "text/html"},                              // no value, dropped
		{"text/html;charset=", "text/html"},                             // empty value, dropped
		{`text/html;charset="a b"`, `text/html;charset="a b"`},          // requoted
	} {
		m, ok := parseMIMEType(tc.in)
		if !ok {
			t.Errorf("%q failed to parse", tc.in)
			continue
		}
		if got := m.String(); got != tc.want {
			t.Errorf("%q -> %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "text", "/html", "text/", "te xt/html"} {
		if _, ok := parseMIMEType(bad); ok {
			t.Errorf("%q parsed but should have failed", bad)
		}
	}
}
