package dom

import (
	"encoding/json"
	"fmt"
	"strings"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// The parse op speaks a compact structural encoding — nested JSON arrays,
// tagged by node type in the first slot — because the live tree is built on
// the guest side out of guest objects, and this is the narrowest bridge that
// carries the whole parse across:
//
//	[1, name, namespace, [k, ns, v, ...], [children...]]  element
//	[3, data]                                             text
//	[8, data]                                             comment
//	[10, name]                                            doctype
//
// The numbers are the DOM's own nodeType values, so the guest builder is a
// switch over vocabulary it already has.

func encodeNode(n *html.Node) any {
	switch n.Type {
	case html.ElementNode:
		attrs := make([]any, 0, len(n.Attr)*3)
		for _, a := range n.Attr {
			attrs = append(attrs, a.Key, a.Namespace, a.Val)
		}
		kids := make([]any, 0)
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if k := encodeNode(c); k != nil {
				kids = append(kids, k)
			}
		}
		return []any{1, n.Data, n.Namespace, attrs, kids}
	case html.TextNode:
		return []any{3, n.Data}
	case html.CommentNode:
		return []any{8, n.Data}
	case html.DoctypeNode:
		return []any{10, n.Data}
	default:
		return nil
	}
}

// opParseHTML(text, contextTag) parses text with x/net/html — the whole HTML5
// parsing algorithm — and returns the JSON encoding of the result's top-level
// nodes. An empty contextTag is a full-document parse (the result includes
// the doctype and the <html> element the algorithm synthesizes); a tag name
// is the fragment case, which is what innerHTML's setter means.
func opParseHTML(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("dom_parse_html: (text, contextTag?) required")
	}
	text := args[0].String()
	contextTag := ""
	if len(args) > 1 {
		contextTag = args[1].String()
	}

	var top []any
	if contextTag == "" {
		doc, err := html.Parse(strings.NewReader(text))
		if err != nil {
			return spidermonkey.ValueOf(`{"error":"parse failed"}`), nil
		}
		for c := doc.FirstChild; c != nil; c = c.NextSibling {
			if k := encodeNode(c); k != nil {
				top = append(top, k)
			}
		}
	} else {
		ctx := &html.Node{
			Type:     html.ElementNode,
			Data:     contextTag,
			DataAtom: atom.Lookup([]byte(contextTag)),
		}
		nodes, err := html.ParseFragment(strings.NewReader(text), ctx)
		if err != nil {
			return spidermonkey.ValueOf(`{"error":"parse failed"}`), nil
		}
		for _, n := range nodes {
			if k := encodeNode(n); k != nil {
				top = append(top, k)
			}
		}
	}
	if top == nil {
		top = []any{}
	}
	b, err := json.Marshal(top)
	if err != nil {
		return spidermonkey.ValueOf(`{"error":"encode failed"}`), nil
	}
	return spidermonkey.ValueOf(string(b)), nil
}

// opParseSelector(text) parses a Selectors-grammar string into an AST the
// guest matcher walks. Parsing is host-side because the grammar is a
// specification-defined text format; matching is guest-side because it runs
// against live guest objects. Errors return {"error": ...} and become the
// SyntaxError DOMException querySelector specifies.
func opParseSelector(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("dom_parse_selector: (text) required")
	}
	list, err := parseSelectorList(args[0].String())
	if err != nil {
		b, _ := json.Marshal(map[string]string{"error": err.Error()})
		return spidermonkey.ValueOf(string(b)), nil
	}
	b, err := json.Marshal(list)
	if err != nil {
		return spidermonkey.ValueOf(`{"error":"encode failed"}`), nil
	}
	return spidermonkey.ValueOf(string(b)), nil
}
