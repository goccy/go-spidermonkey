// compat/web: XMLHttpRequest (https://xhr.spec.whatwg.org/).
//
// This is the one browser-only API in this package: Node does not have it, which
// is why it is its own feature and why compat/nodejs asks for a surface that
// leaves it out (see features.go). It is here because the Web Platform Tests use
// it to check things that have nothing to do with XHR itself — how a header
// value is normalized, how a blob: URL is read, how a response body is decoded —
// and those are all this runtime's own behaviour.
//
// It is built on fetch rather than on the host directly. The two are specified
// over the same underlying fetch algorithm, so a second path to the network
// would be a second set of answers to every question about redirects, CORS and
// permissions.
//
// Synchronous send() is not implemented: it must block until the response
// arrives, and there is one loop goroutine to block. It reports that rather than
// pretending, because a caller that asked for synchronous behaviour and silently
// got asynchronous behaviour is worse off than one that got an error.
(() => {
	"use strict";

	const UNSENT = 0, OPENED = 1, HEADERS_RECEIVED = 2, LOADING = 3, DONE = 4;

	// The methods a request may not use, and the headers a caller may not set.
	// Both lists are the same ones fetch enforces; XHR states them again because
	// it rejects rather than ignoring in the method case.
	const FORBIDDEN_METHODS = new Set(["CONNECT", "TRACE", "TRACK"]);
	const UPPERCASE_METHODS = new Set(["DELETE", "GET", "HEAD", "OPTIONS", "POST", "PUT"]);
	const TOKEN = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;

	globalThis.ProgressEvent = class ProgressEvent extends Event {
		constructor(type, init = {}) {
			super(type, init);
			this.lengthComputable = !!init.lengthComputable;
			this.loaded = init.loaded === undefined ? 0 : Number(init.loaded);
			this.total = init.total === undefined ? 0 : Number(init.total);
		}
	};

	// The event-handler attributes live on the shared base, as they do in the
	// specification, so an upload object answers to the same set.
	const HANDLERS = ["loadstart", "progress", "abort", "error", "load", "timeout", "loadend"];

	class XMLHttpRequestEventTarget extends EventTarget {
		constructor() {
			super();
			this._on = {};
		}
	}
	for (const type of HANDLERS) {
		Object.defineProperty(XMLHttpRequestEventTarget.prototype, "on" + type, {
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
	globalThis.XMLHttpRequestEventTarget = XMLHttpRequestEventTarget;
	globalThis.XMLHttpRequestUpload = class XMLHttpRequestUpload extends XMLHttpRequestEventTarget {};

	class XMLHttpRequest extends XMLHttpRequestEventTarget {
		constructor() {
			super();
			this._reset();
			this.timeout = 0;
			this.withCredentials = false;
			this.responseType = "";
			this.upload = new globalThis.XMLHttpRequestUpload();
			this._onrsc = null;
		}
		_reset() {
			this._state = UNSENT;
			this._method = "GET";
			this._url = "";
			this._headers = new Headers();
			this._resp = null;      // the fetch Response
			this._bytes = null;     // its body, once read
			this._error = null;     // the failure that ended it, if any
			this._aborted = false;
			this._controller = null;
			this._sent = false;
		}
		get onreadystatechange() { return this._onrsc; }
		set onreadystatechange(fn) { this._onrsc = typeof fn === "function" ? fn : null; }
		get readyState() { return this._state; }

		_setState(state) {
			this._state = state;
			if (this._onrsc) this._onrsc.call(this, new Event("readystatechange"));
			this.dispatchEvent(new Event("readystatechange"));
		}
		_fire(target, type, loaded, total) {
			target.dispatchEvent(new ProgressEvent(type, {
				lengthComputable: total > 0, loaded, total,
			}));
		}

		open(method, url, async = true, username, password) {
			const m = String(method);
			if (!TOKEN.test(m)) throw new DOMException(`open: ${m} is not a method`, "SyntaxError");
			if (FORBIDDEN_METHODS.has(m.toUpperCase())) {
				throw new DOMException(`open: ${m} is a forbidden method`, "SecurityError");
			}
			if (async === false) {
				// See the note at the top of this file.
				throw new DOMException(
					"open: synchronous XMLHttpRequest is not supported by this runtime", "InvalidAccessError");
			}
			this._reset();
			this._method = UPPERCASE_METHODS.has(m.toUpperCase()) ? m.toUpperCase() : m;
			let resolved;
			try {
				resolved = new URL(String(url), globalThis.location ? globalThis.location.href : undefined);
			} catch {
				throw new DOMException(`open: ${String(url)} is not a URL`, "SyntaxError");
			}
			if (username !== undefined && username !== null) resolved.username = String(username);
			if (password !== undefined && password !== null) resolved.password = String(password);
			this._url = resolved.href;
			this._setState(OPENED);
		}

		setRequestHeader(name, value) {
			if (this._state !== OPENED || this._sent) {
				throw new DOMException("setRequestHeader: the request is not open", "InvalidStateError");
			}
			// XHR REJECTS a malformed name or value, where the Headers constructor
			// throws a TypeError; the name here is SyntaxError.
			const n = String(name);
			const v = String(value).replace(/^[\t\n\r ]+|[\t\n\r ]+$/g, "");
			if (!TOKEN.test(n) || /[\0\n\r]/.test(v)) {
				throw new DOMException("setRequestHeader: invalid header name or value", "SyntaxError");
			}
			// A forbidden header is IGNORED, not refused: it is the user agent's to
			// set, and the caller has made no mistake by trying.
			this._headers.append(n, v);
		}

		abort() {
			this._aborted = true;
			if (this._controller) this._controller.abort();
			if (this._state === OPENED && this._sent || this._state === HEADERS_RECEIVED || this._state === LOADING) {
				this._setState(DONE);
				this._fire(this, "abort", 0, 0);
				this._fire(this, "loadend", 0, 0);
			}
			this._state = UNSENT;
		}

		send(body = null) {
			if (this._state !== OPENED || this._sent) {
				throw new DOMException("send: the request is not open", "InvalidStateError");
			}
			this._sent = true;
			this._controller = new AbortController();
			const init = {
				method: this._method,
				headers: this._headers,
				signal: this._controller.signal,
				redirect: "follow",
			};
			if (body !== null && body !== undefined && this._method !== "GET" && this._method !== "HEAD") {
				init.body = body;
			}
			this._fire(this, "loadstart", 0, 0);

			const timer = this.timeout > 0
				? setTimeout(() => {
					this._timedOut = true;
					if (this._controller) this._controller.abort();
				}, this.timeout)
				: null;

			fetch(this._url, init).then(
				async (res) => {
					if (this._aborted) return;
					this._resp = res;
					this._setState(HEADERS_RECEIVED);
					this._setState(LOADING);
					const buf = await res.arrayBuffer();
					if (this._aborted) return;
					this._bytes = new Uint8Array(buf);
					if (timer !== null) clearTimeout(timer);
					this._setState(DONE);
					const n = this._bytes.length;
					this._fire(this, "progress", n, n);
					this._fire(this, "load", n, n);
					this._fire(this, "loadend", n, n);
				},
				(err) => {
					if (timer !== null) clearTimeout(timer);
					if (this._aborted) return;
					this._error = err;
					this._setState(DONE);
					this._fire(this, this._timedOut ? "timeout" : "error", 0, 0);
					this._fire(this, "loadend", 0, 0);
				});
		}

		get status() { return this._resp ? this._resp.status : 0; }
		get statusText() { return this._resp ? this._resp.statusText : ""; }
		get responseURL() { return this._resp ? (this._resp.url || "") : ""; }

		getResponseHeader(name) {
			if (!this._resp) return null;
			try { return this._resp.headers.get(String(name)); } catch { return null; }
		}
		getAllResponseHeaders() {
			if (!this._resp) return "";
			const out = [];
			for (const [k, v] of this._resp.headers) out.push(`${k}: ${v}\r\n`);
			return out.sort().join("");
		}
		overrideMimeType(mime) {
			if (this._state === LOADING || this._state === DONE) {
				throw new DOMException("overrideMimeType: the response is already being delivered", "InvalidStateError");
			}
			this._override = String(mime);
		}

		// _decode names the charset the body should be read as: an override wins,
		// then the response's own Content-Type, then UTF-8.
		_decode() {
			const ct = this._override ?? (this._resp ? this._resp.headers.get("content-type") : null);
			let charset = "utf-8";
			if (ct) {
				const m = /charset=([^;]+)/i.exec(ct);
				if (m) charset = m[1].trim().replace(/^"|"$/g, "");
			}
			try {
				return new TextDecoder(charset).decode(this._bytes);
			} catch {
				return new TextDecoder().decode(this._bytes);
			}
		}

		get responseText() {
			if (this.responseType !== "" && this.responseType !== "text") {
				throw new DOMException("responseText: the response type is not text", "InvalidStateError");
			}
			if (!this._bytes) return "";
			return this._decode();
		}
		get response() {
			if (!this._bytes) return this.responseType === "" || this.responseType === "text" ? "" : null;
			switch (this.responseType) {
				case "arraybuffer":
					return this._bytes.slice().buffer;
				case "blob": {
					const ct = this._resp ? this._resp.headers.get("content-type") : null;
					return new Blob([this._bytes], ct ? { type: ct } : undefined);
				}
				case "json":
					try { return JSON.parse(this._decode()); } catch { return null; }
				default:
					return this._decode();
			}
		}
	}
	for (const [name, value] of [["UNSENT", UNSENT], ["OPENED", OPENED],
		["HEADERS_RECEIVED", HEADERS_RECEIVED], ["LOADING", LOADING], ["DONE", DONE]]) {
		for (const target of [XMLHttpRequest, XMLHttpRequest.prototype]) {
			Object.defineProperty(target, name, { value, enumerable: true });
		}
	}
	globalThis.XMLHttpRequest = XMLHttpRequest;
})();
