package web

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// enumerableGlobals are the globals Node.js leaves ENUMERABLE. Everything else
// a runtime installs — console, URL, TextEncoder, Event, DOMException, Buffer,
// process, the stream classes — is defined non-enumerable, so
// `for (const k in globalThis)` sees almost nothing.
//
// This is not cosmetic. Node's own test suite asserts on exactly that walk: its
// leaked-globals check runs on 'exit' in every single test, and reported every
// web global this layer installs as a leak. Any code that enumerates the global
// object — sandbox and snapshot tooling especially — sees a different world
// when the attribute is wrong. A plain `globalThis.X = …` creates an enumerable
// property, which is what both compat layers were doing.
//
// The list is Node's, read off a real node binary rather than assumed.
var enumerableGlobals = []string{
	"__dirname", "__filename", "atob", "btoa", "clearImmediate",
	"clearInterval", "clearTimeout", "crypto", "exports", "fetch", "global",
	"globalThis", "localStorage", "module", "navigator", "performance",
	"queueMicrotask", "require", "sessionStorage", "setImmediate",
	"setInterval", "setTimeout", "structuredClone",
}

// SnapshotGlobals returns the enumerable global names that already exist, for
// HideNewGlobals to diff against.
func SnapshotGlobals(js *spidermonkey.JS) (string, error) {
	r, err := js.Eval(context.Background(), `Object.keys(globalThis).join(" ")`)
	if err != nil {
		return "", err
	}
	if r.Error != nil {
		return "", r.Error
	}
	return r.Value.String(), nil
}

// HideNewGlobals makes every enumerable global added since the snapshot
// non-enumerable, except the ones a real Node.js keeps enumerable. Each compat
// layer calls it once, so a layer only ever hides what it installed itself.
func HideNewGlobals(js *spidermonkey.JS, before string) error {
	src := `
		(function (beforeList, keepList) {
			const before = new Set(beforeList.split(" "));
			const keep = new Set(keepList);
			for (const name of Object.keys(globalThis)) {
				if (before.has(name) || keep.has(name)) continue;
				const d = Object.getOwnPropertyDescriptor(globalThis, name);
				if (!d || !d.configurable) continue;
				d.enumerable = false;
				Object.defineProperty(globalThis, name, d);
			}
		})(` + jsLiteral(before) + `, ` + jsLiteral(enumerableGlobals) + `);
	`
	r, err := js.Eval(context.Background(), src)
	if err != nil {
		return err
	}
	if r.Error != nil {
		return fmt.Errorf("web: hiding installed globals: %w", r.Error)
	}
	return nil
}

func jsLiteral(v any) string {
	if names, ok := v.([]string); ok {
		sort.Strings(names)
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only strings and []string reach this
	}
	return string(b)
}
