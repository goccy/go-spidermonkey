// compat/web builtins: the WinterTC vocabulary implemented in JS over the
// __web_ops host functions. Evaluated once by web.Install; __web_ops is
// captured and removed from the global scope.
// __web_ops stays global until every builtin file has captured it; the last
// step of web.Install deletes it.
(() => {
	"use strict";
	const ops = globalThis.__web_ops;

	// ---------------------------------------------------------------- errors

	class DOMException extends Error {
		constructor(message = "", name = "Error") {
			super(message);
			this.name = name;
		}
	}
	globalThis.DOMException ??= DOMException;

	// --------------------------------------------------------------- console

	function inspect(v, depth, seen) {
		switch (typeof v) {
			case "string": return depth === 0 ? v : JSON.stringify(v);
			case "number": case "boolean": case "undefined": return String(v);
			case "bigint": return String(v) + "n";
			case "symbol": return String(v);
			case "function": return `[Function: ${v.name || "anonymous"}]`;
		}
		if (v === null) return "null";
		if (v instanceof Error) {
			// SpiderMonkey stacks do NOT include the message line; always
			// compose both.
			const head = `${v.name}: ${v.message}`;
			return v.stack ? `${head}\n${v.stack}` : head;
		}
		if (v instanceof Date) return v.toISOString();
		if (v instanceof RegExp) return String(v);
		if (seen.has(v)) return "[Circular]";
		if (depth > 4) return Array.isArray(v) ? "[Array]" : "[Object]";
		seen.add(v);
		try {
			if (Array.isArray(v)) {
				return v.length ? `[ ${v.map((x) => inspect(x, depth + 1, seen)).join(", ")} ]` : "[]";
			}
			if (ArrayBuffer.isView(v)) {
				const items = Array.prototype.slice.call(v, 0, 32).join(", ");
				const more = v.length > 32 ? ", ..." : "";
				return `${v.constructor.name}(${v.length}) [ ${items}${more} ]`;
			}
			if (v instanceof Map) {
				const items = [...v].map(([k, x]) => `${inspect(k, depth + 1, seen)} => ${inspect(x, depth + 1, seen)}`);
				return `Map(${v.size}) {${items.length ? " " + items.join(", ") + " " : ""}}`;
			}
			if (v instanceof Set) {
				const items = [...v].map((x) => inspect(x, depth + 1, seen));
				return `Set(${v.size}) {${items.length ? " " + items.join(", ") + " " : ""}}`;
			}
			const keys = Object.keys(v);
			if (!keys.length) return "{}";
			return `{ ${keys.map((k) => {
				let val;
				try { val = v[k]; } catch (e) { return `${k}: [Getter: threw ${e && e.name || "Error"}]`; }
				return `${k}: ${inspect(val, depth + 1, seen)}`;
			}).join(", ")} }`;
		} finally {
			seen.delete(v);
		}
	}

	// printf-style formatting like util.format: if the first arg is a string with
	// %s/%d/%i/%f/%j/%o/%O/%c specifiers, substitute the following args; trailing
	// args are appended. Without specifiers, inspect+join every arg.
	const formatConsole = (args) => {
		const f = args[0];
		if (typeof f !== "string" || !/%[sdifjoOc%]/.test(f)) {
			return args.map((a) => inspect(a, 0, new Set())).join(" ");
		}
		let i = 1;
		let out = f.replace(/%([sdifjoOc%])/g, (m, spec) => {
			if (spec === "%") return "%";
			if (i >= args.length) return m;
			const a = args[i++];
			switch (spec) {
				case "s": return typeof a === "string" ? a : inspect(a, 0, new Set());
				case "d": return typeof a === "bigint" ? a + "n" : typeof a === "symbol" ? "NaN" : String(Number(a));
				case "i": return typeof a === "bigint" ? a + "n" : typeof a === "symbol" ? "NaN" : String(parseInt(a, 10));
				case "f": return typeof a === "symbol" ? "NaN" : String(parseFloat(a));
				case "j": try { return JSON.stringify(a); } catch { return "[Circular]"; }
				case "o": case "O": return inspect(a, 0, new Set());
				case "c": return ""; // CSS directive, ignored in a text console
				default: return m;
			}
		});
		for (; i < args.length; i++) out += " " + inspect(args[i], 0, new Set());
		return out;
	};
	const consoleWrite = (level, args) => {
		ops.console_write(level, formatConsole(args));
	};
	globalThis.console = {
		log: (...a) => consoleWrite(0, a),
		info: (...a) => consoleWrite(0, a),
		debug: (...a) => consoleWrite(0, a),
		warn: (...a) => consoleWrite(1, a),
		error: (...a) => consoleWrite(1, a),
		assert: (cond, ...a) => { if (!cond) consoleWrite(1, ["Assertion failed:", ...a]); },
	};

	// -------------------------------------------------- TextEncoder / TextDecoder

	function utf8Encode(str) {
		const out = [];
		for (let i = 0; i < str.length; i++) {
			let c = str.charCodeAt(i);
			if (c >= 0xd800 && c <= 0xdbff && i + 1 < str.length) {
				const lo = str.charCodeAt(i + 1);
				if (lo >= 0xdc00 && lo <= 0xdfff) {
					c = 0x10000 + ((c - 0xd800) << 10) + (lo - 0xdc00);
					i++;
				} else c = 0xfffd; // lone high surrogate
			} else if (c >= 0xd800 && c <= 0xdfff) c = 0xfffd; // lone surrogate
			if (c < 0x80) out.push(c);
			else if (c < 0x800) out.push(0xc0 | (c >> 6), 0x80 | (c & 63));
			else if (c < 0x10000) out.push(0xe0 | (c >> 12), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
			else out.push(0xf0 | (c >> 18), 0x80 | ((c >> 12) & 63), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
		}
		return new Uint8Array(out);
	}

	// incompleteUtf8Tail returns how many trailing bytes of `bytes` form an
	// as-yet-incomplete UTF-8 sequence (0 if the buffer ends on a boundary), so
	// a streaming decoder can hold them back for the next chunk.
	function incompleteUtf8Tail(bytes) {
		const n = bytes.length;
		let i = n - 1;
		let cont = 0;
		while (i >= 0 && (bytes[i] & 0xc0) === 0x80 && cont < 3) { i--; cont++; }
		if (i < 0) return 0;
		const lead = bytes[i];
		let need;
		if (lead < 0x80) need = 1;
		else if ((lead & 0xe0) === 0xc0) need = 2;
		else if ((lead & 0xf0) === 0xe0) need = 3;
		else if ((lead & 0xf8) === 0xf0) need = 4;
		else return 0; // invalid lead — let the decoder emit a replacement now
		const have = n - i;
		return have < need ? have : 0;
	}

	function utf8Decode(bytes, fatal) {
		let out = "";
		const bad = () => {
			if (fatal) throw new TypeError("TextDecoder: invalid UTF-8");
			out += "�";
		};
		for (let i = 0; i < bytes.length;) {
			const b = bytes[i];
			let cp, len;
			if (b < 0x80) { cp = b; len = 1; }
			else if ((b & 0xe0) === 0xc0) { cp = b & 0x1f; len = 2; }
			else if ((b & 0xf0) === 0xe0) { cp = b & 0x0f; len = 3; }
			else if ((b & 0xf8) === 0xf0) { cp = b & 0x07; len = 4; }
			else { bad(); i++; continue; }
			if (i + len > bytes.length) { bad(); i++; continue; }
			let ok = true;
			for (let j = 1; j < len; j++) {
				const cb = bytes[i + j];
				if ((cb & 0xc0) !== 0x80) { ok = false; break; }
				cp = (cp << 6) | (cb & 0x3f);
			}
			const overlong = (len === 2 && cp < 0x80) || (len === 3 && cp < 0x800) || (len === 4 && cp < 0x10000);
			if (!ok || overlong || cp > 0x10ffff || (cp >= 0xd800 && cp <= 0xdfff)) { bad(); i++; continue; }
			i += len;
			out += String.fromCodePoint(cp);
		}
		return out;
	}

	globalThis.TextEncoder = class TextEncoder {
		get encoding() { return "utf-8"; }
		encode(input = "") { return utf8Encode(String(input)); }
		// encodeInto(src, dest): write UTF-8 of src into dest, returning how many
		// source code units were read and bytes written (WHATWG). Commonly used
		// for zero-copy encoding; its absence crashes such libraries.
		encodeInto(source, dest) {
			const s = String(source);
			let read = 0, written = 0;
			for (const ch of s) {
				const bytes = utf8Encode(ch);
				if (written + bytes.length > dest.length) break;
				dest.set(bytes, written);
				written += bytes.length;
				read += ch.length; // surrogate pairs count as 2 units
			}
			return { read, written };
		}
	};

	// Encodings supported without ICU tables: utf-8, latin1 (1:1 code points),
	// and utf-16le (fixed 2-byte). Others still throw (need engine ICU).
	const DECODER_LABELS = {
		"utf-8": "utf8", "utf8": "utf8", "unicode-1-1-utf-8": "utf8",
		"latin1": "latin1", "iso-8859-1": "latin1", "windows-1252": "latin1",
		"utf-16le": "utf16le", "utf-16": "utf16le", "ucs-2": "utf16le", "ucs2": "utf16le",
	};
	globalThis.TextDecoder = class TextDecoder {
		constructor(label = "utf-8", options = {}) {
			const enc = DECODER_LABELS[String(label).toLowerCase()];
			if (!enc) throw new RangeError(`TextDecoder: unsupported encoding ${label}`);
			this._enc = enc;
			this.fatal = !!options.fatal;
			this.ignoreBOM = !!options.ignoreBOM;
		}
		get encoding() { return this._enc === "utf8" ? "utf-8" : this._enc === "utf16le" ? "utf-16le" : "windows-1252"; }
		decode(input, options = {}) {
			const stream = !!options.stream;
			let bytes;
			if (input === undefined) bytes = new Uint8Array(0);
			else if (input instanceof ArrayBuffer) bytes = new Uint8Array(input);
			else if (ArrayBuffer.isView(input)) bytes = new Uint8Array(input.buffer, input.byteOffset, input.byteLength);
			else throw new TypeError("TextDecoder.decode: expected an ArrayBuffer or ArrayBufferView");
			// Prepend bytes held back from a previous streaming call (an
			// incomplete multi-byte sequence at the last chunk boundary).
			if (this._pending && this._pending.length) {
				const merged = new Uint8Array(this._pending.length + bytes.length);
				merged.set(this._pending, 0);
				merged.set(bytes, this._pending.length);
				bytes = merged;
			}
			this._pending = null;
			if (this._enc === "latin1") {
				let s = "";
				for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
				return s;
			}
			if (this._enc === "utf16le") {
				// A trailing odd byte can't form a code unit yet — hold it.
				if (stream && bytes.length % 2 === 1) {
					this._pending = bytes.slice(bytes.length - 1);
					bytes = bytes.subarray(0, bytes.length - 1);
				}
				let start = 0;
				if (!this.ignoreBOM && bytes.length >= 2 && bytes[0] === 0xff && bytes[1] === 0xfe) start = 2;
				let s = "";
				for (let i = start; i + 1 < bytes.length; i += 2) s += String.fromCharCode(bytes[i] | (bytes[i + 1] << 8));
				return s;
			}
			if (!this.ignoreBOM && bytes.length >= 3 && bytes[0] === 0xef && bytes[1] === 0xbb && bytes[2] === 0xbf) {
				bytes = bytes.subarray(3);
			}
			// Hold back an incomplete trailing UTF-8 sequence so a code point
			// split across chunks isn't turned into U+FFFD.
			if (stream) {
				const keep = incompleteUtf8Tail(bytes);
				if (keep > 0) {
					this._pending = bytes.slice(bytes.length - keep);
					bytes = bytes.subarray(0, bytes.length - keep);
				}
			}
			return utf8Decode(bytes, this.fatal);
		}
	};

	// ---------------------------------------------------------- atob / btoa

	const B64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

	globalThis.btoa = function btoa(data) {
		const s = String(data);
		let out = "";
		for (let i = 0; i < s.length; i += 3) {
			const c0 = s.charCodeAt(i), c1 = s.charCodeAt(i + 1), c2 = s.charCodeAt(i + 2);
			if (c0 > 0xff || c1 > 0xff || c2 > 0xff) {
				throw new DOMException("btoa: character out of latin1 range", "InvalidCharacterError");
			}
			const n = (c0 << 16) | ((c1 | 0) << 8) | (c2 | 0);
			out += B64[(n >> 18) & 63] + B64[(n >> 12) & 63]
				+ (Number.isNaN(c1) ? "=" : B64[(n >> 6) & 63])
				+ (Number.isNaN(c2) ? "=" : B64[n & 63]);
		}
		return out;
	};

	globalThis.atob = function atob(data) {
		let s = String(data).replace(/[\t\n\f\r ]+/g, "");
		if (s.length % 4 === 0) s = s.replace(/==?$/, "");
		if (s.length % 4 === 1 || /[^A-Za-z0-9+/]/.test(s)) {
			throw new DOMException("atob: invalid base64", "InvalidCharacterError");
		}
		let out = "", buf = 0, bits = 0;
		for (const ch of s) {
			buf = (buf << 6) | B64.indexOf(ch);
			bits += 6;
			if (bits >= 8) {
				bits -= 8;
				out += String.fromCharCode((buf >> bits) & 0xff);
			}
		}
		return out;
	};

	// ------------------------------------------- EventTarget / AbortController

	globalThis.Event ??= class Event {
		constructor(type, init = {}) {
			this.type = String(type);
			this.target = null;
			this.currentTarget = null;
			this.defaultPrevented = false;
			this.bubbles = !!init.bubbles;
			this.cancelable = !!init.cancelable;
		}
		preventDefault() { if (this.cancelable) this.defaultPrevented = true; }
		stopPropagation() {}
		stopImmediatePropagation() { this._stopImmediate = true; }
	};

	globalThis.EventTarget ??= class EventTarget {
		constructor() { this._listeners = new Map(); }
		addEventListener(type, callback, options = {}) {
			if (callback === null || callback === undefined) return;
			type = String(type);
			let list = this._listeners.get(type);
			if (!list) this._listeners.set(type, list = []);
			if (list.some((l) => l.callback === callback)) return;
			list.push({ callback, once: !!(options === true ? false : options.once) });
		}
		removeEventListener(type, callback) {
			const list = this._listeners.get(String(type));
			if (!list) return;
			const i = list.findIndex((l) => l.callback === callback);
			if (i >= 0) list.splice(i, 1);
		}
		dispatchEvent(event) {
			event.target = event.currentTarget = this;
			const list = this._listeners.get(event.type);
			if (list) {
				for (const l of [...list]) {
					if (l.once) this.removeEventListener(event.type, l.callback);
					if (typeof l.callback === "function") l.callback.call(this, event);
					else if (l.callback && typeof l.callback.handleEvent === "function") l.callback.handleEvent(event);
					if (event._stopImmediate) break;
				}
			}
			return !event.defaultPrevented;
		}
	};

	class AbortSignal extends EventTarget {
		constructor() {
			super();
			this.aborted = false;
			this.reason = undefined;
			this.onabort = null;
		}
		throwIfAborted() { if (this.aborted) throw this.reason; }
		static abort(reason) {
			const s = new AbortSignal();
			abortSignal(s, reason);
			return s;
		}
		static timeout(ms) {
			const s = new AbortSignal();
			// The timer must NOT keep the loop alive on its own (Node unref's it):
			// otherwise a completed handler still holding an AbortSignal.timeout(30s)
			// delays the whole loop/response until the timeout fires.
			const t = setTimeout(() => abortSignal(s, new DOMException("The operation timed out", "TimeoutError")), ms);
			if (t && typeof t.unref === "function") t.unref();
			return s;
		}
	}
	function abortSignal(signal, reason) {
		if (signal.aborted) return;
		signal.aborted = true;
		signal.reason = reason !== undefined ? reason : new DOMException("The operation was aborted", "AbortError");
		const ev = new Event("abort");
		if (typeof signal.onabort === "function") signal.onabort.call(signal, ev);
		signal.dispatchEvent(ev);
	}
	globalThis.AbortSignal = AbortSignal;
	globalThis.AbortController = class AbortController {
		constructor() { this.signal = new AbortSignal(); }
		abort(reason) { abortSignal(this.signal, reason); }
	};

	// ------------------------------------------------------- small globals

	globalThis.queueMicrotask ??= (fn) => {
		if (typeof fn !== "function") throw new TypeError("queueMicrotask: callback is not a function");
		Promise.resolve().then(fn);
	};

	// JSON-limited for now: functions/symbols are dropped, cycles throw, and
	// platform objects are not cloneable — see the plan's Phase 1 notes.
	globalThis.structuredClone ??= (value) => {
		if (value === undefined) return undefined;
		return JSON.parse(JSON.stringify(value));
	};

	globalThis.performance ??= {
		timeOrigin: Date.now(),
		now: () => ops.perf_now(),
	};

	globalThis.crypto ??= {};
	globalThis.crypto.getRandomValues ??= (array) => {
		if (!ArrayBuffer.isView(array)) {
			throw new TypeError("getRandomValues: expected a typed array");
		}
		if (array.byteLength > 65536) {
			throw new DOMException("getRandomValues: request exceeds 65536 bytes", "QuotaExceededError");
		}
		// The host returns the random bytes as a plain array (data, not a
		// handle); copy them into the caller's view byte-wise.
		const rand = ops.random_bytes(array.byteLength);
		const view = new Uint8Array(array.buffer, array.byteOffset, array.byteLength);
		for (let i = 0; i < rand.length; i++) view[i] = rand[i];
		return array;
	};
	globalThis.crypto.randomUUID ??= () => {
		const b = crypto.getRandomValues(new Uint8Array(16));
		b[6] = (b[6] & 0x0f) | 0x40; // version 4
		b[8] = (b[8] & 0x3f) | 0x80; // variant 10
		const hex = [...b].map((x) => x.toString(16).padStart(2, "0")).join("");
		return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
	};

	// ------------------------------------------------ URL / URLSearchParams
	// Pragmatic WHATWG subset: no punycode/IDNA, simplified path
	// normalization. Grown as the flagship targets demand.

	function encodeQueryComponent(s) {
		return encodeURIComponent(s)
			.replace(/[!'()*]/g, (c) => "%" + c.charCodeAt(0).toString(16).toUpperCase())
			.replace(/%20/g, "+");
	}
	function decodeQueryComponent(s) {
		return decodeURIComponent(String(s).replace(/\+/g, " "));
	}

	class URLSearchParams {
		constructor(init = "") {
			this._pairs = [];
			this._url = null;
			if (typeof init === "string") {
				if (init.startsWith("?")) init = init.slice(1);
				for (const part of init.split("&")) {
					if (!part) continue;
					const eq = part.indexOf("=");
					const k = eq < 0 ? part : part.slice(0, eq);
					const v = eq < 0 ? "" : part.slice(eq + 1);
					let dk = k, dv = v;
					try { dk = decodeQueryComponent(k); dv = decodeQueryComponent(v); } catch {}
					this._pairs.push([dk, dv]);
				}
			} else if (init instanceof URLSearchParams) {
				this._pairs = init._pairs.map((p) => [...p]);
			} else if (Array.isArray(init)) {
				for (const pair of init) {
					if (!pair || pair.length !== 2) throw new TypeError("URLSearchParams: each init pair needs 2 items");
					this._pairs.push([String(pair[0]), String(pair[1])]);
				}
			} else if (init && typeof init === "object") {
				for (const k of Object.keys(init)) this._pairs.push([k, String(init[k])]);
			}
		}
		get size() { return this._pairs.length; }
		append(k, v) { this._pairs.push([String(k), String(v)]); this._sync(); }
		delete(k) { k = String(k); this._pairs = this._pairs.filter((p) => p[0] !== k); this._sync(); }
		get(k) { k = String(k); const p = this._pairs.find((p) => p[0] === k); return p ? p[1] : null; }
		getAll(k) { k = String(k); return this._pairs.filter((p) => p[0] === k).map((p) => p[1]); }
		has(k) { k = String(k); return this._pairs.some((p) => p[0] === k); }
		set(k, v) {
			k = String(k);
			let found = false;
			this._pairs = this._pairs.filter((p) => {
				if (p[0] !== k) return true;
				if (found) return false;
				found = true;
				p[1] = String(v);
				return true;
			});
			if (!found) this._pairs.push([k, String(v)]);
			this._sync();
		}
		sort() { this._pairs.sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0)); this._sync(); }
		toString() { return this._pairs.map(([k, v]) => encodeQueryComponent(k) + "=" + encodeQueryComponent(v)).join("&"); }
		forEach(cb, thisArg) { for (const [k, v] of [...this._pairs]) cb.call(thisArg, v, k, this); }
		*entries() { for (const p of this._pairs) yield [...p]; }
		*keys() { for (const [k] of this._pairs) yield k; }
		*values() { for (const [, v] of this._pairs) yield v; }
		[Symbol.iterator]() { return this.entries(); }
		_sync() { if (this._url) this._url._search = this._pairs.length ? "?" + this.toString() : ""; }
	}
	globalThis.URLSearchParams = URLSearchParams;

	const ABSOLUTE_URL = /^([a-zA-Z][a-zA-Z0-9+.\-]*):(?:\/\/([^/?#]*))?([^?#]*)(\?[^#]*)?(#.*)?$/;
	const DEFAULT_PORTS = { "http:": "80", "https:": "443", "ws:": "80", "wss:": "443", "ftp:": "21" };
	const SPECIAL = new Set([...Object.keys(DEFAULT_PORTS), "file:"]);

	function parseAuthority(auth) {
		let username = "", password = "", host = auth;
		const at = auth.lastIndexOf("@");
		if (at >= 0) {
			const ui = auth.slice(0, at);
			host = auth.slice(at + 1);
			const c = ui.indexOf(":");
			if (c < 0) username = ui;
			else { username = ui.slice(0, c); password = ui.slice(c + 1); }
		}
		let hostname = host, port = "";
		if (host.startsWith("[")) { // IPv6 literal
			const end = host.indexOf("]");
			if (end < 0) throw new TypeError("Invalid URL: bad IPv6 host");
			hostname = host.slice(0, end + 1);
			const rest = host.slice(end + 1);
			if (rest.startsWith(":")) port = rest.slice(1);
		} else {
			const c = host.lastIndexOf(":");
			if (c >= 0) { hostname = host.slice(0, c); port = host.slice(c + 1); }
		}
		if (port && (!/^[0-9]+$/.test(port) || Number(port) > 65535)) throw new TypeError("Invalid URL: bad port");
		return { username, password, hostname: hostname.toLowerCase(), port };
	}

	function removeDotSegments(path) {
		if (!path.startsWith("/")) return path;
		const out = [];
		const segs = path.split("/");
		const last = segs[segs.length - 1];
		const trailingSlash = last === "" || last === "." || last === "..";
		for (const seg of segs) {
			if (seg === "" || seg === ".") continue;
			if (seg === "..") out.pop();
			else out.push(seg);
		}
		return "/" + out.join("/") + (trailingSlash && out.length ? "/" : "");
	}

	class URL {
		constructor(url, base) {
			url = String(url);
			let m = ABSOLUTE_URL.exec(url);
			if (!m && base === undefined) throw new TypeError(`Invalid URL: ${url}`);
			if (m) {
				this._protocol = m[1].toLowerCase() + ":";
				const auth = m[2] !== undefined ? parseAuthority(m[2]) : { username: "", password: "", hostname: "", port: "" };
				this._username = auth.username;
				this._password = auth.password;
				this._hostname = auth.hostname;
				this._port = auth.port === DEFAULT_PORTS[this._protocol] ? "" : auth.port;
				this._pathname = m[2] !== undefined ? removeDotSegments(m[3] || "/") : (m[3] || "");
				if (m[2] !== undefined && !this._pathname.startsWith("/")) this._pathname = "/" + this._pathname;
				this._search = m[4] || "";
				this._hash = m[5] || "";
			} else {
				const b = base instanceof URL ? base : new URL(String(base));
				this._protocol = b._protocol;
				this._username = b._username;
				this._password = b._password;
				this._hostname = b._hostname;
				this._port = b._port;
				this._hash = "";
				if (url.startsWith("//")) {
					const rest = url.slice(2);
					const slash = rest.search(/[/?#]/);
					const auth = parseAuthority(slash < 0 ? rest : rest.slice(0, slash));
					this._username = auth.username;
					this._password = auth.password;
					this._hostname = auth.hostname;
					this._port = auth.port === DEFAULT_PORTS[this._protocol] ? "" : auth.port;
					const after = slash < 0 ? "" : rest.slice(slash);
					const sub = /^([^?#]*)(\?[^#]*)?(#.*)?$/.exec(after);
					this._pathname = removeDotSegments(sub[1] || "/");
					this._search = sub[2] || "";
					this._hash = sub[3] || "";
				} else if (url.startsWith("#")) {
					this._pathname = b._pathname;
					this._search = b._search;
					this._hash = url;
				} else if (url.startsWith("?")) {
					this._pathname = b._pathname;
					const sub = /^(\?[^#]*)?(#.*)?$/.exec(url);
					this._search = sub[1] || "";
					this._hash = sub[2] || "";
				} else {
					const sub = /^([^?#]*)(\?[^#]*)?(#.*)?$/.exec(url);
					let path = sub[1] || "";
					if (path.startsWith("/")) path = removeDotSegments(path);
					else if (path === "") path = b._pathname;
					else {
						const dir = b._pathname.slice(0, b._pathname.lastIndexOf("/") + 1);
						path = removeDotSegments(dir + path);
					}
					this._pathname = path;
					this._search = sub[2] || "";
					this._hash = sub[3] || "";
				}
			}
			if (SPECIAL.has(this._protocol) && this._pathname === "") this._pathname = "/";
			this._searchParams = null;
		}
		get protocol() { return this._protocol; }
		set protocol(v) { v = String(v); this._protocol = (v.endsWith(":") ? v : v + ":").toLowerCase(); }
		get username() { return this._username; }
		set username(v) { this._username = String(v); }
		get password() { return this._password; }
		set password(v) { this._password = String(v); }
		get hostname() { return this._hostname; }
		set hostname(v) { this._hostname = String(v).toLowerCase(); }
		get port() { return this._port; }
		set port(v) {
			v = String(v);
			this._port = v === DEFAULT_PORTS[this._protocol] ? "" : v;
		}
		get host() { return this._hostname + (this._port ? ":" + this._port : ""); }
		set host(v) {
			const a = parseAuthority(String(v));
			this._hostname = a.hostname;
			if (a.port) this.port = a.port;
		}
		get pathname() { return this._pathname; }
		set pathname(v) {
			v = String(v);
			this._pathname = removeDotSegments(v.startsWith("/") ? v : "/" + v);
		}
		get search() { return this._search; }
		set search(v) {
			v = String(v);
			this._search = v === "" ? "" : v.startsWith("?") ? v : "?" + v;
			if (this._searchParams) this._searchParams._pairs = new URLSearchParams(this._search)._pairs;
		}
		get hash() { return this._hash; }
		set hash(v) {
			v = String(v);
			this._hash = v === "" ? "" : v.startsWith("#") ? v : "#" + v;
		}
		get origin() {
			if (SPECIAL.has(this._protocol)) return `${this._protocol}//${this.host}`;
			return "null";
		}
		get searchParams() {
			if (!this._searchParams) {
				this._searchParams = new URLSearchParams(this._search);
				this._searchParams._url = this;
			}
			return this._searchParams;
		}
		get href() {
			let s = this._protocol;
			if (this._hostname !== "" || SPECIAL.has(this._protocol)) {
				s += "//";
				if (this._username || this._password) {
					s += this._username;
					if (this._password) s += ":" + this._password;
					s += "@";
				}
				s += this.host;
			}
			return s + this._pathname + this._search + this._hash;
		}
		set href(v) {
			const u = new URL(String(v));
			for (const k of ["_protocol", "_username", "_password", "_hostname", "_port", "_pathname", "_search", "_hash"]) {
				this[k] = u[k];
			}
			this._searchParams = null;
		}
		toString() { return this.href; }
		toJSON() { return this.href; }
		static canParse(url, base) {
			try { new URL(url, base); return true; } catch { return false; }
		}
	}
	globalThis.URL = URL;

	// ------------------------------------------------------- ReadableStream
	// Spec subset: getReader/read/releaseLock/cancel, values()/async
	// iteration. Read-driven pull (spec's ShouldCallPull-with-pending-reads,
	// the model React's renderToReadableStream uses at highWaterMark 0): each
	// read gives the source exactly one pull. The chunk may arrive
	// synchronously — a fetch/Response body blocks in the host during pull —
	// or asynchronously — React flushes once flowing and pushes via the
	// controller from its own scheduler; either way the read's waiter resolves
	// when the enqueue/close/error lands. No polling, no re-pull loop.

	class ReadableStreamDefaultReader {
		constructor(stream) {
			if (stream._locked) throw new TypeError("ReadableStream is locked");
			stream._locked = true;
			this._stream = stream;
		}
		read() {
			const s = this._stream;
			if (!s) return Promise.reject(new TypeError("reader has released its lock"));
			if (s._queue.length > 0) return Promise.resolve({ value: s._queue.shift(), done: false });
			if (s._errored) return Promise.reject(s._errorValue);
			if (s._closed) return Promise.resolve({ value: undefined, done: true });
			const waiter = new Promise((resolve, reject) => s._waiters.push({ resolve, reject }));
			s._pull();
			return waiter;
		}
		releaseLock() { if (this._stream) { this._stream._locked = false; this._stream = null; } }
		cancel(reason) {
			const s = this._stream;
			this.releaseLock();
			return s ? s.cancel(reason) : Promise.resolve(undefined);
		}
	}

	class ReadableStream {
		constructor(underlyingSource = {}) {
			this._source = underlyingSource;
			this._queue = [];
			this._closed = false;
			this._errored = false;
			this._errorValue = undefined;
			this._locked = false;
			this._waiters = []; // pending read() resolvers (push-style sources)
			const self = this;
			this._controller = {
				enqueue(chunk) {
					if (self._closed || self._errored) return; // no enqueue after close/error
					const w = self._waiters.shift();
					if (w) w.resolve({ value: chunk, done: false });
					else self._queue.push(chunk);
				},
				close() {
					if (self._closed || self._errored) return;
					self._closed = true;
					for (const w of self._waiters.splice(0)) w.resolve({ value: undefined, done: true });
				},
				error(e) {
					if (self._closed || self._errored) return;
					self._errored = true;
					self._errorValue = e;
					self._queue = []; // drop buffered chunks; the stream is errored
					for (const w of self._waiters.splice(0)) w.reject(e);
				},
			};
			if (underlyingSource.start) underlyingSource.start(this._controller);
		}
		get locked() { return this._locked; }
		// Give the source one pull for the read that just registered a waiter.
		// A push-style source (no pull — a TransformStream readable, or any
		// stream fed only through its controller) has nothing to pull and just
		// waits for its feeder. A pull may return a promise (React); a rejection
		// there surfaces as a stream error so the pending read doesn't hang.
		_pull() {
			if (this._closed || this._errored || !this._source.pull) return;
			let pulled;
			try { pulled = this._source.pull(this._controller); }
			catch (e) { this._controller.error(e); return; }
			if (pulled && typeof pulled.then === "function") {
				Promise.resolve(pulled).catch((e) => this._controller.error(e));
			}
		}
		getReader() { return new ReadableStreamDefaultReader(this); }
		cancel(reason) {
			this._queue = [];
			this._closed = true;
			// Resolve any read pending at cancel time with {done:true}; a later
			// controller.close() would now no-op (closed guard), so cancel must
			// flush the waiters itself.
			for (const w of this._waiters.splice(0)) w.resolve({ value: undefined, done: true });
			if (this._source.cancel) this._source.cancel(reason);
			return Promise.resolve(undefined);
		}
		values({ preventCancel = false } = {}) {
			const reader = this.getReader();
			return {
				next() {
					return reader.read().then((r) => {
						if (r.done) reader.releaseLock();
						return r;
					});
				},
				return(value) {
					const finish = preventCancel
						? (reader.releaseLock(), Promise.resolve(undefined))
						: reader.cancel(value);
					return Promise.resolve(finish).then(() => ({ value, done: true }));
				},
				[Symbol.asyncIterator]() { return this; },
			};
		}
		[Symbol.asyncIterator](opts) { return this.values(opts); }
		async pipeTo(destination, options = {}) {
			const reader = this.getReader();
			const writer = destination.getWriter();
			// The abort path below rejects the writer's `closed` promise; this
			// writer is internal to pipeTo and nobody else observes it, so mark
			// it handled to avoid a spurious unhandled rejection.
			writer.closed.catch(() => {});
			try {
				for (;;) {
					const { value, done } = await reader.read();
					if (done) break;
					await writer.write(value);
				}
				if (options.preventClose !== true) await writer.close();
				else writer.releaseLock();
			} catch (e) {
				if (options.preventAbort !== true) await writer.abort(e);
				throw e;
			} finally {
				reader.releaseLock();
			}
		}
		pipeThrough(transform, options) {
			this.pipeTo(transform.writable, options).catch(() => {});
			return transform.readable;
		}
		// tee() splits the stream into two branches that each receive every
		// chunk (React's renderToReadableStream + Next App Router use it). It is
		// DEMAND-driven: the source is read only when a branch pulls, so an
		// unbounded source is not eagerly drained into memory, and the source is
		// cancelled once BOTH branches cancel.
		tee() {
			const reader = this.getReader();
			let c1, c2;
			let reading = false;
			let pullAgain = false;
			let cancelled1 = false;
			let cancelled2 = false;
			const pump = () => {
				// One source read at a time; a pull arriving mid-read (e.g. two
				// reads on the same branch) sets pullAgain so we read again once
				// the current one lands, instead of silently dropping the demand.
				if (reading) { pullAgain = true; return; }
				reading = true;
				reader.read().then(({ value, done }) => {
					reading = false;
					if (done) { c1.close(); c2.close(); return; }
					if (!cancelled1) c1.enqueue(value);
					if (!cancelled2) c2.enqueue(value);
					if (pullAgain) { pullAgain = false; pump(); }
				}).catch((e) => { c1.error(e); c2.error(e); });
			};
			const maybeCancel = (reason) => { if (cancelled1 && cancelled2) reader.cancel(reason); };
			const branch1 = new ReadableStream({
				start(c) { c1 = c; },
				pull() { pump(); },
				cancel(reason) { cancelled1 = true; maybeCancel(reason); },
			});
			const branch2 = new ReadableStream({
				start(c) { c2 = c; },
				pull() { pump(); },
				cancel(reason) { cancelled2 = true; maybeCancel(reason); },
			});
			return [branch1, branch2];
		}
	}
	globalThis.ReadableStream = ReadableStream;

	class WritableStreamDefaultWriter {
		constructor(stream) {
			if (stream._locked) throw new TypeError("WritableStream is locked");
			stream._locked = true;
			this._stream = stream;
			this.ready = Promise.resolve();
			this.desiredSize = 1;
			this.closed = new Promise((resolve, reject) => { stream._closedResolve = resolve; stream._closedReject = reject; });
		}
		write(chunk) {
			const s = this._stream;
			if (!s || s._state !== "writable") return Promise.reject(new TypeError("cannot write to this stream"));
			// Serialize writes: chaining on the previous one keeps two un-awaited
			// write() calls from running the sink concurrently (which for a
			// TransformStream would interleave transform/enqueue). A queued write
			// re-checks state, so an abort/close between enqueue and execution
			// stops it from still reaching the sink.
			s._writeChain = (s._writeChain || Promise.resolve()).then(() => {
				if (!s || s._state !== "writable") return undefined;
				return s._sink.write ? s._sink.write(chunk, s._controller) : undefined;
			});
			return s._writeChain;
		}
		async close() {
			const s = this._stream;
			if (!s || s._state !== "writable") throw new TypeError("cannot close this stream");
			await (s._writeChain || Promise.resolve()); // flush queued writes first
			s._state = "closed";
			if (s._sink.close) await s._sink.close();
			s._closedResolve();
		}
		async abort(reason) {
			const s = this._stream;
			if (!s) return;
			s._state = "errored";
			if (s._sink.abort) await s._sink.abort(reason);
			// Per spec, aborting REJECTS the writer's closed promise with the
			// reason (it previously resolved it, defeating error handling).
			if (s._closedReject) s._closedReject(reason);
		}
		releaseLock() {
			if (this._stream) {
				this._stream._locked = false;
				this._stream = null;
			}
		}
	}

	class WritableStream {
		constructor(underlyingSink = {}) {
			this._sink = underlyingSink;
			this._locked = false;
			this._state = "writable";
			this._controller = { error: () => { this._state = "errored"; } };
			if (underlyingSink.start) underlyingSink.start(this._controller);
		}
		get locked() { return this._locked; }
		getWriter() { return new WritableStreamDefaultWriter(this); }
		async close() {
			const w = this.getWriter();
			await w.close();
			w.releaseLock();
		}
		async abort(reason) {
			const w = this.getWriter();
			await w.abort(reason);
			w.releaseLock();
		}
	}
	globalThis.WritableStream = WritableStream;

	class TransformStream {
		constructor(transformer = {}) {
			let rc;
			this.readable = new ReadableStream({ start(c) { rc = c; } });
			const controller = {
				enqueue: (chunk) => rc.enqueue(chunk),
				terminate: () => rc.close(),
				error: (e) => rc.error(e),
				get desiredSize() { return 1; },
			};
			this.writable = new WritableStream({
				// A throw/rejection in transform or flush must error the READABLE
				// side too, not just reject the write — otherwise a consumer
				// reading transform.readable directly (not via pipeThrough) would
				// hang forever on a transform that failed.
				write: async (chunk) => {
					try {
						if (transformer.transform) await transformer.transform(chunk, controller);
						else controller.enqueue(chunk);
					} catch (e) { rc.error(e); throw e; }
				},
				close: async () => {
					try {
						if (transformer.flush) await transformer.flush(controller);
					} catch (e) { rc.error(e); throw e; }
					rc.close();
				},
				abort: (e) => rc.error(e),
			});
			if (transformer.start) transformer.start(controller);
		}
	}
	globalThis.TransformStream = TransformStream;

	// ---------------------------------------- Headers / Request / Response
	// The fetch-vocabulary classes user code constructs (Workers handlers,
	// Hono, ...). Bodies are buffered (string | BufferSource | null);
	// ReadableStream request/response bodies come later.

	class Headers {
		constructor(init) {
			this._map = new Map(); // lowercased name -> array of values
			if (init instanceof Headers) {
				// Copy raw values, preserving each Set-Cookie separately.
				for (const [k, arr] of init._map) this._map.set(k, [...arr]);
			} else if (Array.isArray(init)) {
				for (const pair of init) {
					if (!pair || pair.length !== 2) throw new TypeError("Headers: each init pair needs 2 items");
					this.append(pair[0], pair[1]);
				}
			} else if (init && typeof init === "object") {
				for (const k of Object.keys(init)) this.set(k, init[k]);
			}
		}
		append(name, value) {
			const k = String(name).toLowerCase();
			const arr = this._map.get(k);
			if (arr) arr.push(String(value));
			else this._map.set(k, [String(value)]);
		}
		set(name, value) { this._map.set(String(name).toLowerCase(), [String(value)]); }
		// get combines multiple values with ", " per WHATWG (including Set-Cookie);
		// getSetCookie is the only way to recover individual Set-Cookie values.
		get(name) { const a = this._map.get(String(name).toLowerCase()); return a ? a.join(", ") : null; }
		getSetCookie() { return [...(this._map.get("set-cookie") || [])]; }
		has(name) { return this._map.has(String(name).toLowerCase()); }
		delete(name) { this._map.delete(String(name).toLowerCase()); }
		forEach(cb, thisArg) { for (const [k, v] of this.entries()) cb.call(thisArg, v, k, this); }
		*entries() { for (const k of [...this._map.keys()].sort()) yield [k, this._map.get(k).join(", ")]; }
		*keys() { for (const [k] of this.entries()) yield k; }
		*values() { for (const [, v] of this.entries()) yield v; }
		[Symbol.iterator]() { return this.entries(); }
	}
	globalThis.Headers = Headers;

	// (boundary/name escaping helpers)
	// Escape a FormData field name / filename for a Content-Disposition header,
	// exactly as Node/undici do: a raw " or CR/LF would break header quoting and
	// inject headers, so percent-encode them.
	const escapeFormName = (s) => String(s).replace(/\r/g, "%0D").replace(/\n/g, "%0A").replace(/"/g, "%22");
	// A random multipart boundary so it cannot be predicted and made to appear
	// inside the content (a predictable boundary is a part-injection vector).
	const randomBoundary = () => {
		const b = ops.random_bytes(16);
		let s = "";
		for (let i = 0; i < b.length; i++) s += (b[i] & 0xff).toString(16).padStart(2, "0");
		return "----GSMFormBoundary" + s;
	};
	const concatChunks = (chunks) => {
		let n = 0;
		for (const c of chunks) n += c.length;
		const out = new Uint8Array(n);
		let o = 0;
		for (const c of chunks) { out.set(c, o); o += c.length; }
		return out;
	};

	// normalizeBody turns a body init into { bytes, contentType }. The implied
	// contentType lets Request/Response set a default Content-Type (as the spec
	// requires) and is essential for FormData, whose multipart boundary must match
	// the header. Returning the raw bytes (not String(init)) is what stops a Blob
	// or FormData body from being serialized to "[object Blob]".
	function normalizeBody(init) {
		if (init === null || init === undefined) return { bytes: null, contentType: null };
		if (typeof init === "string") return { bytes: utf8Encode(init), contentType: "text/plain;charset=UTF-8" };
		if (init instanceof URLSearchParams) return { bytes: utf8Encode(init.toString()), contentType: "application/x-www-form-urlencoded;charset=UTF-8" };
		if (init instanceof ArrayBuffer) return { bytes: new Uint8Array(init.slice(0)), contentType: null };
		if (ArrayBuffer.isView(init)) {
			return { bytes: new Uint8Array(init.buffer.slice(init.byteOffset, init.byteOffset + init.byteLength)), contentType: null };
		}
		if (globalThis.Blob && init instanceof globalThis.Blob) {
			return { bytes: new Uint8Array(init._bytes), contentType: init.type || null };
		}
		if (globalThis.FormData && init instanceof globalThis.FormData) {
			const boundary = randomBoundary();
			const chunks = [];
			for (const [name, value] of init) {
				let head = `--${boundary}\r\nContent-Disposition: form-data; name="${escapeFormName(name)}"`;
				if (globalThis.Blob && value instanceof globalThis.Blob) {
					const fn = value.name !== undefined ? value.name : "blob";
					head += `; filename="${escapeFormName(fn)}"\r\n`;
					if (value.type) head += `Content-Type: ${value.type}\r\n`;
					head += "\r\n";
					chunks.push(utf8Encode(head), new Uint8Array(value._bytes), utf8Encode("\r\n"));
				} else {
					head += "\r\n\r\n";
					chunks.push(utf8Encode(head + String(value) + "\r\n"));
				}
			}
			chunks.push(utf8Encode(`--${boundary}--\r\n`));
			return { bytes: concatChunks(chunks), contentType: `multipart/form-data; boundary=${boundary}` };
		}
		if (init instanceof ReadableStream) throw new TypeError("ReadableStream bodies are not supported yet");
		return { bytes: utf8Encode(String(init)), contentType: null };
	}

	// setBody stores the body bytes on a Request/Response and, if the body implies
	// a Content-Type and none was set explicitly, applies it (spec behavior).
	function setBody(target, init) {
		const { bytes, contentType } = normalizeBody(init);
		target._body = bytes;
		if (contentType && target.headers && !target.headers.has("content-type")) {
			target.headers.set("content-type", contentType);
		}
	}

	// Shared buffered-body surface for Request and Response.
	const bodyMixin = {
		get body() {
			if (this._body === null) return null;
			const chunk = new Uint8Array(this._body);
			let delivered = false;
			return new ReadableStream({
				pull(controller) {
					if (delivered) controller.close();
					else { delivered = true; controller.enqueue(chunk); }
				},
			});
		},
		async text() { this.bodyUsed = true; return this._body === null ? "" : utf8Decode(this._body, false); },
		async json() { return JSON.parse(await this.text()); },
		async bytes() { this.bodyUsed = true; return this._body === null ? new Uint8Array(0) : new Uint8Array(this._body); },
		async arrayBuffer() { return (await this.bytes()).buffer; },
		async blob() {
			this.bodyUsed = true;
			const type = this.headers && this.headers.get ? (this.headers.get("content-type") || "") : "";
			return new Blob([this._body === null ? new Uint8Array(0) : new Uint8Array(this._body)], { type });
		},
	};

	class Request {
		constructor(input, init = {}) {
			const from = input instanceof Request ? input : null;
			this.url = from ? from.url : String(input);
			this.method = String(init.method ?? (from ? from.method : "GET")).toUpperCase();
			this.headers = new Headers(init.headers ?? (from ? from.headers : undefined));
			this.signal = init.signal ?? (from ? from.signal : undefined);
			if (init.body !== undefined) setBody(this, init.body);
			else this._body = from ? from._body : null;
			this.bodyUsed = false;
		}
		clone() { return new Request(this); }
	}
	Object.defineProperties(Request.prototype, Object.getOwnPropertyDescriptors(bodyMixin));
	globalThis.Request = Request;

	class Response {
		constructor(body = null, init = {}) {
			this.status = init.status !== undefined ? Number(init.status) : 200;
			this.statusText = init.statusText !== undefined ? String(init.statusText) : "";
			this.headers = new Headers(init.headers);
			setBody(this, body);
			this.ok = this.status >= 200 && this.status <= 299;
			this.redirected = false;
			this.url = "";
			this.bodyUsed = false;
		}
		clone() {
			const r = new Response(null, { status: this.status, statusText: this.statusText, headers: this.headers });
			r._body = this._body === null ? null : new Uint8Array(this._body);
			return r;
		}
		static json(data, init = {}) {
			const r = new Response(JSON.stringify(data), init);
			r.headers.set("content-type", "application/json");
			return r;
		}
		static redirect(url, status = 302) {
			const r = new Response(null, { status });
			r.headers.set("location", String(url));
			return r;
		}
		static error() {
			const r = new Response(null, { status: 0 });
			r.ok = false;
			return r;
		}
	}
	Object.defineProperties(Response.prototype, Object.getOwnPropertyDescriptors(bodyMixin));
	globalThis.Response = Response;

	// -------------------------------------------------------------- timers

	// A Timeout-like handle: a lot of ecosystem code does `const t =
	// setTimeout(...); t.unref()`. The handle coerces to its numeric id (via
	// Symbol.toPrimitive), so clearTimeout(handle) still works. unref/ref toggle
	// whether the timer keeps the loop alive (idempotently, so repeated calls
	// don't skew the loop's ref accounting).
	const makeTimer = (id) => ({
		_id: id,
		_reffed: true,
		unref() { if (this._reffed) { this._reffed = false; ops.timer_ref(id, false); } return this; },
		ref() { if (!this._reffed) { this._reffed = true; ops.timer_ref(id, true); } return this; },
		hasRef() { return this._reffed; },
		refresh() { return this; },
		close() { globalThis.clearTimeout(id); return this; },
		[Symbol.toPrimitive]() { return id; },
	});
	// runTimerCb isolates a timer callback throw: a throw in one timer must not
	// tear down the whole event loop. Route it to the platform's uncaught-
	// exception channel (installed by the Node layer); only if unhandled does it
	// rethrow so a genuine uncaught exception still surfaces to the host.
	const runTimerCb = (fn) => {
		try {
			fn();
		} catch (e) {
			const emit = globalThis.__emit_uncaught;
			if (!(emit && emit(e))) throw e;
		}
	};
	globalThis.setTimeout = function setTimeout(handler, delay, ...args) {
		const fn = typeof handler === "function" ? handler : () => (0, eval)(String(handler));
		const cb = () => runTimerCb(args.length ? () => fn(...args) : fn);
		return makeTimer(ops.timer_set(cb, Number(delay) || 0, false));
	};
	globalThis.setInterval = function setInterval(handler, delay, ...args) {
		const fn = typeof handler === "function" ? handler : () => (0, eval)(String(handler));
		const cb = () => runTimerCb(args.length ? () => fn(...args) : fn);
		return makeTimer(ops.timer_set(cb, Number(delay) || 0, true));
	};
	globalThis.clearTimeout = globalThis.clearInterval = (id) => {
		if (id !== undefined && id !== null) ops.timer_clear(Number(id) || 0);
	};
})();
