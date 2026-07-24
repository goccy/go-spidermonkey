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
		socket.destroy = () => { socket.destroyed = true; };
		socket.end = () => {};
		socket.setTimeout = () => socket;
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
				if (this.headers[key] === undefined) this.headers[key] = value;
				else if (key === "set-cookie") this.headers[key] = [].concat(this.headers[key], value);
				else this.headers[key] += ", " + value;
			}
			this.complete = false;
			this.aborted = false;
		}
		setTimeout() { return this; }
	}

	class ServerResponse extends Writable {
		constructor(init = {}) {
			super();
			this._reqId = init.reqId;
			this.socket = this.connection = init.socket || makeSocket();
			this.req = init.req;
			this.statusCode = 200;
			this.statusMessage = undefined;
			this.headersSent = false;
			this.finished = false;
			this.sendDate = true;
			this._headers = new Map(); // lowercased -> { name, value }
		}
		setHeader(name, value) {
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
		writeHead(statusCode, reasonOrHeaders, headers) {
			this.statusCode = statusCode;
			if (typeof reasonOrHeaders === "string") this.statusMessage = reasonOrHeaders;
			else if (reasonOrHeaders !== undefined) headers = reasonOrHeaders;
			if (headers) {
				if (Array.isArray(headers)) {
					for (let i = 0; i + 1 < headers.length; i += 2) this.setHeader(headers[i], headers[i + 1]);
				} else {
					for (const k of Object.keys(headers)) this.setHeader(k, headers[k]);
				}
			}
			return this;
		}
		flushHeaders() { this._ensureHead(); }
		writeContinue() {}
		setTimeout() { return this; }
		_ensureHead() {
			if (this.headersSent || this._reqId === undefined) return;
			this.headersSent = true;
			const pairs = [];
			for (const { name, value } of this._headers.values()) {
				if (Array.isArray(value)) for (const v of value) pairs.push([name, String(v)]);
				else pairs.push([name, String(value)]);
			}
			ops.http_respond(this._reqId, this.statusCode | 0, JSON.stringify(pairs));
		}
		_write(chunk, encoding, callback) {
			this._ensureHead();
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
	}

	const servers = new Map(); // server id -> Server

	class Server extends EventEmitter {
		constructor(handler) {
			super();
			if (handler) this.on("request", handler);
			this.listening = false;
			this.timeout = 0;
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
				ops.http_close(this._id);
				servers.delete(this._id);
				this._id = undefined;
				this.listening = false;
			}
			if (callback) process.nextTick(() => callback(null));
			process.nextTick(() => this.emit("close"));
			return this;
		}
		setTimeout() { return this; }
		ref() { if (this._unreffed) { ops.loop_ref(true); this._unreffed = false; } return this; }
		unref() { if (!this._unreffed && this._id !== undefined) { ops.loop_ref(false); this._unreffed = true; } return this; }
	}

	// reqId -> IncomingMessage, for streaming the request body in.
	const openRequests = new Map();

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
		const req = new IncomingMessage({ method, url, rawHeaders, socket });
		// _read is the backpressure signal: the Readable calls it when it wants
		// more, which tells the host body pump to send the next chunk.
		req._read = () => { if (hasBody) ops.http_body_resume(reqId); };
		// Body chunks arrive via __node_http_body; register for routing.
		openRequests.set(reqId, req);
		const res = new ServerResponse({ reqId, socket, req });
		try {
			server.emit("request", req, res);
		} catch (e) {
			console.error("Unhandled request handler error:", e instanceof Error ? `${e.name}: ${e.message}\n${e.stack || ""}` : String(e));
			try {
				if (!res.headersSent) {
					res.statusCode = 500;
					res.end("Internal Server Error");
				} else {
					res.end();
				}
			} catch {}
		}
		// The request body streams in through __node_http_body; the handler
		// attached its 'data'/'end' listeners synchronously above (the
		// Readable buffers any chunks that arrive first).
	};

	class Agent { constructor() { this.options = {}; } destroy() {} }
	const isErr = (r) => r !== null && typeof r === "object" && typeof r.code === "string" && !(r instanceof Uint8Array);

	// ClientRequest: a Writable that buffers the body, then runs the
	// synchronous http_client_req op and delivers an IncomingMessage-shaped
	// response through the 'response' event.
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
			this.method = (o.method || "GET").toUpperCase();
			this._headers = {};
			for (const [k, v] of Object.entries(o.headers || {})) this._headers[k] = v;
			const scheme = o.protocol ? o.protocol.replace(":", "") : "http";
			const host = o.hostname || o.host || "127.0.0.1";
			const port = o.port ? ":" + o.port : "";
			this._url = o.href || `${scheme}://${host}${port}${o.path || "/"}`;
			this._chunks = [];
			if (cb) this.once("response", cb);
		}
		setHeader(name, value) { this._headers[name] = value; return this; }
		getHeader(name) { return this._headers[name]; }
		removeHeader(name) { delete this._headers[name]; }
		_write(chunk, encoding, callback) { this._chunks.push(chunk); callback(); }
		_final(callback) {
			const body = this._chunks.length ? Buffer.concat(this._chunks.map((c) => (typeof c === "string" ? Buffer.from(c) : c))) : Buffer.alloc(0);
			// Async: the op returns immediately and drives one of these callbacks
			// from the loop once the round-trip finishes off the loop goroutine.
			const onResponse = (r) => {
				const res = new IncomingMessage({ method: this.method, url: this._url, rawHeaders: r.headers });
				res.statusCode = r.status;
				res.statusMessage = r.statusText;
				const bodyBuf = Object.setPrototypeOf(r.body, Buffer.prototype);
				this.emit("response", res);
				process.nextTick(() => { if (bodyBuf.length) res.push(bodyBuf); res.push(null); });
			};
			const onError = (e) => { const err = new Error(e.message); err.code = e.code; this.emit("error", err); };
			ops.http_client_req(this.method, this._url, JSON.stringify(this._headers), body, onResponse, onError);
			callback();
		}
		abort() { this.destroy(); }
		setTimeout() { return this; }
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
			options = { ...base, ...(options || {}) };
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

	core.http = {
		METHODS,
		STATUS_CODES,
		Server,
		IncomingMessage,
		ServerResponse,
		OutgoingMessage: ServerResponse,
		ClientRequest,
		createServer: (options, handler) => new Server(typeof options === "function" ? options : handler),
		request: httpRequest,
		get: httpGet,
		Agent,
		globalAgent: new Agent(),
		maxHeaderSize: 16384,
		validateHeaderName: (name) => { if (!/^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/.test(name)) throw new TypeError(`Invalid header name: ${name}`); },
		validateHeaderValue: (name, value) => { if (value === undefined) throw new TypeError(`Invalid value for header ${name}`); },
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
			// Reuse the same server-id space the http dispatch keys on.
			if (this.__id === undefined) this.__id = (globalThis.__node_next_https = (globalThis.__node_next_https || 900000) + 1);
			return this.__id;
		}
	}

	core.https = {
		Server: HttpsServer,
		Agent,
		globalAgent: new Agent(),
		createServer: (options, handler) => new HttpsServer(options, handler),
		request: httpRequest,
		get: httpGet,
	};
})();
