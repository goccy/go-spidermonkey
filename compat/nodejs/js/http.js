// compat/nodejs: node:http — the SERVER side, implemented directly over
// Go's net/http (no node:net layer): Go owns accept/parse/keep-alive; each
// request is dispatched to __node_http_dispatch on the event loop, and the
// response flows back through the http_respond/http_write/http_end ops.
// The client side (http.request) is not implemented yet. Evaluated last;
// cleans up the registry global.
(() => {
	"use strict";
	const ops = globalThis.__node_ops;
	const core = globalThis.__node_core_registry; // extended.js (after) deletes it

	const EventEmitter = core.events;
	const { Readable, Writable } = core.stream;

	const METHODS = [
		"ACL", "BIND", "CHECKOUT", "CONNECT", "COPY", "DELETE", "GET", "HEAD",
		"LINK", "LOCK", "M-SEARCH", "MERGE", "MKACTIVITY", "MKCALENDAR",
		"MKCOL", "MOVE", "NOTIFY", "OPTIONS", "PATCH", "POST", "PROPFIND",
		"PROPPATCH", "PURGE", "PUT", "REBIND", "REPORT", "SEARCH", "SOURCE",
		"SUBSCRIBE", "TRACE", "UNBIND", "UNLINK", "UNLOCK", "UNSUBSCRIBE",
	];

	const STATUS_CODES = {
		100: "Continue", 101: "Switching Protocols", 200: "OK", 201: "Created",
		202: "Accepted", 204: "No Content", 206: "Partial Content",
		301: "Moved Permanently", 302: "Found", 303: "See Other",
		304: "Not Modified", 307: "Temporary Redirect", 308: "Permanent Redirect",
		400: "Bad Request", 401: "Unauthorized", 403: "Forbidden",
		404: "Not Found", 405: "Method Not Allowed", 406: "Not Acceptable",
		408: "Request Timeout", 409: "Conflict", 410: "Gone",
		411: "Length Required", 412: "Precondition Failed", 413: "Payload Too Large",
		414: "URI Too Long", 415: "Unsupported Media Type", 416: "Range Not Satisfiable",
		417: "Expectation Failed", 422: "Unprocessable Entity", 426: "Upgrade Required",
		428: "Precondition Required", 429: "Too Many Requests", 431: "Request Header Fields Too Large",
		500: "Internal Server Error", 501: "Not Implemented", 502: "Bad Gateway",
		503: "Service Unavailable", 504: "Gateway Timeout", 505: "HTTP Version Not Supported",
	};

	// Idle-timeout plumbing (Node's socket.setTimeout semantics) shared by the
	// per-request socket shim, req/res delegation, and ClientRequest: an unref'd
	// timer emits 'timeout' on the target after ms of inactivity; any activity
	// (request-body chunk in, response write out, client response progress)
	// restarts the countdown. The connection is NOT torn down on timeout — Node
	// leaves that to the application. setTimeout(0) disables.
	function idleTimeoutArm(target) {
		const ms = target._timeoutMs;
		if (!ms || target.destroyed) return;
		const t = target._timeoutTimer;
		if (t && typeof t.refresh === "function") { t.refresh(); return; } // restart in place, keeps unref state
		if (t) clearTimeout(t);
		const timer = setTimeout(() => { target._timeoutTimer = null; target.emit("timeout"); }, ms);
		if (timer && typeof timer.unref === "function") timer.unref(); // never keeps the loop alive
		target._timeoutTimer = timer;
	}
	function idleTimeoutClear(target) {
		if (target._timeoutTimer) { clearTimeout(target._timeoutTimer); target._timeoutTimer = null; }
	}
	function idleTimeoutTouch(target) { if (target && target._timeoutMs) idleTimeoutArm(target); }
	function idleSetTimeout(target, ms, cb) {
		ms = Number(ms) || 0;
		if (typeof cb === "function") target.once("timeout", cb);
		idleTimeoutClear(target);
		target.timeout = target._timeoutMs = ms;
		if (ms > 0) idleTimeoutArm(target);
		return target;
	}

	function makeSocket(remoteAddress, remotePort) {
		const socket = new EventEmitter();
		socket.remoteAddress = remoteAddress || "127.0.0.1";
		socket.remotePort = remotePort || 0;
		socket.remoteFamily = "IPv4";
		socket.localAddress = "127.0.0.1";
		socket.localPort = 0;
		socket.encrypted = false;
		socket.readable = true;
		socket.writable = true;
		socket.destroyed = false;
		socket._timeoutMs = 0;
		socket._timeoutTimer = null;
		socket.destroy = () => { socket.destroyed = true; idleTimeoutClear(socket); };
		socket.end = () => {};
		socket.setTimeout = (ms, cb) => idleSetTimeout(socket, ms, cb);
		socket._touch = () => idleTimeoutTouch(socket);
		socket.setNoDelay = () => socket;
		socket.setKeepAlive = () => socket;
		socket.address = () => ({ address: "127.0.0.1", family: "IPv4", port: 0 });
		socket.unref = () => socket;
		socket.ref = () => socket;
		return socket;
	}

	class IncomingMessage extends Readable {
		constructor(init = {}) {
			super();
			this.method = init.method;
			this.url = init.url;
			this.httpVersion = "1.1";
			this.httpVersionMajor = 1;
			this.httpVersionMinor = 1;
			this.socket = this.connection = init.socket || makeSocket();
			this.headers = {};
			this.rawHeaders = [];
			for (const [name, value] of init.rawHeaders || []) {
				this.rawHeaders.push(name, value);
				const key = name.toLowerCase();
				// Node ALWAYS exposes set-cookie as an array, even for a single value.
				if (key === "set-cookie") {
					if (this.headers[key] === undefined) this.headers[key] = [value];
					else this.headers[key].push(value);
				} else if (this.headers[key] === undefined) {
					this.headers[key] = value;
				} else {
					this.headers[key] += ", " + value;
				}
			}
			this.complete = false;
			this.aborted = false;
		}
		setTimeout(ms, cb) {
			// Node delegates to the underlying socket (message.socket.setTimeout).
			if (this.socket && typeof this.socket.setTimeout === "function") this.socket.setTimeout(ms, cb);
			return this;
		}
	}

	// A header name has to be an HTTP token and a value has to be something
	// that can go on the wire. Both are named errors, because a header silently
	// dropped or written as "undefined" is a bug the caller finds much later,
	// in a client.
	const TOKEN_RE = /^[\^`\-\w!#$%&'*+.|~]+$/;
	function checkHeaderName(name) {
		if (typeof name !== "string" || name === "" || !TOKEN_RE.test(name)) {
			throw Object.assign(new TypeError(`Header name must be a valid HTTP token [${JSON.stringify(String(name))}]`),
				{ code: "ERR_INVALID_HTTP_TOKEN" });
		}
	}
	function checkHeaderValue(name, value) {
		if (value === undefined) {
			throw Object.assign(new TypeError(`Invalid value "undefined" for header "${name}"`),
				{ code: "ERR_HTTP_INVALID_HEADER_VALUE" });
		}
	}

	class ServerResponse extends Writable {
		constructor(init = {}) {
			super();
			this._reqId = init.reqId;
			// The server always hands a socket in; a bare new OutgoingMessage()
			// has NONE until its 'socket' event, and setTimeout queues on it.
			this.socket = this.connection = init.socket ?? null;
			this.req = init.req;
			this.statusCode = 200;
			this.statusMessage = undefined;
			this.headersSent = false;
			this.finished = false;
			this.sendDate = true;
			this._headers = new Map(); // lowercased -> { name, value }
		}
		setHeader(name, value) {
			checkHeaderName(name);
			checkHeaderValue(name, value);
			this._headers.set(String(name).toLowerCase(), { name: String(name), value });
			return this;
		}
		getHeader(name) {
			const e = this._headers.get(String(name).toLowerCase());
			return e ? e.value : undefined;
		}
		getHeaders() {
			const out = Object.create(null);
			for (const [key, e] of this._headers) out[key] = e.value;
			return out;
		}
		getHeaderNames() { return [...this._headers.keys()]; }
		hasHeader(name) { return this._headers.has(String(name).toLowerCase()); }
		removeHeader(name) { this._headers.delete(String(name).toLowerCase()); }
		// Node >= 16.17. Unlike setHeader this ACCUMULATES, which is the whole
		// point: Set-Cookie and Vary are sent as repeated fields, and collapsing
		// them to one value silently drops all but the last.
		appendHeader(name, value) {
			const key = String(name).toLowerCase();
			const e = this._headers.get(key);
			if (e === undefined) return this.setHeader(name, value);
			const merged = (Array.isArray(e.value) ? e.value : [e.value])
				.concat(Array.isArray(value) ? value : [value]);
			this._headers.set(key, { name: e.name, value: merged });
			return this;
		}
		writeHead(statusCode, reasonOrHeaders, headers) {
			// Headers already on the wire cannot be rewritten, and a status code
			// outside 100..999 is not one — both are named errors, because a
			// silently-accepted 99 produces a response no client can parse.
			if (this.headersSent) {
				throw Object.assign(new Error("Cannot write headers after they are sent to the client"),
					{ code: "ERR_HTTP_HEADERS_SENT" });
			}
			// Node coerces with |0 and range-checks the result, but reports the
			// ORIGINAL value, so the message names what the caller actually wrote.
			const code = statusCode | 0;
			if (code < 100 || code > 999) {
				const shown = statusCode !== null && typeof statusCode === "object"
					? (Array.isArray(statusCode) ? "[]" : "{}")
					: String(statusCode);
				throw Object.assign(new RangeError(`Invalid status code: ${shown}`),
					{ code: "ERR_HTTP_INVALID_STATUS_CODE" });
			}
			this.statusCode = code;
			if (typeof reasonOrHeaders === "string") this.statusMessage = reasonOrHeaders;
			else {
				// A status with no standard reason phrase reports "unknown", which
				// is what Node puts on the wire for it.
				this.statusMessage = STATUS_CODES[code] || "unknown";
				if (reasonOrHeaders !== undefined) headers = reasonOrHeaders;
			}
			if (headers) {
				if (Array.isArray(headers)) {
					// A flat header array must be name/value PAIRS; an odd length
					// means one of them has no partner and would be dropped.
					if (headers.length % 2 !== 0) {
						throw Object.assign(new TypeError(`The argument 'headers' is invalid. Received ${JSON.stringify(headers)}`),
							{ code: "ERR_INVALID_ARG_VALUE" });
					}
					for (let i = 0; i + 1 < headers.length; i += 2) this.setHeader(headers[i], headers[i + 1]);
				} else {
					for (const k of Object.keys(headers)) this.setHeader(k, headers[k]);
				}
			}
			// Node commits (sends) the headers on writeHead, so headersSent becomes
			// true immediately — error-handling middleware branches on it.
			this._ensureHead();
			return this;
		}
		flushHeaders() { this._ensureHead(); }
		writeContinue() {}
		setTimeout(ms, cb) {
			// Node delegates to the socket — the one it has, or the one its
			// 'socket' event will deliver — and the message re-emits the
			// socket's timeout as its own.
			if (cb) this.once("timeout", cb);
			const arm = (socket) => {
				if (socket && typeof socket.setTimeout === "function") {
					socket.setTimeout(ms, () => this.emit("timeout"));
				}
			};
			if (this.socket) arm(this.socket);
			else this.once("socket", arm);
			return this;
		}
		_ensureHead() {
			if (this.headersSent) return;
			// The headers are COMMITTED here whether or not a request is bound
			// yet: writeHead's contract is that nothing more may be added, and a
			// response that reports otherwise lets a caller silently lose them.
			this.headersSent = true;
			if (this._reqId === undefined) return;
			const pairs = [];
			for (const { name, value } of this._headers.values()) {
				if (Array.isArray(value)) for (const v of value) pairs.push([name, String(v)]);
				else pairs.push([name, String(value)]);
			}
			ops.http_respond(this._reqId, this.statusCode | 0, JSON.stringify(pairs));
		}
		_write(chunk, encoding, callback) {
			this._ensureHead();
			if (this.socket && this.socket._touch) this.socket._touch(); // response write = activity
			if (this._reqId !== undefined && chunk.length) {
				// The op fires our callback only once the chunk has been flushed
				// to the socket (off the loop goroutine), so a slow client
				// naturally backpressures the guest Writable.
				ops.http_write(this._reqId, chunk, callback);
			} else {
				callback();
			}
		}
		_final(callback) {
			this._ensureHead();
			if (this._reqId !== undefined) ops.http_end(this._reqId);
			callback();
		}
		end(chunk, encoding, cb) {
			if (typeof chunk === "function") { cb = chunk; chunk = undefined; encoding = undefined; }
			else if (typeof encoding === "function") { cb = encoding; encoding = undefined; }
			// If end() supplies the whole body and no data has streamed yet (headers
			// not committed), set Content-Length so the response isn't chunked — Node
			// auto-sets it for res.end(body).
			if (!this.headersSent && chunk != null && !this.hasHeader("content-length") && !this.hasHeader("transfer-encoding")) {
				const buf = typeof chunk === "string" ? Buffer.from(chunk, typeof encoding === "string" ? encoding : "utf8") : chunk;
				this.setHeader("Content-Length", buf.length);
			}
			return Writable.prototype.end.call(this, chunk, encoding, cb);
		}
	}

	const servers = new Map(); // server id -> Server

	class Server extends EventEmitter {
		constructor(options, handler) {
			super();
			if (typeof options === "function") { handler = options; options = undefined; }
			if (handler) this.on("request", handler);
			this.listening = false;
			this.timeout = 0;
			// Node lets a caller substitute its own request/response classes, and
			// several tests do exactly that to observe the objects the server
			// builds. Ignoring the option silently gave them the base classes.
			this._IncomingMessage = (options && options.IncomingMessage) || IncomingMessage;
			this._ServerResponse = (options && options.ServerResponse) || ServerResponse;
		}
		listen(...args) {
			let port = 0;
			let host = "127.0.0.1";
			let callback;
			if (typeof args[args.length - 1] === "function") callback = args.pop();
			if (typeof args[0] === "object" && args[0] !== null) {
				port = args[0].port ?? 0;
				host = args[0].host ?? host;
			} else {
				if (args[0] !== undefined) port = args[0];
				if (typeof args[1] === "string") host = args[1];
			}
			const r = ops.http_listen(host, Number(port) || 0);
			if (r && r.code) {
				const err = Object.assign(new Error(r.message), { code: r.code });
				process.nextTick(() => this.emit("error", err));
				return this;
			}
			this._id = r.id;
			this._port = r.port;
			this._host = host;
			servers.set(r.id, this);
			this.listening = true;
			if (callback) this.once("listening", callback);
			process.nextTick(() => this.emit("listening"));
			return this;
		}
		address() {
			return this.listening ? { address: this._host, family: "IPv4", port: this._port } : null;
		}
		close(callback) {
			if (this._id !== undefined) {
				// Rebalance the loop ref accounting before dropping the listen's
				// AddPending, so an unref'd-then-closed server leaves no stray
				// unref offset behind.
				if (this._unreffed) { ops.loop_ref(true); this._unreffed = false; }
				const id = this._id;
				servers.delete(this._id);
				this._id = undefined;
				this.listening = false;
				// Node's server.close() drains in-flight requests, then fires the
				// callback and emits 'close'. The host graceful shutdown invokes this
				// once every active response has finished — do not fire early.
				ops.http_close(id, () => {
					if (callback) callback(null);
					this.emit("close");
				});
			} else {
				const err = Object.assign(new Error("Server is not running."), { code: "ERR_SERVER_NOT_RUNNING" });
				if (callback) process.nextTick(() => callback(err));
			}
			return this;
		}
		setTimeout(ms, cb) {
			// Node: sets the per-connection idle timeout applied to new sockets;
			// the callback is registered as a 'timeout' listener on the server.
			// Applied per request socket in __node_http_dispatch.
			this.timeout = Number(ms) || 0;
			if (typeof cb === "function") this.on("timeout", cb);
			return this;
		}
		ref() { if (this._unreffed) { ops.loop_ref(true); this._unreffed = false; } return this; }
		unref() { if (!this._unreffed && this._id !== undefined) { ops.loop_ref(false); this._unreffed = true; } return this; }
	}

	// reqId -> IncomingMessage, for streaming the request body in.
	const openRequests = new Map();
	// reqId -> ServerResponse, kept for the whole response lifetime (unlike
	// openRequests, which clears when the request body ends) so a client
	// disconnect can still reach res/req after a bodyless GET has "ended".
	const openResponses = new Map();

	// id -> IncomingMessage for an http-client response whose body is streaming in.
	const openClientResponses = new Map();

	// The host streams client response-body chunks here: a Buffer for data, null
	// for a clean end, or false when the connection dropped before the body
	// completed (aborted/truncated).
	globalThis.__node_http_client_body = (id, chunk) => {
		const res = openClientResponses.get(id);
		if (!res) return;
		if (chunk === false) {
			openClientResponses.delete(id);
			if (res._timeoutPeer) idleTimeoutClear(res._timeoutPeer);
			res.aborted = true;
			const err = new Error("aborted");
			err.code = "ECONNRESET";
			if (res.listenerCount("error") > 0) res.destroy(err);
			else res.destroy();
		} else if (chunk === null || chunk === undefined) {
			res.complete = true;
			openClientResponses.delete(id);
			if (res._timeoutPeer) idleTimeoutClear(res._timeoutPeer); // round-trip done: stop the idle timer
			res.push(null);
		} else {
			idleTimeoutTouch(res._timeoutPeer); // body progress = activity
			res.push(Object.setPrototypeOf(chunk, Buffer.prototype));
		}
	};

	// The host calls this when the client disconnects before the guest ended the
	// response (ctx cancellation). Emit the disconnect on res ('close') and, if the
	// request body hasn't completed, on req ('aborted' + 'close') — the canonical
	// SSE / long-poll cancellation hooks that a bodyless request would otherwise
	// never receive.
	globalThis.__node_http_aborted = (reqId) => {
		const res = openResponses.get(reqId);
		if (!res) return;
		openResponses.delete(reqId);
		const req = res.req;
		if (req && !req.complete && !req.aborted) {
			req.aborted = true;
			openRequests.delete(reqId);
			req.emit("aborted");
			if (req.listenerCount("error") > 0) {
				const err = new Error("aborted");
				err.code = "ECONNRESET";
				req.destroy(err);
			} else {
				req.destroy();
			}
		}
		// res.destroy() emits 'close'; it's idempotent, so a response that raced to
		// end normally just no-ops here.
		if (!res.destroyed) res.destroy();
	};

	// The host streams request-body chunks here: a Buffer for data, null for a
	// clean end-of-body, or false when the client disconnected before the full
	// declared body arrived (a truncated/aborted request).
	globalThis.__node_http_body = (reqId, chunk) => {
		const req = openRequests.get(reqId);
		if (!req) return;
		if (chunk === false) {
			// Aborted: do NOT mark complete or emit 'end'; surface the abort like
			// Node so a handler doesn't treat a truncated body as whole. Emit the
			// error only when a listener exists — an unhandled 'error' would throw
			// out of this host callback and take down the loop for what is a normal
			// client disconnect; a handler that only cares about 'end' still sees
			// req.aborted === true and req.complete === false.
			req.aborted = true;
			openRequests.delete(reqId);
			req.emit("aborted");
			const err = new Error("aborted");
			err.code = "ECONNRESET";
			if (req.listenerCount("error") > 0) req.destroy(err);
			else req.destroy();
		} else if (chunk === null || chunk === undefined) {
			req.complete = true;
			req.push(null);
			openRequests.delete(reqId);
		} else {
			if (req.socket && req.socket._touch) req.socket._touch(); // body chunk = activity
			req.push(Object.setPrototypeOf(chunk, Buffer.prototype));
		}
	};

	globalThis.__node_http_dispatch = (serverId, reqId, method, url, rawHeaders, hasBody, remoteAddr, encrypted) => {
		const server = servers.get(serverId);
		if (!server) {
			ops.http_respond(reqId, 503, "[]");
			ops.http_end(reqId);
			return;
		}
		let remoteAddress = "127.0.0.1", remotePort = 0;
		if (typeof remoteAddr === "string" && remoteAddr.includes(":")) {
			const i = remoteAddr.lastIndexOf(":");
			remoteAddress = remoteAddr.slice(0, i).replace(/^\[|\]$/g, "");
			remotePort = Number(remoteAddr.slice(i + 1)) || 0;
		}
		const socket = makeSocket(remoteAddress, remotePort);
		socket.encrypted = !!encrypted;
		// server.setTimeout(ms) applies to each connection's socket; a socket
		// timeout also surfaces as the server's 'timeout' event (with the socket).
		if (server.timeout > 0) {
			socket.setTimeout(server.timeout);
			socket.on("timeout", () => server.emit("timeout", socket));
		}
		const ReqClass = (server && server._IncomingMessage) || IncomingMessage;
		const req = new ReqClass({ method, url, rawHeaders, socket });
		// _read is the backpressure signal: the Readable calls it when it wants
		// more, which tells the host body pump to send the next chunk.
		req._read = () => { if (hasBody) ops.http_body_resume(reqId); };
		// Body chunks arrive via __node_http_body; register for routing.
		openRequests.set(reqId, req);
		const ResClass = (server && server._ServerResponse) || ServerResponse;
		const res = new ResClass({ reqId, socket, req });
		openResponses.set(reqId, res);
		// If the handler ends the response without draining the request body, the
		// host pump stops WITHOUT sending a terminal chunk, so __node_http_body
		// never clears the map. Clean up when the response completes to avoid
		// leaking the IncomingMessage (and its buffered chunk) per undrained POST.
		res.once("finish", () => { openRequests.delete(reqId); openResponses.delete(reqId); idleTimeoutClear(socket); });
		res.once("close", () => { openRequests.delete(reqId); openResponses.delete(reqId); idleTimeoutClear(socket); });
		try {
			server.emit("request", req, res);
		} catch (e) {
			// Answer the client first — a half-written response would leave the
			// peer waiting — and then let the exception be what it is. A throw in
			// a request handler is an UNCAUGHT exception in Node: it reaches
			// 'uncaughtException' and, with no listener, ends the process.
			// Swallowing it here left the server listening and the program alive
			// forever, which is how a whole family of http tests never finished.
			try {
				if (!res.headersSent) {
					res.statusCode = 500;
					res.end("Internal Server Error");
				} else {
					res.end();
				}
			} catch { /* the socket is already gone */ }
			if (globalThis.__node_emit_uncaught) globalThis.__node_emit_uncaught(e);
			else throw e;
		}
		// The request body streams in through __node_http_body; the handler
		// attached its 'data'/'end' listeners synchronously above (the
		// Readable buffers any chunks that arrive first).
	};

	// http.Agent — real keep-alive connection pooling. Each Agent has a stable id
	// that keys a persistent host-side http.Transport (see clientForAgent in Go),
	// so sequential requests through a keepAlive agent reuse the TCP connection and
	// maxSockets throttles concurrent connections. A plain `new Agent()` defaults
	// keepAlive:false (Node), but http.globalAgent is keepAlive:true (Node v19+).
	let __nextAgentId = 0;
	class Agent {
		static _validate(options) {
			if (!options || typeof options !== "object") return;
			if (options.maxTotalSockets !== undefined && options.maxTotalSockets !== null) {
				const v = options.maxTotalSockets;
				if (typeof v !== "number") {
					throw Object.assign(new TypeError(`The "maxTotalSockets" argument must be of type number. Received type ${typeof v} (${typeof v === "string" ? `'${v}'` : String(v)})`),
						{ code: "ERR_INVALID_ARG_TYPE" });
				}
				if (Number.isNaN(v) || v <= 0) {
					throw Object.assign(new RangeError(`The value of "maxTotalSockets" is out of range. It must be > 0. Received ${v}`),
						{ code: "ERR_OUT_OF_RANGE" });
				}
			}
		}
		constructor(options = {}) {
			Agent._validate(options);
			this.options = { ...options };
			this.keepAlive = options.keepAlive ?? false;
			this.keepAliveMsecs = options.keepAliveMsecs ?? 1000;
			this.maxSockets = options.maxSockets ?? Infinity;
			this.maxFreeSockets = options.maxFreeSockets ?? 256;
			this.maxTotalSockets = options.maxTotalSockets ?? Infinity;
			this._agentId = ++__nextAgentId;
			// Node exposes these bookkeeping maps; keep empty shapes for compat.
			this.requests = {}; this.sockets = {}; this.freeSockets = {};
		}
		// The descriptor the host uses to select/build this agent's connection pool.
		_config() {
			return {
				id: this._agentId,
				keepAlive: !!this.keepAlive,
				maxSockets: Number.isFinite(this.maxSockets) ? (this.maxSockets | 0) : 0,
				maxFreeSockets: Number.isFinite(this.maxFreeSockets) ? (this.maxFreeSockets | 0) : 0,
			};
		}
		getName(options = {}) {
			// Node's pool key: host:port:localAddress plus the family, which is
			// what a caller inspects to reason about which socket it will get.
			const parts = [options.host || "localhost", options.port ?? "", options.localAddress ?? ""];
			if (options.family === 4 || options.family === 6) parts.push(options.family);
			return parts.join(":");
		}
		// createConnection is the hook a caller overrides to supply its own
		// socket; the default opens one through net, which is what an agent
		// with no override does.
		createConnection(options, callback) {
			const socket = core.net.createConnection(options);
			if (typeof callback === "function") {
				socket.once("connect", () => callback(null, socket));
				socket.once("error", (e) => callback(e));
			}
			return socket;
		}
		reuseSocket() {}
		keepSocketAlive() { return true; }
		destroy() { if (ops.http_agent_close) ops.http_agent_close(this._agentId); }
	}
	const isErr = (r) => r !== null && typeof r === "object" && typeof r.code === "string" && !(r instanceof Uint8Array);

	// ClientRequest: a Writable delivering an IncomingMessage-shaped response
	// through the 'response' event. Two body paths: the buffered fast path (the
	// whole body is known at req.end(body) — Content-Length) and the streaming
	// path (req.write() before req.end() — chunked Transfer-Encoding, so headers
	// and the first chunk reach the server without waiting for the whole body).
	class ClientRequest extends Writable {
		constructor(options, cb) {
			super();
			// Node accepts (url), (url, options), (options); url may be a string
			// or URL, and options may add/override method/headers/path.
			let o;
			if (typeof options === "string" || options instanceof URL) {
				o = parseRequestURL(String(options));
			} else {
				o = { ...options };
				if (o.headers) o.headers = { ...o.headers };
			}
			const received = (v) => {
				if (v === null || v === undefined) return ` Received ${v}`;
				if (typeof v === "function") return ` Received function ${v.name}`;
				if (typeof v === "object") {
					const n = v.constructor && v.constructor.name;
					return n ? ` Received an instance of ${n}` : ` Received ${String(v)}`;
				}
				let sv = typeof v === "string" ? `'${v}'` : String(v);
				if (sv.length > 28) sv = sv.slice(0, 25) + "...";
				return ` Received type ${typeof v} (${sv})`;
			};
			// Node's request-option validation, in its shapes and its order:
			// the protocol must be the module's own, the hostname a string (or
			// absent), the method a valid HTTP token, and the path free of
			// unescaped characters — control bytes, spaces, and non-ASCII.
			if (o.protocol && o.protocol !== "http:" && o.protocol !== "https:") {
				throw Object.assign(
					new TypeError(`Protocol "${o.protocol}" not supported. Expected "http:"`),
					{ code: "ERR_INVALID_PROTOCOL" });
			}
			if (o.hostname !== undefined && o.hostname !== null && typeof o.hostname !== "string") {
				throw Object.assign(
					new TypeError(`The "options.hostname" property must be of type string or one of undefined or null.${received(o.hostname)}`),
					{ code: "ERR_INVALID_ARG_TYPE" });
			}
			if (o.host !== undefined && o.host !== null && typeof o.host !== "string") {
				throw Object.assign(
					new TypeError(`The "options.host" property must be of type string or one of undefined or null.${received(o.host)}`),
					{ code: "ERR_INVALID_ARG_TYPE" });
			}
			if (o.method !== undefined) {
				if (typeof o.method !== "string") {
					throw Object.assign(new TypeError('The "method" argument must be of type string.'),
						{ code: "ERR_INVALID_ARG_TYPE" });
				}
				if (!/^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/.test(o.method)) {
					throw Object.assign(new TypeError(`Method must be a valid HTTP token ["${o.method}"]`),
						{ code: "ERR_INVALID_HTTP_TOKEN" });
				}
			}
			if (o.timeout !== undefined && typeof o.timeout !== "number") {
				const recv = o.timeout === null ? " Received null"
					: ` Received type ${typeof o.timeout} (${String(o.timeout)})`;
				throw Object.assign(new TypeError(`The "timeout" argument must be of type number.${recv}`),
					{ code: "ERR_INVALID_ARG_TYPE" });
			}
			if (o.path !== undefined && o.path !== null) {
				const pathStr = String(o.path);
				// The characters a request line cannot carry raw (RFC 9110's
				// grammar): controls, space, and anything past ASCII.
				if (/[\u0000-\u0020\u007f-\uffff]/.test(pathStr)) {
					throw Object.assign(new TypeError("Request path contains unescaped characters"),
						{ code: "ERR_UNESCAPED_CHARACTERS" });
				}
			}
			this.method = (o.method || "GET").toUpperCase();
			this._headers = {};
			for (const [k, v] of Object.entries(o.headers || {})) this._headers[k] = v;
			const scheme = o.protocol ? o.protocol.replace(":", "") : "http";
			const host = o.hostname || o.host || "127.0.0.1";
			const port = o.port ? ":" + o.port : "";
			this._url = o.href || `${scheme}://${host}${port}${o.path || "/"}`;
			this._agentOpt = o.agent; // false = no pooling; undefined = globalAgent
			// https options travel with the request, not the agent: an https
			// request against a self-signed server passes rejectUnauthorized /
			// ca / servername here, and without forwarding them every such
			// request failed the handshake against the system roots.
			this._tls = tlsOptionsOf(o);
			this._chunks = [];
			this._streaming = false; // switched on by an explicit write() before end()
			this._started = false;   // the streaming request op has been dispatched
			this._ended = false;
			this._timeoutMs = 0;
			this._timeoutTimer = null;
			// Node: options.timeout arms the idle timeout at request creation.
			if (cb) this.once("response", cb);
			// The client socket exists once the request is actually SENT: a
			// request that is destroyed (or never ends) must not leave a
			// socket and its idle timer behind — two hundred abandoned
			// requests did exactly that and exhausted the heap.
			this._timeoutOption = o.timeout;
		}
		setHeader(name, value) { this._headers[name] = value; return this; }
		getHeader(name) { return this._headers[name]; }
		removeHeader(name) { delete this._headers[name]; }
		// The keep-alive pool descriptor for this request: an explicit `agent:false`
		// disables pooling; otherwise the request's agent (or the globalAgent).
		_agentConfig() {
			// A keep-alive transport is cached per agent id, so a request with
			// its own TLS options must not reuse (or poison) the shared one.
			if (this._agentOpt === false || this._tls) {
				return JSON.stringify(this._tls ? { tls: this._tls } : {});
			}
			const agent = this._agentOpt || core.http.globalAgent;
			return agent && typeof agent._config === "function" ? JSON.stringify(agent._config()) : "{}";
		}
		// The response/error callbacks the host op drives from the loop once the
		// round-trip makes progress. Shared by both the buffered and streaming paths.
		_sendHandlers() {
			const onResponse = (r) => {
				const res = new IncomingMessage({ method: this.method, url: this._url, rawHeaders: r.headers });
				res.statusCode = r.status;
				res.statusMessage = r.statusText;
				idleTimeoutTouch(this); // response headers arrived = activity
				res._timeoutPeer = this; // body chunks keep resetting the request's idle timer
				// The body streams in via __node_http_client_body chunks (routed by
				// id), so 'response' fires on the headers — a streaming/SSE endpoint
				// works and the whole body is never buffered in host memory.
				const id = r.id;
				res._read = () => ops.http_client_body_resume(id);
				res.once("close", () => {
					if (openClientResponses.get(id) === res) {
						openClientResponses.delete(id);
						// If the consumer abandoned the body early, stop the host pump.
						if (!res.complete) ops.http_client_body_cancel(id);
					}
				});
				openClientResponses.set(id, res);
				// A response NOBODY is listening for still has to be drained. The
				// host pump waits for the guest to ask for more, so an unread body
				// parks it forever and the request's handle keeps the event loop
				// alive — `http.request(url).end()` with no 'response' handler
				// would never let the program exit. Node does exactly this: if the
				// emit finds no listener, it dumps the body.
				if (!this.emit("response", res)) res.resume();
			};
			// A user abort()/destroy() cancels the round-trip, which surfaces as a
			// context-canceled onError; don't re-emit 'error' for the request the
			// caller just destroyed (Node's bare destroy() emits no 'error').
			const onError = (e) => { if (this.destroyed) return; const err = new Error(e.message); err.code = e.code; this.emit("error", err); };
			return { onResponse, onError };
		}
		// Start the streaming (chunked) request: sends the headers now so the first
		// body chunk reaches the server promptly. Idempotent.
		_ensureStarted() {
			if (this._started) return;
			this._started = true;
			assignClientSocket(this);
			const { onResponse, onError } = this._sendHandlers();
			this._reqId = ops.http_client_req_stream(this.method, this._url, JSON.stringify(this._headers), this._agentConfig(), onResponse, onError);
		}
		write(chunk, encoding, cb) {
			// An explicit write() before end() means the body isn't known up front:
			// stream it chunked. (end(body) writes internally too, but only after
			// _ended is set, so a one-shot end(body) stays on the buffered path.)
			if (!this._ended) this._streaming = true;
			return Writable.prototype.write.call(this, chunk, encoding, cb);
		}
		end(chunk, encoding, cb) {
			if (typeof chunk === "function") { cb = chunk; chunk = undefined; encoding = undefined; }
			else if (typeof encoding === "function") { cb = encoding; encoding = undefined; }
			this._ended = true;
			return Writable.prototype.end.call(this, chunk, encoding, cb);
		}
		_write(chunk, encoding, callback) {
			if (this._streaming) {
				this._ensureStarted();
				const buf = typeof chunk === "string" ? Buffer.from(chunk, typeof encoding === "string" ? encoding : "utf8") : chunk;
				if (buf && buf.length) ops.http_client_write(this._reqId, buf);
				idleTimeoutTouch(this); // request body write = activity
				callback();
			} else {
				this._chunks.push(chunk);
				callback();
			}
		}
		_final(callback) {
			if (this._streaming) {
				this._ensureStarted();
				ops.http_client_end(this._reqId);
				callback();
				return;
			}
			// Buffered fast path: the whole body is known, so Node sets Content-Length.
			const body = this._chunks.length ? Buffer.concat(this._chunks.map((c) => (typeof c === "string" ? Buffer.from(c) : c))) : Buffer.alloc(0);
			const { onResponse, onError } = this._sendHandlers();
			// The op returns the request/body id so abort()/destroy() can cancel the
			// round-trip (otherwise an aborted or unconsumed response leaves the host
			// body pump parked and the event loop never idles).
			assignClientSocket(this);
			this._reqId = ops.http_client_req(this.method, this._url, JSON.stringify(this._headers), body, this._agentConfig(), onResponse, onError);
			callback();
		}
		// Node's abort(): ONE 'abort' event however many times it is called, on the
		// next tick, plus the deprecated `aborted` flag — then the destroy. It used
		// to only destroy, so `req.on('abort', …)` never fired and any test (or
		// application) waiting for it waited forever, which is what left ~195
		// quarantined http tests hanging with their servers still open.
		abort() {
			if (this.aborted) return;
			this.aborted = true;
			process.nextTick(() => this.emit("abort"));
			this.destroy();
		}
		_destroy(err, cb) {
			idleTimeoutClear(this);
			// Cancel the round-trip so an aborted/unconsumed response releases the
			// host body pump (which the base Writable.destroy invokes exactly once).
			if (this._reqId !== undefined && this._reqId !== null) {
				ops.http_client_body_cancel(this._reqId);
				this._reqId = undefined;
			}
			cb(err);
		}
		setTimeout(ms, cb) {
			// The timeout lives on the SOCKET (socket.timeout, its 'timeout'
			// listener count), and the request re-emits through the one
			// persistent forwarder installed at socket creation. An explicit
			// setTimeout on a still-connecting socket waits for 'connect' —
			// which is why the option timeout is visible at 'socket' and this
			// one only afterwards.
			if (cb) this.once("timeout", cb);
			const arm = (socket) => {
				if (!socket || typeof socket.setTimeout !== "function") return;
				if (socket.connecting) socket.once("connect", () => socket.setTimeout(ms));
				else socket.setTimeout(ms);
			};
			if (this.socket) arm(this.socket);
			else this.once("socket", arm);
			return this;
		}
	}

	// assignSocket gives a client request its socket and walks the two states
	// Node makes observable: 'socket' on the tick the socket is assigned,
	// 'connect' the tick after. The timeout OPTION arms at assignment; an
	// explicit setTimeout() during connect waits for it (see setTimeout).
	function assignClientSocket(req) {
		if (req.socket || req.destroyed) return;
		const sock = makeSocket();
		sock.connecting = true;
		sock.on("timeout", () => req.emit("timeout"));
		req.socket = req.connection = sock;
		if (req._timeoutOption) sock.setTimeout(req._timeoutOption);
		for (const pending of req._pendingSocketTimeouts || []) pending(sock);
		req._pendingSocketTimeouts = null;
		req.emit("socket", sock);
		process.nextTick(() => {
			if (req.destroyed) return;
			sock.connecting = false;
			sock.emit("connect");
		});
	}

	function parseRequestURL(url) {
		const u = new URL(url);
		return { protocol: u.protocol, hostname: u.hostname, port: u.port, path: u.pathname + u.search, href: u.href };
	}
	// Normalize Node's (url), (url, cb), (url, options, cb), (options, cb)
	// call shapes into a single {options, cb}.
	function normalizeRequestArgs(args) {
		let url, options, cb;
		if (typeof args[0] === "string" || args[0] instanceof URL) {
			url = args[0];
			if (typeof args[1] === "function") { cb = args[1]; }
			else { options = args[1]; cb = args[2]; }
		} else {
			options = args[0]; cb = args[1];
		}
		if (url !== undefined) {
			const base = parseRequestURL(String(url));
			const override = options || {};
			options = { ...base, ...override };
			// A URL-component override (path/hostname/host/port/protocol) makes the
			// base href stale; drop it so ClientRequest rebuilds from components.
			if (["path", "hostname", "host", "port", "protocol"].some((k) => k in override)) {
				delete options.href;
			}
		}
		return { options: options || {}, cb };
	}
	function httpRequest(...args) {
		const { options, cb } = normalizeRequestArgs(args);
		return new ClientRequest(options, cb);
	}
	function httpGet(...args) {
		const req = httpRequest(...args);
		req.method = "GET";
		req.end();
		return req;
	}

	// Node's constructors are ES5 functions, so `http.Server(opts)` works as
	// well as `new http.Server(opts)` — and its own suite uses the bare form.
	// A class throws "class constructors must be invoked with 'new'", which was
	// the whole of that failure group. callableClass keeps the class (so
	// `extends` and instanceof still work) behind a callable façade.
	function callableClass(Cls) {
		// Reflect.construct with new.target, not `new Cls(...)`: the façade is
		// also used as a BASE CLASS, and a constructor that returns a fresh
		// object discards the subclass instance super() was initialising — the
		// subclass's own methods then vanish.
		const f = function (...args) {
			return Reflect.construct(Cls, args, new.target || Cls);
		};
		f.prototype = Cls.prototype;
		Object.setPrototypeOf(f, Cls);
		Object.defineProperty(f, "name", { value: Cls.name, configurable: true });
		return f;
	}

	core.http = {
		METHODS,
		STATUS_CODES,
		Server: callableClass(Server),
		IncomingMessage: callableClass(IncomingMessage),
		ServerResponse: callableClass(ServerResponse),
		OutgoingMessage: callableClass(ServerResponse),
		ClientRequest: callableClass(ClientRequest),
		createServer: (options, handler) => new Server(options, handler),
		request: httpRequest,
		get: httpGet,
		Agent: callableClass(Agent),
		// Node v19+: the default global agent is keep-alive.
		globalAgent: new Agent({ keepAlive: true }),
		maxHeaderSize: 16384,
		validateHeaderName: (name) => { if (!/^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/.test(name)) throw new TypeError(`Invalid header name: ${name}`); },
		validateHeaderValue: (name, value) => { if (value === undefined) throw new TypeError(`Invalid value for header ${name}`); },
	};

	// Node's underscored http modules. They are deprecated aliases for pieces of
	// node:http, but a great deal of published code still requires them by name,
	// so each maps onto the thing it names here.
	//
	// _http_common's HTTPParser is deliberately absent: parsing belongs to Go's
	// net/http in this runtime, so there is no parser object to hand back, and
	// inventing one that answers to the shape but not the behaviour would be
	// worse than saying so.
	core._http_server = {
		ServerResponse: core.http.ServerResponse,
		STATUS_CODES,
		kConnectionsCheckingInterval: Symbol("http.connectionsCheckingInterval"),
	};
	core._http_client = { ClientRequest: core.http.ClientRequest };
	core._http_incoming = {
		IncomingMessage: core.http.IncomingMessage,
		readStart: (socket) => { if (socket && socket.resume) socket.resume(); },
		readStop: (socket) => { if (socket && socket.pause) socket.pause(); },
	};
	core._http_outgoing = {
		OutgoingMessage: core.http.OutgoingMessage,
		kHighWaterMark: Symbol("http.highWaterMark"),
	};
	core._http_agent = { Agent: core.http.Agent, globalAgent: core.http.globalAgent };
	core._http_common = {
		METHODS,
		methods: METHODS,
		CRLF: "\r\n",
		chunkExpression: /(?:^|\W)chunked(?:$|\W)/i,
		continueExpression: /(?:^|\W)100-continue(?:$|\W)/i,
		_checkIsHttpToken: (val) => typeof val === "string" && /^[\^`\-\w!#$%&'*+.|~]+$/.test(val),
		_checkInvalidHeaderChar: (val) => /[^\t\x20-\x7e\x80-\xff]/.test(String(val)),
	};

	// HTTPS server: an http Server that binds a TLS listener (https_listen)
	// instead of the plaintext http_listen. The dispatch machinery is shared.
	class HttpsServer extends Server {
		constructor(options, handler) {
			super(typeof options === "function" ? options : handler);
			this._tls = typeof options === "object" ? options : {};
		}
		listen(...args) {
			let port = 0, host = "127.0.0.1", callback;
			if (typeof args[args.length - 1] === "function") callback = args.pop();
			if (typeof args[0] === "object" && args[0] !== null) { port = args[0].port ?? 0; host = args[0].host ?? host; }
			else { if (args[0] !== undefined) port = args[0]; if (typeof args[1] === "string") host = args[1]; }
			if (!this._tls.cert || !this._tls.key) {
				const err = new Error("https.createServer requires { cert, key }");
				process.nextTick(() => this.emit("error", err));
				return this;
			}
			const r = ops.https_listen(this._httpId(), String(host), Number(port) || 0, this._tls.cert, this._tls.key);
			if (r && r.code) { const e = Object.assign(new Error(r.message), { code: r.code }); process.nextTick(() => this.emit("error", e)); return this; }
			this._id = r.id; this._port = r.port; this._host = host;
			servers.set(r.id, this);
			this.listening = true;
			if (callback) this.once("listening", callback);
			process.nextTick(() => this.emit("listening"));
			return this;
		}
		_httpId() {
			// Reuse the same server-id space the http dispatch keys on. The
			// counter lives in this module, NOT on globalThis: Node's suite checks
			// for globals a test leaked, and an implementation detail of ours
			// showing up there failed 24 tests that had nothing to do with https.
			if (this.__id === undefined) this.__id = ++nextHttpsID;
			return this.__id;
		}
	}

	// nextHttpsID is module state; __node_set_next_https lets a test drive it to
	// a chosen value without putting the counter itself on globalThis.
	let nextHttpsID = 900000;
	// Non-enumerable, because Node's suite lists the enumerable globals a test
	// leaked and compares them against a fixed set — which is exactly the check
	// the old counter tripped.
	Object.defineProperty(globalThis, "__node_set_next_https", {
		value: (n) => { nextHttpsID = Number(n); }, configurable: true, enumerable: false,
	});

	// tlsOptionsOf picks the https settings out of a request's options. It
	// returns undefined when none were given, so an ordinary request keeps
	// using the shared keep-alive transport.
	function tlsOptionsOf(o) {
		if (!o || typeof o !== "object") return undefined;
		const out = {};
		if (o.rejectUnauthorized !== undefined) out.rejectUnauthorized = !!o.rejectUnauthorized;
		if (o.ca !== undefined && o.ca !== null) {
			const list = Array.isArray(o.ca) ? o.ca : [o.ca];
			out.ca = list.map((c) => (typeof c === "string" ? c : Buffer.from(c).toString("utf8"))).join("\n");
		}
		if (typeof o.servername === "string") out.servername = o.servername;
		return Object.keys(out).length ? out : undefined;
	}

	// https defaults an options object with no explicit protocol to "https:" —
	// otherwise ClientRequest defaults to "http" and the request goes out in
	// PLAINTEXT (a silent security downgrade). A string URL carries its own
	// scheme and is left untouched; callbacks/other args pass through.
	const httpsify = (a) =>
		a && typeof a === "object" && a.href === undefined && typeof a.protocol !== "string"
			? { ...a, protocol: "https:" }
			: a;
	core.https = {
		Server: callableClass(HttpsServer),
		Agent: callableClass(Agent),
		globalAgent: new Agent({ keepAlive: true }),
		createServer: (options, handler) => new HttpsServer(options, handler),
		request: (...args) => httpRequest(...args.map(httpsify)),
		get: (...args) => httpGet(...args.map(httpsify)),
	};
})();
