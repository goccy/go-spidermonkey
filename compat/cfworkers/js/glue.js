// cfworkers glue: the guest half of the request/response plumbing. Evaluated
// once per worker instance, after compat/web's builtins (Request/Response/
// Headers come from there) and before the user's module is booted.
(() => {
	"use strict";
	const state = { pending: [], result: null };

	// ------------------------------------------------------------- Cache API
	// In-memory caches.default / caches.open(name). Keys are the request URL;
	// values are cloned Responses. Per-instance (a warm isolate keeps it).

	const cacheStores = new Map();
	function makeCache() {
		const store = new Map();
		return {
			async match(request) {
				const url = typeof request === "string" ? request : request.url;
				const cached = store.get(url);
				return cached ? cached.clone() : undefined;
			},
			async put(request, response) {
				const url = typeof request === "string" ? request : request.url;
				if (response.status === 206) throw new TypeError("Cannot cache a partial response");
				store.set(url, response.clone());
			},
			async delete(request) {
				const url = typeof request === "string" ? request : request.url;
				return store.delete(url);
			},
		};
	}
	globalThis.caches = {
		default: makeCache(),
		async open(name) {
			if (!cacheStores.has(name)) cacheStores.set(name, makeCache());
			return cacheStores.get(name);
		},
		async has(name) { return cacheStores.has(name); },
		async delete(name) { return cacheStores.delete(name); },
	};

	// ---------------------------------------------------------- WebSocketPair
	// An in-process client/server socket pair (no external upgrade): what a
	// Worker returns from response.webSocket. Messages sent on one end arrive
	// on the other.

	class InProcessWebSocket extends EventTarget {
		constructor() {
			super();
			this.readyState = 0; // CONNECTING
			this._peer = null;
			this.OPEN = 1;
		}
		accept() { this.readyState = 1; }
		send(data) {
			if (this._peer) {
				const ev = new Event("message");
				ev.data = data;
				queueMicrotask(() => this._peer.dispatchEvent(ev));
			}
		}
		close(code, reason) {
			this.readyState = 3;
			if (this._peer && this._peer.readyState !== 3) {
				const ev = new Event("close");
				ev.code = code ?? 1000;
				ev.reason = reason ?? "";
				queueMicrotask(() => this._peer.dispatchEvent(ev));
			}
		}
		addEventListener(type, fn) { super.addEventListener(type, fn); }
	}
	globalThis.WebSocketPair = function WebSocketPair() {
		const a = new InProcessWebSocket();
		const b = new InProcessWebSocket();
		a._peer = b;
		b._peer = a;
		return { 0: a, 1: b };
	};

	// Build the Request the handler sees from host-supplied parts.
	globalThis.__cfw_make_request = (method, url, headerPairs, body) =>
		new Request(url, {
			method,
			headers: headerPairs,
			body: body === null || body === undefined ? undefined : body,
		});

	// Kick the handler; completion lands in state.result via the microtask
	// queue (drained by the host loop).
	globalThis.__cfw_run = (req) => {
		state.result = null;
		state.pending = []; // drop the previous request's settled waitUntil promises
		const ctx = {
			waitUntil: (p) => { state.pending.push(Promise.resolve(p).catch(() => {})); },
			passThroughOnException: () => {},
		};
		Promise.resolve()
			.then(() => globalThis.__cfw_handler.fetch(req, globalThis.__cfw_env, ctx))
			.then((resp) => { state.result = { ok: true, resp }; })
			.catch((err) => {
				// SpiderMonkey stacks do not include the message line; compose
				// both so the host error is actually diagnosable.
				const msg = err instanceof Error
					? `${err.name}: ${err.message}${err.stack ? "\n" + err.stack : ""}`
					: String(err);
				state.result = { ok: false, error: msg };
			});
	};

	// Non-fetch handlers (scheduled cron, queue). They return no HTTP
	// response; the host just drives them to completion and checks for error.
	globalThis.__cfw_run_scheduled = (cron, scheduledTime) => {
		state.result = null;
		state.pending = [];
		if (typeof globalThis.__cfw_handler.scheduled !== "function") {
			state.result = { ok: false, error: "worker has no scheduled() handler" };
			return;
		}
		const ctx = { waitUntil: (p) => state.pending.push(Promise.resolve(p).catch(() => {})), passThroughOnException: () => {} };
		const event = { cron: String(cron), scheduledTime: Number(scheduledTime), type: "scheduled" };
		Promise.resolve()
			.then(() => globalThis.__cfw_handler.scheduled(event, globalThis.__cfw_env, ctx))
			.then(() => { state.result = { ok: true, resp: null }; })
			.catch((err) => { state.result = { ok: false, error: String((err && err.stack) || err) }; });
	};

	globalThis.__cfw_run_queue = (batchJSON) => {
		state.result = null;
		state.pending = [];
		if (typeof globalThis.__cfw_handler.queue !== "function") {
			state.result = { ok: false, error: "worker has no queue() handler" };
			return;
		}
		const parsed = JSON.parse(batchJSON);
		const messages = parsed.messages.map((m, i) => ({
			id: m.id ?? String(i),
			timestamp: new Date(m.timestamp ?? 0),
			body: m.body,
			attempts: 1,
			ack() {},
			retry() {},
		}));
		const batch = { queue: parsed.queue, messages, ackAll() {}, retryAll() {} };
		const ctx = { waitUntil: (p) => state.pending.push(Promise.resolve(p).catch(() => {})), passThroughOnException: () => {} };
		Promise.resolve()
			.then(() => globalThis.__cfw_handler.queue(batch, globalThis.__cfw_env, ctx))
			.then(() => { state.result = { ok: true, resp: null }; })
			.catch((err) => { state.result = { ok: false, error: String((err && err.stack) || err) }; });
	};

	globalThis.__cfw_has_handler = (name) => typeof globalThis.__cfw_handler[name] === "function";

	globalThis.__cfw_status = () => (state.result === null ? "pending" : state.result.ok ? "ok" : "error");
	globalThis.__cfw_error = () => String(state.result.error);

	globalThis.__cfw_response_meta = () => {
		const r = state.result.resp;
		if (!r || typeof r !== "object" || typeof r.status !== "number"
			|| !r.headers || typeof r.headers.entries !== "function") {
			throw new TypeError("handler did not return a Response");
		}
		// entries() combines multiple Set-Cookie into one comma-joined value,
		// which corrupts cookies on the wire — emit each Set-Cookie as its own
		// header pair instead.
		const pairs = [];
		for (const [k, v] of r.headers.entries()) {
			if (k === "set-cookie") continue;
			pairs.push([k, v]);
		}
		if (typeof r.headers.getSetCookie === "function") {
			for (const c of r.headers.getSetCookie()) pairs.push(["set-cookie", c]);
		}
		return JSON.stringify({
			status: r.status,
			statusText: String(r.statusText || ""),
			headers: pairs,
		});
	};

	// The buffered body bytes (Uint8Array) or null.
	globalThis.__cfw_response_body = () => {
		const r = state.result.resp;
		return r._body === null || r._body === undefined ? null : r._body;
	};
})();
