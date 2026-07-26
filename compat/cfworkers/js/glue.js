// cfworkers glue: the guest half of the request/response plumbing. Evaluated
// once per worker instance, after compat/web's builtins (Request/Response/
// Headers come from there) and before the user's module is booted.
(() => {
	"use strict";
	const state = { pending: [], result: null, waiting: 0 };
	// SpiderMonkey stacks do not include the message line; compose name+message
	// so a host error (fetch/scheduled/queue handler throw) is diagnosable.
	const fmtErr = (err) => (err instanceof Error
		? `${err.name}: ${err.message}${err.stack ? "\n" + err.stack : ""}`
		: String(err));
	// Track live waitUntil promises so the host can drain until they settle,
	// rather than until the loop goes fully idle (a leftover timer would
	// otherwise pin the pooled instance for the whole drain timeout).
	const trackWaitUntil = (p) => {
		state.waiting++;
		Promise.resolve(p).catch(() => {}).finally(() => { state.waiting--; });
	};
	globalThis.__cfw_waiting = () => state.waiting;

	// The shared web-layer timer wrapper (runTimerCb) routes a background
	// setTimeout/setInterval callback throw here. Without a handler the throw
	// would rethrow out of the loop, abort RunUntil, and fail an otherwise-valid
	// in-flight fetch response. On real Workers an uncaught background exception
	// is reported but does not sink the request, so record it and keep the loop
	// running. Returning true tells runTimerCb the throw was handled.
	globalThis.__emit_uncaught = (e) => {
		try { console.error("Uncaught (in background task):", (e && e.stack) || e); } catch { /* ignore */ }
		return true;
	};

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
	globalThis.__cfw_make_request = (method, url, headerPairs, body) => {
		const req = new Request(url, {
			method,
			headers: headerPairs,
			body: body === null || body === undefined ? undefined : body,
		});
		// request.cf: the Cloudflare-specific request metadata object. Real edge
		// values don't exist here, so provide a workerd-local-dev-style stub with
		// neutral values — code that reads request.cf.country etc. must not throw.
		req.cf = {
			colo: "DEV",
			country: "XX",
			city: "Development",
			continent: "XX",
			region: "Development",
			regionCode: "XX",
			postalCode: "00000",
			metroCode: "0",
			latitude: "0.00000",
			longitude: "0.00000",
			timezone: "Etc/UTC",
			httpProtocol: "HTTP/1.1",
			tlsVersion: "",
			tlsCipher: "",
			asn: 0,
			asOrganization: "Development",
			requestPriority: "",
			clientTcpRtt: 0,
			clientAcceptEncoding: "gzip, br",
			edgeRequestKeepAliveStatus: 1,
		};
		return req;
	};

	// Kick the handler; completion lands in state.result via the microtask
	// queue (drained by the host loop).
	globalThis.__cfw_run = (req) => {
		state.result = null;
		state.pending = []; state.waiting = 0; // drop the previous request's waitUntil state
		const ctx = {
			waitUntil: (p) => trackWaitUntil(p),
			passThroughOnException: () => {},
		};
		Promise.resolve()
			.then(() => globalThis.__cfw_handler.fetch(req, globalThis.__cfw_env, ctx))
			.then((resp) => { state.result = { ok: true, resp }; })
			.catch((err) => { state.result = { ok: false, error: fmtErr(err) }; });
	};

	// A response is streamed (not buffered) when its body only exists as a
	// ReadableStream: a guest `new Response(readable)` (_bodyStream), or a native
	// fetch() Response returned straight through (the reverse-proxy pattern —
	// no `_body` field at all, body is the host-backed stream).
	globalThis.__cfw_response_needs_stream = () => {
		const r = state.result && state.result.resp;
		if (!r || typeof r !== "object") return false;
		if (r._bodyStream) return true;
		return r._body === undefined && !!r.body;
	};

	// Pump the response's stream body to the host: `write(gen, chunkU8)`
	// writes+flushes one chunk to the client, `done(gen)` ends the response,
	// `fail(gen, msg)` aborts it. All three are SHARED per-worker Go functions;
	// gen ties this pump to its own request so a stale pump (client vanished,
	// worker kept the stream alive) can never write into a later response.
	globalThis.__cfw_stream_body = (gen, write, done, fail) => {
		const r = state.result.resp;
		const stream = (r._bodyStream ?? r.body) || null;
		if (!stream) { done(gen); return; }
		let reader;
		try { reader = stream.getReader(); }
		catch (e) { fail(gen, fmtErr(e)); return; }
		const enc = new TextEncoder();
		const pump = () => reader.read().then(({ value, done: eof }) => {
			if (eof) { done(gen); return; }
			// Normalize the chunk guest-side: the host op takes raw bytes.
			let u8;
			if (typeof value === "string") u8 = enc.encode(value);
			else if (value instanceof Uint8Array) u8 = value;
			else if (value instanceof ArrayBuffer) u8 = new Uint8Array(value);
			else if (ArrayBuffer.isView(value)) u8 = new Uint8Array(value.buffer, value.byteOffset, value.byteLength);
			else throw new TypeError("response stream chunks must be strings or BufferSource");
			if (u8.length > 0) write(gen, u8);
			return pump();
		}).catch((e) => {
			// A failed client write or an errored stream: release the source and
			// tell the host to abort the response.
			try { reader.cancel(e); } catch { /* already done */ }
			fail(gen, fmtErr(e));
		});
		pump();
	};

	// Non-fetch handlers (scheduled cron, queue). They return no HTTP
	// response; the host just drives them to completion and checks for error.
	globalThis.__cfw_run_scheduled = (cron, scheduledTime) => {
		state.result = null;
		state.pending = []; state.waiting = 0;
		if (typeof globalThis.__cfw_handler.scheduled !== "function") {
			state.result = { ok: false, error: "worker has no scheduled() handler" };
			return;
		}
		const ctx = { waitUntil: (p) => trackWaitUntil(p), passThroughOnException: () => {} };
		const event = { cron: String(cron), scheduledTime: Number(scheduledTime), type: "scheduled" };
		Promise.resolve()
			.then(() => globalThis.__cfw_handler.scheduled(event, globalThis.__cfw_env, ctx))
			.then(() => { state.result = { ok: true, resp: null }; })
			.catch((err) => { state.result = { ok: false, error: fmtErr(err) }; });
	};

	globalThis.__cfw_run_queue = (batchJSON) => {
		state.result = null;
		state.pending = []; state.waiting = 0;
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
		const ctx = { waitUntil: (p) => trackWaitUntil(p), passThroughOnException: () => {} };
		Promise.resolve()
			.then(() => globalThis.__cfw_handler.queue(batch, globalThis.__cfw_env, ctx))
			.then(() => { state.result = { ok: true, resp: null }; })
			.catch((err) => { state.result = { ok: false, error: fmtErr(err) }; });
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
