package wpt

// The .html test form. A browser loads the page, executes its script elements
// in order, and testharness completes on the load event; this runner does the
// same with the DOM module standing in for the page: the document is parsed
// into the live tree, the scripts run as steps, and the runner dispatches
// load. Reftests, crash tests and manual tests have no harness to report to —
// only files that pull in resources/testharness.js are runnable, which is the
// same content-based classification the upstream manifest makes.

import (
	"strings"

	"golang.org/x/net/html"
)

// htmlScript is one <script> element, in document order.
type htmlScript struct {
	src    string // the src attribute, "" for an inline script
	module bool   // type="module"
	text   string // the inline body when src == ""
}

// htmlTest is what the runner needs from a parsed .html test file.
type htmlTest struct {
	scripts  []htmlScript
	long     bool     // <meta name="timeout" content="long">
	variants []string // <meta name="variant" content="?...">
	harness  bool     // references /resources/testharness.js
}

// jsScriptType reports whether a script element's type attribute names
// classic JavaScript — an unrecognized type (a JSON template, a shader) is
// data, and executing it as code would be an invention.
func jsScriptType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "", "text/javascript", "application/javascript", "text/ecmascript",
		"application/ecmascript":
		return true
	}
	return false
}

func parseHTMLTest(src string) htmlTest {
	var out htmlTest
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return out
	}
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			attr := func(name string) string {
				for _, a := range n.Attr {
					if a.Key == name {
						return a.Val
					}
				}
				return ""
			}
			switch n.Data {
			case "script":
				typ := attr("type")
				s := htmlScript{src: attr("src"), module: strings.EqualFold(strings.TrimSpace(typ), "module")}
				if !s.module && !jsScriptType(typ) {
					break // a data block, not code
				}
				if strings.HasSuffix(s.src, "/resources/testharness.js") ||
					strings.HasSuffix(s.src, "/resources/testharnessreport.js") {
					// The harness is loaded by the runner; the report file is
					// what the runner IS.
					out.harness = true
					break
				}
				if s.src == "" {
					var b strings.Builder
					for c := n.FirstChild; c != nil; c = c.NextSibling {
						if c.Type == html.TextNode {
							b.WriteString(c.Data)
						}
					}
					s.text = b.String()
				}
				out.scripts = append(out.scripts, s)
			case "meta":
				switch attr("name") {
				case "timeout":
					if attr("content") == "long" {
						out.long = true
					}
				case "variant":
					out.variants = append(out.variants, attr("content"))
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

// isHTMLTestFile reports whether the file at root/p is a runnable .html test:
// a testharness page, not a reftest reference or a support file. The check
// reads the content because that is how the upstream manifest classifies —
// nothing in the NAME of an .html file says which kind it is.
func isHTMLTestFile(p string, src []byte) bool {
	base := p[strings.LastIndex(p, "/")+1:]
	stem := base
	if i := strings.LastIndex(stem, "."); i >= 0 {
		stem = stem[:i]
	}
	if strings.HasSuffix(stem, "-ref") || strings.HasSuffix(stem, "-notref") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "resources" || seg == "support" || seg == "tools" {
			return false
		}
	}
	return strings.Contains(string(src), "/resources/testharness.js")
}
