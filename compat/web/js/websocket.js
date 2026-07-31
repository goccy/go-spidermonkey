// compat/web: WebSocket (https://websockets.spec.whatwg.org/).
//
// The protocol is in Go (websocket.go). What is here is the part of WebSocket
// that is not the protocol: the URL and subprotocol validation the constructor
// performs before anything is dialled, readyState and its transitions, the
// event objects, binaryType, and bufferedAmount.
//
// bufferedAmount is tracked here rather than queried from the host on purpose.
// The attribute may only change at a task boundary — send() must be able to
// read back exactly what it just queued, and a test does — so it is a number
// this side owns: send() adds, and a "drain" report from the host (a task)
// subtracts what actually went out.
(() => {
	"use strict";

	const CONNECTING = 0, OPEN = 1, CLOSING = 2, CLOSED = 3;
	const TOKEN = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;

	// Ports fetch refuses to connect to (https://fetch.spec.whatwg.org/#port-blocking).
	// A WebSocket to one of them fails the connection rather than throwing, so the
	// list belongs to the connect step and not to the constructor's validation.
	const BAD_PORTS = new Set([
		0, 1, 7, 9, 11, 13, 15, 17, 19, 20, 21, 22, 23, 25, 37, 42, 43, 53, 69, 77,
		79, 87, 95, 101, 102, 103, 104, 109, 110, 111, 113, 115, 117, 119, 123, 135,
		137, 139, 143, 161, 179, 389, 427, 465, 512, 513, 514, 515, 526, 530, 531,
		532, 540, 548, 554, 556, 563, 587, 601, 636, 989, 990, 993, 995, 1719, 1720,
		1723, 2049, 3659, 4045, 4190, 5060, 5061, 6000, 6566, 6665, 6666, 6667,
		6668, 6669, 6679, 6697, 10080,
	]);

	// CloseEvent's three members are IDL attributes: prototype accessors over
	// slots, not own data properties, so the prototype has them and nothing can
	// rewrite one on an event it received.
	class CloseEvent extends Event {
		constructor(type, init = {}) {
			super(type, init);
			const opts = init === null || init === undefined ? {} : init;
			Object.defineProperties(this, {
				_wasClean: { value: Boolean(opts.wasClean) },
				_code: { value: opts.code === undefined ? 0 : Number(opts.code) },
				_reason: { value: opts.reason === undefined ? "" : String(opts.reason) },
			});
		}
		get wasClean() { return this._wasClean; }
		get code() { return this._code; }
		get reason() { return this._reason; }
	}
	Object.defineProperty(CloseEvent.prototype, Symbol.toStringTag, { value: "CloseEvent", configurable: true });
	globalThis.CloseEvent = CloseEvent;

	// Every live socket, by the handle the host addresses it with. The host calls
	// __ws_dispatch with that handle; the socket itself is never passed across.
	const sockets = new Map();

	// The constructor's argument checks, separated from the connecting, because
	// WebSocketStream has to make exactly the same ones and then NOT connect when
	// its signal is already aborted. Registered under a symbol so the sibling
	// file can reach it without adding a global.
	function checkWebSocketArguments(url, protocols) {
		let parsed;
		try {
			parsed = new URL(String(url), globalThis.location ? globalThis.location.href : undefined);
		} catch {
			throw new DOMException(`WebSocket: ${String(url)} is not a URL`, "SyntaxError");
		}
		// http/https are accepted and normalized: the two schemes address the same
		// endpoints, and the url attribute reports the ws form.
		if (parsed.protocol === "http:") parsed.protocol = "ws:";
		else if (parsed.protocol === "https:") parsed.protocol = "wss:";
		if (parsed.protocol !== "ws:" && parsed.protocol !== "wss:") {
			throw new DOMException(`WebSocket: ${parsed.protocol} is not a WebSocket scheme`, "SyntaxError");
		}
		if (parsed.hash !== "" || String(url).endsWith("#")) {
			throw new DOMException("WebSocket: the URL must not have a fragment", "SyntaxError");
		}
		const list = typeof protocols === "string" ? [protocols]
			: (protocols === undefined ? [] : Array.from(protocols, String));
		const seen = new Set();
		for (const p of list) {
			if (!TOKEN.test(p)) {
				throw new DOMException(`WebSocket: ${p} is not a valid subprotocol`, "SyntaxError");
			}
			const key = p.toLowerCase();
			if (seen.has(key)) {
				throw new DOMException(`WebSocket: subprotocol ${p} is repeated`, "SyntaxError");
			}
			seen.add(key);
		}
		return { parsed, list };
	}
	Object.defineProperty(globalThis, Symbol.for("go-spidermonkey.checkWebSocketArguments"), {
		value: checkWebSocketArguments, configurable: true,
	});

	class WebSocket extends EventTarget {
		constructor(url, protocols = []) {
			super();
			if (arguments.length < 1) {
				throw new TypeError("WebSocket: a URL is required");
			}
			const { parsed, list } = checkWebSocketArguments(url, protocols);

			this._url = parsed.href;
			this._state = CONNECTING;
			this._protocol = "";
			this._extensions = "";
			this._buffered = 0;
			this._binaryType = "blob";
			this._on = {};
			// The close the guest asked for, remembered until the socket is CLOSED so
			// a close during CONNECTING can report itself rather than an error.
			this._closeRequested = false;

			const port = parsed.port === "" ? (parsed.protocol === "wss:" ? 443 : 80) : Number(parsed.port);
			if (BAD_PORTS.has(port)) {
				// Failing the connection, not throwing: the constructor has succeeded,
				// and the failure is reported as events on the next turn.
				this._id = 0;
				queueMicrotask(() => this._failed());
				return;
			}
			this._id = __ws_connect(parsed.href, list.join(","));
			sockets.set(this._id, this);
		}

		get url() { return this._url; }
		get readyState() { return this._state; }
		get protocol() { return this._protocol; }
		get extensions() { return this._extensions; }
		get bufferedAmount() { return this._buffered; }

		get binaryType() { return this._binaryType; }
		set binaryType(value) {
			// A value that is neither "blob" nor "arraybuffer" is IGNORED. It used to
			// be a SyntaxError, and the test names still say so, but the attribute is
			// an enumeration now and an unknown enumeration value is dropped.
			if (value === "blob" || value === "arraybuffer") this._binaryType = value;
		}

		send(data) {
			if (this._state === CONNECTING) {
				throw new DOMException("send: the socket is still connecting", "InvalidStateError");
			}
			if (this._state !== OPEN) {
				// Once closing or closed the data is discarded, silently: the socket
				// reported its state already and send() is not where that is raised.
				return;
			}
			let binary = true, payload = data, length;
			if (typeof data === "string") {
				binary = false;
				payload = data;
				// bufferedAmount counts the bytes on the wire, which for a text frame
				// is the UTF-8 length and not the number of UTF-16 code units.
				length = utf8Length(data);
			} else if (data instanceof Blob) {
				// A Blob's bytes are not available synchronously, so the queue is fed
				// once they are. bufferedAmount is charged now, as the size is known.
				length = data.size;
				this._buffered += length;
				data.arrayBuffer().then((buf) => {
					if (this._state === OPEN) __ws_send(this._id, true, new Uint8Array(buf));
				});
				return;
			} else if (data instanceof ArrayBuffer) {
				payload = new Uint8Array(data);
				length = payload.length;
			} else if (ArrayBuffer.isView(data)) {
				// The view's own window into its buffer, not the whole buffer.
				payload = new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
				length = payload.length;
			} else {
				binary = false;
				payload = String(data);
				length = utf8Length(payload);
			}
			this._buffered += length;
			__ws_send(this._id, binary, payload);
		}

		close(code, reason) {
			// close(reason) with no code is an error, not a shorthand: an absent code
			// is distinct from a bad one, and only UNDEFINED is absent — null is a
			// value, and it converts to 0, which is not a permitted close code.
			if (code !== undefined) {
				const n = Number(code);
				if (!(n === 1000 || (n >= 3000 && n <= 4999))) {
					throw new DOMException(`close: ${code} is not a permitted close code`, "InvalidAccessError");
				}
				code = n;
			} else {
				code = 0;
			}
			if (reason !== undefined && reason !== null) {
				reason = String(reason);
				if (utf8Length(reason) > 123) {
					throw new DOMException("close: the reason is longer than 123 bytes", "SyntaxError");
				}
			} else {
				reason = "";
			}
			if (this._state === CLOSING || this._state === CLOSED) return;
			this._closeRequested = true;
			// CLOSING is observable immediately, including from the open handler that
			// called close() — the handshake has been started, not completed.
			this._state = CLOSING;
			if (this._id !== 0) __ws_close(this._id, code, reason);
		}

		// _failed reports a connection that never opened.
		_failed() {
			if (this._state === CLOSED) return;
			this._state = CLOSED;
			__dispatch_trusted(this, new Event("error"));
			__dispatch_trusted(this, new globalThis.CloseEvent("close", { code: 1006, reason: "", wasClean: false }));
		}

		_dispatch(kind, a, b, c) {
			switch (kind) {
				case "open":
					// A close() during CONNECTING already moved to CLOSING; the socket
					// did open, but it must not be reported as open.
					if (this._state !== CONNECTING) return;
					this._state = OPEN;
					this._protocol = a;
					this._extensions = b;
					__dispatch_trusted(this, new Event("open"));
					return;
				case "drain":
					this._buffered = Math.max(0, this._buffered - a);
					return;
				case "message": {
					if (this._state !== OPEN) return;
					let data = b;
					if (a) {
						data = this._binaryType === "blob" ? new Blob([b]) : b.slice().buffer;
					}
					__dispatch_trusted(this, new MessageEvent("message", { data, origin: originOf(this._url) }));
					return;
				}
				case "close": {
					sockets.delete(this._id);
					if (this._state === CLOSED) return;
					const opened = this._state !== CONNECTING;
					this._state = CLOSED;
					// An error event precedes the close only when the connection FAILED —
					// it never opened, or it ended without a close handshake. A close the
					// guest asked for is not a failure however it ended.
					if (!c && !opened) __dispatch_trusted(this, new Event("error"));
					__dispatch_trusted(this, new globalThis.CloseEvent("close", { code: a, reason: b, wasClean: !!c }));
					return;
				}
			}
		}
	}

	for (const type of ["open", "message", "error", "close"]) {
		Object.defineProperty(WebSocket.prototype, "on" + type, {
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
	for (const [name, value] of [["CONNECTING", CONNECTING], ["OPEN", OPEN],
		["CLOSING", CLOSING], ["CLOSED", CLOSED]]) {
		for (const target of [WebSocket, WebSocket.prototype]) {
			Object.defineProperty(target, name, { value, enumerable: true });
		}
	}

	// utf8Length is the byte length a string occupies on the wire. TextEncoder
	// would allocate the bytes to count them; only the count is wanted.
	function utf8Length(s) {
		let n = 0;
		for (let i = 0; i < s.length; i++) {
			const c = s.codePointAt(i);
			if (c > 0xffff) { n += 4; i++; }
			else if (c > 0x7ff) n += 3;
			else if (c > 0x7f) n += 2;
			else n += 1;
		}
		return n;
	}

	function originOf(href) {
		try {
			const u = new URL(href);
			return (u.protocol === "wss:" ? "https:" : "http:") + "//" + u.host;
		} catch {
			return "";
		}
	}

	// The single entry point the host calls, on the loop, once per transition.
	globalThis.__ws_dispatch = (id, kind, a, b, c) => {
		const ws = sockets.get(id);
		if (ws) ws._dispatch(kind, a, b, c);
	};
	globalThis.WebSocket = WebSocket;
})();
