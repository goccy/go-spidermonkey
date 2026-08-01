// compat/web: the Origin interface
// (https://html.spec.whatwg.org/multipage/browsers.html#the-origin-interface).
//
// An origin is either a (scheme, host, port) tuple or an opaque token equal
// only to itself. The tuple comes straight from URL.origin — the URL layer
// already knows that blob: borrows its inner URL's origin and that data:,
// about: and file: get nothing — and the registrable-domain half of
// isSameSite is answered by the host, where the public suffix list lives.
(() => {
	"use strict";

	const ops = globalThis.__web_ops;

	// state: per-Origin internals. A tuple origin holds its serialization and
	// parts; an opaque one holds a token, unique per creation, shared only by
	// Origin.from(anotherOrigin) — which is what "same origin" means for it.
	const state = new WeakMap();
	const stateOf = (o) => {
		const s = state.get(o);
		if (!s) throw new TypeError("called on an object that is not an Origin");
		return s;
	};

	function fromParsedURL(u) {
		const ser = u.origin;
		if (ser === "null") return { token: {} };
		return { ser, scheme: u.protocol.replace(/:$/, ""), host: new URL(ser).hostname };
	}

	class Origin {
		// The constructor mints a fresh opaque origin; tuples come from from().
		constructor() {
			state.set(this, { token: {} });
		}
		static from(input) {
			const out = Object.create(Origin.prototype);
			if (typeof input === "string") {
				let u;
				try {
					u = new URL(input);
				} catch {
					throw new TypeError(`Origin.from: "${input}" is not a valid URL`);
				}
				state.set(out, fromParsedURL(u));
				return out;
			}
			if (input instanceof Origin) {
				// A copy, not a sibling: sharing the token is what keeps an
				// opaque origin same-origin with the origin it came from.
				state.set(out, stateOf(input));
				return out;
			}
			if (input instanceof URL) {
				state.set(out, fromParsedURL(input));
				return out;
			}
			if (input === globalThis) {
				const loc = globalThis.location;
				const ser = loc === undefined ? "null" : new URL(loc.href).origin;
				state.set(out, ser === "null" ? { token: {} } : fromParsedURL(new URL(loc.href)));
				return out;
			}
			// Location, MessageEvent and friends deliberately throw: a Location
			// may be cross-origin and a constructed event's origin is just a
			// string someone typed. Only the types above carry a real origin.
			throw new TypeError("Origin.from: expected a string, URL, Origin, or global object");
		}
		get opaque() {
			return stateOf(this).token !== undefined;
		}
		isSameOrigin(other) {
			const a = stateOf(this), b = stateOf(other);
			if (a.token !== undefined || b.token !== undefined) return a.token === b.token;
			return a.ser === b.ser;
		}
		isSameSite(other) {
			const a = stateOf(this), b = stateOf(other);
			if (a.token !== undefined || b.token !== undefined) return a.token === b.token;
			if (a.scheme !== b.scheme) return false;
			const rdA = String(ops.origin_registrable_domain(a.host));
			const rdB = String(ops.origin_registrable_domain(b.host));
			if (rdA === "" || rdB === "") return a.host === b.host;
			return rdA === rdB;
		}
		toJSON() {
			const s = stateOf(this);
			return s.token !== undefined ? "null" : s.ser;
		}
	}
	Object.defineProperty(Origin.prototype, Symbol.toStringTag, {
		value: "Origin", configurable: true,
	});

	globalThis.Origin ??= Origin;
})();
