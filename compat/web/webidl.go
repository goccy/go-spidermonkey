package web

// webidl.go: giving this package's interfaces the property attributes Web IDL
// specifies for their members.
//
// An IDL operation or attribute is
// { writable: true, enumerable: TRUE, configurable: true } — a plain data or
// accessor property that shows up in a for-in walk and in
// Object.keys(Interface.prototype). An ES class member is the opposite: methods
// and accessors declared in a class body are non-enumerable. So every interface
// written as a `class` here had the wrong attributes on every one of its
// members, and it is not cosmetic: idlharness asserts the attribute directly
// (70 failing subtests in streams alone), and anything that enumerates a
// prototype — a polyfill deciding what to patch, a serializer, a test double —
// sees a different object than a browser shows it.
//
// It is fixed in one sweep rather than at 200 definition sites: the rule is one
// rule, and stating it once is the only way it stays true for the next
// interface someone adds.

import (
	"context"
	"fmt"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// SnapshotOwnGlobals returns every own global name, enumerable or not, so
// NormalizeIDLMembers can tell this package's interfaces from the engine's
// built-ins. (SnapshotGlobals is the enumerable-only companion, which is what
// HideNewGlobals needs.)
func SnapshotOwnGlobals(js *spidermonkey.JS) (string, error) {
	r, err := js.Eval(context.Background(), `Object.getOwnPropertyNames(globalThis).join(" ")`)
	if err != nil {
		return "", err
	}
	if r.Error != nil {
		return "", r.Error
	}
	return r.Value.String(), nil
}

// NormalizeIDLMembers makes the members of every interface added since the
// snapshot enumerable, on the prototype and on the interface object alike.
//
// What it deliberately leaves alone: `constructor`, and an interface object's
// own `name`, `length` and `prototype`, all of which IDL keeps non-enumerable;
// symbol-keyed properties, so Symbol.toStringTag and Symbol.iterator keep their
// specified attributes (getOwnPropertyNames does not report them); and anything
// whose name begins with an underscore, which is this package's own internal
// state and is not part of any interface.
func NormalizeIDLMembers(js *spidermonkey.JS, before string) error {
	src := `
		(function (beforeList) {
			const before = new Set(beforeList.split(" "));
			const skipOnInterface = new Set(["prototype", "name", "length", "constructor"]);
			function normalize(target, skip) {
				for (const key of Object.getOwnPropertyNames(target)) {
					if (skip.has(key) || key.startsWith("_")) continue;
					const d = Object.getOwnPropertyDescriptor(target, key);
					if (!d || !d.configurable || d.enumerable) continue;
					d.enumerable = true;
					Object.defineProperty(target, key, d);
				}
			}
			for (const name of Object.getOwnPropertyNames(globalThis)) {
				// "__"-prefixed names are host plumbing, and anything that already
				// existed is the engine's rather than ours to re-attribute.
				if (before.has(name) || name.startsWith("__")) continue;
				const d = Object.getOwnPropertyDescriptor(globalThis, name);
				if (!d || typeof d.value !== "function") continue;
				const proto = d.value.prototype;
				if (proto && (typeof proto === "object" || typeof proto === "function")) {
					normalize(proto, new Set(["constructor"]));
				}
				normalize(d.value, skipOnInterface);
			}
		})(` + jsLiteral(before) + `);
	`
	r, err := js.Eval(context.Background(), src)
	if err != nil {
		return err
	}
	if r.Error != nil {
		return fmt.Errorf("web: normalizing IDL member attributes: %w", r.Error)
	}
	return nil
}
