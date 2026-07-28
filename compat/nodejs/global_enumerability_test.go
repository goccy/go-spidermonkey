package nodejs_test

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"testing/fstest"

	spidermonkey "github.com/goccy/go-spidermonkey"
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
	// Node's list, minus the CommonJS wrapper variables that only exist inside a
	// module (this walk runs at the top level) and the storage globals this
	// runtime does not provide.
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
	// Globals this runtime legitimately adds to the enumerable set.
	for _, ok := range []string{"require", "module", "exports", "__filename", "__dirname", "navigator"} {
		delete(extra, ok)
	}
	if len(extra) > 0 {
		var names []string
		for n := range extra {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Errorf("these globals are enumerable but are not in Node: %v", names)
	}
}
