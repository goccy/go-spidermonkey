package web_test

// ECMA-429, the WinterTC Minimum Common Web API, is the standard that says which
// web APIs a server-side JavaScript runtime must have. It is what Bun, Deno,
// Node and workerd are measured against as "WinterTC conformant", and it is the
// surface WinterTC's own test suite — a subset of WPT, still being assembled as
// of the TPAC 2025 session — will cover. Until that suite exists, the
// specification's own list IS the checklist, so it is written down here and
// executed.
//
// Source: https://min-common-api.proposal.wintertc.org/ (ECMA-429), whose text
// lives at WinterTC55/proposal-minimum-common-api. The lists below are that
// document's "Common interfaces" and "Common methods and properties" sections,
// transcribed in the spec's own order and grouped by the standard each name
// comes from — so a reader can check the transcription against the source
// rather than trusting it.
//
// This is deliberately a surface test and nothing more. Whether each API is
// CORRECT is what wpt/ measures; what this pins is that none of them is absent,
// which is the failure mode a per-directory conformance score cannot show —
// a missing API has no failing tests.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// ecma429Interfaces are the interfaces the standard requires on globalThis.
var ecma429Interfaces = map[string][]string{
	"DOM":         {"AbortController", "AbortSignal", "Event", "EventTarget"},
	"HTML":        {"CustomEvent", "ErrorEvent", "MessageChannel", "MessageEvent", "MessagePort", "PromiseRejectionEvent"},
	"WebIDL":      {"DOMException"},
	"Fetch":       {"Headers", "Request", "Response"},
	"XHR":         {"FormData"},
	"FileAPI":     {"Blob", "File"},
	"Compression": {"CompressionStream", "DecompressionStream"},
	"Streams": {
		"ByteLengthQueuingStrategy", "CountQueuingStrategy",
		"ReadableByteStreamController", "ReadableStream",
		"ReadableStreamBYOBReader", "ReadableStreamBYOBRequest",
		"ReadableStreamDefaultController", "ReadableStreamDefaultReader",
		"TransformStream", "TransformStreamDefaultController",
		"WritableStream", "WritableStreamDefaultController", "WritableStreamDefaultWriter",
	},
	"Encoding":    {"TextDecoder", "TextDecoderStream", "TextEncoder", "TextEncoderStream"},
	"URL":         {"URL", "URLSearchParams"},
	"URLPattern":  {"URLPattern"},
	"WebCrypto":   {"Crypto", "CryptoKey", "SubtleCrypto"},
	"HR-Time":     {"Performance"},
	"WASM-JS-API": {"WebAssembly"},
}

// ecma429Globals are the methods and properties the standard requires.
var ecma429Globals = map[string][]string{
	"HTML": {
		"atob", "btoa", "clearTimeout", "clearInterval", "navigator",
		"queueMicrotask", "reportError", "self", "setTimeout", "setInterval",
		"structuredClone",
	},
	"Fetch":     {"fetch"},
	"Console":   {"console"},
	"WebCrypto": {"crypto"},
	"HR-Time":   {"performance"},
}

// ecma429Members are the members of a required namespace or object that the
// standard names individually. A namespace that exists but is empty would pass
// the interface check above and still be useless.
var ecma429Members = map[string][]string{
	"WebAssembly": {
		"Global", "Instance", "Memory", "Module", "Table", "Tag", "Exception",
		"CompileError", "LinkError", "RuntimeError",
		"compile", "compileStreaming", "instantiate", "instantiateStreaming",
		"JSTag", "validate",
	},
	"navigator": {"userAgent"},
}

// ecma429EventHandlers are the global event handler attributes the standard
// requires of a runtime whose global IS an EventTarget — which this one's is.
// (A runtime whose global cannot be an EventTarget, Node being the example the
// standard itself gives, must report the same events by another route and must
// NOT define these; see compat/nodejs.)
var ecma429EventHandlers = []string{"onerror", "onunhandledrejection", "onrejectionhandled"}

// known429Gaps are the requirements this runtime does not yet meet. The test
// reports them as gaps rather than failures so that the list itself is the
// record — an empty map is the goal, and a name silently disappearing from the
// requirement lists is what this arrangement is meant to prevent.
//
// Anything listed here MUST have a reason. "Not done yet" is a reason; being
// hard is not an excuse for leaving it undescribed.
var known429Gaps = map[string]string{}

func TestECMA429Surface(t *testing.T) {
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

	typeOf := func(expr string) string {
		r, err := js.Eval(context.Background(), expr)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if r.Error != nil {
			return "threw: " + r.Error.Error()
		}
		return r.Value.String()
	}

	report := func(kind, spec, name, expr string) {
		if typeOf(expr) != "undefined" {
			return
		}
		if reason, known := known429Gaps[strings.Split(name, ".")[0]]; known {
			t.Logf("KNOWN GAP  %-34s (%s, %s): %s", name, kind, spec, reason)
			return
		}
		t.Errorf("ECMA-429 requires %s %q (%s) and this installation does not define it",
			kind, name, spec)
	}

	for spec, names := range ecma429Interfaces {
		for _, name := range names {
			report("interface", spec, name, fmt.Sprintf("typeof globalThis[%q]", name))
		}
	}
	for spec, names := range ecma429Globals {
		for _, name := range names {
			report("global", spec, name, fmt.Sprintf("typeof globalThis[%q]", name))
		}
	}
	for owner, members := range ecma429Members {
		for _, m := range members {
			report("member", owner, owner+"."+m,
				fmt.Sprintf("globalThis[%q] === undefined ? \"undefined\" : typeof globalThis[%q][%q]", owner, owner, m))
		}
	}
	for _, name := range ecma429EventHandlers {
		// An event handler attribute is null when unset, so its presence is a
		// property question rather than a typeof one.
		if typeOf(fmt.Sprintf("String(%q in globalThis)", name)) != "true" {
			t.Errorf("ECMA-429 requires the global event handler attribute %q, and this installation does not define it", name)
		}
	}
}

// TestECMA429GapsAreReal keeps known429Gaps honest in the other direction: an
// entry that has been implemented must be deleted, or the list stops describing
// the runtime and starts excusing it.
func TestECMA429GapsAreReal(t *testing.T) {
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

	for name, reason := range known429Gaps {
		r, err := js.Eval(context.Background(), fmt.Sprintf(`typeof globalThis[%q]`, name))
		if err != nil {
			t.Fatal(err)
		}
		if r.Error == nil && r.Value.String() != "undefined" {
			t.Errorf("%q is listed as a known ECMA-429 gap (%q) but the installation defines it: "+
				"remove it from known429Gaps", name, reason)
		}
	}
}
