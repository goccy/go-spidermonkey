// compat/web: Web Locks (https://w3c.github.io/web-locks/).
//
// This one lives entirely in the guest, and deliberately so: the Web Locks
// algorithm is promise orchestration over in-memory state — who holds what,
// who waits, and in which order requests become grantable — with no I/O and
// no blocking anywhere. The lock's lifetime IS a promise's lifetime (the one
// the callback returns), so the state machine belongs where the promises live.
//
// Scope: one lock manager per interpreter, which is one agent. Locks shared
// across agents need the agents to share state through the host; that surface
// arrives with Worker, and the manager here is written so a host-backed queue
// can replace the in-memory one without the API layer noticing.
(() => {
	"use strict";

	// One record per request, held or waiting, in request order.
	// {name, mode, granted, lock, releasedResolve, releasedReject, clientId}
	const held = [];
	const queue = [];
	const clientId = (() => {
		// One opaque id per agent, as query() reports.
		const b = crypto.getRandomValues(new Uint8Array(8));
		return [...b].map((x) => x.toString(16).padStart(2, "0")).join("");
	})();

	class Lock {
		constructor(name, mode) {
			this._name = name;
			this._mode = mode;
		}
		get name() { return this._name; }
		get mode() { return this._mode; }
	}

	// grantable: no one HOLDS an incompatible lock on name, and no EARLIER
	// request is still waiting for name — the queue is fair per resource.
	function grantable(name, mode, before) {
		for (const h of held) {
			if (h.name === name && (h.mode === "exclusive" || mode === "exclusive")) return false;
		}
		for (const w of queue) {
			if (w === before) break;
			if (w.name === name) return false;
		}
		return true;
	}

	// The queue is processed ASYNCHRONOUSLY, as the specification's lock task
	// queue is. The difference is observable: an abort signaled synchronously
	// after request() must win against a grant the request was already
	// eligible for — the grant has not happened yet, however available the
	// lock was.
	let scheduled = false;
	function schedule() {
		if (scheduled) return;
		scheduled = true;
		queueMicrotask(() => {
			scheduled = false;
			processQueue();
		});
	}

	// processQueue grants whatever became grantable, in order. A steal entry is
	// unconditional when its turn comes: it breaks whoever holds the name and
	// takes the lock — which is why steal travels the SAME queue as everything
	// else. A victim that was only requested a moment before the steal has
	// already been granted by its own earlier turn, and that ordering is
	// observable: the victim's promise must reject as "stolen", not resolve as
	// "never granted".
	function processQueue() {
		for (const w of [...queue]) {
			if (w.steal) {
				queue.splice(queue.indexOf(w), 1);
				for (const h of [...held]) {
					if (h.name !== w.name) continue;
					held.splice(held.indexOf(h), 1);
					h.rejectRequest(new DOMException("locks.request: the lock was stolen", "AbortError"));
				}
				grant(w);
				continue;
			}
			if (!grantable(w.name, w.mode, w)) continue;
			queue.splice(queue.indexOf(w), 1);
			grant(w);
		}
	}

	function grant(w) {
		const lock = new Lock(w.name, w.mode);
		const record = { name: w.name, mode: w.mode, clientId, rejectRequest: w.reject };
		held.push(record);
		if (w.signal) w.signal.removeEventListener("abort", w.onAbort);
		// The callback runs in a microtask; its settled promise releases the lock
		// and settles the request's own promise the same way.
		let result;
		try {
			result = Promise.resolve(w.callback(lock));
		} catch (e) {
			result = Promise.reject(e);
		}
		record.release = () => {
			const i = held.indexOf(record);
			if (i >= 0) held.splice(i, 1);
			schedule();
		};
		result.then(
			(v) => { record.release(); w.resolve(v); },
			(e) => { record.release(); w.reject(e); },
		);
	}

	class LockManager {
		async request(name, optionsOrCallback, maybeCallback) {
			let options = optionsOrCallback, callback = maybeCallback;
			if (callback === undefined) {
				options = {};
				callback = optionsOrCallback;
			}
			if (typeof callback !== "function") {
				throw new TypeError("locks.request: a callback is required");
			}
			name = String(name);
			const mode = options.mode === undefined ? "exclusive" : String(options.mode);
			if (mode !== "exclusive" && mode !== "shared") {
				throw new TypeError(`locks.request: ${mode} is not a lock mode`);
			}
			const ifAvailable = !!options.ifAvailable;
			const steal = !!options.steal;
			const signal = options.signal;
			// Reserved names, and the option combinations the specification rules
			// out before anything is queued.
			if (name.startsWith("-")) {
				throw new DOMException("locks.request: names starting with '-' are reserved", "NotSupportedError");
			}
			if (steal && ifAvailable) {
				throw new DOMException("locks.request: steal and ifAvailable cannot be combined", "NotSupportedError");
			}
			if (steal && mode !== "exclusive") {
				throw new DOMException("locks.request: only an exclusive lock can steal", "NotSupportedError");
			}
			if (signal !== undefined && (steal || ifAvailable)) {
				throw new DOMException("locks.request: a signal cannot be combined with steal or ifAvailable", "NotSupportedError");
			}
			if (signal !== undefined && !(signal instanceof AbortSignal)) {
				throw new TypeError("locks.request: signal is not an AbortSignal");
			}
			if (signal && signal.aborted) {
				throw signal.reason !== undefined ? signal.reason
					: new DOMException("locks.request: the request was aborted", "AbortError");
			}

			if (steal) {
				return await new Promise((resolve, reject) => {
					queue.push({ steal: true, name, mode, callback, resolve, reject });
					schedule();
				});
			}

			if (ifAvailable && !grantable(name, mode, null)) {
				// Not available: the callback still runs, with null, and its result
				// is still the request's result — including a throw.
				return await callback(null);
			}

			return await new Promise((resolve, reject) => {
				const w = { name, mode, callback, resolve, reject, signal };
				if (signal) {
					w.onAbort = () => {
						const i = queue.indexOf(w);
						if (i < 0) return; // already granted; abort no longer applies
						queue.splice(i, 1);
						reject(signal.reason !== undefined ? signal.reason
							: new DOMException("locks.request: the request was aborted", "AbortError"));
						schedule();
					};
					signal.addEventListener("abort", w.onAbort);
				}
				queue.push(w);
				schedule();
			});
		}

		async query() {
			// The snapshot is taken through the same task queue the grants travel:
			// a request made just before query() has been granted by the time the
			// answer is assembled, which is what the ordering tests check.
			await new Promise((resolve) => queueMicrotask(resolve));
			return {
				held: held.map((h) => ({ name: h.name, mode: h.mode, clientId })),
				pending: queue.map((w) => ({ name: w.name, mode: w.mode, clientId })),
			};
		}
	}

	// The brand a caller reads back with Object.prototype.toString; idlharness
	// checks it for both interfaces.
	Object.defineProperty(Lock.prototype, Symbol.toStringTag, { value: "Lock", configurable: true });
	Object.defineProperty(LockManager.prototype, Symbol.toStringTag, { value: "LockManager", configurable: true });

	globalThis.Lock = Lock;
	globalThis.LockManager = LockManager;
	const manager = new LockManager();
	// locks is an IDL attribute: it lives on navigator's PROTOTYPE, not on the
	// instance — assert_idl_attribute checks exactly that.
	Object.defineProperty(Object.getPrototypeOf(globalThis.navigator), "locks", {
		get: () => manager, enumerable: true, configurable: true,
	});
})();
