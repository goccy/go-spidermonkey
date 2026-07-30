// compat/web: the global SCOPE interfaces
// (https://html.spec.whatwg.org/multipage/workers.html).
//
// Which of these exist is a property of what the global IS, not of which
// features an embedding selected — a worker's global answers to
// WorkerGlobalScope and has a WorkerNavigator, a document's global answers to
// Window. So this file is driven by Options.Scope rather than by the feature
// table, and it runs last, after everything it decorates exists.
//
// The classes are brands and behaviour, not fakes: `self instanceof
// DedicatedWorkerGlobalScope` holds because Symbol.hasInstance says the global
// is the one instance there is, which is exactly the relationship a real
// worker global has to its interface. Nothing here pretends a facility exists
// that does not — importScripts is defined only when the host supplied a way
// to fetch a script, and postMessage only reaches a parent when there is one.
(() => {
	"use strict";

	const scope = globalThis.__web_scope ?? "window";
	delete globalThis.__web_scope;
	const locationURL = globalThis.__web_location;
	delete globalThis.__web_location;

	// A global-scope interface has exactly one instance: the global. Answering
	// instanceof that way is how a runtime whose global cannot literally be
	// constructed from one of these classes still reports the right shape.
	function globalScopeClass(name, parent) {
		const cls = parent ? class extends parent {} : class {};
		Object.defineProperty(cls, "name", { value: name, configurable: true });
		Object.defineProperty(cls, Symbol.hasInstance, {
			value: (v) => v === globalThis, configurable: true,
		});
		Object.defineProperty(globalThis, name, {
			value: cls, writable: true, enumerable: false, configurable: true,
		});
		return cls;
	}

	// ------------------------------------------------------------ location
	// WorkerLocation and Location are both read-only views of one URL. The
	// members are prototype accessors, which is what an IDL attribute is and
	// what the suite's returns-same-object test reads them as.
	function defineLocationClass(name) {
		const cls = class {
			constructor(href) {
				Object.defineProperty(this, "_url", { value: new URL(href) });
			}
			toString() { return this._url.href; }
			toJSON() { return this._url.href; }
		};
		Object.defineProperty(cls, "name", { value: name, configurable: true });
		for (const part of ["href", "origin", "protocol", "host", "hostname",
			"port", "pathname", "search", "hash"]) {
			Object.defineProperty(cls.prototype, part, {
				get() { return this._url[part]; }, enumerable: true, configurable: true,
			});
		}
		Object.defineProperty(cls.prototype, Symbol.toStringTag, { value: name, configurable: true });
		Object.defineProperty(globalThis, name, {
			value: cls, writable: true, enumerable: false, configurable: true,
		});
		return cls;
	}

	const isWorker = scope !== "window";
	const LocationClass = defineLocationClass(isWorker ? "WorkerLocation" : "Location");
	if (locationURL !== undefined) {
		// ONE instance, so `location === location` and a property added to it
		// stays — a fresh object per access is a different object every time.
		const loc = new LocationClass(locationURL);
		Object.defineProperty(globalThis, "location", {
			get: () => loc, enumerable: true, configurable: true,
		});
	}

	// ------------------------------------------------------------ navigator
	// navigator's interface is WorkerNavigator in a worker and Navigator
	// otherwise; its members live on the prototype either way (see builtins.js,
	// which created the object with a prototype for exactly this reason).
	const navProto = Object.getPrototypeOf(globalThis.navigator);
	// A function, not a class: the interface object's `prototype` has to BE the
	// object navigator already inherits from — idlharness compares them — and a
	// class's prototype is non-writable.
	const NavClass = function () { throw new TypeError("Illegal constructor"); };
	Object.defineProperty(NavClass, "name", {
		value: isWorker ? "WorkerNavigator" : "Navigator", configurable: true,
	});
	NavClass.prototype = navProto;
	Object.defineProperty(navProto, "constructor", { value: NavClass, writable: true, configurable: true });
	Object.defineProperty(NavClass, Symbol.hasInstance, {
		value: (v) => v === globalThis.navigator, configurable: true,
	});
	Object.defineProperty(globalThis, NavClass.name, {
		value: NavClass, writable: true, enumerable: false, configurable: true,
	});
	Object.defineProperty(navProto, Symbol.toStringTag, { value: NavClass.name, configurable: true });
	// hardwareConcurrency is the count of parallel execution contexts the host
	// can actually run. The embedding reports it; 1 is the honest answer when it
	// did not, since one interpreter is one thread.
	if (!("hardwareConcurrency" in navProto)) {
		const n = Number(globalThis.__web_concurrency) || 1;
		Object.defineProperty(navProto, "hardwareConcurrency", {
			get: () => n, enumerable: true, configurable: true,
		});
	}
	delete globalThis.__web_concurrency;

	// ------------------------------------------------------------ the scope
	if (!isWorker) {
		globalScopeClass("Window");
		return;
	}

	const WorkerGlobalScope = globalScopeClass("WorkerGlobalScope");
	switch (scope) {
		case "sharedworker":
			globalScopeClass("SharedWorkerGlobalScope", WorkerGlobalScope);
			break;
		case "serviceworker":
			globalScopeClass("ServiceWorkerGlobalScope", WorkerGlobalScope);
			break;
		default:
			globalScopeClass("DedicatedWorkerGlobalScope", WorkerGlobalScope);
			break;
	}

	// close() ends the worker. With no parent to report to there is nothing to
	// end, so the flag is recorded and the host may act on it — a closed worker
	// must at least stop being reported as running.
	if (typeof globalThis.close !== "function") {
		Object.defineProperty(globalThis, "close", {
			value: function close() { globalThis.__web_closed = true; },
			writable: true, enumerable: true, configurable: true,
		});
	}

	// A dedicated worker's global has postMessage; a shared one's does not (its
	// ports arrive through connect events). __web_post_to_parent is installed by
	// the host when there IS a parent; without one the message has nowhere to go
	// and the call is a no-op, which is also what it returns: undefined.
	if (scope === "dedicatedworker" && typeof globalThis.postMessage !== "function") {
		Object.defineProperty(globalThis, "postMessage", {
			value: function postMessage(message, transferOrOptions) {
				if (arguments.length < 1) {
					throw new TypeError("postMessage: a message is required");
				}
				const send = globalThis.__web_post_to_parent;
				if (typeof send === "function") send(message, transferOrOptions);
			},
			writable: true, enumerable: true, configurable: true,
		});
	}

	// The worker global's event handler attributes, with the same
	// [TreatNonCallableAsNull] behaviour every other one has: assigning a
	// non-callable clears it, and reading back gives null.
	const handlers = {};
	for (const type of ["message", "messageerror", "connect", "languagechange", "offline", "online"]) {
		if (("on" + type) in globalThis) continue;
		Object.defineProperty(globalThis, "on" + type, {
			get() { return handlers[type] ?? null; },
			set(fn) {
				const prev = handlers[type];
				if (prev) globalThis.removeEventListener(type, prev);
				handlers[type] = typeof fn === "function" ? fn : null;
				if (handlers[type]) globalThis.addEventListener(type, handlers[type]);
			},
			configurable: true, enumerable: true,
		});
	}
})();
