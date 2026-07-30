// compat/web: BroadcastChannel
// (https://html.spec.whatwg.org/multipage/web-messaging.html#broadcasting-to-other-browsing-contexts).
//
// A channel delivers a message to every OTHER channel with the same name — never
// to the sender itself, which is the rule the whole API turns on. Within one
// agent that is all there is to it, and it is all in JavaScript: the registry is
// a map from name to the open channels, and delivery is a task per recipient.
//
// Across agents it is not implemented, and the reason is structural rather than
// unfinished: an agent has no host access (docs/engine-followups.md item 11), so
// a worker's channels cannot reach this registry. A cross-agent BroadcastChannel
// would have to route through the Worker message transport, which requires the
// parent to know which of its workers hold a channel of a given name — real
// work, and a different design from this one. What is here is complete for the
// scope it claims, and a message that cannot reach another agent is not silently
// dropped: nothing here pretends to have sent one.
(() => {
	"use strict";

	// name -> the channels open on it, in creation order. Delivery order is
	// creation order, which is what the standard specifies.
	const channels = new Map();

	class BroadcastChannel extends EventTarget {
		constructor(name) {
			if (!new.target) throw new TypeError("BroadcastChannel must be called with new");
			if (arguments.length < 1) {
				throw new TypeError("BroadcastChannel: a name is required");
			}
			super();
			this._name = String(name);
			this._closed = false;
			this._on = {};
			let list = channels.get(this._name);
			if (!list) channels.set(this._name, list = []);
			list.push(this);
		}

		get name() { return this._name; }

		postMessage(message) {
			if (this._closed) {
				throw new DOMException("postMessage: the channel is closed", "InvalidStateError");
			}
			if (arguments.length < 1) {
				throw new TypeError("postMessage: a message is required");
			}
			// The message is cloned ONCE, here, so every recipient gets the value as
			// it was at the moment of sending — a later mutation of the original is
			// not observable through the channel. A value that cannot be cloned is
			// the sender's error, raised before anything is delivered.
			const snapshot = structuredClone(message);
			const targets = (channels.get(this._name) || []).filter((c) => c !== this && !c._closed);
			for (const target of targets) {
				// A task per recipient: the event must not arrive before postMessage
				// has returned, and the recipients must not see each other's handlers
				// run inside their own.
				setTimeout(() => {
					if (target._closed) return;
					__dispatch_trusted(target, new MessageEvent("message", {
						data: snapshot,
						origin: globalThis.location ? globalThis.location.origin : "",
					}));
				}, 0);
			}
		}

		close() {
			if (this._closed) return;
			this._closed = true;
			const list = channels.get(this._name);
			if (!list) return;
			const i = list.indexOf(this);
			if (i >= 0) list.splice(i, 1);
			if (list.length === 0) channels.delete(this._name);
		}
	}

	for (const type of ["message", "messageerror"]) {
		Object.defineProperty(BroadcastChannel.prototype, "on" + type, {
			get() { return this._on[type] ?? null; },
			set(fn) {
				const prev = this._on[type];
				if (prev) this.removeEventListener(type, prev);
				this._on[type] = typeof fn === "function" ? fn : null;
				if (this._on[type]) this.addEventListener(type, this._on[type]);
			},
			configurable: true, enumerable: true,
		});
	}
	Object.defineProperty(BroadcastChannel.prototype, Symbol.toStringTag, {
		value: "BroadcastChannel", configurable: true,
	});
	globalThis.BroadcastChannel = BroadcastChannel;
})();
