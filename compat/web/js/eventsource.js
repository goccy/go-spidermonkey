// compat/web: EventSource
// (https://html.spec.whatwg.org/multipage/server-sent-events.html).
//
// The protocol — the request, the stream parser, the reconnection schedule —
// is in Go (eventsource.go). This is the API shape: URL resolution and the
// SyntaxError it can raise, readyState and its transitions, the event handler
// attributes, and the MessageEvents the parser's reports become.
(() => {
	"use strict";

	const CONNECTING = 0, OPEN = 1, CLOSED = 2;

	// Every live source by its host handle, for __es_dispatch.
	const sources = new Map();

	class EventSource extends EventTarget {
		constructor(url, init = {}) {
			super();
			if (arguments.length < 1) {
				throw new TypeError("EventSource: a URL is required");
			}
			let resolved;
			try {
				resolved = new URL(String(url), globalThis.location ? globalThis.location.href : undefined);
			} catch {
				throw new DOMException(`EventSource: ${String(url)} is not a URL`, "SyntaxError");
			}
			this._url = resolved.href;
			this._origin = resolved.origin;
			this._withCredentials = !!(init && init.withCredentials);
			this._state = CONNECTING;
			this._on = {};
			this._id = __es_connect(this._url);
			sources.set(this._id, this);
		}

		get url() { return this._url; }
		get readyState() { return this._state; }
		get withCredentials() { return this._withCredentials; }

		close() {
			if (this._state === CLOSED) return;
			this._state = CLOSED;
			sources.delete(this._id);
			__es_close(this._id);
		}

		_dispatch(kind, a, b, c) {
			if (this._state === CLOSED) return;
			switch (kind) {
				case "open":
					this._state = OPEN;
					__dispatch_trusted(this, new Event("open"));
					return;
				case "message":
					__dispatch_trusted(this, new MessageEvent(a === "" ? "message" : a, {
						data: b, lastEventId: c, origin: this._origin,
					}));
					return;
				case "error":
					// a: fatal. A failed connection is CLOSED for good; a dropped one
					// goes back to CONNECTING while the host waits out the retry delay.
					this._state = a ? CLOSED : CONNECTING;
					if (a) sources.delete(this._id);
					__dispatch_trusted(this, new Event("error"));
					return;
			}
		}
	}

	for (const type of ["open", "message", "error"]) {
		Object.defineProperty(EventSource.prototype, "on" + type, {
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
	for (const [name, value] of [["CONNECTING", CONNECTING], ["OPEN", OPEN], ["CLOSED", CLOSED]]) {
		for (const target of [EventSource, EventSource.prototype]) {
			Object.defineProperty(target, name, { value, enumerable: true });
		}
	}

	globalThis.__es_dispatch = (id, kind, a, b, c) => {
		const es = sources.get(id);
		if (es) es._dispatch(kind, a, b, c);
	};
	globalThis.EventSource = EventSource;
})();
