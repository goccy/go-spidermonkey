// Package dom is the document tree slice of the web platform, as an opt-in
// module for compat/web.
//
// It is a separate package for the same reason canvas is: weight. The HTML
// parser (golang.org/x/net/html — the whole HTML5 parsing algorithm) and the
// selector engine are dependencies an embedding that never touches a document
// should not link. Importing this package and passing Module() to
// web.InstallWith is the opt-in; a build that does neither carries none of it.
//
//	web.InstallWith(js, web.Options{
//		Modules: []web.Module{dom.Module()},
//	})
//
// The module provides the "dom" feature (the Node tree, Document and the
// document instance) and "dom-parsing" (DOMParser). Everything it installs is
// [Exposed=Window]: worker scopes are stripped of the lot by the scope layer.
//
// What is deliberately still TODO — not out of scope — is everything that
// needs more than one window or a renderer: browsing contexts
// (iframe.contentWindow, window.open, navigation), layout, and CSSOM beyond
// the small style bag. See docs/dom-module-plan.md.
package dom

import (
	_ "embed"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

//go:embed dom.js
var domJS string

// Module is what an embedding passes to web.Options.Modules to opt in.
func Module() web.Module {
	return web.Module{
		Features: map[web.Feature][]string{
			web.FeatureDOM: {
				"Node", "Document", "DocumentFragment", "DocumentType",
				"CharacterData", "Text", "Comment", "ProcessingInstruction",
				"Element", "HTMLElement", "HTMLUnknownElement", "HTMLHtmlElement",
				"HTMLHeadElement", "HTMLBodyElement", "HTMLDivElement",
				"HTMLSpanElement", "HTMLScriptElement", "HTMLAnchorElement",
				"HTMLTitleElement",
				"Attr", "NamedNodeMap", "HTMLCollection", "NodeList",
				"DOMTokenList", "DOMImplementation", "document",
			},
			web.FeatureDOMParsing: {"DOMParser"},
		},
		Script: domJS,
		Ops: func(js *spidermonkey.JS) (map[string]spidermonkey.Func, func(), error) {
			return map[string]spidermonkey.Func{
				"dom_parse_html":     opParseHTML,
				"dom_parse_selector": opParseSelector,
			}, nil, nil
		},
	}
}
