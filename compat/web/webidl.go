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
//
// Two more rules are enforced the same way, for the same reason.
//
// An interface whose prototype carries Symbol.for("go-spidermonkey.idlChecked")
// keeps its own receiver checks and only has its attributes made enumerable.
// The two forms are not interchangeable: an operation that returns a promise
// rejects when the receiver is wrong where one that returns a value throws, and
// only the interface itself knows which of its members are which.
//
// An interface's members only work on ITS instances. `Interface.prototype.attr`
// read off the prototype, or `Interface.prototype.method.call({})`, is a
// TypeError — the prototype is not an instance of itself, and a bare object
// never was. Written by hand this is a brand check at the top of every member;
// written once it is "the receiver must have this prototype in its chain",
// which is the same statement and cannot be forgotten on the next member.
//
// Required-argument counts are NOT enforced here. A JavaScript function's own
// `length` looks like the count, but it only is one where every optional
// parameter was written with a default — and these interfaces were not written
// under that rule, so `AbortSignal.abort(reason)` reads as one required
// argument when the argument is optional. Deriving the count from a declaration
// that does not carry it would refuse correct calls. It belongs at the
// definition sites, where the answer is known.

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
			const IDL_CHECKED = Symbol.for("go-spidermonkey.idlChecked");
			const seen = new WeakSet();
			// %AsyncFunction.prototype%, by identity rather than by name: an
			// operation written as an async function is one that returns a promise,
			// and Web IDL says such an operation REPORTS a bad receiver as a
			// rejection instead of throwing.
			const AsyncFunctionProto = Object.getPrototypeOf(async function () {});
			const skipOnInterface = new Set(["prototype", "name", "length", "constructor"]);

			// arityGuard wraps a prototype method so calling it on something that is
			// not an instance is a TypeError. The wrapper reports the same name and
			// length as what it wraps: a caller inspecting the operation must see the
			// operation, not the guard.
			function arityGuard(fn, brandProto) {
				if (brandProto === null) return fn;
				const rejects = Object.getPrototypeOf(fn) === AsyncFunctionProto;
				const guarded = function (...args) {
					if (!brandProto.isPrototypeOf(this)) {
						const e = new TypeError("Illegal invocation: the receiver is not of the right type");
						if (rejects) return Promise.reject(e);
						throw e;
					}
					return Reflect.apply(fn, this, args);
				};
				Object.defineProperty(guarded, "name", { value: fn.name, configurable: true });
				Object.defineProperty(guarded, "length", { value: fn.length, configurable: true });
				return guarded;
			}

			// accessorGuard wraps a getter/setter so reading or writing the attribute
			// on something that is not an instance is a TypeError.
			function accessorGuard(fn, brandProto, isSetter, key) {
				if (typeof fn !== "function") return fn;
				const guarded = isSetter
					? function (value) {
						if (!brandProto.isPrototypeOf(this)) {
							throw new TypeError("Illegal invocation: the receiver is not of the right type");
						}
						return Reflect.apply(fn, this, [value]);
					}
					: function () {
						if (!brandProto.isPrototypeOf(this)) {
							throw new TypeError("Illegal invocation: the receiver is not of the right type");
						}
						return Reflect.apply(fn, this, []);
					};
				// An attribute's accessors are named "get x" / "set x", which is not
				// what a shorthand getter in an object literal is called.
				Object.defineProperty(guarded, "name", { value: (isSetter ? "set " : "get ") + key, configurable: true });
				return guarded;
			}

			// brandProto is the prototype a member's receiver must inherit from, or
			// null on an interface object (a static member has no receiver to check).
			function normalize(target, skip, brandProto) {
				for (const key of Object.getOwnPropertyNames(target)) {
					if (skip.has(key) || key.startsWith("_")) continue;
					const d = Object.getOwnPropertyDescriptor(target, key);
					if (!d || !d.configurable) continue;
					let changed = false;
					if (!d.enumerable) { d.enumerable = true; changed = true; }
					if (typeof d.value === "function") {
						const guarded = arityGuard(d.value, brandProto);
						if (guarded !== d.value) { d.value = guarded; changed = true; }
					} else if (brandProto !== null && (d.get || d.set)) {
						if (d.get) { d.get = accessorGuard(d.get, brandProto, false, key); changed = true; }
						if (d.set) { d.set = accessorGuard(d.set, brandProto, true, key); changed = true; }
					}
					if (changed) Object.defineProperty(target, key, d);
				}
			}
			// normalizeInterface applies the rules to one interface object and its
			// prototype.
			function normalizeInterface(fn) {
				const proto = fn.prototype;
				if (proto && (typeof proto === "object" || typeof proto === "function")) {
					// An interface that already checks its own receiver keeps its own
					// answer. The two are not interchangeable: an operation that
					// returns a promise must REJECT rather than throw when the
					// receiver is wrong, and a guard that throws would turn every one
					// of those into a synchronous exception.
					if (!proto[IDL_CHECKED]) normalize(proto, new Set(["constructor"]), proto);
					else normalize(proto, new Set(["constructor"]), null);
				}
				normalize(fn, skipOnInterface, null);
			}

			for (const name of Object.getOwnPropertyNames(globalThis)) {
				// "__"-prefixed names are host plumbing, and anything that already
				// existed is the engine's rather than ours to re-attribute.
				if (before.has(name) || name.startsWith("__")) continue;
				const d = Object.getOwnPropertyDescriptor(globalThis, name);
				if (!d) continue;
				if (typeof d.value === "function") {
					normalizeInterface(d.value);
					continue;
				}
				// A NAMESPACE is an ordinary object whose members are operations and
				// interfaces — WebAssembly is one. Its interfaces are reachable only
				// through it, so a sweep that looked at function-valued globals alone
				// left every one of them unattributed.
				// self/window/globalThis name the global object itself; walking it as a
				// namespace would walk every global a second time, and one of those is
				// the global object again.
				if (d.value === null || typeof d.value !== "object" || d.value === globalThis) continue;
				if (seen.has(d.value)) continue;
				seen.add(d.value);
				normalize(d.value, new Set(), null);
				for (const member of Object.getOwnPropertyNames(d.value)) {
					if (member.startsWith("_")) continue;
					const md = Object.getOwnPropertyDescriptor(d.value, member);
					if (!md || typeof md.value !== "function" || !md.value.prototype) continue;
					normalizeInterface(md.value);
				}
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
