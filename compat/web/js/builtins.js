// compat/web builtins: the WinterTC vocabulary implemented in JS over the
// __web_ops host functions. Evaluated once by web.Install; __web_ops is
// captured and removed from the global scope.
// __web_ops stays global until every builtin file has captured it; the last
// step of web.Install deletes it.
(() => {
	"use strict";
	const ops = globalThis.__web_ops;

	// ---------------------------------------------------------------- errors

	// The legacy numeric codes. They are not decoration: the Web Platform Tests'
	// assert_throws_dom checks `code` as well as `name`, so a DOMException without
	// one fails every such assertion no matter which name it carries. Names added
	// after the legacy list have no code and report 0.
	const DOM_EXCEPTION_CODES = {
		IndexSizeError: 1, HierarchyRequestError: 3, WrongDocumentError: 4,
		InvalidCharacterError: 5, NoModificationAllowedError: 7, NotFoundError: 8,
		NotSupportedError: 9, InUseAttributeError: 10, InvalidStateError: 11,
		SyntaxError: 12, InvalidModificationError: 13, NamespaceError: 14,
		InvalidAccessError: 15, TypeMismatchError: 17, SecurityError: 18,
		NetworkError: 19, AbortError: 20, URLMismatchError: 21,
		QuotaExceededError: 22, TimeoutError: 23, InvalidNodeTypeError: 24,
		DataCloneError: 25,
	};

	class DOMException extends Error {
		constructor(message = "", name = "Error") {
			super(message);
			this.name = name;
		}
		get code() { return DOM_EXCEPTION_CODES[this.name] ?? 0; }
	}
	// The constants live on both the interface and its prototype, as they do for
	// every Web IDL interface that has them.
	for (const [name, code] of Object.entries(DOM_EXCEPTION_CODES)) {
		// INDEX_SIZE_ERR from IndexSizeError: the legacy spelling is the name with
		// its words separated and "Error" shortened to "ERR".
		const legacy = name.replace(/Error$/, "").replace(/([a-z0-9])([A-Z])/g, "$1_$2").toUpperCase() + "_ERR";
		for (const target of [DOMException, DOMException.prototype]) {
			Object.defineProperty(target, legacy, { value: code, enumerable: true });
		}
	}
	globalThis.DOMException ??= DOMException;

	// --------------------------------------------------------------- console

	// How many entries of an array, Map or Set are rendered, matching Node's
	// util.inspect maxArrayLength default. This is not cosmetic: formatting every
	// element of a large collection builds an intermediate proportional to it, and
	// `console.log(new Array(10_000_000).fill("x"))` — which WPT's
	// console-log-large-array test does — exhausted the interpreter's memory
	// outright.
	const MAX_ENTRIES = 100;

	function moreItems(total) {
		const n = total - MAX_ENTRIES;
		return n > 0 ? `, ... ${n} more item${n === 1 ? "" : "s"}` : "";
	}

	// take reads at most MAX_ENTRIES entries from an iterable without
	// materializing all of it.
	function take(it) {
		const out = [];
		for (const x of it) {
			if (out.length >= MAX_ENTRIES) break;
			out.push(x);
		}
		return out;
	}

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
				if (!v.length) return "[]";
				const shown = v.slice(0, MAX_ENTRIES).map((x) => inspect(x, depth + 1, seen));
				return `[ ${shown.join(", ")}${moreItems(v.length)} ]`;
			}
			if (ArrayBuffer.isView(v)) {
				const items = Array.prototype.slice.call(v, 0, 32).join(", ");
				const more = v.length > 32 ? ", ..." : "";
				return `${v.constructor.name}(${v.length}) [ ${items}${more} ]`;
			}
			if (v instanceof Map) {
				const items = take(v).map(([k, x]) => `${inspect(k, depth + 1, seen)} => ${inspect(x, depth + 1, seen)}`);
				const body = items.join(", ") + moreItems(v.size);
				return `Map(${v.size}) {${items.length ? " " + body + " " : ""}}`;
			}
			if (v instanceof Set) {
				const items = take(v).map((x) => inspect(x, depth + 1, seen));
				const body = items.join(", ") + moreItems(v.size);
				return `Set(${v.size}) {${items.length ? " " + body + " " : ""}}`;
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

	// utf8Decode implements the WHATWG UTF-8 decoder state machine
	// (https://encoding.spec.whatwg.org/#utf-8-decoder). In lenient mode an
	// ill-formed sequence yields ONE U+FFFD per maximal subpart: a truncated
	// lead+continuation run is a single error, and a byte that fails the
	// second-byte boundary check (E0→A0-BF, ED→80-9F, F0→90-BF, F4→80-8F) is
	// pushed back and re-examined as a potential lead byte.
	function utf8Decode(bytes, fatal) {
		let out = "";
		const bad = () => {
			if (fatal) throw new TypeError("TextDecoder: invalid UTF-8");
			out += "�";
		};
		let cp = 0, seen = 0, needed = 0, lower = 0x80, upper = 0xbf;
		for (let i = 0; i < bytes.length; i++) {
			const b = bytes[i];
			if (needed === 0) {
				if (b < 0x80) out += String.fromCharCode(b);
				else if (b >= 0xc2 && b <= 0xdf) { needed = 1; cp = b & 0x1f; }
				else if (b >= 0xe0 && b <= 0xef) {
					if (b === 0xe0) lower = 0xa0;
					if (b === 0xed) upper = 0x9f;
					needed = 2;
					cp = b & 0x0f;
				} else if (b >= 0xf0 && b <= 0xf4) {
					if (b === 0xf0) lower = 0x90;
					if (b === 0xf4) upper = 0x8f;
					needed = 3;
					cp = b & 0x07;
				} else bad();
				continue;
			}
			if (b < lower || b > upper) {
				cp = 0; needed = 0; seen = 0; lower = 0x80; upper = 0xbf;
				bad();
				i--; // restore the byte: it may start a new sequence
				continue;
			}
			lower = 0x80;
			upper = 0xbf;
			cp = (cp << 6) | (b & 0x3f);
			if (++seen === needed) {
				out += String.fromCodePoint(cp);
				cp = 0; needed = 0; seen = 0;
			}
		}
		if (needed !== 0) bad(); // EOF inside a sequence: one replacement total
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

	// Encodings that need no conversion table: utf-8, latin1 (1:1 code points)
	// and utf-16le (fixed 2-byte). Every other label throws for now. Adding one
	// is a HOST-side job (a byte table, or golang.org/x/text/encoding behind a
	// host op) — TextDecoder is a web API, so the engine's ICU does not offer it
	// even though Intl is fully built in.
	// Per WHATWG, the latin1/iso-8859-1/windows-1252 labels ALL use the
	// windows-1252 decoder, which differs from ISO-8859-1 only in 0x80-0x9F (euro,
	// smart quotes, dashes, ellipsis, ™). The high half (0xA0-0xFF) is identical.
	const WIN1252_80_9F = [0x20AC, 0x81, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021, 0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0x8D, 0x017D, 0x8F, 0x90, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014, 0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0x9D, 0x017E, 0x0178];
	const DECODER_LABELS = {
		"utf-8": "utf8", "utf8": "utf8", "unicode-1-1-utf-8": "utf8",
		"latin1": "win1252", "iso-8859-1": "win1252", "windows-1252": "win1252",
		"utf-16le": "utf16le", "utf-16": "utf16le", "ucs-2": "utf16le", "ucs2": "utf16le",
	};
	globalThis.TextDecoder = class TextDecoder {
		constructor(label = "utf-8", options = {}) {
			const normalized = String(label).trim().toLowerCase();
			const enc = DECODER_LABELS[normalized];
			if (enc) {
				this._enc = enc;
				this._name = enc === "utf8" ? "utf-8" : enc === "utf16le" ? "utf-16le" : "windows-1252";
			} else {
				// Every other encoding in the standard lives in a table on the
				// host: Shift_JIS, GBK, Big5, EUC-KR, ISO-2022-JP, the ISO-8859
				// and windows-125x families, utf-16be. The label is validated
				// here so an unknown one is still a RangeError.
				const name = ops.text_encoding_name(normalized);
				if (!name) throw new RangeError(`TextDecoder: unsupported encoding ${label}`);
				this._enc = "host";
				this._name = name;
				this._label = normalized;
			}
			this.fatal = !!options.fatal;
			this.ignoreBOM = !!options.ignoreBOM;
		}
		get encoding() { return this._name; }
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
			if (this._enc === "host") {
				// The host decodes whole buffers; a streaming call holds nothing
				// back, so a multi-byte sequence split across chunks decodes as
				// two malformed pieces. That is the one thing this path does not
				// do that the built-in ones do.
				const r = ops.text_decode(this._label, bytes, this.fatal);
				if (r && typeof r === "object" && r.error) throw new TypeError(r.error);
				return String(r);
			}
			if (this._enc === "win1252") {
				let s = "";
				for (let i = 0; i < bytes.length; i++) {
					const b = bytes[i];
					s += String.fromCharCode(b >= 0x80 && b <= 0x9F ? WIN1252_80_9F[b - 0x80] : b);
				}
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
			const opts = options && typeof options === "object" ? options : {};
			const signal = opts.signal;
			// WHATWG: an already-aborted signal means the listener is never added.
			if (signal && signal.aborted) return;
			let list = this._listeners.get(type);
			if (!list) this._listeners.set(type, list = []);
			if (list.some((l) => l.callback === callback)) return;
			const entry = { callback, once: !!opts.once, signalCleanup: null };
			if (signal && typeof signal.addEventListener === "function") {
				// Aborting the signal removes THIS listener; if the listener is
				// removed normally (or fires with once), the abort hook is
				// detached too so the signal doesn't accumulate dead closures.
				const onAbort = () => this.removeEventListener(type, callback);
				signal.addEventListener("abort", onAbort);
				entry.signalCleanup = () => signal.removeEventListener("abort", onAbort);
			}
			list.push(entry);
		}
		removeEventListener(type, callback) {
			const list = this._listeners.get(String(type));
			if (!list) return;
			const i = list.findIndex((l) => l.callback === callback);
			if (i >= 0) {
				const [entry] = list.splice(i, 1);
				if (entry.signalCleanup) entry.signalCleanup();
			}
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

	// globalThis as an event target. The web dispatches host-originated events
	// here rather than at an object the guest made — `unhandledrejection` below
	// is one, and it is why the delegation exists at all.
	const globalTarget = new EventTarget();
	globalThis.addEventListener ??= (...a) => globalTarget.addEventListener(...a);
	globalThis.removeEventListener ??= (...a) => globalTarget.removeEventListener(...a);
	globalThis.dispatchEvent ??= (e) => globalTarget.dispatchEvent(e);

	globalThis.PromiseRejectionEvent ??= class PromiseRejectionEvent extends Event {
		constructor(type, init = {}) {
			super(type, init);
			this.promise = init.promise;
			this.reason = init.reason;
		}
	};

	// The host calls this once per promise rejection that reached a microtask
	// checkpoint with nothing to handle it. Such a rejection is visible only to
	// the engine — an async function's promise is created by the engine, so it
	// never passes through any Promise wrapper defined here — so the host is
	// the only possible source, and this is where it becomes a web event.
	//
	// Cancelable: a handler that calls preventDefault() takes responsibility
	// for the rejection and suppresses the default report, exactly as in a
	// browser.
	globalThis.__unhandled_rejection = (reason, promise) => {
		const ev = new globalThis.PromiseRejectionEvent("unhandledrejection", {
			cancelable: true, promise, reason,
		});
		globalTarget.dispatchEvent(ev);
		if (typeof globalThis.onunhandledrejection === "function") {
			globalThis.onunhandledrejection(ev);
		}
		if (!ev.defaultPrevented) {
			console.error("Uncaught (in promise)", reason);
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
		// AbortSignal.any([...]) — aborts as soon as ANY source signal aborts (the
		// standard "combine a user abort with a timeout" fetch pattern). When one
		// fires, the listeners on the OTHER sources are removed, so a reused
		// long-lived source signal doesn't accumulate a listener per any() call.
		static any(signals) {
			const s = new AbortSignal();
			const cleanups = [];
			const settle = (reason) => {
				while (cleanups.length) cleanups.pop()();
				abortSignal(s, reason);
			};
			for (const src of signals) {
				if (src.aborted) { settle(src.reason); break; }
				const h = () => settle(src.reason);
				src.addEventListener("abort", h);
				cleanups.push(() => src.removeEventListener("abort", h));
			}
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

	// structuredClone is installed by extended.js (full Map/Set/Date/typed-array/
	// cycle-preserving clone); no JSON-limited fallback is needed here.

	globalThis.performance ??= {
		timeOrigin: Date.now(),
		now: () => ops.perf_now(),
	};

	// User Timing API (mark/measure + getEntries*) and PerformanceObserver.
	// Implemented here in the web layer so both compat/web and compat/nodejs
	// (which installs web first) share one entry buffer and one observer set;
	// node's perf_hooks re-exports these off globalThis rather than duplicating.
	(() => {
		const perf = globalThis.performance;
		if (typeof perf.mark === "function") return; // already installed
		const perfNow = () => (typeof perf.now === "function" ? perf.now() : Date.now());
		const supportedEntryTypes = Object.freeze(["mark", "measure"]);
		const entries = []; // buffered PerformanceEntry objects, in creation order
		const observers = new Set();

		class PerformanceEntry {
			constructor(name, entryType, startTime, duration, detail) {
				this.name = name;
				this.entryType = entryType;
				this.startTime = startTime;
				this.duration = duration;
				if (detail !== undefined) this.detail = detail;
			}
			toJSON() {
				const o = { name: this.name, entryType: this.entryType, startTime: this.startTime, duration: this.duration };
				if ("detail" in this) o.detail = this.detail;
				return o;
			}
		}
		class PerformanceMark extends PerformanceEntry {
			constructor(name, options) {
				const o = options || {};
				super(String(name), "mark", o.startTime != null ? o.startTime : perfNow(), 0, o.detail);
			}
		}
		class PerformanceMeasure extends PerformanceEntry {
			constructor(name, startTime, duration, detail) {
				super(String(name), "measure", startTime, duration, detail);
			}
		}
		class PerformanceObserverEntryList {
			constructor(list) { this._list = list; }
			getEntries() { return this._list.slice(); }
			getEntriesByName(name, type) {
				return this._list.filter((e) => e.name === name && (type == null || e.entryType === type));
			}
			getEntriesByType(type) { return this._list.filter((e) => e.entryType === type); }
		}

		function record(entry) {
			entries.push(entry);
			for (const obs of observers) obs._maybeQueue(entry);
			return entry;
		}
		function resolveMark(m) {
			if (typeof m === "number") return m;
			for (let i = entries.length - 1; i >= 0; i--) {
				if (entries[i].entryType === "mark" && entries[i].name === m) return entries[i].startTime;
			}
			// A handful of hardcoded navigation-timing marks resolve to timeOrigin-
			// relative 0 in Node; anything else is a programmer error.
			const err = new Error(`The "${m}" performance mark has not been set`);
			err.name = "SyntaxError";
			throw err;
		}
		function clearEntries(type, name) {
			for (let i = entries.length - 1; i >= 0; i--) {
				if (entries[i].entryType === type && (name == null || entries[i].name === name)) entries.splice(i, 1);
			}
		}

		class PerformanceObserver {
			constructor(cb) {
				if (typeof cb !== "function") throw new TypeError("PerformanceObserver requires a callback function");
				this._cb = cb;
				this._types = new Set();
				this._buffer = [];
				this._scheduled = false;
			}
			observe(options = {}) {
				const types = options.entryTypes || (options.type != null ? [options.type] : []);
				for (const t of types) this._types.add(t);
				observers.add(this);
				if (options.buffered) {
					for (const e of entries) if (this._types.has(e.entryType)) this._maybeQueue(e);
				}
			}
			disconnect() {
				observers.delete(this);
				this._buffer.length = 0;
				this._types.clear();
			}
			takeRecords() { const b = this._buffer.slice(); this._buffer.length = 0; return b; }
			_maybeQueue(entry) {
				if (!this._types.has(entry.entryType)) return;
				this._buffer.push(entry);
				if (this._scheduled) return;
				this._scheduled = true;
				// Deliver on a fresh microtask so the callback runs after the code
				// that created the entry (observers are asynchronous in Node/browsers).
				queueMicrotask(() => {
					this._scheduled = false;
					const list = this._buffer.splice(0);
					if (!list.length) return;
					this._cb(new PerformanceObserverEntryList(list), this);
				});
			}
			static get supportedEntryTypes() { return supportedEntryTypes; }
		}

		Object.assign(perf, {
			mark(name, options) { return record(new PerformanceMark(name, options)); },
			measure(name, startOrOptions, endMark) {
				let start, end, detail;
				if (startOrOptions != null && typeof startOrOptions === "object") {
					detail = startOrOptions.detail;
					const hasStart = startOrOptions.start != null;
					const hasEnd = startOrOptions.end != null;
					const hasDur = startOrOptions.duration != null;
					if (hasStart) start = resolveMark(startOrOptions.start);
					if (hasEnd) end = resolveMark(startOrOptions.end);
					if (hasDur) {
						if (hasStart && !hasEnd) end = start + startOrOptions.duration;
						else if (hasEnd && !hasStart) start = end - startOrOptions.duration;
					}
					if (start == null) start = 0;
					if (end == null) end = perfNow();
				} else {
					start = startOrOptions != null ? resolveMark(startOrOptions) : 0;
					end = endMark != null ? resolveMark(endMark) : perfNow();
				}
				return record(new PerformanceMeasure(String(name), start, end - start, detail));
			},
			getEntries() { return entries.slice(); },
			getEntriesByName(name, type) {
				return entries.filter((e) => e.name === name && (type == null || e.entryType === type));
			},
			getEntriesByType(type) { return entries.filter((e) => e.entryType === type); },
			clearMarks(name) { clearEntries("mark", name); },
			clearMeasures(name) { clearEntries("measure", name); },
			clearResourceTimings() {},
			supportedEntryTypes,
		});

		globalThis.PerformanceObserver = PerformanceObserver;
		globalThis.PerformanceEntry = PerformanceEntry;
		globalThis.PerformanceMark = PerformanceMark;
		globalThis.PerformanceMeasure = PerformanceMeasure;
		globalThis.PerformanceObserverEntryList = PerformanceObserverEntryList;
	})();

	globalThis.crypto ??= {};
	globalThis.crypto.getRandomValues ??= (array) => {
		if (!ArrayBuffer.isView(array)) {
			throw new TypeError("getRandomValues: expected a typed array");
		}
		// Float and DataView are not integer typed arrays; the spec throws.
		if (array instanceof Float32Array || array instanceof Float64Array || array instanceof DataView) {
			throw new DOMException("getRandomValues: unsupported array type", "TypeMismatchError");
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
	// WHATWG basic URL parser subset: input tab/newline stripping, backslash
	// handling for special schemes, per-component percent-encode sets, IDNA
	// (lowercase + NFC + punycode) hosts, IPv4 normalization, and dot-segment
	// path normalization. Grown as the flagship targets demand.

	function encodeQueryComponent(s) {
		// application/x-www-form-urlencoded keeps *-._ and alphanumerics literal and
		// percent-encodes the rest. encodeURIComponent leaves !'()*~ raw, of which
		// the form serializer encodes !'()~ but NOT * — match that exactly.
		return encodeURIComponent(s)
			.replace(/[!'()~]/g, (c) => "%" + c.charCodeAt(0).toString(16).toUpperCase())
			.replace(/%20/g, "+");
	}
	function decodeQueryComponent(s) {
		return decodeURIComponent(String(s).replace(/\+/g, " "));
	}

	// Calling a URLSearchParams method on the wrong receiver, or with a required
	// argument left off, is a named failure rather than a generic TypeError:
	// callers (and Node's own suite) match on the code. Without the checks
	// `params.get.call(undefined)` read properties off undefined and
	// `params.append("a")` silently stored the string "undefined" as a value.
	const invalidThis = (what) => Object.assign(
		new TypeError(`Value of "this" must be of type ${what}`), { code: "ERR_INVALID_THIS" });
	const missingArgs = (...names) => Object.assign(
		new TypeError(`The ${names.map((n) => `"${n}"`).join(" and ")} argument${names.length > 1 ? "s" : ""} must be specified`),
		{ code: "ERR_MISSING_ARGS" });

	// The iterator entries()/keys()/values() hand back. It is its own type with
	// its own brand check, and it reads the pair list LIVE — the spec has
	// iteration observe changes made while it runs.
	class URLSearchParamsIterator {
		constructor(params, kind) { this._params = params; this._kind = kind; this._i = 0; }
		next() {
			if (!(this instanceof URLSearchParamsIterator)) throw invalidThis("URLSearchParamsIterator");
			const pairs = this._params._pairs;
			if (this._i >= pairs.length) return { value: undefined, done: true };
			const [k, v] = pairs[this._i++];
			return { value: this._kind === "key" ? k : this._kind === "value" ? v : [k, v], done: false };
		}
		[Symbol.iterator]() { return this; }
	}
	Object.defineProperty(URLSearchParamsIterator.prototype, Symbol.toStringTag,
		{ value: "URLSearchParams Iterator", configurable: true });

	const brand = (v) => { if (!(v instanceof URLSearchParams)) throw invalidThis("URLSearchParams"); };
	// A name or value is stringified the way the spec's IDL conversion is, which
	// REFUSES a symbol rather than producing "Symbol()". String() would happily
	// accept one and store a value no caller ever meant to pass.
	const str = (v) => {
		if (typeof v === "symbol") throw new TypeError("Cannot convert a Symbol value to a string");
		return String(v);
	};

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
			} else if (init && typeof init[Symbol.iterator] === "function") {
				// Any iterable of [name, value] pairs (Array, Map, Set, generator).
				for (const pair of init) {
					const p = [...pair];
					if (p.length !== 2) throw new TypeError("URLSearchParams: each init pair needs 2 items");
					this._pairs.push([str(p[0]), str(p[1])]);
				}
			} else if (init && typeof init === "object") {
				for (const k of Object.keys(init)) this._pairs.push([k, str(init[k])]);
			}
		}
		get size() { brand(this); return this._pairs.length; }
		append(k, v) {
			brand(this);
			if (arguments.length < 2) throw missingArgs("name", "value");
			this._pairs.push([str(k), str(v)]);
			this._sync();
		}
		delete(k, v) {
			brand(this);
			if (arguments.length < 1) throw missingArgs("name");
			k = str(k);
			// The two-arg form (WHATWG) deletes only tuples matching BOTH name and value.
			this._pairs = arguments.length > 1
				? this._pairs.filter((p) => !(p[0] === k && p[1] === str(v)))
				: this._pairs.filter((p) => p[0] !== k);
			this._sync();
		}
		get(k) {
			brand(this);
			if (arguments.length < 1) throw missingArgs("name");
			k = str(k);
			const p = this._pairs.find((p) => p[0] === k);
			return p ? p[1] : null;
		}
		getAll(k) {
			brand(this);
			if (arguments.length < 1) throw missingArgs("name");
			k = str(k);
			return this._pairs.filter((p) => p[0] === k).map((p) => p[1]);
		}
		has(k, v) {
			brand(this);
			if (arguments.length < 1) throw missingArgs("name");
			k = str(k);
			return arguments.length > 1
				? this._pairs.some((p) => p[0] === k && p[1] === str(v))
				: this._pairs.some((p) => p[0] === k);
		}
		set(k, v) {
			brand(this);
			if (arguments.length < 2) throw missingArgs("name", "value");
			k = str(k);
			let found = false;
			this._pairs = this._pairs.filter((p) => {
				if (p[0] !== k) return true;
				if (found) return false;
				found = true;
				p[1] = str(v);
				return true;
			});
			if (!found) this._pairs.push([k, str(v)]);
			this._sync();
		}
		sort() { brand(this); this._pairs.sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0)); this._sync(); }
		toString() { brand(this); return this._pairs.map(([k, v]) => encodeQueryComponent(k) + "=" + encodeQueryComponent(v)).join("&"); }
		forEach(cb, thisArg) {
			brand(this);
			if (typeof cb !== "function") throw new TypeError("Callback must be a function");
			// Live, like the iterator: the list is re-read each step, so a
			// callback that appends is observed by the walk that is running.
			for (let i = 0; i < this._pairs.length; i++) {
				const [k, v] = this._pairs[i];
				cb.call(thisArg, v, k, this);
			}
		}
		entries() { brand(this); return new URLSearchParamsIterator(this, "entry"); }
		keys() { brand(this); return new URLSearchParamsIterator(this, "key"); }
		values() { brand(this); return new URLSearchParamsIterator(this, "value"); }
		[Symbol.iterator]() { return this.entries(); }
		_sync() { if (this._url) this._url._search = this._pairs.length ? "?" + this.toString() : ""; }
	}
	globalThis.URLSearchParams = URLSearchParams;

	// url.domainToASCII / domainToUnicode of the Node compat layer. The IDNA is
	// host-side (x/net/idna): this file used to carry an RFC 3492 punycode codec
	// and an "IDNA-lite" ToASCII beside it, which is a UTS-46 implementation
	// without UTS-46's mapping tables.
	globalThis.__url_domain_to_ascii = (d) => ops.url_domain_to_ascii(urlBytes(d));
	globalThis.__url_domain_to_unicode = (d) => ops.url_domain_to_unicode(urlBytes(d));


	// The URL interface. Every algorithm behind it — parsing, host parsing,
	// percent-encoding, the setters' state overrides — is host-side in Go, where
	// it is the state machine the URL Standard defines and is checked directly
	// against the standard's own test data. What is left here is the shape of the
	// interface: brand checks, coercion, the live searchParams object, and
	// keeping the component cache in step.
	const urlOK = (r) => {
		if (r && r.__urlError) throw new TypeError(r.message);
		return r;
	};
	// URL text crosses to the host as UTF-8 bytes, not as a string: the string
	// bridge stops at the first NUL, and a NUL in a URL is something the standard
	// has rules for ("\u0000http://h/" trims it, "http://a\u0000b/" fails) — none
	// of which can run on an input that was already cut short.
	const urlBytes = (s) => new TextEncoder().encode(String(s));

	class URL {
		constructor(url, base) {
			// A URL argument is stringified via its href, so passing a URL as a
			// base does not depend on toString being the original one.
			const b = base === undefined ? "" : (base instanceof URL ? base.href : String(base));
			this._c = urlOK(ops.url_parse(urlBytes(url), urlBytes(b)));
			this._sp = null;
		}
		// _apply runs one setter host-side. A setter the standard says to ignore
		// comes back with the URL unchanged, so there is nothing to check here.
		_apply(attr, value) {
			this._c = urlOK(ops.url_set(urlBytes(this._c.href), attr, urlBytes(value)));
			// The query may have changed under a live searchParams; re-read it
			// rather than leaving the two disagreeing.
			if (this._sp) this._sp._pairs = new URLSearchParams(this._c.search)._pairs;
		}
		get href() { return this._c.href; }
		set href(v) { this._c = urlOK(ops.url_parse(urlBytes(v), urlBytes(""))); this._sp = null; }
		get protocol() { return this._c.protocol; }
		set protocol(v) { this._apply("protocol", v); }
		get username() { return this._c.username; }
		set username(v) { this._apply("username", v); }
		get password() { return this._c.password; }
		set password(v) { this._apply("password", v); }
		get host() { return this._c.host; }
		set host(v) { this._apply("host", v); }
		get hostname() { return this._c.hostname; }
		set hostname(v) { this._apply("hostname", v); }
		get port() { return this._c.port; }
		set port(v) { this._apply("port", v); }
		get pathname() { return this._c.pathname; }
		set pathname(v) { this._apply("pathname", v); }
		get search() { return this._c.search; }
		set search(v) { this._apply("search", v); }
		get hash() { return this._c.hash; }
		set hash(v) { this._apply("hash", v); }
		get origin() { return this._c.origin; }
		get searchParams() {
			if (!this._sp) {
				this._sp = new URLSearchParams(this._c.search);
				this._sp._url = this;
			}
			return this._sp;
		}
		toString() { return this.href; }
		toJSON() { return this.href; }
		static parse(url, base) {
			try { return new URL(url, base); } catch { return null; }
		}
		static canParse(url, base) {
			try { new URL(url, base); return true; } catch { return false; }
		}
	}
	// URLSearchParams writes back through the search setter, so a change to the
	// parameters is a change to the URL and goes through the same host path.
	Object.defineProperty(URL.prototype, "_search", {
		set(v) { this._c = urlOK(ops.url_set(urlBytes(this._c.href), "search", urlBytes(v))); },
		configurable: true,
	});
	// createObjectURL hands out a blob: URL that names an in-memory Blob. It is
	// how FileAPI expects a Blob to be read by anything that takes a URL —
	// fetch, above all — and without it the whole FileAPI/url group could not
	// even start.
	const objectURLs = new Map(); // blob: URL -> Blob
	URL.createObjectURL = function createObjectURL(obj) {
		if (!(obj instanceof Blob)) {
			throw new TypeError("createObjectURL expects a Blob or File");
		}
		let origin = "null";
		try { origin = (globalThis.location && globalThis.location.origin) || "null"; } catch { /* no location */ }
		const url = "blob:" + origin + "/" + crypto.randomUUID();
		objectURLs.set(url, obj);
		return url;
	};
	URL.revokeObjectURL = function revokeObjectURL(url) {
		objectURLs.delete(String(url));
	};
	// blobForObjectURL is how fetch resolves one; the fragment is not part of
	// the key, as for any URL.
	globalThis.__blob_for_object_url = (url) => objectURLs.get(String(url).split("#")[0]);

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
			// closed: resolves undefined when the stream closes, rejects with the
			// stream's error, rejects TypeError on releaseLock (WHATWG). The stream
			// keeps a back-pointer so controller.close/error can settle it.
			stream._reader = this;
			this.closed = new Promise((resolve, reject) => { this._closedResolve = resolve; this._closedReject = reject; });
			// Internally mark handled: a consumer that never touches reader.closed
			// must not see an unhandledRejection when the stream errors.
			this.closed.catch(() => {});
			if (stream._closed) this._closedResolve(undefined);
			else if (stream._errored) this._closedReject(stream._errorValue);
		}
		read() {
			const s = this._stream;
			if (!s) return Promise.reject(new TypeError("reader has released its lock"));
			if (s._queue.length > 0) {
				s._queueTotalSize -= s._queueSizes.shift();
				return Promise.resolve({ value: s._queue.shift(), done: false });
			}
			if (s._errored) return Promise.reject(s._errorValue);
			if (s._closed) return Promise.resolve({ value: undefined, done: true });
			const waiter = new Promise((resolve, reject) => s._waiters.push({ resolve, reject }));
			s._pull();
			return waiter;
		}
		releaseLock() {
			if (this._stream) {
				const s = this._stream;
				// WHATWG: releasing the lock rejects any outstanding read() requests
				// with a TypeError (a still-pending read must not hang forever).
				if (s._waiters.length) {
					for (const w of s._waiters.splice(0)) w.reject(new TypeError("Reader was released and can no longer be used to read from its previous stream"));
				}
				// releaseLock rejects closed with a TypeError — unless it already
				// settled (close/error/cancel first: settlement is one-shot).
				this._closedReject(new TypeError("Reader was released and can no longer be used to monitor the stream's closedness"));
				s._locked = false;
				s._reader = null;
				this._stream = null;
			}
		}
		cancel(reason) {
			const s = this._stream;
			// cancel() closes the stream: outstanding reads settle as done (that is
			// cancel's contract, not releaseLock's reject), and closed resolves —
			// settle it BEFORE releaseLock's TypeError rejection can win.
			if (s) {
				for (const w of s._waiters.splice(0)) w.resolve({ value: undefined, done: true });
				this._closedResolve(undefined);
			}
			this.releaseLock();
			return s ? s.cancel(reason) : Promise.resolve(undefined);
		}
	}

	// A BYOB reader reads INTO the caller's buffer. This implementation copies
	// out of the stream's own queue rather than handing the source the caller's
	// memory to fill (the spec's byobRequest), so a source written against
	// controller.byobRequest sees none — but every consumer that only calls
	// read(view) gets the bytes, in the right view type, with the leftovers
	// kept for the next read.
	class ReadableStreamBYOBReader {
		constructor(stream) {
			if (stream._locked) throw new TypeError("ReadableStream is locked");
			stream._locked = true;
			this._stream = stream;
			stream._reader = this;
			this.closed = new Promise((resolve, reject) => { this._closedResolve = resolve; this._closedReject = reject; });
			this.closed.catch(() => {});
			if (stream._closed) this._closedResolve(undefined);
			else if (stream._errored) this._closedReject(stream._errorValue);
		}
		// read(view, { min }) fills view and does not resolve until at least `min`
		// ELEMENTS have arrived — the point of min is to stop a caller having to
		// reassemble a record that came in pieces. A stream that closes first
		// resolves with whatever did arrive and done: true; it is not an error to
		// have asked for more than the stream had.
		read(view, options = {}) {
			const s = this._stream;
			if (!s) return Promise.reject(new TypeError("reader has released its lock"));
			if (!ArrayBuffer.isView(view)) {
				return Promise.reject(new TypeError("BYOB read expects an ArrayBufferView"));
			}
			if (view.byteLength === 0) {
				return Promise.reject(new TypeError("BYOB read expects a non-empty view"));
			}
			const bytesPerElement = view.BYTES_PER_ELEMENT || 1;
			const elements = view.byteLength / bytesPerElement;
			let min = 1;
			if (options !== null && options !== undefined && options.min !== undefined) {
				const raw = Number(options.min);
				// A negative or fractional min is a malformed argument; zero is a
				// request to wait for nothing, which the standard singles out.
				if (!Number.isInteger(raw) || raw < 0) {
					return Promise.reject(new TypeError("read: min must be a non-negative integer"));
				}
				if (raw === 0) {
					return Promise.reject(new TypeError("read: min must be greater than 0"));
				}
				if (raw > elements) {
					return Promise.reject(new RangeError("read: min is larger than the view"));
				}
				min = raw;
			}
			const minBytes = min * bytesPerElement;
			const out = new Uint8Array(view.buffer, view.byteOffset, view.byteLength);
			let written = 0;

			// drain moves as much of the queue as will fit into the view, picking up
			// where the previous wakeup left off.
			const drain = () => {
				while (written < out.length && (s._byobLeftover || s._queue.length > 0)) {
					let chunk = s._byobLeftover;
					if (!chunk) {
						s._queueTotalSize -= s._queueSizes.shift();
						chunk = s._queue.shift();
						chunk = ArrayBuffer.isView(chunk)
							? new Uint8Array(chunk.buffer, chunk.byteOffset, chunk.byteLength)
							: new Uint8Array(chunk);
					}
					const take = Math.min(out.length - written, chunk.length);
					out.set(chunk.subarray(0, take), written);
					written += take;
					s._byobLeftover = take < chunk.length ? chunk.subarray(take) : null;
				}
			};
			// The result is a view of the SAME type over the filled prefix, as the
			// spec requires — a Uint16Array read gives back a Uint16Array.
			const result = (done) => {
				const Ctor = view.constructor;
				const elems = Math.floor(written / bytesPerElement);
				return { value: new Ctor(view.buffer, view.byteOffset, elems), done };
			};

			drain();
			if (written >= minBytes) return Promise.resolve(result(false));
			if (s._errored) return Promise.reject(s._errorValue);
			if (s._closed) return Promise.resolve(result(true));

			// Not enough yet: wait for more, and keep waiting until min is met or
			// the stream ends. Resolving on the first chunk is what a plain read
			// does; min exists precisely to not do that.
			return new Promise((resolve, reject) => {
				const wait = () => s._waiters.push({
					resolve: () => {
						drain();
						if (written >= minBytes) return resolve(result(false));
						if (s._closed) return resolve(result(true));
						if (s._errored) return reject(s._errorValue);
						wait();
						s._pull();
					},
					reject,
				});
				wait();
				s._pull();
			});
		}
		releaseLock() {
			if (this._stream) {
				this._stream._locked = false;
				this._stream._reader = null;
				this._closedReject(new TypeError("reader has released its lock"));
				this._stream = null;
			}
		}
		cancel(reason) {
			const s = this._stream;
			if (!s) return Promise.reject(new TypeError("reader has released its lock"));
			s._locked = false;
			s._reader = null;
			this._stream = null;
			return s.cancel(reason);
		}
	}
	globalThis.ReadableStreamBYOBReader = ReadableStreamBYOBReader;

	// A byte stream's controller is a distinct type in the standard, and the
	// suite reaches for it by name. The queue-copying BYOB reader above does not
	// use a byobRequest, so this reports null for one — a source written against
	// controller.byobRequest sees nothing to fill and falls back to enqueue().
	class ReadableByteStreamController {
		constructor(inner) { this._inner = inner; }
		get byobRequest() { return null; }
		get desiredSize() { return this._inner.desiredSize; }
		enqueue(chunk) {
			if (!ArrayBuffer.isView(chunk)) {
				throw new TypeError("a byte stream can only enqueue an ArrayBufferView");
			}
			return this._inner.enqueue(chunk);
		}
		close() { return this._inner.close(); }
		error(e) { return this._inner.error(e); }
	}
	globalThis.ReadableByteStreamController = ReadableByteStreamController;

	class ReadableStream {
		constructor(underlyingSource = {}, strategy = {}) {
			this._source = underlyingSource;
			// A byte stream carries Uint8Array chunks and can be read THROUGH a
			// caller-supplied buffer (a BYOB read). Without it, every BYOB
			// consumer — and 49 subtests of the streams suite — hit "Cannot use a
			// BYOB reader with a non-byte stream" and stopped there.
			this._isByteStream = underlyingSource && underlyingSource.type === "bytes";
			this._queue = [];
			this._queueSizes = []; // parallel to _queue: each chunk's strategy size
			this._queueTotalSize = 0;
			this._closed = false;
			this._errored = false;
			this._errorValue = undefined;
			this._locked = false;
			this._reader = null; // the active default reader (for its closed promise)
			this._waiters = []; // pending read() resolvers (push-style sources)
			this._progress = 0; // bumped whenever a pull produces (enqueue/close/error)
			// Queuing strategy: default HWM 1 and size 1 per chunk; a
			// CountQueuingStrategy/ByteLengthQueuingStrategy (or plain object with
			// highWaterMark/size) overrides both, feeding controller.desiredSize.
			const hwm = strategy && strategy.highWaterMark !== undefined ? Number(strategy.highWaterMark) : 1;
			if (Number.isNaN(hwm) || hwm < 0) throw new RangeError("Invalid highWaterMark");
			this._highWaterMark = hwm;
			this._sizeFn = strategy && typeof strategy.size === "function" ? strategy.size : null;
			const self = this;
			this._controller = {
				// desiredSize = highWaterMark - total queued size; 0 once close has
				// been requested; null once errored (WHATWG). This is what makes the
				// canonical `pull(c) { if (c.desiredSize > 0) c.enqueue(x); }` work.
				get desiredSize() {
					if (self._errored) return null;
					if (self._closed) return 0;
					return self._highWaterMark - self._queueTotalSize;
				},
				enqueue(chunk) {
					// WHATWG: enqueue after close/error throws TypeError (it used to be
					// a silent no-op here, hiding source bugs).
					if (self._closed || self._errored) throw new TypeError("Cannot enqueue a chunk into a readable stream that is closed or errored");
					// The size function runs for EVERY chunk (even one handed straight
					// to a pending read) and an invalid result errors the stream (spec:
					// throwing size() or a non-finite/negative size → RangeError).
					let size = 1;
					if (self._sizeFn) {
						try { size = Number(self._sizeFn(chunk)); }
						catch (e) { this.error(e); throw e; }
						if (Number.isNaN(size) || size < 0 || !Number.isFinite(size)) {
							const e = new RangeError("The return value of a queuing strategy's size function must be a finite, non-negative number");
							this.error(e);
							throw e;
						}
					}
					self._progress++;
					const w = self._waiters.shift();
					if (w) { w.resolve({ value: chunk, done: false }); return; }
					self._queue.push(chunk);
					self._queueSizes.push(size);
					self._queueTotalSize += size;
				},
				close() {
					if (self._closed || self._errored) return;
					self._closed = true;
					self._progress++;
					for (const w of self._waiters.splice(0)) w.resolve({ value: undefined, done: true });
					if (self._reader) self._reader._closedResolve(undefined);
				},
				error(e) {
					if (self._closed || self._errored) return;
					self._errored = true;
					self._errorValue = e;
					self._progress++;
					self._queue = []; // drop buffered chunks; the stream is errored
					self._queueSizes = [];
					self._queueTotalSize = 0;
					for (const w of self._waiters.splice(0)) w.reject(e);
					if (self._reader) self._reader._closedReject(e);
				},
			};
			if (this._isByteStream) this._controller = new ReadableByteStreamController(this._controller);
			if (underlyingSource.start) underlyingSource.start(this._controller);
		}
		get locked() { return this._locked; }
		// Give the source one pull for the read that just registered a waiter.
		// A push-style source (no pull — a TransformStream readable, or any
		// stream fed only through its controller) has nothing to pull and just
		// waits for its feeder. A pull may return a promise (React); a rejection
		// there surfaces as a stream error so the pending read doesn't hang.
		_pull() {
			// Only ONE pull may be in flight at a time. Without this, two
			// concurrent read()s (both finding the queue empty) would each invoke
			// source.pull — and an async source (fetch) would run two overlapping
			// off-loop body reads on the same buffer/connection (a data race). When
			// the in-flight pull settles, pull again if reads are still waiting.
			if (this._closed || this._errored || !this._source.pull || this._pulling) return;
			this._pulling = true;
			const mark = this._progress;
			let pulled;
			try { pulled = this._source.pull(this._controller); }
			catch (e) { this._pulling = false; this._controller.error(e); return; }
			Promise.resolve(pulled).then(
				() => {
					this._pulling = false;
					// Re-pull ONLY if this pull actually produced (enqueue/close) and
					// reads are still waiting. A push-style source with a no-op pull
					// makes no progress, so re-pulling would spin forever — leave it
					// to wait for its external feeder.
					if (this._progress !== mark && this._waiters.length > 0 && this._queue.length === 0) this._pull();
				},
				(e) => { this._pulling = false; this._controller.error(e); },
			);
		}
		getReader(opts) {
			if (opts && opts.mode !== undefined && opts.mode !== "byob") {
				throw new TypeError(`Invalid reader mode ${opts.mode}`);
			}
			if (opts && opts.mode === "byob") {
				// A BYOB reader on a stream that is not a byte stream must throw
				// rather than silently hand back a default reader that ignores the
				// caller's buffer — which would corrupt its reads.
				if (!this._isByteStream) throw new TypeError("Cannot use a BYOB reader with a non-byte stream");
				return new ReadableStreamBYOBReader(this);
			}
			return new ReadableStreamDefaultReader(this);
		}
		cancel(reason) {
			// WHATWG: cancelling a locked stream rejects with TypeError and must not
			// touch the underlying source (the lock holder owns the stream).
			if (this._locked) return Promise.reject(new TypeError("Cannot cancel a stream that already has a reader"));
			this._queue = [];
			this._queueSizes = [];
			this._queueTotalSize = 0;
			this._closed = true;
			// Recorded separately from _closed: a body whose stream was CANCELLED is
			// disturbed and can never be read again, where one that merely reached
			// its end is simply finished.
			this._cancelled = true;
			// Resolve any read pending at cancel time with {done:true}; a later
			// controller.close() would now no-op (closed guard), so cancel must
			// flush the waiters itself.
			for (const w of this._waiters.splice(0)) w.resolve({ value: undefined, done: true });
			if (this._source.cancel) this._source.cancel(reason);
			return Promise.resolve(undefined);
		}
		// ReadableStream.from(anyIterable): pulls the (sync or async) iterator
		// lazily — one next() per pull — and forwards cancel to iterator.return
		// so a generator's finally blocks run (Node 20.6+ / WHATWG).
		static from(iterable) {
			if (iterable === null || iterable === undefined) throw new TypeError("ReadableStream.from requires an iterable");
			let iterator;
			if (typeof iterable[Symbol.asyncIterator] === "function") iterator = iterable[Symbol.asyncIterator]();
			else if (typeof iterable[Symbol.iterator] === "function") iterator = iterable[Symbol.iterator]();
			else if (typeof iterable.next === "function") iterator = iterable; // a bare iterator
			else throw new TypeError("ReadableStream.from called on a non-iterable");
			return new ReadableStream({
				async pull(c) {
					const r = await iterator.next();
					if (r.done) c.close();
					else c.enqueue(await r.value);
				},
				async cancel(reason) {
					if (typeof iterator.return === "function") {
						try { await iterator.return(reason); } catch { /* iterator cleanup failure is not the canceller's problem */ }
					}
				},
			});
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
			const signal = options.signal;
			const abortReason = () => (signal && signal.reason !== undefined
				? signal.reason
				: new DOMException("The operation was aborted", "AbortError"));
			// A signal already aborted at call time rejects immediately with the
			// abort reason and transfers NOTHING (WHATWG); source/destination are
			// still cancelled/aborted per the prevent* flags.
			if (signal && signal.aborted) {
				const reason = abortReason();
				if (options.preventAbort !== true) { try { await destination.abort(reason); } catch { /* already errored */ } }
				if (options.preventCancel !== true) { try { await this.cancel(reason); } catch { /* locked/errored */ } }
				throw reason;
			}
			const reader = this.getReader();
			const writer = destination.getWriter();
			// The abort path below rejects the writer's `closed` promise; this
			// writer is internal to pipeTo and nobody else observes it, so mark
			// it handled to avoid a spurious unhandled rejection.
			writer.closed.catch(() => {});
			// A mid-pipe abort must interrupt an await that may never settle (a
			// source that stops producing) — race every step against the signal.
			let onAbort = null;
			let abortPromise = null;
			if (signal) {
				abortPromise = new Promise((_, reject) => {
					onAbort = () => reject(abortReason());
					signal.addEventListener("abort", onAbort);
				});
				abortPromise.catch(() => {}); // handled via the races below
			}
			const raced = (p) => (abortPromise ? Promise.race([p, abortPromise]) : p);
			try {
				for (;;) {
					const { value, done } = await raced(reader.read());
					if (done) break;
					await raced(writer.write(value));
				}
				if (options.preventClose !== true) await writer.close();
				else writer.releaseLock();
			} catch (e) {
				if (options.preventAbort !== true) { try { await writer.abort(e); } catch { /* already errored */ } }
				// Cancel the SOURCE too (WHATWG: a destination failure cancels the
				// readable) so an upstream fetch body / stream is released rather than
				// left streaming — the write above throws once the destination is
				// aborted/cancelled, and this stops the source.
				if (options.preventCancel !== true) { try { await reader.cancel(e); } catch { /* already released */ } }
				throw e;
			} finally {
				if (signal && onAbort) signal.removeEventListener("abort", onAbort);
				// BOTH locks go, however the pipe ended. Leaving the destination locked
				// meant a stream could be piped to exactly once, for the lifetime of
				// the process — the writer here is pipeTo's own, and nobody else can
				// release it.
				reader.releaseLock();
			}
		}
		pipeThrough(transform, options) {
			// Every argument is brand-checked BEFORE anything is piped, and a
			// failure is a synchronous TypeError — pipeThrough returns a stream,
			// not a promise, so a rejection has nowhere to go. Unchecked, a
			// malformed pair produced an undefined "readable" and the error
			// surfaced somewhere unrelated later.
			if (transform === null || typeof transform !== "object") {
				throw new TypeError("pipeThrough expects a { readable, writable } pair");
			}
			// The members are read ONE AT A TIME, in the order the dictionary
			// declares them, and each is validated before the next is touched. The
			// order is observable: a pair whose readable is wrong must fail without
			// ever having asked for its writable, and the suite checks exactly that
			// with a getter.
			const readable = transform.readable;
			if (!(readable instanceof ReadableStream)) {
				throw new TypeError("pipeThrough: transform.readable is not a ReadableStream");
			}
			const writable = transform.writable;
			if (!(writable instanceof WritableStream)) {
				throw new TypeError("pipeThrough: transform.writable is not a WritableStream");
			}
			if (options !== undefined && options !== null && typeof options !== "object") {
				throw new TypeError("pipeThrough: options must be an object");
			}
			// The options are converted before anything is piped, so a bad signal is
			// a synchronous throw rather than a pipe that fails later.
			if (options && options.signal !== undefined && options.signal !== null &&
				!(options.signal instanceof AbortSignal)) {
				throw new TypeError("pipeThrough: options.signal is not an AbortSignal");
			}
			if (this.locked) throw new TypeError("pipeThrough: the source stream is locked");
			if (writable.locked) throw new TypeError("pipeThrough: transform.writable is locked");
			this.pipeTo(writable, options).catch(() => {});
			return readable;
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

	// The two standard queuing strategies (WHATWG Streams §9): plain
	// {highWaterMark, size} carriers the ReadableStream constructor consumes.
	class CountQueuingStrategy {
		constructor(init) {
			if (init === null || typeof init !== "object" || init.highWaterMark === undefined) throw new TypeError("CountQueuingStrategy requires {highWaterMark}");
			this.highWaterMark = Number(init.highWaterMark);
		}
		size() { return 1; }
	}
	globalThis.CountQueuingStrategy = CountQueuingStrategy;

	class ByteLengthQueuingStrategy {
		constructor(init) {
			if (init === null || typeof init !== "object" || init.highWaterMark === undefined) throw new TypeError("ByteLengthQueuingStrategy requires {highWaterMark}");
			this.highWaterMark = Number(init.highWaterMark);
		}
		size(chunk) { return chunk.byteLength; }
	}
	globalThis.ByteLengthQueuingStrategy = ByteLengthQueuingStrategy;

	class WritableStreamDefaultWriter {
		constructor(stream) {
			if (stream._locked) throw new TypeError("WritableStream is locked");
			stream._locked = true;
			this._stream = stream;
			this.ready = Promise.resolve();
			this.desiredSize = 1;
			this.closed = new Promise((resolve, reject) => { stream._closedResolve = resolve; stream._closedReject = reject; });
			// Mark `closed` internally handled so a sink/transform error that rejects
			// it doesn't surface as an unhandledRejection when the caller only
			// awaited write() (native streams don't leak one here). The caller's own
			// .then/.catch on `closed` still works.
			this.closed.catch(() => {});
		}
		write(chunk) {
			const s = this._stream;
			if (!s || s._state !== "writable") return Promise.reject(new TypeError("cannot write to this stream"));
			// Serialize writes: chaining on the previous one keeps two un-awaited
			// write() calls from running the sink concurrently (which for a
			// TransformStream would interleave transform/enqueue). A queued write
			// re-checks state, so an abort/close between enqueue and execution
			// stops it from still reaching the sink.
			const p = (s._writeChain || Promise.resolve()).then(() => {
				if (!s || s._state !== "writable") return undefined;
				return s._sink.write ? s._sink.write(chunk, s._controller) : undefined;
			});
			// A sink-write failure errors the whole stream: reject `closed` and move
			// to "errored" so later writes reject at the guard above (WHATWG). Node
			// left the stream writable and `closed` pending (a hang on await closed).
			p.catch((e) => {
				if (s._state === "writable") {
					s._state = "errored";
					if (s._closedReject) s._closedReject(e);
				}
			});
			// The sequencing chain swallows the error so it doesn't poison the next
			// write's continuation; the caller still sees the real rejection via p.
			s._writeChain = p.catch(() => {});
			return p;
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
			// Idempotent: aborting an already-closed/errored/aborted stream resolves
			// WITHOUT re-invoking the sink (WHATWG), so a sink that frees a resource
			// in both close and abort isn't double-released.
			if (s._state !== "writable") return;
			s._state = "errored";
			if (s._sink.abort) await s._sink.abort(reason);
			// Per spec, aborting REJECTS the writer's closed promise with the
			// reason (it previously resolved it, defeating error handling).
			if (s._closedReject) s._closedReject(reason);
		}
		releaseLock() {
			if (this._stream) {
				// WHATWG: releasing the writer rejects its closed promise with a
				// TypeError — a no-op if close/abort already settled it.
				if (this._stream._closedReject) this._stream._closedReject(new TypeError("Writer was released and can no longer be used to monitor the stream's closedness"));
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
			let cancelled = false, cancelReason;
			this.readable = new ReadableStream({
				start(c) { rc = c; },
				// Cancelling the readable side (e.g. a consumer that got the output of
				// pipeThrough and cancels, or breaks a for-await) must stop the
				// writable side: the next write throws, so an upstream pipeTo aborts
				// and cancels ITS source (the fetch body), instead of streaming
				// forever into a dropped readable.
				cancel(reason) { cancelled = true; cancelReason = reason; },
			});
			const controller = {
				enqueue: (chunk) => rc.enqueue(chunk),
				terminate: () => rc.close(),
				error: (e) => rc.error(e),
				get desiredSize() { return rc.desiredSize; },
			};
			this.writable = new WritableStream({
				// A throw/rejection in transform or flush must error the READABLE
				// side too, not just reject the write — otherwise a consumer
				// reading transform.readable directly (not via pipeThrough) would
				// hang forever on a transform that failed.
				write: async (chunk) => {
					if (cancelled) throw cancelReason ?? new DOMException("The transform stream's readable side was cancelled", "AbortError");
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

	// A header name is an HTTP token; a header value may hold anything but NUL,
	// LF and CR, and is normalized by stripping HTTP whitespace from both ends.
	// Note what is NOT forbidden: the other control characters are legal in a
	// value, and a value made only of whitespace normalizes to the empty string
	// rather than being rejected.
	const HEADER_TOKEN = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;

	function headerName(name, op) {
		const n = String(name);
		if (!HEADER_TOKEN.test(n)) {
			throw new TypeError(`Headers.${op}: ${JSON.stringify(n)} is not a valid header name`);
		}
		return n.toLowerCase();
	}

	function headerValue(value, op) {
		const v = String(value).replace(/^[\t\n\r ]+|[\t\n\r ]+$/g, "");
		if (/[\0\n\r]/.test(v)) {
			throw new TypeError(`Headers.${op}: the header value contains a forbidden character`);
		}
		return v;
	}

	// Forbidden request headers are the ones the user agent alone controls. A
	// request's Headers silently IGNORES them rather than failing, because the
	// caller has not made a mistake — the header is simply not theirs to set.
	const FORBIDDEN_REQUEST_HEADERS = new Set([
		"accept-charset", "accept-encoding", "access-control-request-headers",
		"access-control-request-method", "connection", "content-length", "cookie",
		"cookie2", "date", "dnt", "expect", "host", "keep-alive", "origin",
		"referer", "set-cookie", "te", "trailer", "transfer-encoding", "upgrade", "via",
	]);

	// The methods a request may never use. A header that smuggles one past the
	// method field is forbidden for the same reason the method itself is.
	const FORBIDDEN_METHODS = new Set(["CONNECT", "TRACE", "TRACK"]);
	const METHOD_OVERRIDE_HEADERS = new Set(["x-http-method", "x-http-method-override", "x-method-override"]);

	function isForbiddenRequestHeader(name, value) {
		if (FORBIDDEN_REQUEST_HEADERS.has(name) || name.startsWith("proxy-") || name.startsWith("sec-")) {
			return true;
		}
		if (METHOD_OVERRIDE_HEADERS.has(name)) {
			return value.split(",").some((m) => FORBIDDEN_METHODS.has(m.trim().toUpperCase()));
		}
		// A Range header is forbidden except in the one shape a media element
		// would send.
		if (name === "range") return !/^bytes=[0-9]+-[0-9]*$/.test(value);
		return false;
	}

	// A byte that stops a header from being CORS-safelisted. These are the bytes
	// that would let a value smuggle structure past a server that trusts it.
	const CORS_UNSAFE = /[\0-\x08\x0a-\x1f"():<>?@[\\\]{}\x7f]/;

	const SAFELISTED_MIME = new Set([
		"application/x-www-form-urlencoded", "multipart/form-data", "text/plain",
	]);

	// The CORS-safelisted request headers are the ones a page could already have
	// sent with a form, so a no-cors request may carry them and nothing else.
	function isCORSSafelisted(name, value) {
		if (value.length > 128) return false;
		switch (name) {
			case "accept":
				return !CORS_UNSAFE.test(value);
			case "accept-language":
			case "content-language":
				return /^[0-9A-Za-z *,\-.;=]*$/.test(value);
			case "content-type": {
				if (CORS_UNSAFE.test(value)) return false;
				const mime = ops.mime_type(value);
				return !!mime && SAFELISTED_MIME.has(String(mime.essence ?? mime).toLowerCase());
			}
			case "range":
				return /^bytes=[0-9]+-[0-9]*$/.test(value);
		}
		return false;
	}

	// setHeadersGuard is how a Request or a Response says which headers its own
	// Headers object may hold. It is applied after the init has been copied in,
	// which is the order the standard uses: the object is filled, then adopted.
	function setHeadersGuard(headers, guard) {
		headers._guard = guard;
		if (guard === "request" || guard === "request-no-cors" || guard === "response") {
			// Re-run the filter over what is already there, since those entries
			// arrived while the guard was still "none".
			for (const k of [...headers._map.keys()]) {
				const v = headers._map.get(k).join(", ");
				if (headers._blocked(k, v, "adopt")) headers._map.delete(k);
			}
		}
		return headers;
	}

	class Headers {
		constructor(init) {
			this._map = new Map(); // lowercased name -> array of values
			// The guard decides which headers this object will hold at all: see
			// setHeadersGuard. "none" is a free-standing Headers, which filters
			// nothing.
			Object.defineProperty(this, "_guard", { value: "none", writable: true });
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
		// _blocked applies the guard. An immutable Headers throws; every other
		// guard drops the header quietly, which is what the standard asks for —
		// the operation "succeeds" and simply has no effect.
		_blocked(name, value, op) {
			switch (this._guard) {
				case "immutable":
					throw new TypeError(`Headers.${op}: these headers are immutable`);
				case "request":
					return isForbiddenRequestHeader(name, value);
				case "request-no-cors":
					return !isCORSSafelisted(name, value);
				case "response":
					return name === "set-cookie" || name === "set-cookie2";
			}
			return false;
		}
		append(name, value) {
			const k = headerName(name, "append");
			const v = headerValue(value, "append");
			// A no-cors append is judged on the COMBINED value, since that is what
			// would go on the wire.
			const existing = this._map.get(k);
			const combined = existing ? existing.join(", ") + ", " + v : v;
			if (this._blocked(k, combined, "append")) return;
			if (existing) existing.push(v);
			else this._map.set(k, [v]);
		}
		set(name, value) {
			const k = headerName(name, "set");
			const v = headerValue(value, "set");
			if (this._blocked(k, v, "set")) return;
			this._map.set(k, [v]);
		}
		// get combines multiple values with ", " per WHATWG (including Set-Cookie);
		// getSetCookie is the only way to recover individual Set-Cookie values.
		get(name) { const a = this._map.get(headerName(name, "get")); return a ? a.join(", ") : null; }
		getSetCookie() { return [...(this._map.get("set-cookie") || [])]; }
		has(name) { return this._map.has(headerName(name, "has")); }
		delete(name) {
			const k = headerName(name, "delete");
			if (this._blocked(k, this.get(k) ?? "", "delete")) return;
			this._map.delete(k);
		}
		forEach(cb, thisArg) { for (const [k, v] of this.entries()) cb.call(thisArg, v, k, this); }
		*entries() {
			for (const k of [...this._map.keys()].sort()) {
				// Set-Cookie is special-cased by the Fetch "sort and combine" step:
				// each cookie is its OWN entry, never comma-joined (a comma inside an
				// Expires date would make the joined value unparseable).
				if (k === "set-cookie") { for (const v of this._map.get(k)) yield [k, v]; }
				else yield [k, this._map.get(k).join(", ")];
			}
		}
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
		if (init instanceof ReadableStream) throw new TypeError("ReadableStream bodies must be handled before buffering");
		return { bytes: utf8Encode(String(init)), contentType: null };
	}

	// drainBodyStream reads a ReadableStream body to completion and concatenates
	// the chunks (the WHATWG "fully read body" used by text()/json()/arrayBuffer()
	// and by fetch() for a streamed request body).
	async function drainBodyStream(stream) {
		const reader = stream.getReader(); // throws TypeError if locked
		const chunks = [];
		for (;;) {
			const { value, done } = await reader.read();
			if (done) break;
			chunks.push(toBodyChunk(value));
		}
		return concatChunks(chunks);
	}

	// setBody stores the body on a Request/Response: a ReadableStream is kept
	// as-is (streamed on demand / drained by the consumers), anything else is
	// buffered to bytes. If the body implies a Content-Type and none was set
	// explicitly, apply it (spec behavior).
	function setBody(target, init) {
		if (init instanceof ReadableStream) {
			target._body = null;
			target._bodyStream = init;
			return;
		}
		const { bytes, contentType } = normalizeBody(init);
		target._body = bytes;
		if (contentType && target.headers && !target.headers.has("content-type")) {
			target.headers.set("content-type", contentType);
		}
	}

	// toBodyChunk normalizes one ReadableStream body chunk to bytes. Strings are
	// tolerated (UTF-8-encoded) because in-repo sources — and plenty of user code
	// — enqueue text; anything else must be a BufferSource per spec.
	function toBodyChunk(value) {
		if (typeof value === "string") return utf8Encode(value);
		if (value instanceof ArrayBuffer) return new Uint8Array(value.slice(0));
		if (ArrayBuffer.isView(value)) return new Uint8Array(value.buffer.slice(value.byteOffset, value.byteOffset + value.byteLength));
		throw new TypeError("ReadableStream body chunks must be strings or BufferSource");
	}

	// Shared body surface for Request and Response: buffered bytes in _body, or
	// a ReadableStream in _bodyStream (never both non-null).
	const bodyMixin = {
		get body() {
			if (this._bodyStream) return this._bodyStream;
			if (this._body === null) return null;
			// ONE stream, memoized. A fresh stream per access meant cancelling
			// `response.body` cancelled something nobody else could see, and the body
			// stayed readable — where the standard has that cancel disturb the body
			// for good.
			const chunk = new Uint8Array(this._body);
			let delivered = false;
			this._bodyStream = new ReadableStream({
				pull(controller) {
					if (delivered) controller.close();
					else { delivered = true; controller.enqueue(chunk); }
				},
			});
			return this._bodyStream;
		},
		// A guest-constructed Request/Response body may be read only once (WHATWG);
		// a second read throws a TypeError, and bodyUsed reflects that.
		_useBody() {
			// A body is "disturbed" once its stream has been read from OR cancelled,
			// not only once a consumer here has taken it: `body.cancel()` ends the
			// body just as surely as reading it does.
			if (this.bodyUsed || (this._bodyStream && (this._bodyStream.locked || this._bodyStream._cancelled))) {
				throw new TypeError("Body has already been consumed.");
			}
			this.bodyUsed = true;
		},
		// _bodyBytes: the single "fully read body" step every consumer shares. A
		// stream body is drained to completion (rejecting if locked/already used).
		async _bodyBytes() {
			this._useBody();
			if (this._bodyStream) return drainBodyStream(this._bodyStream);
			return this._body === null ? new Uint8Array(0) : new Uint8Array(this._body);
		},
		async text() { const b = await this._bodyBytes(); return b.length === 0 ? "" : utf8Decode(b, false); },
		async json() { return JSON.parse(await this.text()); },
		async bytes() { return this._bodyBytes(); },
		async arrayBuffer() { return (await this._bodyBytes()).buffer; },
		async blob() {
			const b = await this._bodyBytes();
			const type = this.headers && this.headers.get ? (this.headers.get("content-type") || "") : "";
			return new Blob([b], { type });
		},
		async formData() {
			const ct = this.headers && this.headers.get ? (this.headers.get("content-type") || "") : "";
			const b = await this._bodyBytes();
			if (ct.startsWith("multipart/form-data")) return parseMultipartForm(b, ct);
			const fd = new FormData();
			for (const [k, v] of new URLSearchParams(utf8Decode(b, false))) fd.append(k, v);
			return fd;
		},
	};

	// WHATWG makes url/method/headers/signal READONLY accessors on
	// Request.prototype, and that is not cosmetic: a subclass is allowed to
	// shadow them with its own getter, and Next.js does exactly that
	// (`class NextRequest extends Request { get url() {…} }`). Assigning
	// `this.url` in the constructor would then be a write to a getter-only
	// property, which throws in strict mode — class bodies are always strict —
	// and takes down every request. So the state lives behind a symbol and the
	// public shape is accessors, as specified.
	const kRequestState = Symbol("Request state");

	class Request {
		constructor(input, init = {}) {
			const from = input instanceof Request ? input : null;
			const url = from ? from.url : String(input);
			// WHATWG/undici: constructing a Request from a URL that carries credentials
			// throws a TypeError (they are NOT turned into an Authorization header).
			if (!from) {
				let parsed = null;
				try { parsed = new URL(url); } catch { parsed = null; }
				if (parsed && (parsed.username || parsed.password)) {
					throw new TypeError("Request cannot be constructed from a URL that includes credentials");
				}
			}
			this[kRequestState] = {
				url,
				method: String(init.method ?? (from ? from.method : "GET")).toUpperCase(),
				headers: setHeadersGuard(
					new Headers(init.headers ?? (from ? from.headers : undefined)),
					String(init.mode ?? (from ? from.mode : "") ?? "") === "no-cors" ? "request-no-cors" : "request"),
				signal: init.signal ?? (from ? from.signal : undefined),
			};
			// Workers' request.cf survives new Request(req) (workerd behavior).
			if (from && from.cf !== undefined) this.cf = from.cf;
			this._bodyStream = null;
			if (init.body !== undefined) setBody(this, init.body);
			else {
				this._body = from ? from._body : null;
				if (from && from._bodyStream) this._bodyStream = from._bodyStream;
			}
			this.bodyUsed = false;
		}
		get url() { return this[kRequestState].url; }
		get method() { return this[kRequestState].method; }
		get headers() { return this[kRequestState].headers; }
		get signal() { return this[kRequestState].signal; }
		clone() {
			// A stream body is tee'd (WHATWG): each side gets every chunk, and the
			// original keeps working. Cloning a used/locked body throws.
			if (this._bodyStream) {
				if (this.bodyUsed || this._bodyStream.locked) throw new TypeError("Cannot clone a Request whose body is used or locked");
				const [b1, b2] = this._bodyStream.tee();
				this._bodyStream = b1;
				const r = new Request(this);
				r._bodyStream = b2;
				return r;
			}
			return new Request(this);
		}
	}
	Object.defineProperties(Request.prototype, Object.getOwnPropertyDescriptors(bodyMixin));
	globalThis.Request = Request;

	class Response {
		constructor(body = null, init = {}) {
			this.status = init.status !== undefined ? Number(init.status) : 200;
			// WHATWG: a public Response status must be in 200-599 (internal null-body
			// statuses like error()'s 0 bypass this via _makeStatus below).
			if (init.status !== undefined && (this.status < 200 || this.status > 599)) {
				throw new RangeError("Response status must be in the range 200-599");
			}
			// WHATWG: null-body statuses cannot carry a body — otherwise a 204
			// with a stream body would reach the wire and abort the connection.
			if (body !== null && body !== undefined &&
				(this.status === 204 || this.status === 205 || this.status === 304)) {
				throw new TypeError(`Response constructor: status ${this.status} cannot have a body`);
			}
			this.statusText = init.statusText !== undefined ? String(init.statusText) : "";
			this.headers = setHeadersGuard(new Headers(init.headers), "response");
			this._bodyStream = null;
			setBody(this, body);
			this.ok = this.status >= 200 && this.status <= 299;
			this.redirected = false;
			this.url = "";
			this.bodyUsed = false;
		}
		clone() {
			// Build without a status in the init so the constructor range guard is
			// skipped, then copy status/ok directly — an error response (status 0) or
			// any opaque status must clone without a RangeError.
			const r = new Response(null, { statusText: this.statusText, headers: this.headers });
			r.status = this.status;
			r.ok = this.ok;
			r.type = this.type;
			if (this._bodyStream) {
				// tee() the stream body (WHATWG): both responses replay every chunk.
				// A used/locked body cannot be cloned.
				if (this.bodyUsed || this._bodyStream.locked) throw new TypeError("Cannot clone a Response whose body is used or locked");
				const [b1, b2] = this._bodyStream.tee();
				this._bodyStream = b1;
				r._bodyStream = b2;
				r._body = null;
			} else {
				r._body = this._body === null ? null : new Uint8Array(this._body);
			}
			return r;
		}
		static json(data, init = {}) {
			// Whether the CALLER supplied a content-type (the stringified body itself
			// makes setBody auto-add text/plain, so check init.headers, not the final
			// headers). Apply application/json unless the caller set one (preserving
			// e.g. application/problem+json).
			const callerCT = init.headers !== undefined && new Headers(init.headers).has("content-type");
			const r = new Response(JSON.stringify(data), init);
			if (!callerCT) r.headers.set("content-type", "application/json");
			return r;
		}
		static redirect(url, status = 302) {
			if (![301, 302, 303, 307, 308].includes(status)) {
				throw new RangeError("Invalid redirect status code");
			}
			const target = new URL(url); // throws TypeError on an unparseable URL
			const r = new Response(null, { status });
			r.headers.set("location", target.href);
			return r;
		}
		static error() {
			const r = new Response(null);
			r.status = 0;
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
	// arm() (re)schedules the underlying host timer and returns its id, so
	// refresh() can restart it with the same callback/delay (the common idle-
	// timeout-reset idiom, and Node internals). _id is mutable across refresh.
	// Outstanding timers, by host id. Node reports these through
	// process.getActiveResourcesInfo(), and answering "nothing is pending" while
	// timers were in flight made every leak-check and shutdown-ordering test
	// read the loop as already idle.
	const liveTimers = new Map();
	globalThis.__active_timers = () => [...liveTimers.values()];
	const makeTimer = (arm, kind) => ({
		_id: arm(),
		_reffed: true,
		_kind: kind,
		unref() { if (this._reffed) { this._reffed = false; ops.timer_ref(this._id, false); } return this; },
		ref() { if (!this._reffed) { this._reffed = true; ops.timer_ref(this._id, true); } return this; },
		hasRef() { return this._reffed; },
		refresh() {
			liveTimers.delete(this._id);
			ops.timer_clear(this._id);
			this._id = arm();
			liveTimers.set(this._id, this._kind);
			if (!this._reffed) ops.timer_ref(this._id, false); // preserve unref state
			return this;
		},
		close() { globalThis.clearTimeout(this._id); return this; },
		[Symbol.toPrimitive]() { return this._id; },
	});
	// A one-shot timer stops being active the moment it fires; an interval stays
	// active until it is cleared.
	const trackTimer = (arm, kind, repeating) => {
		const t = makeTimer(arm, kind);
		liveTimers.set(t._id, kind);
		if (!repeating) t._oneShot = true;
		return t;
	};
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
	// reportError(e): report to the global error channel without throwing (a stable
	// global in browsers and Node >=17).
	globalThis.reportError ??= (e) => {
		const emit = globalThis.__emit_uncaught;
		if (!(emit && emit(e))) { try { console.error(e); } catch { /* ignore */ } }
	};
	globalThis.setTimeout = function setTimeout(handler, delay, ...args) {
		const fn = typeof handler === "function" ? handler : () => (0, eval)(String(handler));
		let self;
		// A one-shot timer is still active WHILE its callback runs — Node reports
		// it from inside — and stops being active once the callback returns.
		const cb = () => {
			try { runTimerCb(args.length ? () => fn(...args) : fn); }
			finally { if (self) liveTimers.delete(self._id); }
		};
		const d = Number(delay) || 0;
		self = trackTimer(() => ops.timer_set(cb, d, false), "Timeout", false);
		return self;
	};
	globalThis.setInterval = function setInterval(handler, delay, ...args) {
		const fn = typeof handler === "function" ? handler : () => (0, eval)(String(handler));
		const cb = () => runTimerCb(args.length ? () => fn(...args) : fn);
		const d = Number(delay) || 0;
		return trackTimer(() => ops.timer_set(cb, d, true), "Timeout", true);
	};
	globalThis.clearTimeout = globalThis.clearInterval = (id) => {
		if (id === undefined || id === null) return;
		const n = Number(id) || 0;
		liveTimers.delete(n);
		ops.timer_clear(n);
	};

	// fetch: a thin JS wrapper over the native host fetch. WHATWG allows a Headers
	// instance and non-string header values as init.headers, and URLSearchParams /
	// FormData / Blob as init.body; the native path only understands a plain
	// {string:string} headers object and a Uint8Array body. Normalize both here (and
	// accept a Request as input) so those common shapes don't throw at the boundary.
	// requestOrigin/requestReferrer implement the two headers every fetch
	// carries and this one did not send at all, which is why the suite's
	// fixtures reported them as absent.
	//
	// The environment's own URL is `location.href`; an embedding with no
	// location has no origin to speak for, so both headers are omitted rather
	// than invented.
	function environmentURL() {
		try {
			const href = globalThis.location && globalThis.location.href;
			return href ? new URL(href) : null;
		} catch { return null; }
	}

	// Origin travels with a CORS request and with any method outside the
	// safelist — a POST states its origin even in no-cors mode.
	function originHeaderFor(mode, method, envURL) {
		if (!envURL) return null;
		const m = String(method || "GET").toUpperCase();
		if (mode === "cors" || (m !== "GET" && m !== "HEAD")) return envURL.origin;
		return null;
	}

	// Referer under the default policy, strict-origin-when-cross-origin: the
	// full URL to a same-origin peer, the bare origin cross-origin, and nothing
	// at all when stepping down from https to http.
	function referrerHeaderFor(policy, envURL, targetURL) {
		if (!envURL || !targetURL) return null;
		const p = String(policy || "strict-origin-when-cross-origin");
		if (p === "no-referrer") return null;
		// The referrer never carries credentials or a fragment.
		const full = envURL.origin + envURL.pathname + envURL.search;
		if (p === "unsafe-url") return full;
		const downgrade = envURL.protocol === "https:" && targetURL.protocol === "http:";
		if (downgrade && (p === "strict-origin-when-cross-origin" || p === "strict-origin" ||
			p === "no-referrer-when-downgrade")) {
			return null;
		}
		const originRef = envURL.origin + "/";
		if (p === "origin") return originRef;
		if (p === "same-origin") return envURL.origin === targetURL.origin ? full : null;
		if (p === "origin-when-cross-origin" || p === "strict-origin-when-cross-origin") {
			return envURL.origin === targetURL.origin ? full : originRef;
		}
		if (p === "strict-origin") return originRef;
		return full; // "no-referrer-when-downgrade" and anything unrecognized
	}

	// checkCORS enforces the part of the Fetch spec that decides whether a
	// cross-origin response may be handed to the caller at all. None of it was
	// enforced: every cross-origin fetch resolved, so the ~265 subtests that
	// assert a request MUST be rejected all reported "Should have rejected".
	//
	// A request whose mode is "same-origin" never leaves the origin; a "cors"
	// request needs the server's permission in Access-Control-Allow-Origin; a
	// "no-cors" request is allowed but its response is opaque.
	function corsError(what) {
		return new TypeError("Failed to fetch: " + what);
	}

	// A network failure rejects with a TypeError. The host reports one as a
	// plain string ("Get \"http://…\": dial tcp …"), and rejecting with that
	// string meant every test asserting `promise_rejects_js(TypeError)` saw a
	// string instead — the failure was right, its type was not.
	function asFetchError(err) {
		if (err instanceof Error) return err;
		return new TypeError("Failed to fetch: " + String(err));
	}

	function corsAllowsResponse(res, origin) {
		const allow = res.headers && res.headers.get("access-control-allow-origin");
		if (!allow) return false;
		if (allow === "*") {
			// A wildcard cannot authorize a credentialed request.
			return true;
		}
		return allow === origin;
	}

	// --- CORS preflight ---------------------------------------------------
	// A cross-origin request that is not "simple" may not be sent until the
	// server has said it is welcome. Skipping the preflight meant a request the
	// spec forbids went out anyway, and the suite's preflight fixtures reported
	// that none had been made.
	const CORS_SAFELISTED_REQUEST_HEADERS = new Set([
		"accept", "accept-language", "content-language", "content-type", "range",
	]);
	const CORS_SAFELISTED_CONTENT_TYPES = new Set([
		"application/x-www-form-urlencoded", "multipart/form-data", "text/plain",
	]);
	// Response headers a cors response exposes without being asked to.
	const CORS_SAFELISTED_RESPONSE_HEADERS = new Set([
		"cache-control", "content-language", "content-length", "content-type",
		"expires", "last-modified", "pragma",
	]);

	function isSafelistedRequestHeader(name, value) {
		const n = String(name).toLowerCase();
		if (!CORS_SAFELISTED_REQUEST_HEADERS.has(n)) return false;
		if (n === "content-type") {
			const mime = String(value).split(";")[0].trim().toLowerCase();
			return CORS_SAFELISTED_CONTENT_TYPES.has(mime);
		}
		return true;
	}

	// needsPreflight reports whether the request escapes the safelist, and names
	// the headers that did so (the preflight has to list them).
	function needsPreflight(method, headers) {
		const m = String(method || "GET").toUpperCase();
		const unsafe = [];
		for (const [k, v] of headers) {
			const n = String(k).toLowerCase();
			// The user agent's own headers are never part of the author's request.
			if (n === "origin" || n === "referer" || n === "user-agent") continue;
			if (!isSafelistedRequestHeader(n, v)) unsafe.push(n);
		}
		const simpleMethod = m === "GET" || m === "HEAD" || m === "POST";
		if (simpleMethod && unsafe.length === 0) return null;
		return { method: m, headers: unsafe.sort() };
	}

	// splitList parses a comma-separated header into a lowercased Set.
	function splitList(value) {
		const out = new Set();
		for (const part of String(value || "").split(",")) {
			const t = part.trim().toLowerCase();
			if (t) out.add(t);
		}
		return out;
	}

	async function runPreflight(url, need, origin) {
		const h = { origin, "access-control-request-method": need.method };
		if (need.headers.length) h["access-control-request-headers"] = need.headers.join(",");
		// Browsers send Accept: */* on a preflight, and the suite's fixture
		// rejects a preflight without it.
		h.accept = "*/*";
		// Through the same normalization the real response gets: the raw host
		// object has no Headers to read the permissions out of.
		const res = trackBodyUsed(await globalThis.__native_fetch(url, { method: "OPTIONS", headers: h, redirect: "error" }));
		if (!(res.status >= 200 && res.status < 300)) {
			throw corsError("preflight responded " + res.status);
		}
		const allowOrigin = res.headers.get("access-control-allow-origin");
		if (allowOrigin !== "*" && allowOrigin !== origin) {
			throw corsError("preflight did not allow " + origin);
		}
		const allowMethods = splitList(res.headers.get("access-control-allow-methods"));
		if (!allowMethods.has("*") && !allowMethods.has(need.method.toLowerCase()) &&
			!(need.method === "GET" || need.method === "HEAD" || need.method === "POST")) {
			throw corsError("preflight did not allow method " + need.method);
		}
		const allowHeaders = splitList(res.headers.get("access-control-allow-headers"));
		if (!allowHeaders.has("*")) {
			for (const n of need.headers) {
				if (!allowHeaders.has(n)) throw corsError("preflight did not allow header " + n);
			}
		}
	}

	// filterCORSResponseHeaders removes what a cors response may not expose: a
	// page sees the safelist plus whatever Access-Control-Expose-Headers names.
	function filterCORSResponseHeaders(res) {
		const exposed = splitList(res.headers.get("access-control-expose-headers"));
		const remove = [];
		for (const [k] of res.headers) {
			const n = String(k).toLowerCase();
			if (CORS_SAFELISTED_RESPONSE_HEADERS.has(n) || exposed.has("*") || exposed.has(n)) continue;
			remove.push(k);
		}
		for (const k of remove) {
			try { res.headers.delete(k); } catch { /* an immutable guard: leave it */ }
		}
		return res;
	}

	// --- cross-origin redirect chain ---------------------------------------
	// A CORS request is re-checked at EVERY hop: each response must permit the
	// current origin, a hop that leaves the origin makes the next request's
	// origin "null", and a redirect to a URL carrying credentials is a network
	// error. Letting the host follow the chain hides all of that — only the
	// final response was ever judged, so ~150 subtests that require a rejection
	// mid-chain saw a resolved fetch.
	const REDIRECT_STATUSES = new Set([301, 302, 303, 307, 308]);
	const KNOWN_REFERRER_POLICIES = new Set([
		"", "no-referrer", "no-referrer-when-downgrade", "same-origin", "origin",
		"strict-origin", "origin-when-cross-origin", "strict-origin-when-cross-origin",
		"unsafe-url",
	]);

	// A filtered response is what the caller sees when it is not allowed to see
	// the real one. The status of 0 and the empty headers are not a placeholder —
	// they are the whole point: an opaque response carries no information about
	// the resource beyond the fact that the request completed.
	function filteredResponse(type, url) {
		const res = new Response(null, { status: 200 });
		for (const [k, v] of [["status", 0], ["statusText", ""], ["ok", false],
			["type", type], ["url", url ?? ""], ["redirected", false]]) {
			try { Object.defineProperty(res, k, { configurable: true, value: v }); } catch { /* ignore */ }
		}
		res.headers = setHeadersGuard(new Headers(), "immutable");
		return res;
	}

	async function followCORSRedirects(url, nInit, headers, envURL, referrerPolicy, explicitReferrer) {
		// "error" and "manual" never follow anything: the first redirect is either
		// a network error or the end of the exchange.
		const mode = String(nInit.__redirect ?? "follow");
		let current = url;
		let origin = envURL.origin;
		let hops = 0;
		// A redirect response may declare its own Referrer-Policy, and the next
		// hop uses THAT. Carrying the caller's policy the whole way down ignored
		// the server's instruction.
		let policy = referrerPolicy;
		// The referrer NARROWS as the chain proceeds and never widens again: once a
		// hop has stripped it to an origin, a later hop with a permissive policy
		// still has only that origin to send. Recomputing from the document URL
		// each time handed the full URL back after it had already been withheld.
		let refSource = envURL;
		for (;;) {
			// The headers are materialized per hop: nInit.headers is only filled
			// in by the single-shot dispatch path, and the Origin changes once a
			// hop has left the origin.
			const hdrObj = {};
			for (const [k, v] of headers) hdrObj[k] = v;
			hdrObj.origin = origin;
			// The referrer is decided per HOP, not once: a policy that gives the
			// full URL to a same-origin peer gives only the origin to the next
			// one. Computing it once sent the first hop's referrer all the way
			// down the chain.
			if (!explicitReferrer) {
				const ref = refSource ? referrerHeaderFor(policy, refSource, new URL(current)) : null;
				if (ref) hdrObj.referer = ref;
				else delete hdrObj.referer;
				try { refSource = ref ? new URL(ref) : null; } catch { refSource = null; }
			}
			const step = { ...nInit, redirect: "manual", headers: hdrObj };
			const res = trackBodyUsed(await globalThis.__native_fetch(current, step));
			const target = new URL(current);
			const crossHop = target.origin !== envURL.origin;
			if (crossHop && !corsAllowsResponse(res, origin)) {
				throw corsError("no Access-Control-Allow-Origin for " + origin + " at " + target.origin);
			}
			if (!REDIRECT_STATUSES.has(res.status)) {
				if (crossHop) filterCORSResponseHeaders(res);
				try { Object.defineProperty(res, "redirected", { configurable: true, value: hops > 0 }); } catch { /* ignore */ }
				if (crossHop) {
					try { Object.defineProperty(res, "type", { configurable: true, value: "cors" }); } catch { /* ignore */ }
				}
				return res;
			}
			if (mode === "error") {
				throw corsError("the response is a redirect and redirect mode is \"error\"");
			}
			if (mode === "manual") {
				// The caller asked to see the redirect itself, and is shown nothing
				// about it: the URL is the one it requested, not the one it was sent to.
				return filteredResponse("opaqueredirect", current);
			}
			const loc = res.headers.get("location");
			if (!loc) return res;
			let next;
			try { next = new URL(loc, current); } catch { throw corsError("redirect to an unparseable URL"); }
			// A redirect may not carry credentials in the URL.
			if (next.username || next.password) throw corsError("redirect to a URL with credentials");
			if (next.protocol !== "http:" && next.protocol !== "https:") {
				throw corsError("redirect to a non-HTTP URL");
			}
			const declared = res.headers.get("referrer-policy");
			if (declared) {
				// A header may list several, most-preferred last; take the last
				// one this implementation knows.
				for (const p of String(declared).split(",").map((x) => x.trim().toLowerCase())) {
					if (KNOWN_REFERRER_POLICIES.has(p)) policy = p;
				}
			}
			hops++;
			if (hops > 20) throw corsError("too many redirects");
			// Once a hop has left the origin the request is no longer same-origin
			// to anything: its origin becomes opaque.
			if (next.origin !== target.origin) origin = "null";
			// 303, and 301/302 on a non-GET, drop the method and body.
			if (res.status === 303 || ((res.status === 301 || res.status === 302) &&
				String(step.method || "GET").toUpperCase() === "POST")) {
				nInit = { ...nInit, method: "GET" };
				delete nInit.body;
			}
			current = next.href;
		}
	}

	globalThis.fetch = function fetch(input, init) {
		// fetch must ALWAYS return a promise: a normalization failure (a malformed
		// headers init, an unsupported ReadableStream body) must reject, not throw
		// synchronously — a sync throw is uncatchable by fetch(...).catch(...).
		try {
			init = init || {};
			const isReq = globalThis.Request && input instanceof globalThis.Request;
			const url = isReq ? input.url : String(input);
			// WHATWG/undici: a URL that carries credentials is rejected (NOT converted
			// to an Authorization header). Only throw when the URL actually parses with
			// a username/password so relative inputs keep their current behavior.
			let parsed = null;
			try { parsed = new URL(url); } catch { parsed = null; }
			if (parsed && (parsed.username || parsed.password)) {
				throw new TypeError("Request cannot be constructed from a URL that includes credentials");
			}
			const headers = setHeadersGuard(new Headers(isReq ? input.headers : undefined), "request");
			if (init.headers !== undefined && init.headers !== null) {
				for (const [k, v] of new Headers(init.headers)) headers.set(k, v);
			}
			// Past this point the user agent is speaking, not the caller, so its own
			// headers are written straight into the map: Origin and Referer are
			// forbidden to the caller precisely because they are set here.
			const uaSet = (k, v) => headers._map.set(k, [String(v)]);
			const nInit = {};
			const method = init.method || (isReq ? input.method : undefined);
			if (method !== undefined) nInit.method = method;
			// Origin and Referer are set by the user agent, not by the caller, so
			// an explicit header of either wins and is left alone.
			const envURL = environmentURL();
			const mode = String(init.mode ?? (isReq ? input.mode : undefined) ?? "cors");
			if (!headers.has("origin")) {
				const origin = originHeaderFor(mode, method, envURL);
				if (origin) uaSet("origin", origin);
			}
			// A blob: URL is served from memory, not the network.
			if (parsed && parsed.protocol === "blob:") {
				const blob = globalThis.__blob_for_object_url(url);
				if (!blob) {
					return Promise.reject(new TypeError("Failed to fetch: no object URL " + url));
				}
				const m = String(method || "GET").toUpperCase();
				// A blob: URL answers only GET. Anything else is a network error, not
				// a 405: there is no server to have refused the method.
				if (m !== "GET") {
					return Promise.reject(new TypeError("Failed to fetch: blob: URLs answer only GET"));
				}
				return blob.arrayBuffer().then((buf) => {
					const res = new Response(buf, {
						status: 200,
						// A synthesized response still has a reason phrase: XHR reports
						// statusText, and "" is not what a 200 says.
						statusText: "OK",
						headers: {
							"content-type": blob.type || "",
							"content-length": String(blob.size),
						},
					});
					// A blob: response is same-origin, so its type is "basic" and its url
					// is the one that was asked for — a Response built by hand reports
					// neither, because it did not come from a fetch.
					for (const [k, v] of [["type", "basic"], ["url", url]]) {
						try { Object.defineProperty(res, k, { configurable: true, value: v }); } catch { /* ignore */ }
					}
					return res;
				});
			}
			// Only a network scheme can be cross-origin. data:, blob: and file:
			// are fetched by their own scheme handler, not through CORS — judging
			// them by origin rejected every data: URL, since its origin is "null".
			const networkScheme = !!parsed && (parsed.protocol === "http:" || parsed.protocol === "https:");
			const crossOrigin = !!(envURL && networkScheme && envURL.origin !== parsed.origin);
			if (mode === "same-origin" && crossOrigin) {
				throw corsError("a same-origin request cannot go to " + parsed.origin);
			}
			const chainPolicy = init.referrerPolicy ?? (isReq ? input.referrerPolicy : undefined);
			const chainExplicitReferrer = headers.has("referer") ||
				((init.referrer ?? (isReq ? input.referrer : undefined)) !== undefined &&
					(init.referrer ?? (isReq ? input.referrer : undefined)) !== "about:client");
			if (!headers.has("referer")) {
				const explicit = init.referrer ?? (isReq ? input.referrer : undefined);
				const policy = chainPolicy;
				let ref = null;
				if (explicit !== undefined && explicit !== "about:client") {
					ref = null;
					if (String(explicit) !== "") {
						try {
							const u = new URL(String(explicit), envURL ? envURL.href : undefined);
							u.username = ""; u.password = ""; u.hash = "";
							ref = u.href;
						} catch { ref = String(explicit); }
					}
				} else {
					ref = referrerHeaderFor(policy, envURL, parsed);
				}
				if (ref) uaSet("referer", ref);
			}
			if (init.redirect !== undefined && init.redirect !== null) nInit.redirect = String(init.redirect);
			// The cache mode reaches the host, which is where the cache lives.
			if (init.cache !== undefined && init.cache !== null) nInit.cache = String(init.cache);
			if (init.cache !== undefined && init.cache !== null) nInit.cache = String(init.cache);
			const signal = init.signal || (isReq ? input.signal : undefined);
			// A signal already aborted at call time rejects immediately with its reason
			// (a DOMException "AbortError" by default) — the host is never called.
			if (signal && signal.aborted) {
				return Promise.reject(signal.reason ?? new DOMException("The operation was aborted", "AbortError"));
			}
			// Wire an abort listener to the in-flight host request: a unique id lets
			// the host cancel the Go request context when 'abort' fires. The listener
			// is removed when the fetch settles so a reused signal never accumulates
			// dead closures (and no host op runs against a finished request).
			let onAbort = null;
			if (signal) {
				const abortId = "f" + (globalThis.__fetch_abort_seq = (globalThis.__fetch_abort_seq || 0) + 1);
				nInit.__abortId = abortId;
				onAbort = () => { try { globalThis.__native_fetch_abort(abortId); } catch { /* already settled */ } };
			}
			const cleanup = () => { if (signal && onAbort) signal.removeEventListener("abort", onAbort); };
			let bodyInit = init.body;
			if (bodyInit === undefined && isReq) bodyInit = input._bodyStream ?? input._body;
			const dispatch = () => {
				const hdrObj = {};
				for (const [k, v] of headers) hdrObj[k] = v;
				nInit.headers = hdrObj;
				if (signal && onAbort) signal.addEventListener("abort", onAbort);
				return globalThis.__native_fetch(url, nInit).then(
					(res) => {
						cleanup();
						const tracked = trackBodyUsed(res);
						if (crossOrigin && mode === "cors") {
							if (!corsAllowsResponse(tracked, envURL.origin)) {
								throw corsError("no Access-Control-Allow-Origin for " + envURL.origin);
							}
							filterCORSResponseHeaders(tracked);
						}
						if (crossOrigin && mode === "no-cors") return opaqueResponse(tracked);
						if (crossOrigin) {
							try { Object.defineProperty(tracked, "type", { configurable: true, value: "cors" }); } catch { /* ignore */ }
						}
						return tracked;
					},
					(err) => {
						cleanup();
						// A mid-flight abort surfaces from the host as a context-cancel
						// error string; reject with the signal's reason (a proper
						// DOMException "AbortError") instead of that opaque string.
						if (signal && signal.aborted) throw (signal.reason ?? new DOMException("The operation was aborted", "AbortError"));
						throw asFetchError(err);
					},
				);
			};
			// A cross-origin request outside the safelist is preflighted first, and
			// only dispatched if the server allows it. The headers must be final
			// by then — a body contributes a Content-Type, and that is one of the
			// things the safelist is about.
			const redirectMode = nInit.redirect || "follow";
			// The chain is driven in the guest whenever the environment has an
			// origin to judge hops against — not only when the FIRST hop is
			// already cross-origin. A same-origin request that redirects to
			// another origin needs exactly the same per-hop checks, and letting
			// the host follow it hid them: a redirect to a URL carrying
			// credentials, or one whose new origin does not permit us, went
			// through unnoticed.
			// The chain is driven in the guest for EVERY redirect mode, not just
			// "follow": "error" and "manual" are answers about the redirect itself,
			// and only the guest knows it saw one.
			nInit.__redirect = redirectMode;
			const chained = !!envURL && networkScheme && mode !== "no-cors";
			const send = () => {
				const go = chained
					? () => {
						if (signal && onAbort) signal.addEventListener("abort", onAbort);
						return followCORSRedirects(url, nInit, headers, envURL, chainPolicy, chainExplicitReferrer).then(
							(res) => { cleanup(); return res; },
							(err) => {
								cleanup();
								if (signal && signal.aborted) throw (signal.reason ?? new DOMException("The operation was aborted", "AbortError"));
								throw asFetchError(err);
							});
					}
					: dispatch;
				const need = crossOrigin && mode === "cors" ? needsPreflight(method, headers) : null;
				if (!need) return go();
				return runPreflight(url, need, envURL.origin).then(go);
			};
			// A ReadableStream request body is drained to bytes before dispatch (the
			// native path sends a buffered body; no chunked uploads yet).
			if (bodyInit instanceof ReadableStream) {
				return drainBodyStream(bodyInit).then((bytes) => {
					if (bytes.length > 0) nInit.body = bytes;
					return send();
				});
			}
			if (bodyInit !== undefined && bodyInit !== null) {
				const { bytes, contentType } = normalizeBody(bodyInit);
				if (bytes !== null) nInit.body = bytes;
				if (contentType && !headers.has("content-type")) headers.set("content-type", contentType);
			}
			return send();
		} catch (e) {
			return Promise.reject(e);
		}
	};

	// trackBodyUsed makes response.bodyUsed reflect consumption and turns a second
	// body read into a rejected TypeError at the JS layer (the host readAll is the
	// backstop for the stream path). The native Response is Go-built, so we wrap its
	// body consumers here rather than mutate a freed host handle.
	function trackBodyUsed(res) {
		if (!res || typeof res !== "object") return res;
		// The host ships response headers as raw name/value pairs (__headerEntries),
		// each Set-Cookie kept separate. Rebuild a real Headers instance so
		// forEach/set/append/delete/keys/values/iteration and new Headers(res.headers)
		// all work — the Go side no longer ships a headers object.
		if (Array.isArray(res.__headerEntries)) {
			try { res.headers = new Headers(res.__headerEntries); } catch { /* leave undefined */ }
		}
		let used = false;
		for (const m of ["arrayBuffer", "bytes", "blob", "text", "json", "formData"]) {
			const orig = res[m];
			if (typeof orig !== "function") continue;
			try {
				Object.defineProperty(res, m, {
					configurable: true, writable: true,
					value: function (...a) {
						if (used) return Promise.reject(new TypeError("Body has already been consumed."));
						used = true;
						return orig.apply(this, a);
					},
				});
			} catch { /* non-configurable native method: rely on the host backstop */ }
		}
		// The native Response omits blob()/formData()/type — add them, delegating to
		// the used-tracked bytes()/text() so the single-read guard still applies.
		const ct = res.headers && res.headers.get ? (res.headers.get("content-type") || "") : "";
		const define = (name, value) => { try { Object.defineProperty(res, name, { configurable: true, writable: true, value }); } catch { /* ignore */ } };
		if (typeof res.bytes === "function" && typeof res.blob !== "function") {
			define("blob", function () { return this.bytes().then((b) => new Blob([b], ct ? { type: ct } : undefined)); });
		}
		if (typeof res.text === "function" && typeof res.formData !== "function") {
			define("formData", function () {
				if (ct.startsWith("multipart/form-data")) return this.bytes().then((b) => parseMultipartForm(b, ct));
				return this.text().then((t) => { const fd = new FormData(); for (const [k, v] of new URLSearchParams(t)) fd.append(k, v); return fd; });
			});
		}
		if (res.type === undefined) { try { Object.defineProperty(res, "type", { configurable: true, value: "basic" }); } catch { /* ignore */ } }
		try {
			Object.defineProperty(res, "bodyUsed", { configurable: true, get: () => used });
		} catch { /* leave the native field as-is */ }
		return res;
	}

	// opaqueResponse is what a no-cors cross-origin fetch hands back: status 0,
	// no headers, an empty body and type "opaque". The bytes were fetched, but
	// the spec says the page may not see them.
	function opaqueResponse(res) {
		const empty = new Response(null, { status: 200 });
		for (const [k] of [...empty.headers]) empty.headers.delete(k);
		for (const [name, value] of [["type", "opaque"], ["status", 0], ["ok", false], ["statusText", ""], ["url", ""]]) {
			try { Object.defineProperty(empty, name, { configurable: true, value }); } catch { /* ignore */ }
		}
		return empty;
	}

	// parseMultipartForm parses a multipart/form-data body into a FormData. Bytes
	// are handled via latin1 so binary file parts survive intact.
	function parseMultipartForm(bytes, contentType) {
		const fd = new FormData();
		const m = /boundary=("?)([^";]+)\1/i.exec(contentType);
		if (!m) return fd;
		const boundary = "--" + m[2];
		// A TRUE 1:1 latin1 mapping — NOT TextDecoder("latin1"), which is the
		// windows-1252 decoder here and would mangle bytes 0x80-0x9F of a binary
		// file part. The following string ops (boundary split, header regexes) are
		// all ASCII, so an ISO-8859-1 string round-trips the bytes exactly.
		let text = "";
		for (let i = 0; i < bytes.length; i++) text += String.fromCharCode(bytes[i]);
		for (const part of text.split(boundary)) {
			if (!part || part === "--\r\n" || part.trim() === "" || part.trim() === "--") continue;
			const headerEnd = part.indexOf("\r\n\r\n");
			if (headerEnd < 0) continue;
			const headerText = part.slice(2, headerEnd);
			const bodyText = part.slice(headerEnd + 4, part.length - 2); // drop the trailing CRLF
			const nameM = /name="([^"]*)"/i.exec(headerText);
			if (!nameM) continue;
			const fileM = /filename="([^"]*)"/i.exec(headerText);
			if (fileM) {
				const ctM = /content-type:\s*([^\r\n]+)/i.exec(headerText);
				const bodyBytes = Uint8Array.from(bodyText, (c) => c.charCodeAt(0) & 0xff);
				fd.append(nameM[1], new File([bodyBytes], fileM[1], ctM ? { type: ctM[1].trim() } : undefined));
			} else {
				// A text field's value is UTF-8 — decode the raw bytes, not the
				// latin1 string (else non-ASCII values come through as mojibake).
				fd.append(nameM[1], new TextDecoder().decode(Uint8Array.from(bodyText, (c) => c.charCodeAt(0) & 0xff)));
			}
		}
		return fd;
	}
})();
