package nodejs_test

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"testing"
	"testing/fstest"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// Node defines its globals NON-enumerable, apart from a specific handful, so
// `for (const k in globalThis)` sees almost nothing. Matching that matters
// beyond tidiness: Node's own test suite walks exactly that list on 'exit' in
// every test and reports anything unexpected as a leaked global — with plain
// assignment (an enumerable property) every web global this layer installs was
// reported, failing hundreds of tests that have nothing to do with globals.
//
// The expected set is what a real `node` prints for the same walk.
func TestGlobalsAreNonEnumerableLikeNode(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{FS: fstest.MapFS{}})
	_ = rt
	r, err := js.Eval(context.Background(), `
		(() => { const out = []; for (const k in globalThis) out.push(k); return JSON.stringify(out.sort()); })()
	`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("threw: %v", r.Error)
	}
	var got []string
	if err := json.Unmarshal([]byte(r.Value.String()), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// What a real `node` prints for this walk in a FILE. The CommonJS wrapper
	// variables are deliberately absent: in a file they are wrapper parameters,
	// not globals, and Node's own leaked-globals check rejects them as globals.
	// (`node -e` does report them, which is why the list was read from a file.)
	want := []string{
		"atob", "btoa", "clearImmediate", "clearInterval", "clearTimeout",
		"crypto", "fetch", "global", "performance", "queueMicrotask",
		"setImmediate", "setInterval", "setTimeout", "structuredClone",
	}
	sort.Strings(want)
	extra := map[string]bool{}
	for _, g := range got {
		extra[g] = true
	}
	for _, w := range want {
		delete(extra, w)
	}
	// navigator is conditionally present in Node and is fine either way.
	delete(extra, "navigator")
	if len(extra) > 0 {
		var names []string
		for n := range extra {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Errorf("these globals are enumerable but are not in Node: %v", names)
	}
}

// TestNodeDoesNotExposeBrowserOnlyGlobals is the other half of the feature
// boundary in compat/web: Node shares that implementation, so something has to
// assert that it shares only the part Node actually has. Without this, a global
// added to compat/web for a browser API would appear in the Node runtime and
// nothing would notice.
func TestNodeDoesNotExposeBrowserOnlyGlobals(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{FS: fstest.MapFS{}})
	_ = rt
	for _, name := range web.FeatureGlobals(web.FeatureXMLHttpRequest) {
		r, err := js.Eval(context.Background(), "String(typeof globalThis["+strconv.Quote(name)+"])")
		if err != nil {
			t.Fatalf("eval: %v", err)
		}
		if got := r.Value.String(); got != "undefined" {
			t.Errorf("Node exposes %s (typeof %s); it is a browser API and compat/nodejs "+
				"must not inherit it from compat/web", name, got)
		}
	}
}
