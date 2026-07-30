// compat/web: Worker, from the parent's side
// (https://html.spec.whatwg.org/multipage/workers.html).
//
// The thread is the engine's (worker.go, over the agent facility); this file is
// the API: the constructor's URL resolution, postMessage, terminate, and the
// events the parent observes. The script is FETCHED here rather than in Go,
// because fetch already applies the embedding's permissions, understands
// blob: and data: URLs, and reports a failure the way the constructor must —
// asynchronously, as an error event, since `new Worker(url)` returns before the
// script has loaded.
(() => {
	"use strict";

	const workers = new Map(); // agent id -> Worker

	class Worker extends EventTarget {
		constructor(scriptURL, options = {}) {
			super();
			if (arguments.length < 1) {
				throw new TypeError("Worker: a script URL is required");
			}
			let resolved;
			try {
				resolved = new URL(String(scriptURL), globalThis.location ? globalThis.location.href : undefined);
			} catch {
				throw new DOMException(`Worker: ${String(scriptURL)} is not a URL`, "SyntaxError");
			}
			this._on = {};
			this._id = 0;
			this._terminated = false;
			// Messages posted before the thread exists are queued, not lost: the
			// constructor returns immediately and a caller may post at once.
			this._pending = [];
			this._name = options && options.name !== undefined ? String(options.name) : "";

			// The load is asynchronous, so a failure is an error EVENT and not a
			// throw — the constructor has already returned by the time it is known.
			fetch(resolved.href)
				.then((res) => {
					if (!res.ok) throw new Error(`Worker: ${resolved.href} returned ${res.status}`);
					return res.text();
				})
				.then((source) => {
					if (this._terminated) return;
					this._id = __worker_spawn(source, resolved.href, this);
					workers.set(this._id, this);
					for (const m of this._pending.splice(0)) __worker_post(this._id, m);
				})
				.catch(() => {
					// "Fail the worker": one error event, and nothing running.
					if (this._terminated) return;
					__dispatch_trusted(this, new Event("error"));
				});
		}

		postMessage(message, transferOrOptions) {
			if (arguments.length < 1) {
				throw new TypeError("postMessage: a message is required");
			}
			if (this._terminated) return;
			if (this._id === 0) this._pending.push(message);
			else __worker_post(this._id, message);
		}

		terminate() {
			this._terminated = true;
			this._pending.length = 0;
			if (this._id !== 0) {
				__worker_terminate(this._id);
				workers.delete(this._id);
				this._id = 0;
			}
		}

		// _emit is what the host calls, once per transition.
		_emit(type, value) {
			switch (type) {
				case "message":
					__dispatch_trusted(this, new MessageEvent("message", { data: value }));
					return;
				case "error": {
					// A worker's uncaught exception reaches the parent as an ErrorEvent
					// carrying the message. The error VALUE cannot cross a thread, so
					// the message is what there is, and `error` is null rather than a
					// stand-in that would claim to be the thrown object.
					const ev = new ErrorEvent("error", {
						message: String(value ?? ""), cancelable: true,
					});
					__dispatch_trusted(this, ev);
					if (!ev.defaultPrevented) console.error("Uncaught in worker:", String(value ?? ""));
					return;
				}
				case "close":
					// The thread has ended. There is no 'close' event on Worker in the
					// standard; the parent learns of it only through what the worker
					// said before it went, so nothing is dispatched.
					workers.delete(this._id);
					this._id = 0;
					return;
			}
		}
	}

	for (const type of ["message", "messageerror", "error"]) {
		Object.defineProperty(Worker.prototype, "on" + type, {
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
	Object.defineProperty(Worker.prototype, Symbol.toStringTag, { value: "Worker", configurable: true });
	globalThis.Worker = Worker;
})();
