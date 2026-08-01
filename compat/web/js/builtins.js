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

	// The legacy code constants. Six of them name conditions no modern API
	// raises — DOMSTRING_SIZE_ERR, NO_DATA_ALLOWED_ERR, NO_MODIFICATION_ALLOWED,
	// INUSE_ATTRIBUTE_ERR, VALIDATION_ERR, URL_MISMATCH_ERR — so they have no
	// entry in the name table above and would have been missing entirely, though
	// the interface still defines them. They are listed here by their numbers,
	// which is all they have ever been.
	const DOM_EXCEPTION_LEGACY_CODES = {
		INDEX_SIZE_ERR: 1, DOMSTRING_SIZE_ERR: 2, HIERARCHY_REQUEST_ERR: 3,
		WRONG_DOCUMENT_ERR: 4, INVALID_CHARACTER_ERR: 5, NO_DATA_ALLOWED_ERR: 6,
		NO_MODIFICATION_ALLOWED_ERR: 7, NOT_FOUND_ERR: 8, NOT_SUPPORTED_ERR: 9,
		INUSE_ATTRIBUTE_ERR: 10, INVALID_STATE_ERR: 11, SYNTAX_ERR: 12,
		INVALID_MODIFICATION_ERR: 13, NAMESPACE_ERR: 14, INVALID_ACCESS_ERR: 15,
		VALIDATION_ERR: 16, TYPE_MISMATCH_ERR: 17, SECURITY_ERR: 18,
		NETWORK_ERR: 19, ABORT_ERR: 20, URL_MISMATCH_ERR: 21,
		QUOTA_EXCEEDED_ERR: 22, TIMEOUT_ERR: 23, INVALID_NODE_TYPE_ERR: 24,
		DATA_CLONE_ERR: 25,
	};

	class DOMException extends Error {
		constructor(message = "", name = "Error") {
			super(message);
			// name and message are IDL attributes of DOMException, so they are read
			// from the PROTOTYPE over slots — not own properties, which is where
			// Error puts them and where the interface does not.
			Object.defineProperty(this, "_name", { value: String(name), writable: true });
			Object.defineProperty(this, "_message", { value: String(message), writable: true });
		}
		get name() { return this._name; }
		get message() { return this._message; }
		get code() { return DOM_EXCEPTION_CODES[this._name] ?? 0; }
	}
	// The constants live on both the interface and its prototype, as they do for
	// every Web IDL interface that has them.
	for (const [legacy, code] of Object.entries(DOM_EXCEPTION_LEGACY_CODES)) {
		for (const target of [DOMException, DOMException.prototype]) {
			Object.defineProperty(target, legacy, { value: code, enumerable: true });
		}
	}
	globalThis.DOMException ??= DOMException;

	// QuotaExceededError became an interface of its own, deriving from
	// DOMException and carrying the two numbers that make the failure
	// actionable: how much was asked for, and how much there was. Both are null
	// when the thrower did not say, which is the common case and the reason they
	// are nullable at all.
	class QuotaExceededError extends DOMException {
		constructor(message = "", options = {}) {
			super(message, "QuotaExceededError");
			if (options !== null && options !== undefined && typeof options !== "object") {
				throw new TypeError("QuotaExceededError: options must be an object");
			}
			const o = options || {};
			const num = (v, what) => {
				if (v === undefined || v === null) return null;
				const n = Number(v);
				if (!Number.isFinite(n)) throw new TypeError(`QuotaExceededError: ${what} must be a finite number`);
				return n;
			};
			Object.defineProperty(this, "_quota", { value: num(o.quota, "quota"), writable: true });
			Object.defineProperty(this, "_requested", { value: num(o.requested, "requested"), writable: true });
		}
		get quota() { return this._quota; }
		get requested() { return this._requested; }
	}
	globalThis.QuotaExceededError ??= QuotaExceededError;

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
	// The console namespace, with every operation the standard lists. The
	// grouping, counting and timing ones keep real state — a count that always
	// said 1, or a timer that reported nothing, would be a worse answer than the
	// absence they replace — and the indent that group() establishes is applied
	// to what is actually written.
	const groupIndent = { depth: 0 };
	const counters = new Map();
	const timers = new Map();
	const write = (level, args) => {
		if (groupIndent.depth > 0) consoleWrite(level, ["  ".repeat(groupIndent.depth) + fmtFirst(args), ...args.slice(1)]);
		else consoleWrite(level, args);
	};
	// The indent is prepended to the first argument so a format string keeps its
	// position relative to the arguments that fill it.
	const fmtFirst = (args) => (args.length === 0 ? "" : args[0]);

	globalThis.console = {
		log: (...a) => write(0, a),
		info: (...a) => write(0, a),
		debug: (...a) => write(0, a),
		warn: (...a) => write(1, a),
		error: (...a) => write(1, a),
		trace: (...a) => write(1, ["Trace:", ...a]),
		dir: (...a) => write(0, a),
		dirxml: (...a) => write(0, a),
		table: (...a) => write(0, a),
		assert: (cond, ...a) => { if (!cond) write(1, ["Assertion failed:", ...a]); },
		clear: () => { groupIndent.depth = 0; },
		group: (...a) => { if (a.length) write(0, a); groupIndent.depth++; },
		groupCollapsed: (...a) => { if (a.length) write(0, a); groupIndent.depth++; },
		groupEnd: () => { if (groupIndent.depth > 0) groupIndent.depth--; },
		count: (label = "default") => {
			const key = String(label);
			const n = (counters.get(key) ?? 0) + 1;
			counters.set(key, n);
			write(0, [`${key}: ${n}`]);
		},
		countReset: (label = "default") => { counters.set(String(label), 0); },
		time: (label = "default") => {
			const key = String(label);
			if (timers.has(key)) {
				write(1, [`Timer "${key}" already exists`]);
				return;
			}
			timers.set(key, performance.now());
		},
		timeLog: (label = "default", ...a) => {
			const key = String(label);
			if (!timers.has(key)) { write(1, [`Timer "${key}" does not exist`]); return; }
			write(0, [`${key}: ${(performance.now() - timers.get(key)).toFixed(3)}ms`, ...a]);
		},
		timeEnd: (label = "default") => {
			const key = String(label);
			if (!timers.has(key)) { write(1, [`Timer "${key}" does not exist`]); return; }
			write(0, [`${key}: ${(performance.now() - timers.get(key)).toFixed(3)}ms`]);
			timers.delete(key);
		},
	};
	Object.defineProperty(globalThis.console, Symbol.toStringTag, { value: "console", configurable: true });

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
	// The Encoding Standard strips exactly five characters from a label — tab,
	// LF, FF, CR and space — and nothing else. String.prototype.trim strips every
	// Unicode whitespace character, so a label wrapped in U+00A0, U+2028, U+2029
	// or U+000B was being ACCEPTED where the standard requires a RangeError. That
	// is not a corner: the suite generates it for every label of every encoding,
	// which is 2,967 subtests.
	const stripASCIIWhitespace = (s) => s.replace(/^[\t\n\f\r ]+|[\t\n\f\r ]+$/g, "");

	// Every label in the Encoding Standard is drawn from this set, so a stripped
	// label containing anything else names no encoding. The check has to happen
	// HERE rather than at the host table: golang.org/x/text/encoding/htmlindex
	// does its own fuzzy matching and accepts a label wrapped in U+00A0 or
	// U+2028, which the standard does not. This is not a heuristic — it is the
	// alphabet of the label list itself.
	const LABEL_CHARS = /^[a-z0-9_:.()-]*$/;

	globalThis.TextDecoder = class TextDecoder {
		constructor(label = "utf-8", options = {}) {
			if (options === null || options === undefined) options = {};
			const normalized = stripASCIIWhitespace(String(label)).toLowerCase();
			const enc = DECODER_LABELS[normalized];
			if (enc) {
				this._enc = enc;
				this._name = enc === "utf8" ? "utf-8" : enc === "utf16le" ? "utf-16le" : "windows-1252";
			} else {
				// Every other encoding in the standard lives in a table on the
				// host: Shift_JIS, GBK, Big5, EUC-KR, ISO-2022-JP, the ISO-8859
				// and windows-125x families, utf-16be. The label is validated
				// here so an unknown one is still a RangeError.
				if (!LABEL_CHARS.test(normalized)) {
					throw new RangeError(`TextDecoder: unsupported encoding ${label}`);
				}
				const name = ops.text_encoding_name(normalized);
				if (!name) throw new RangeError(`TextDecoder: unsupported encoding ${label}`);
				this._enc = "host";
				this._name = name;
				this._label = normalized;
			}
			// fatal and ignoreBOM are IDL attributes, so they are prototype
			// accessors over slots rather than own properties written here.
			this._fatal = !!options.fatal;
			this._ignoreBOM = !!options.ignoreBOM;
		}
		get encoding() { return this._name; }
		get fatal() { return this._fatal; }
		get ignoreBOM() { return this._ignoreBOM; }
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
				const r = ops.text_decode(this._label, bytes, this._fatal);
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
			return utf8Decode(bytes, this._fatal);
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

	// Event's members are IDL ATTRIBUTES: prototype accessors over slots the
	// constructor DEFINES rather than assigns. Both halves matter. As own data
	// properties they were writable and absent from Event.prototype, which is
	// where a caller looks for them; and a plain assignment walks the prototype
	// chain, so a page that has put a getter named `type` on Object.prototype —
	// which is exactly what the tests for safe object creation do — made
	// constructing any event throw.
	globalThis.Event ??= class Event {
		constructor(type, init = {}) {
			if (arguments.length < 1) throw new TypeError("Event: a type is required");
			const opts = init === null || init === undefined ? {} : init;
			Object.defineProperties(this, {
				_type: { value: String(type), writable: true },
				_target: { value: null, writable: true },
				_currentTarget: { value: null, writable: true },
				_defaultPrevented: { value: false, writable: true },
				// Writable because the legacy init*Event() methods (initEvent,
				// initStorageEvent) re-initialize an already-constructed event.
				_bubbles: { value: Boolean(opts.bubbles), writable: true },
				_cancelable: { value: Boolean(opts.cancelable), writable: true },
				_composed: { value: Boolean(opts.composed) },
				_trusted: { value: false, writable: true },
				_stopImmediate: { value: false, writable: true },
				_stopped: { value: false, writable: true },
				// The moment the event was created, on the same clock
				// performance.now() reads — which is what timeStamp means.
				_timeStamp: {
					value: globalThis.performance && typeof globalThis.performance.now === "function"
						? globalThis.performance.now() : 0,
					writable: true,
				},
			});
		}
		get type() { return this._type; }
		get target() { return this._target; }
		get srcElement() { return this._target; }
		get currentTarget() { return this._currentTarget; }
		get eventPhase() { return this._currentTarget === null ? 0 : 2; }
		get bubbles() { return this._bubbles; }
		get cancelable() { return this._cancelable; }
		get composed() { return this._composed; }
		get defaultPrevented() { return this._defaultPrevented; }
		get timeStamp() { return this._timeStamp; }
		get returnValue() { return !this._defaultPrevented; }
		set returnValue(value) { if (!value) this.preventDefault(); }
		get cancelBubble() { return this._stopped; }
		set cancelBubble(value) { if (value) this.stopPropagation(); }
		// isTrusted distinguishes an event the RUNTIME fired (a message arriving,
		// a timer's timeout) from one script fired through dispatchEvent. Script
		// can never mint a trusted event: dispatchEvent clears the flag, and only
		// the internal __dispatch_trusted helper below sets it.
		get isTrusted() { return this._trusted; }
		composedPath() { return this._currentTarget === null ? [] : [this._currentTarget]; }
		preventDefault() { if (this._cancelable) this._defaultPrevented = true; }
		stopPropagation() { this._stopped = true; }
		stopImmediatePropagation() { this._stopped = true; this._stopImmediate = true; }
	};
	for (const [name, value] of [["NONE", 0], ["CAPTURING_PHASE", 1], ["AT_TARGET", 2], ["BUBBLING_PHASE", 3]]) {
		for (const target of [globalThis.Event, globalThis.Event.prototype]) {
			Object.defineProperty(target, name, { value, enumerable: true });
		}
	}
	Object.defineProperty(globalThis.Event.prototype, Symbol.toStringTag, {
		value: "Event", configurable: true,
	});

	// __dispatch_trusted(target, event): dispatch as the user agent, which is the
	// one caller allowed to leave isTrusted true. Host-driven surfaces
	// (EventSource, WebSocket, XHR) route their events through this.
	Object.defineProperty(globalThis, "__dispatch_trusted", {
		value(target, event) {
			event._trusted = true;
			return target.dispatchEvent(event, { __keepTrusted: true });
		},
		writable: true, configurable: true, enumerable: false,
	});

	globalThis.EventTarget ??= class EventTarget {
		constructor() { this._listeners = new Map(); }
		// A listener is identified by its callback AND its capture flag, and it is
		// added only once for that pair. `passive` is accepted and recorded: there
		// is no default action to suppress here, but a caller feature-detects it by
		// whether the option is READ, so accepting it silently is the difference
		// between "supported" and "ignored".
		addEventListener(type, callback, options = undefined) {
			if (arguments.length < 2) throw new TypeError("addEventListener requires 2 arguments");
			if (callback === null || callback === undefined) return;
			type = String(type);
			const opts = normalizeListenerOptions(options, "addEventListener");
			// WHATWG: an already-aborted signal means the listener is never added.
			if (opts.signal && opts.signal.aborted) return;
			let list = this._listeners.get(type);
			if (!list) this._listeners.set(type, list = []);
			if (list.some((l) => l.callback === callback && l.capture === opts.capture)) return;
			const entry = {
				callback, capture: opts.capture, once: opts.once, passive: opts.passive,
				removed: false, signalCleanup: null,
			};
			if (opts.signal && typeof opts.signal.addEventListener === "function") {
				// Aborting the signal removes THIS listener; if the listener is
				// removed normally (or fires with once), the abort hook is
				// detached too so the signal doesn't accumulate dead closures.
				const signal = opts.signal;
				const onAbort = () => this.removeEventListener(type, callback, { capture: opts.capture });
				signal.addEventListener("abort", onAbort);
				entry.signalCleanup = () => signal.removeEventListener("abort", onAbort);
			}
			list.push(entry);
		}
		removeEventListener(type, callback, options = undefined) {
			if (arguments.length < 2) throw new TypeError("removeEventListener requires 2 arguments");
			const opts = normalizeListenerOptions(options, "removeEventListener");
			const list = this._listeners.get(String(type));
			if (!list) return;
			const i = list.findIndex((l) => l.callback === callback && l.capture === opts.capture);
			if (i >= 0) {
				const [entry] = list.splice(i, 1);
				// Marked as well as spliced: a dispatch already walking a COPY of the
				// list must not call a listener that has since been removed, which is
				// the whole point of removing one from inside another.
				entry.removed = true;
				if (entry.signalCleanup) entry.signalCleanup();
			}
		}
		dispatchEvent(event, opts) {
			if (arguments.length < 1) throw new TypeError("dispatchEvent requires 1 argument");
			if (!(event instanceof Event)) throw new TypeError("dispatchEvent: the argument must be an Event");
			// A script-dispatched event is never trusted, however it was minted.
			// The one exception is the internal trusted-dispatch helper.
			if (!(opts && opts.__keepTrusted)) event._trusted = false;
			event._target = event._currentTarget = this;
			event._stopped = false;
			event._stopImmediate = false;
			const list = this._listeners.get(event.type);
			if (list) {
				for (const l of [...list]) {
					if (l.removed) continue;
					if (l.once) this.removeEventListener(event.type, l.callback, { capture: l.capture });
					if (typeof l.callback === "function") l.callback.call(this, event);
					else if (l.callback && typeof l.callback.handleEvent === "function") l.callback.handleEvent(event);
					if (event._stopImmediate) break;
				}
			}
			event._currentTarget = null;
			return !event.defaultPrevented;
		}
	};

	// An options argument is either a boolean (the capture flag) or a dictionary.
	// `signal` is an AbortSignal and NOT nullable, so null is a TypeError rather
	// than "no signal" — a caller that passes null has made a mistake.
	function normalizeListenerOptions(options, who) {
		if (typeof options === "boolean") return { capture: options, once: false, passive: false, signal: undefined };
		if (options === null || options === undefined) return { capture: false, once: false, passive: false, signal: undefined };
		if (typeof options !== "object" && typeof options !== "function") {
			return { capture: Boolean(options), once: false, passive: false, signal: undefined };
		}
		const signal = options.signal;
		if (signal !== undefined && !(typeof AbortSignal === "function" && signal instanceof AbortSignal)) {
			throw new TypeError(`${who}: options.signal is not an AbortSignal`);
		}
		return {
			capture: Boolean(options.capture),
			once: Boolean(options.once),
			passive: Boolean(options.passive),
			signal,
		};
	}

	// globalThis as an event target. The web dispatches host-originated events
	// here rather than at an object the guest made — `unhandledrejection` below
	// is one, and it is why the delegation exists at all.
	const globalTarget = new EventTarget();
	globalThis.addEventListener ??= (...a) => globalTarget.addEventListener(...a);
	globalThis.removeEventListener ??= (...a) => globalTarget.removeEventListener(...a);
	globalThis.dispatchEvent ??= (e) => globalTarget.dispatchEvent(e);

	// The global event handler attributes. ECMA-429 requires all three to exist,
	// and they must behave as event handler attributes rather than as plain
	// properties the dispatcher happens to look at: reading gives the current
	// handler or null, assigning a non-callable clears it, and the handler runs as
	// a listener so ordering with addEventListener is the one the web defines.
	//
	// `onerror` is the exception in two ways, both of them in HTML's event handler
	// processing algorithm and neither guessable. On a global scope it is invoked
	// with (message, filename, lineno, colno, error) rather than with the event —
	// but only when the event IS an ErrorEvent — and its return value is inverted:
	// returning TRUE cancels, where every other handler cancels by returning false.
	const globalHandlers = {}; // type -> the handler the attribute reports
	const globalWrappers = {}; // type -> the listener actually registered
	for (const type of ["error", "unhandledrejection", "rejectionhandled"]) {
		Object.defineProperty(globalThis, "on" + type, {
			get() { return globalHandlers[type] ?? null; },
			set(fn) {
				if (globalWrappers[type]) globalTarget.removeEventListener(type, globalWrappers[type]);
				globalHandlers[type] = typeof fn === "function" ? fn : null;
				globalWrappers[type] = null;
				if (!globalHandlers[type]) return;
				const handler = globalHandlers[type];
				globalWrappers[type] = type !== "error" ? handler : (event) => {
					let result;
					if (event instanceof globalThis.ErrorEvent) {
						result = handler.call(globalThis, event.message, event.filename,
							event.lineno, event.colno, event.error);
					} else {
						result = handler.call(globalThis, event);
					}
					if (result === true) event.preventDefault();
				};
				globalTarget.addEventListener(type, globalWrappers[type]);
			},
			configurable: true, enumerable: true,
		});
	}

	// `self` is how a worker — and every .any.js test — spells the global. It is
	// the global itself, not a copy.
	globalThis.self ??= globalThis;

	// A server-side runtime is a secure context: there is no transport between
	// the code and itself to be insecure. A harness that loads code from an
	// http origin overrides this to false, and removes the [SecureContext]-only
	// surface, which is exactly what a browser's exposure gate would have done.
	globalThis.isSecureContext ??= true;

	// navigator.userAgent is how application code identifies the runtime it is on.
	// ECMA-429 requires it to be a single opaque product token, with no version
	// and no comment, so that nothing tries to parse it.
	// navigator's members are IDL attributes, and an IDL attribute lives on the
	// PROTOTYPE — the object itself has no own properties, which testharness's
	// assert_idl_attribute checks.
	globalThis.navigator ??= Object.create(Object.create(Object.prototype, {
		userAgent: { get: () => "go-spidermonkey", enumerable: true, configurable: true },
	}));

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
		// onunhandledrejection is registered AS a listener (see the handler
		// attributes above), so dispatching once reaches both it and anything
		// added with addEventListener — calling it separately would run it twice.
		globalTarget.dispatchEvent(ev);
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
			// The timer KEEPS THE LOOP ALIVE, like any other. It used to be unref'd
			// so that a handler holding a long timeout could not delay a response —
			// but an abort that may or may not happen depending on whether the loop
			// had other work is worse than a late one, and it is not what the signal
			// promises. It showed as a test that hung: the loop went idle before the
			// 5 ms timeout could fire, so the abort never arrived.
			setTimeout(() => abortSignal(s, new DOMException("The operation timed out", "TimeoutError")), ms);
			return s;
		}
		// AbortSignal.any([...]) — aborts as soon as ANY source signal aborts (the
		// standard "combine a user abort with a timeout" fetch pattern). When one
		// fires, the listeners on the OTHER sources are removed, so a reused
		// long-lived source signal doesn't accumulate a listener per any() call.
		static any(signals) {
			const s = new AbortSignal();
			const settle = (reason) => abortSignal(s, reason);
			for (const src of signals) {
				if (src.aborted) { settle(src.reason); break; }
				// An abort ALGORITHM, not a listener: a dependent signal is aborted
				// as part of aborting its source, before either one's event fires.
				addAbortAlgorithm(src, () => settle(src.reason));
			}
			return s;
		}
	}
	// addAbortAlgorithm registers a runtime reaction to a signal's abort. It is
	// internal: script has addEventListener, and the difference between the two
	// is exactly when they run.
	function addAbortAlgorithm(signal, fn) {
		if (signal.aborted) { fn(signal.reason); return; }
		if (!signal._abortAlgorithms) {
			Object.defineProperty(signal, "_abortAlgorithms", { value: [], configurable: true });
		}
		signal._abortAlgorithms.push(fn);
	}
	Object.defineProperty(globalThis, Symbol.for("go-spidermonkey.addAbortAlgorithm"), {
		value: addAbortAlgorithm, configurable: true,
	});

	function abortSignal(signal, reason) {
		if (signal.aborted) return;
		signal.aborted = true;
		signal.reason = reason !== undefined ? reason : new DOMException("The operation was aborted", "AbortError");
		// The signal's ABORT ALGORITHMS run before the event. They are the
		// runtime's own reactions — a subscription closing, a dependent signal
		// following — and they must complete before script sees the abort, so a
		// listener finds the world already torn down. Registering them as
		// listeners instead put them in the queue alongside script's, where the
		// order depended on who subscribed first.
		if (signal._abortAlgorithms) {
			for (const fn of signal._abortAlgorithms.splice(0)) {
				try { fn(signal.reason); } catch (e) { globalThis.reportError(e); }
			}
		}
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

	// Performance is a class rather than a bare object because ECMA-429 requires
	// the INTERFACE to be exposed, not just the instance: `performance instanceof
	// Performance` is what a caller checks, and an object literal cannot answer it.
	// The User Timing members are added to the prototype further down.
	// Performance derives from EventTarget: the timeline dispatches events, and
	// the inheritance is directly observable through the prototype chain.
	class Performance extends EventTarget {
		constructor() { super(); this._timeOrigin = Date.now(); }
		get timeOrigin() { return this._timeOrigin; }
		now() { return ops.perf_now(); }
		toJSON() { return { timeOrigin: this.timeOrigin }; }
	}
	Object.defineProperty(Performance.prototype, Symbol.toStringTag, { value: "Performance", configurable: true });
	globalThis.Performance ??= Performance;
	globalThis.performance ??= new Performance();

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

		// Every member here is an IDL attribute, so each lives on the prototype
		// over a slot. `detail` in particular: it belongs to PerformanceMark and
		// PerformanceMeasure and must be found in the PROTOTYPE chain, which an
		// own property set in a constructor never is — and it is null, not absent,
		// when nothing was supplied.
		class PerformanceEntry {
			constructor(name, entryType, startTime, duration) {
				this._name = name;
				this._entryType = entryType;
				this._startTime = startTime;
				this._duration = duration;
			}
			get name() { return this._name; }
			get entryType() { return this._entryType; }
			get startTime() { return this._startTime; }
			get duration() { return this._duration; }
			toJSON() {
				const o = { name: this.name, entryType: this.entryType, startTime: this.startTime, duration: this.duration };
				if ("detail" in this) o.detail = this.detail;
				return o;
			}
		}
		class PerformanceMark extends PerformanceEntry {
			constructor(name, options) {
				if (options !== null && options !== undefined && typeof options !== "object") {
					throw new TypeError("PerformanceMark: options must be an object");
				}
				const o = options || {};
				if (o.startTime !== undefined) {
					const t = Number(o.startTime);
					if (Number.isNaN(t)) throw new TypeError("PerformanceMark: startTime must be a number");
					if (t < 0) throw new TypeError("PerformanceMark: startTime must not be negative");
				}
				super(String(name), "mark", o.startTime != null ? Number(o.startTime) : perfNow(), 0);
				// detail is structured-CLONED at construction, so a later mutation of
				// what was passed is not observable, and a value that cannot be cloned
				// is reported here rather than at some later read.
				this._detail = o.detail === undefined ? null : structuredClone(o.detail);
			}
			get detail() { return this._detail; }
		}
		class PerformanceMeasure extends PerformanceEntry {
			constructor(name, startTime, duration, detail) {
				super(String(name), "measure", startTime, duration);
				this._detail = detail === undefined ? null : structuredClone(detail);
			}
			get detail() { return this._detail; }
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

		// The User Timing members go on the PROTOTYPE when performance is a
		// Performance instance, because that is where an interface's operations
		// live; a plain object (a host that installed its own performance) still
		// gets them as own properties, which is all it can have.
		const perfTarget = Object.getPrototypeOf(perf) === Object.prototype ? perf : Object.getPrototypeOf(perf);
		Object.assign(perfTarget, {
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

	// Crypto and SubtleCrypto are interfaces ECMA-429 requires on the global, so
	// `crypto` is an instance rather than an object literal. SubtleCrypto's
	// operations are installed onto its prototype by subtle.js, which is where
	// they are implemented; the class is declared here so `crypto.subtle` has
	// something to be an instance OF before that runs.
	// Neither is constructible from script: `crypto` is the only Crypto there is
	// and `crypto.subtle` the only SubtleCrypto, so a constructor call is a
	// TypeError. The internal token is how this file still makes the two it needs.
	const CRYPTO_INTERNAL = Symbol("Crypto.internal");

	class SubtleCrypto {
		// Rest parameters, so the interface object's `length` is the zero the IDL
		// declares rather than the one the internal token would add.
		constructor(...args) {
			if (args[0] !== CRYPTO_INTERNAL) throw new TypeError("Illegal constructor");
		}
	}
	Object.defineProperty(SubtleCrypto.prototype, Symbol.toStringTag, { value: "SubtleCrypto", configurable: true });
	globalThis.SubtleCrypto ??= SubtleCrypto;

	// getRandomValues and randomUUID are OPERATIONS, so they live on the
	// prototype. As own properties of the one instance they were invisible to
	// anything that asks the interface what it offers — and unreachable through
	// Crypto.prototype, which is where a caller looks for them.
	class Crypto {
		constructor(...args) {
			if (args[0] !== CRYPTO_INTERNAL) throw new TypeError("Illegal constructor");
			this._subtle = new globalThis.SubtleCrypto(CRYPTO_INTERNAL);
		}
		get subtle() { return this._subtle; }
		getRandomValues(array) {
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
		}
		randomUUID() {
			const b = this.getRandomValues(new Uint8Array(16));
			b[6] = (b[6] & 0x0f) | 0x40; // version 4
			b[8] = (b[8] & 0x3f) | 0x80; // variant 10
			const hex = [...b].map((x) => x.toString(16).padStart(2, "0")).join("");
			return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
		}
	}
	Object.defineProperty(Crypto.prototype, Symbol.toStringTag, { value: "Crypto", configurable: true });
	globalThis.Crypto ??= Crypto;
	globalThis.crypto ??= new globalThis.Crypto(CRYPTO_INTERNAL);

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

	// The stream classes live in streams.js, which is evaluated before this file.


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
		// Range is deliberately NOT here: the standard keeps it settable, and a
		// blob: fetch answers an invalid one with a network error — dropping the
		// header at set time turned every one of those into a silent whole-blob
		// 200.
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

	// The Headers iterator is a Web IDL "default iterator object": its
	// prototype chains to %IteratorPrototype%, next is a plain data property,
	// and it walks the LIVE sort-and-combine view — an entry removed while
	// iterating is not visited, one appended is.
	const ES_ITERATOR_PROTOTYPE = Object.getPrototypeOf(Object.getPrototypeOf([][Symbol.iterator]()));
	const HeadersIteratorPrototype = Object.create(ES_ITERATOR_PROTOTYPE, {
		next: {
			value: function next() {
				const list = this._headers._sorted();
				if (this._index >= list.length) return { value: undefined, done: true };
				const [k, v] = list[this._index++];
				return {
					value: this._kind === "key" ? k : this._kind === "value" ? v : [k, v],
					done: false,
				};
			},
			writable: true, enumerable: true, configurable: true,
		},
		[Symbol.toStringTag]: { value: "Headers Iterator", configurable: true },
	});

	// teeForClone is fetch's tee: the second branch receives cloned buffers
	// (ReadableStreamTee with cloneForBranch2), shared from streams.js.
	const teeForClone = (stream) => {
		const flagged = globalThis[Symbol.for("go-spidermonkey.streamTeeClone")];
		return flagged ? flagged(stream) : stream.tee();
	};

	class Headers {
		constructor(init = undefined) {
			this._map = new Map(); // lowercased name -> array of values
			// The header list preserves the CASE a name was first written in;
			// combining still keys on the lowercase form.
			Object.defineProperty(this, "_case", { value: new Map() });
			// The guard decides which headers this object will hold at all: see
			// setHeadersGuard. "none" is a free-standing Headers, which filters
			// nothing.
			Object.defineProperty(this, "_guard", { value: "none", writable: true });
			if (init === undefined) return;
			// The init is (sequence or record): a primitive — null included —
			// is neither. Which one an OBJECT is, is decided by GETTING its
			// Symbol.iterator, so a Headers subclass with its own iterator is
			// read through that iterator, and a proxy sees exactly the trap
			// order Web IDL prescribes.
			if (init === null || (typeof init !== "object" && typeof init !== "function")) {
				throw new TypeError("Headers: the init must be an object");
			}
			const iter = init[Symbol.iterator];
			if (iter !== undefined && iter !== null) {
				if (typeof iter !== "function") throw new TypeError("Headers: the init is not iterable");
				for (const pair of init) {
					if (pair === null || (typeof pair !== "object" && typeof pair !== "function")) {
						throw new TypeError("Headers: each init pair must be an object");
					}
					const items = [...pair];
					if (items.length !== 2) throw new TypeError("Headers: each init pair needs 2 items");
					this.append(items[0], items[1]);
				}
			} else {
				// Web IDL's record conversion, trap for trap: [[OwnPropertyKeys]],
				// then per key [[GetOwnProperty]] and — for an enumerable one —
				// [[Get]], interleaved in that order, with the KEY converted to a
				// ByteString before the value is read.
				for (const key of Reflect.ownKeys(init)) {
					if (typeof key !== "string") continue;
					const desc = Reflect.getOwnPropertyDescriptor(init, key);
					if (desc === undefined || !desc.enumerable) continue;
					for (let i = 0; i < key.length; i++) {
						if (key.charCodeAt(i) > 0xFF) {
							throw new TypeError("Headers: a header name must be a ByteString");
						}
					}
					this.append(key, init[key]);
				}
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
			if (!this._case.has(k)) this._case.set(k, String(name));
		}
		set(name, value) {
			const k = headerName(name, "set");
			const v = headerValue(value, "set");
			if (this._blocked(k, v, "set")) return;
			this._map.set(k, [v]);
			if (!this._case.has(k)) this._case.set(k, String(name));
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
			this._case.delete(k);
		}
		forEach(cb, thisArg) { for (const [k, v] of this.entries()) cb.call(thisArg, v, k, this); }
		// _sorted is the fetch "sort and combine" step, computed fresh for
		// every iterator step so iteration observes mutations. Set-Cookie is
		// its one special case: each cookie is its OWN entry, never joined (a
		// comma inside an Expires date would make the joined value
		// unparseable).
		_sorted() {
			const out = [];
			for (const k of [...this._map.keys()].sort()) {
				if (k === "set-cookie") { for (const v of this._map.get(k)) out.push([k, v]); }
				else out.push([k, this._map.get(k).join(", ")]);
			}
			return out;
		}
		_iterator(kind) {
			const it = Object.create(HeadersIteratorPrototype);
			Object.defineProperties(it, {
				_headers: { value: this },
				_index: { value: 0, writable: true },
				_kind: { value: kind },
			});
			return it;
		}
		// _caseOf answers the name as it should travel on the wire.
		_caseOf(k) { return this._case.get(k) ?? k; }
		entries() { return this._iterator("entry"); }
		keys() { return this._iterator("key"); }
		values() { return this._iterator("value"); }
		[Symbol.iterator]() { return this._iterator("entry"); }
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
	// A SHARED buffer is not a BufferSource: the IDL is BufferSource, not
	// [AllowShared] BufferSource, and it could not be one — another agent may
	// rewrite it while it is being sent.
	function isSharedBufferSource(v) {
		if (typeof SharedArrayBuffer !== "function") return false;
		if (v instanceof SharedArrayBuffer) return true;
		return ArrayBuffer.isView(v) && v.buffer instanceof SharedArrayBuffer;
	}

	function normalizeBody(init) {
		if (init === null || init === undefined) return { bytes: null, contentType: null };
		if (isSharedBufferSource(init)) {
			throw new TypeError("a body may not be backed by a SharedArrayBuffer");
		}
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
			// A stream that is already locked or has been read from cannot
			// become a body: there is nothing left to transmit.
			if (init.locked || init._disturbed === true) {
				throw new TypeError("the body stream is locked or disturbed");
			}
			target._body = null;
			target._bodyStream = init;
			return;
		}
		const { bytes, contentType } = normalizeBody(init);
		// An EMPTY body is still a body: `new Response("")` has a body of zero
		// bytes, and `res.body` is a stream that closes at once — where a null body
		// has no stream at all. Storing the empty case as null erased the
		// difference, and `body === null` is exactly what the tests check.
		target._body = bytes;
		if (contentType && target.headers && !target.headers.has("content-type")) {
			target.headers.set("content-type", contentType);
		}
	}

	// toBodyChunk normalizes one ReadableStream body chunk to bytes. Strings are
	// tolerated (UTF-8-encoded) because in-repo sources — and plenty of user code
	// — enqueue text; anything else must be a BufferSource per spec.
	// A stream body carries BYTES. A string chunk is not one — the stream had to
	// be a byte stream to be a body at all, and a caller that enqueued a string
	// has made a mistake that would otherwise be silently encoded for them.
	function toBodyChunk(value) {
		if (value instanceof ArrayBuffer) return new Uint8Array(value.slice(0));
		if (ArrayBuffer.isView(value)) return new Uint8Array(value.buffer.slice(value.byteOffset, value.byteOffset + value.byteLength));
		throw new TypeError("a ReadableStream body may only contain BufferSource chunks");
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
			// A body is a BYTE stream — it carries bytes, and a caller may read it
			// through a buffer of their own. Declared as an ordinary stream it
			// refused every BYOB reader.
			this._bodyStream = new ReadableStream({
				type: "bytes",
				pull(controller) {
					if (delivered || chunk.length === 0) { controller.close(); return; }
					delivered = true;
					controller.enqueue(chunk);
				},
			});
			return this._bodyStream;
		},
		// bodyUsed is "the body's stream is DISTURBED" — read from or cancelled —
		// not "a consumer here has taken it". Reading `response.body` directly and
		// then reading one chunk from it uses the body up just as surely as
		// response.text() does, and a consumer that only tracked its own calls
		// reported false there.
		get bodyUsed() {
			if (this._bodyConsumed === true) return true;
			if (this._bodyStream) return this._bodyStream._disturbed === true;
			return false;
		},
		// A guest-constructed Request/Response body may be read only once (WHATWG);
		// a second read throws a TypeError, and bodyUsed reflects that.
		_useBody() {
			if (this.bodyUsed || (this._bodyStream && this._bodyStream.locked)) {
				throw new TypeError("Body has already been consumed.");
			}
			this._bodyConsumed = true;
		},
		// _bodyBytes: the single "fully read body" step every consumer shares. A
		// stream body is drained to completion (rejecting if locked/already used).
		async _bodyBytes() {
			this._useBody();
			if (this._bodyStream) return drainBodyStream(this._bodyStream);
			return this._body === null ? new Uint8Array(0) : new Uint8Array(this._body);
		},
		async text() { const b = await this._bodyBytes(); return b.length === 0 ? "" : utf8Decode(b, false); },
		// textStream(): the body as a stream of strings, UTF-8 always — the
		// Content-Type charset is deliberately ignored, like text(). Calling it
		// consumes the body immediately, like every other consumer.
		textStream() {
			// A null body has nothing to consume: the stream is empty and
			// bodyUsed stays false, however many times this is called.
			const nullBody = !this._bodyStream && this._body === null;
			if (!nullBody) this._useBody();
			let src = this._bodyStream;
			if (src === undefined || src === null) {
				const bytes = nullBody ? new Uint8Array(0) : new Uint8Array(this._body);
				src = new ReadableStream({
					pull(controller) {
						if (bytes.length) controller.enqueue(bytes);
						controller.close();
					},
				});
			}
			return src.pipeThrough(new TextDecoderStream());
		},
		async json() {
			// A leading BOM is not part of the JSON text; the UTF-8 decode that
			// produced the string already dropped an encoded one, and a LITERAL
			// U+FEFF (a data: URL can carry it) gets the same treatment.
			return JSON.parse((await this.text()).replace(/^\uFEFF/, ""));
		},
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
			if (arguments.length < 1) throw new TypeError("Request: an input is required");
			const from = input instanceof Request ? input : null;
			// A request's url is ABSOLUTE. A relative one is resolved against the
			// environment's base, and one that cannot be resolved is a TypeError —
			// keeping the relative string meant everything downstream had to resolve
			// it again, and a cache keyed on it matched nothing.
			let url = from ? from.url : String(input);
			if (!from) {
				const base = environmentURL();
				let parsed = null;
				try { parsed = base ? new URL(url, base) : new URL(url); } catch { parsed = null; }
				if (parsed === null) throw new TypeError(`Request: ${url} is not a URL`);
				// WHATWG/undici: constructing a Request from a URL that carries
				// credentials throws a TypeError (they are NOT turned into an
				// Authorization header).
				if (parsed.username || parsed.password) {
					throw new TypeError("Request cannot be constructed from a URL that includes credentials");
				}
				url = parsed.href;
			}
			// A method must be a token, and CONNECT, TRACE and TRACK are forbidden
			// to script outright — they let a page reach through a proxy or reflect
			// its own request headers back, which is why they are the user agent's
			// alone. The normalization to upper case applies only to the methods the
			// standard names, so `patch` stays `patch` where `post` becomes `POST`.
			const rawMethod = String(init.method ?? (from ? from.method : "GET"));
			if (!/^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/.test(rawMethod)) {
				throw new TypeError(`Request: ${rawMethod} is not a method`);
			}
			if (FORBIDDEN_METHODS.has(rawMethod.toUpperCase())) {
				throw new TypeError(`Request: ${rawMethod} is a forbidden method`);
			}
			const NORMALIZED_METHODS = ["DELETE", "GET", "HEAD", "OPTIONS", "POST", "PUT"];
			const method = NORMALIZED_METHODS.includes(rawMethod.toUpperCase())
				? rawMethod.toUpperCase() : rawMethod;
			this[kRequestState] = {
				url,
				method,
				headers: setHeadersGuard(
					new Headers(init.headers ?? (from ? from.headers : undefined)),
					String(init.mode ?? (from ? from.mode : "") ?? "") === "no-cors" ? "request-no-cors" : "request"),
				signal: init.signal ?? (from ? from.signal : undefined),
			};
			// Workers' request.cf survives new Request(req) (workerd behavior).
			if (from && from.cf !== undefined) this.cf = from.cf;
			// A stream body means the request body is sent while the response may
			// already be arriving, and the caller has to say so: `duplex: "half"`.
			// Without it the constructor fails, which is how a caller learns that
			// the stream they passed would never have been sent.
			if (init.duplex !== undefined && String(init.duplex) !== "half") {
				throw new TypeError(`Request: ${String(init.duplex)} is not a duplex mode`);
			}
			// Copying an existing Request carries its body across without the caller
			// naming a duplex mode: they did not choose the stream, they are keeping
			// the one the request already had.
			if (init.body instanceof ReadableStream && !(init instanceof Request)
				&& String(init.duplex ?? "") !== "half") {
				throw new TypeError("Request: a ReadableStream body requires duplex: \"half\"");
			}
			this._bodyStream = null;
			if (init.body !== undefined) setBody(this, init.body);
			else {
				this._body = from ? from._body : null;
				if (from && from._bodyStream) this._bodyStream = from._bodyStream;
			}
			this._bodyConsumed = false;
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
			this._bodyConsumed = false;
			// A Response the caller constructed has type "default": it did not come
			// from a fetch, so it is neither basic nor cors nor opaque. Only fetch
			// itself, and Response.error(), say otherwise.
			this.type = "default";
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
				// A used/locked body cannot be cloned. The CLONE's branch carries
				// cloned buffers — fetch tees with cloneForBranch2 — so writing
				// into one response's chunk cannot corrupt the other's.
				if (this.bodyUsed || this._bodyStream.locked) throw new TypeError("Cannot clone a Response whose body is used or locked");
				const [b1, b2] = teeForClone(this._bodyStream);
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
			const text = JSON.stringify(data);
			// Serializing to BYTES is part of the contract: a string that is
			// not well-formed UTF-16 (a lone surrogate) has no UTF-8 form.
			if (text === undefined || (text.isWellFormed && !text.isWellFormed())) {
				throw new TypeError("Response.json: the data cannot be encoded");
			}
			const r = new Response(text, init);
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
			r.type = "error";
			return r;
		}
	}
	Object.defineProperties(Response.prototype, Object.getOwnPropertyDescriptors(bodyMixin));
	globalThis.Response = Response;

	// nativeResponseProto is what a Go-built response's prototype becomes, so
	// that `res instanceof Response` holds — ordinary user code asks, and the
	// WebAssembly streaming entry points are specified to REJECT anything that is
	// not a Response, so an object that answers every Response question without
	// being one fails them for the wrong reason.
	//
	// It sits BETWEEN the response and Response.prototype rather than being
	// Response.prototype itself, because the guest class's body mixin reads guest
	// internals (_body, _bodyStream) a Go-built object does not have. Everything
	// such a response needs is an own property by the time it is branded (see
	// trackBodyUsed); this layer supplies the one member that is not, and
	// deliberately shadows the mixin's internals so none of them can be reached
	// by accident.
	const nativeResponseProto = Object.create(Response.prototype, {
		clone: {
			// A response from fetch clones by TEEING its body, like any other: the
			// two halves replay the same bytes. What is different is that this
			// response's own consumers read the connection directly, and they cannot
			// go on doing that once a tee owns it — so they are re-pointed at this
			// side's branch here. Refusing to clone (which is what this used to do)
			// left every caller that clones a fetch response with nothing.
			value() {
				if (this.bodyUsed) throw new TypeError("clone: the body has already been used");
				const source = this.body;
				if (source && source.locked) throw new TypeError("clone: the body is locked");
				// Built without a status in the init so the constructor's range guard
				// is skipped: an opaque response's status is 0 and clones all the same.
				const build = (stream) => {
					const r = new Response(null, { statusText: this.statusText, headers: new Headers(this.headers) });
					r.status = this.status;
					r.ok = this.status >= 200 && this.status <= 299;
					r.type = this.type;
					r.url = this.url;
					r.redirected = this.redirected === true;
					r._body = null;
					r._bodyStream = stream;
					return r;
				};
				if (source === null || source === undefined) return build(null);
				const [mine, theirs] = source.tee();
				const self = build(mine);
				Object.defineProperty(this, "body", { configurable: true, writable: true, value: mine });
				for (const m of ["arrayBuffer", "bytes", "blob", "text", "json", "formData"]) {
					Object.defineProperty(this, m, {
						configurable: true, writable: true,
						value: function (...a) { return self[m](...a); },
					});
				}
				Object.defineProperty(this, "bodyUsed", { configurable: true, get: () => self.bodyUsed });
				return build(theirs);
			},
			writable: true, configurable: true,
		},
	});
	// The guest class's body-mixin internals are shadowed as ABSENT rather than
	// as errors: absent is what they were before this object had a prototype at
	// all, and sibling layers (compat/cfworkers' passthrough) legitimately test
	// for them to pick a fast path. Throwing here would turn "this response is
	// host-backed" into a failure.
	for (const internal of ["_bodyBytes", "_useBody", "_bodyStream", "_body"]) {
		Object.defineProperty(nativeResponseProto, internal, {
			value: undefined, writable: true, configurable: true,
		});
	}

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
	// reportError(e): report to the global error channel without throwing (a
	// stable global in browsers and Node >=17).
	//
	// On the web that channel is an `error` event on the global, and it is the
	// only way `self.onerror` can ever see an exception the runtime caught rather
	// than one that unwound to the top. Logging to the console instead — which is
	// what this did — left onerror silent, so any code that installs one to
	// collect failures collected nothing. The console is the fallback for when
	// nobody took the event, which is what a browser does too.
	globalThis.reportError ??= (e) => {
		const emit = globalThis.__emit_uncaught;
		if (emit && emit(e)) return;
		let handled = false;
		if (typeof globalThis.ErrorEvent === "function") {
			// Where it happened comes from the error itself when it IS an error: the
			// engine records the position on every Error it makes. A value that is
			// not an Error — code may report a string — carries no position, so the
			// CALL SITE is used instead, read from a stack captured here.
			const at = e instanceof Error
				? { file: e.fileName, line: e.lineNumber, col: e.columnNumber }
				: callSite();
			const ev = new globalThis.ErrorEvent("error", {
				cancelable: true,
				message: e instanceof Error ? String(e.message) : String(e),
				error: e,
				filename: at.file === undefined ? "" : String(at.file),
				lineno: at.line === undefined ? 0 : Number(at.line),
				colno: at.col === undefined ? 0 : Number(at.col),
			});
			handled = !__dispatch_trusted(globalThis, ev);
		}
		if (!handled) { try { console.error(e); } catch { /* ignore */ } }
	};
	// callSite is where reportError was called from. The engine's stack is a
	// foreign format — this runtime does not produce it and cannot change it — so
	// reading a frame out of it is a parse, and the parse is TOTAL: anything that
	// does not match gives an unknown position rather than a wrong one.
	//
	// A frame is `name@file:line:col`, and the first two belong to this function
	// and to reportError itself.
	const STACK_FRAME = /@(.*):(\d+):(\d+)$/;
	function callSite() {
		const frames = String(new Error().stack || "").split("\n");
		for (const frame of frames.slice(2)) {
			const m = STACK_FRAME.exec(frame.trim());
			if (m) return { file: m[1], line: Number(m[2]), col: Number(m[3]) };
		}
		return {};
	}

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
	// parseSingleByteRange applies RFC 9110's byte-range grammar to one range,
	// resolved against a known length. It returns null for anything the grammar
	// refuses — no "bytes=" prefix, no hyphen, a non-ASCII-digit anywhere, more
	// than one range, whitespace inside the range itself, a start past the end —
	// and null for an unsatisfiable range, which for a blob: URL is the same
	// outcome. The rules are exact rather than lenient because the whole point of
	// the tests is the ones a permissive parser would wave through.
	function parseSingleByteRange(value, size) {
		// The standard's "simple range header value", with whitespace allowed:
		// "bytes" OWS "=" OWS digits* OWS "-" OWS digits*, nothing after — one
		// range, no list, and at least one of the two positions stated.
		const raw = String(value);
		let i = 0;
		if (!raw.startsWith("bytes")) return null;
		i = 5;
		const ows = () => { while (raw[i] === " " || raw[i] === "\t") i++; };
		const digits = () => {
			const from = i;
			while (raw[i] >= "0" && raw[i] <= "9") i++;
			return raw.slice(from, i);
		};
		ows();
		if (raw[i] !== "=") return null;
		i++;
		ows();
		const first = digits();
		ows();
		if (raw[i] !== "-") return null;
		i++;
		ows();
		const last = digits();
		if (i !== raw.length) return null;
		if (first === "" && last === "") return null;
		if (first === "") {
			// A suffix range: the LAST n bytes. Zero of them satisfies nothing.
			const n = Number(last);
			if (n === 0) return null;
			return { start: Math.max(0, size - n), end: size - 1 };
		}
		const start = Number(first);
		if (start >= size) return null; // unsatisfiable
		if (last === "") return { start, end: size - 1 };
		const end = Math.min(Number(last), size - 1);
		if (end < start) return null;
		return { start, end };
	}

	function environmentURL() {
		try {
			const href = globalThis.location && globalThis.location.href;
			return href ? new URL(href) : null;
		} catch { return null; }
	}

	// Origin travels with a request whose response is CORS-tainted — a
	// cross-origin request in cors mode — and with any method outside the
	// safelist, where a POST states its origin even in no-cors mode. A
	// SAME-ORIGIN GET states nothing: there is nobody to state it to, and a
	// server that sees one has been told something the standard does not send.
	function originHeaderFor(mode, method, envURL, crossOrigin) {
		if (!envURL) return null;
		const m = String(method || "GET").toUpperCase();
		if (crossOrigin && mode === "cors") return envURL.origin;
		if (m !== "GET" && m !== "HEAD") return envURL.origin;
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

	// Cross-Origin-Resource-Policy is enforced on no-cors responses only: it
	// is a server's way of refusing to be embedded, and a CORS response has
	// already negotiated access more explicitly. "same-site" compares the
	// scheme and host, ports aside.
	function corpCheck(res, envURL, target) {
		const v = String((res.headers && res.headers.get("cross-origin-resource-policy")) || "").trim().toLowerCase();
		if (v === "same-origin") {
			throw corsError("Cross-Origin-Resource-Policy: same-origin at " + target.origin);
		}
		if (v === "same-site" && (target.hostname !== envURL.hostname || target.protocol !== envURL.protocol)) {
			throw corsError("Cross-Origin-Resource-Policy: same-site at " + target.origin);
		}
	}

	function corsAllowsResponse(res, origin, withCredentials) {
		// Optional whitespace around a header value is not part of it.
		if (withCredentials) {
			const allowed = String((res.headers && res.headers.get("access-control-allow-origin")) || "").trim();
			return allowed === origin &&
				String(res.headers.get("access-control-allow-credentials") || "").trim().toLowerCase() === "true";
		}
		const allow = String((res.headers && res.headers.get("access-control-allow-origin")) || "").trim();
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

	// A CORS-unsafe request-header byte. The set is the standard's, and it is
	// what makes `accept: */*` safelisted and `accept: "` not: the value has to
	// be one a server cannot be surprised by, not merely a header with the right
	// name.
	// Everything below 0x20 except TAB, plus the punctuation the standard names.
	// Note what is NOT here: 0x3B (;), which a content-type needs for its
	// parameters — including it refused "text/plain;charset=utf-8", which is the
	// safelisted value a form POST sends.
	const CORS_UNSAFE_BYTE = /[\x00-\x08\x0a-\x1f\x22\x28\x29\x3a\x3c\x3e\x3f\x40\x5b\x5c\x5d\x7b\x7d\x7f]/;
	// accept-language and content-language may hold only these.
	const LANGUAGE_VALUE = /^[\x30-\x39\x41-\x5a\x61-\x7a *,\-.;=]*$/;
	// The safelist stops at 128 bytes per value: a longer one is unsafe however
	// it is spelled.
	const CORS_SAFELIST_VALUE_MAX = 128;

	function isSafelistedRequestHeader(name, value) {
		const n = String(name).toLowerCase();
		if (!CORS_SAFELISTED_REQUEST_HEADERS.has(n)) return false;
		const v = String(value);
		if (utf8Length(v) > CORS_SAFELIST_VALUE_MAX) return false;
		switch (n) {
			case "accept":
				return !CORS_UNSAFE_BYTE.test(v);
			case "accept-language":
			case "content-language":
				return LANGUAGE_VALUE.test(v);
			case "content-type": {
				if (CORS_UNSAFE_BYTE.test(v)) return false;
				const mime = v.split(";")[0].trim().toLowerCase();
				return CORS_SAFELISTED_CONTENT_TYPES.has(mime);
			}
		}
		return true;
	}

	// The byte length of a header value, which is what the limit is measured in.
	function utf8Length(v) {
		return new TextEncoder().encode(v).length;
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

	// splitTokenList is splitList with the grammar enforced: every entry must
	// be an HTTP token (or the wildcard). A preflight that answers with a
	// malformed Access-Control-Allow-* value has not allowed anything.
	const HTTP_TOKEN = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;
	function splitTokenList(value, what) {
		const out = new Set();
		for (const part of String(value || "").split(",")) {
			const t = part.trim().toLowerCase();
			if (!t) continue;
			if (!HTTP_TOKEN.test(t)) throw corsError(`preflight ${what} value "${t}" is not a token`);
			out.add(t);
		}
		return out;
	}

	// The preflight cache: a server's permission holds for Max-Age seconds
	// (5 by default), so a second request within it goes out unasked. Keyed
	// by origin, url and credentials mode, as the specification keys it.
	const preflightCache = new Map();
	function preflightCacheCovers(url, need, origin, withCredentials) {
		const key = origin + "\x00" + url + "\x00" + (withCredentials ? "c" : "");
		const entry = preflightCache.get(key);
		if (!entry || entry.expires < Date.now()) {
			preflightCache.delete(key);
			return false;
		}
		const m = need.method.toLowerCase();
		const simple = m === "get" || m === "head" || m === "post";
		if (!simple && !entry.methods.has(m) && !(entry.methods.has("*") && !withCredentials)) return false;
		const headerWildcard = entry.headers.has("*") && !withCredentials;
		for (const n of need.headers) {
			if (entry.headers.has(n)) continue;
			if (headerWildcard && n !== "authorization") continue;
			return false;
		}
		return true;
	}

	async function runPreflight(url, need, origin, referer, withCredentials) {
		if (preflightCacheCovers(url, need, origin, withCredentials)) return;
		const h = { origin, "access-control-request-method": need.method };
		if (need.headers.length) h["access-control-request-headers"] = need.headers.join(",");
		// Browsers send Accept: */* on a preflight, and the suite's fixture
		// rejects a preflight without it; the preflight also states the same
		// referrer the real request would.
		h.accept = "*/*";
		if (referer) h.referer = referer;
		// Through the same normalization the real response gets: the raw host
		// object has no Headers to read the permissions out of.
		const res = trackBodyUsed(await globalThis.__native_fetch(url, { method: "OPTIONS", headers: h, redirect: "error" }));
		if (!(res.status >= 200 && res.status < 300)) {
			throw corsError("preflight responded " + res.status);
		}
		// With credentials, every wildcard is a LITERAL: "*" grants nothing,
		// the origin must be named, and Allow-Credentials must say true.
		const allowOrigin = String(res.headers.get("access-control-allow-origin") || "").trim();
		if (allowOrigin !== origin && (withCredentials || allowOrigin !== "*")) {
			throw corsError("preflight did not allow " + origin);
		}
		if (withCredentials &&
			String(res.headers.get("access-control-allow-credentials") || "").toLowerCase() !== "true") {
			throw corsError("preflight did not allow credentials");
		}
		const allowMethods = splitTokenList(res.headers.get("access-control-allow-methods"), "Access-Control-Allow-Methods");
		const methodWildcard = allowMethods.has("*") && !withCredentials;
		if (!methodWildcard && !allowMethods.has(need.method.toLowerCase()) &&
			!(need.method === "GET" || need.method === "HEAD" || need.method === "POST")) {
			throw corsError("preflight did not allow method " + need.method);
		}
		const allowHeaders = splitTokenList(res.headers.get("access-control-allow-headers"), "Access-Control-Allow-Headers");
		const wildcard = allowHeaders.has("*") && !withCredentials;
		for (const n of need.headers) {
			if (allowHeaders.has(n)) continue;
			// The wildcard covers every header EXCEPT Authorization, which must be
			// named. It is the one header a server can be talked into accepting by
			// accident, so the standard makes accepting it deliberate.
			if (wildcard && n !== "authorization") continue;
			throw corsError("preflight did not allow header " + n);
		}
		let maxAge = 5;
		const rawAge = res.headers.get("access-control-max-age");
		if (rawAge !== null && /^-?\d+$/.test(String(rawAge).trim())) {
			maxAge = parseInt(rawAge, 10);
		}
		if (maxAge > 0) {
			preflightCache.set(origin + "\x00" + url + "\x00" + (withCredentials ? "c" : ""), {
				expires: Date.now() + Math.min(maxAge, 60) * 1000,
				methods: allowMethods,
				headers: allowHeaders,
			});
		}
	}

	// filterCORSResponseHeaders removes what a cors response may not expose: a
	// page sees the safelist plus whatever Access-Control-Expose-Headers names.
	function filterCORSResponseHeaders(res, withCredentials) {
		const exposed = splitList(res.headers.get("access-control-expose-headers"));
		// With credentials the wildcard is a LITERAL header name; and
		// Set-Cookie is never exposed to a cross-origin caller at all, not
		// even by naming it.
		const wildcard = exposed.has("*") && !withCredentials;
		const remove = [];
		for (const [k] of res.headers) {
			const n = String(k).toLowerCase();
			if (n === "set-cookie" || n === "set-cookie2") { remove.push(k); continue; }
			if (CORS_SAFELISTED_RESPONSE_HEADERS.has(n) || wildcard || exposed.has(n)) continue;
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

	async function followCORSRedirects(url, nInit, headers, envURL, referrerPolicy, explicitReferrer, corsMode, signal, credentialsMode) {
		const withCredentials = credentialsMode === "include";
		// The chain has AWAITS in it — a preflight, each hop — and an abort that
		// lands during one of them must stop the next request from going out. The
		// host-side cancel only reaches a request that has already started, so the
		// signal is re-read here before anything else is sent.
		const abortIfAborted = () => {
			if (signal && signal.aborted) {
				throw (signal.reason ?? new DOMException("The operation was aborted", "AbortError"));
			}
		};
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
		// Response tainting: "cors" once any hop has been cross-origin, and never
		// back again.
		let tainted = false;
		for (;;) {
			// The headers are materialized per hop: nInit.headers is only filled
			// in by the single-shot dispatch path, and the Origin changes once a
			// hop has left the origin.
			const hdrObj = {};
			for (const [k, v] of headers) hdrObj[headers._caseOf ? headers._caseOf(k) : k] = v;
			const hopTarget = new URL(current);
			// The request's origin can become OPAQUE part-way down a redirect chain:
			// once a hop leaves the origin the request came from, the next request no
			// longer speaks for that origin and says so with the literal "null".
			// Fetch's rule keys on the CURRENT url, not on the destination — a hop
			// from the request's own origin to another one still carries the real
			// origin, and only a hop that starts somewhere else loses it.
			//
			// WHETHER to send one is decided per hop, and the taint is STICKY: once
			// a hop has left the origin the response is CORS-tainted for the rest of
			// the chain, so a redirect back to where it started still states its
			// origin — as the literal "null" by then. Deciding hop by hop instead
			// made the last request look like a plain same-origin one.
			if (hopTarget.origin !== envURL.origin) tainted = true;
			if (originHeaderFor(corsMode, nInit.method, envURL, tainted)) {
				hdrObj.origin = origin;
			} else {
				delete hdrObj.origin;
			}
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
			const target = hopTarget;
			// The credentials MODE is the request's; whether cookies actually
			// travel is decided per hop: "same-origin" carries them only on a
			// hop that stayed home.
			step.credentials = credentialsMode === "include"
				|| (credentialsMode !== "omit" && target.origin === envURL.origin)
				? "include" : "omit";
			// A preflight is required for EVERY cross-origin hop that needs one, not
			// only for the first: a redirect to a URL whose headers are not
			// safelisted must ask that server's permission before it is sent
			// anything. Doing it once, before the chain, meant the second server was
			// never consulted and the request went out regardless.
			if (corsMode === "cors" && target.origin !== envURL.origin) {
				const hopNeed = needsPreflight(step.method, headers);
				if (hopNeed) {
					abortIfAborted();
					await runPreflight(current, hopNeed, origin, hdrObj.referer, withCredentials);
				}
			}
			abortIfAborted();
			const res = trackBodyUsed(await globalThis.__native_fetch(current, step));
			const crossHop = target.origin !== envURL.origin;
			if (crossHop && corsMode !== "no-cors" && !corsAllowsResponse(res, origin, withCredentials)) {
				throw corsError("no Access-Control-Allow-Origin for " + origin + " at " + target.origin);
			}
			// A no-cors response never negotiates; the server's only word is
			// Cross-Origin-Resource-Policy, and it is final.
			if (crossHop && corsMode === "no-cors") corpCheck(res, envURL, target);
			if (!REDIRECT_STATUSES.has(res.status)) {
				if (corsMode === "no-cors" && tainted) return opaqueResponse(res);
				if (crossHop) filterCORSResponseHeaders(res, withCredentials);
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
				// A no-cors caller may only observe a redirect that never left
				// the origin; a cross-origin one is a network error.
				if (corsMode === "no-cors" && crossHop) {
					throw corsError("a no-cors request cannot observe a cross-origin redirect");
				}
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
			// The origin goes opaque only when a CROSS-origin hop starts somewhere
			// that is not the request's own origin. Leaving your own origin still
			// carries it — that is how a cross-origin request states who is asking —
			// and it is the SECOND departure, from a place you were only sent to,
			// that leaves nothing left to speak for. Treating every crossing as
			// opaque sent "null" on the first hop, where the origin is exactly what
			// the server needs to decide.
			if (next.origin !== target.origin && origin !== target.origin) origin = "null";
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
			// A relative URL is resolved against the environment's base HERE, so
			// everything downstream — the redirect chain, the origin comparisons,
			// the host — sees an absolute one. A Request has already done it.
			let url = isReq ? input.url : String(input);
			let parsed = null;
			const fetchBase = environmentURL();
			try { parsed = fetchBase ? new URL(url, fetchBase) : new URL(url); } catch { parsed = null; }
			// WHATWG/undici: a URL that carries credentials is rejected (NOT converted
			// to an Authorization header). Only throw when the URL actually parses with
			// a username/password so relative inputs keep their current behavior.
			if (parsed && (parsed.username || parsed.password)) {
				throw new TypeError("Request cannot be constructed from a URL that includes credentials");
			}
			if (parsed) url = parsed.href;
			const headers = setHeadersGuard(new Headers(isReq ? input.headers : undefined), "request");
			if (init.headers !== undefined && init.headers !== null) {
				// Through the intermediate Headers for validation and combining,
				// but with the CASE the caller wrote, which is what travels.
				const given = new Headers(init.headers);
				for (const [k, v] of given) headers.set(given._caseOf(k), v);
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
				// Whether the target is somewhere else decides this, so it is asked
				// here rather than taken from the later cross-origin check — which is
				// about the redirect chain and comes after the headers are final.
				const toElsewhere = !!(envURL && parsed && envURL.origin !== parsed.origin);
				const origin = originHeaderFor(mode, method, envURL, toElsewhere);
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
				// A Range header on a blob: URL is served from memory, and a header
				// the grammar does not accept is a NETWORK ERROR rather than a 416:
				// there is no server to have answered, so there is nothing to answer
				// with. The grammar is RFC 9110's, restricted to a single byte range
				// — a blob: URL answers one range or none.
				const rangeHeader = headers.get("range");
				let range = null;
				if (rangeHeader !== null && rangeHeader !== undefined) {
					range = parseSingleByteRange(rangeHeader, blob.size);
					if (range === null) {
						return Promise.reject(new TypeError("Failed to fetch: the Range header is not a valid single byte range"));
					}
				}
				return blob.arrayBuffer().then((buf) => {
					if (range) {
						const body = buf.slice(range.start, range.end + 1);
						const res = new Response(body, {
							status: 206,
							statusText: "Partial Content",
							headers: {
								"content-type": blob.type || "",
								"content-length": String(body.byteLength),
								"content-range": `bytes ${range.start}-${range.end}/${blob.size}`,
							},
						});
						for (const [k, v] of [["type", "basic"], ["url", url]]) {
							try { Object.defineProperty(res, k, { configurable: true, value: v }); } catch { /* ignore */ }
						}
						return res;
					}
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
							// An explicit referrer is still SUBJECT to the policy: it
							// replaces the document URL as the source, not the rules.
							ref = parsed ? referrerHeaderFor(policy, u, parsed) : u.href;
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
			if (bodyInit !== undefined && bodyInit !== null && (method === "GET" || method === "HEAD")) {
				return Promise.reject(new TypeError(`fetch: a ${method} request cannot have a body`));
			}
			const dispatch = () => {
				const hdrObj = {};
				for (const [k, v] of headers) hdrObj[headers._caseOf ? headers._caseOf(k) : k] = v;
				nInit.headers = hdrObj;
				if (signal && onAbort) signal.addEventListener("abort", onAbort);
				return globalThis.__native_fetch(url, nInit).then(
					(res) => {
						cleanup();
						const tracked = trackBodyUsed(res);
						if (crossOrigin && mode === "cors") {
							if (!corsAllowsResponse(tracked, envURL.origin, withCredentials)) {
								throw corsError("no Access-Control-Allow-Origin for " + envURL.origin);
							}
							filterCORSResponseHeaders(tracked, withCredentials);
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
			// The credentials mode decides both the cookie jar and the CORS
			// contract: with "include", every wildcard becomes a literal.
			const credentialsMode = String(init.credentials ?? (isReq ? input.credentials : "") ?? "") || "same-origin";
			const withCredentials = credentialsMode === "include";
			nInit.credentials = credentialsMode === "omit" ? "omit"
				: (withCredentials || !crossOrigin) ? "include" : "omit";
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
			const chained = !!envURL && networkScheme;
			const send = () => {
				const go = chained
					? () => {
						if (signal && onAbort) signal.addEventListener("abort", onAbort);
						return followCORSRedirects(url, nInit, headers, envURL, chainPolicy, chainExplicitReferrer, mode, signal, credentialsMode).then(
							(res) => { cleanup(); return res; },
							(err) => {
								cleanup();
								if (signal && signal.aborted) throw (signal.reason ?? new DOMException("The operation was aborted", "AbortError"));
								throw asFetchError(err);
							});
					}
					: dispatch;
				// The chained path preflights each hop itself, including the first;
				// preflighting here as well would ask the same server twice and the
				// suite counts preflights.
				const need = !chained && crossOrigin && mode === "cors"
					? needsPreflight(method, headers) : null;
				const started = !need ? go()
					: runPreflight(url, need, envURL.origin, headers.get("referer"), withCredentials).then(go);
				if (!signal) return started;
				// An abort ENDS the fetch, whatever stage it is at. Cancelling the
				// host request releases the connection, but the moment the promise
				// rejects must not depend on how far the request had got — a signal
				// that fires while the guest is between hops, or while the host is
				// mid-round-trip, has to reject with the abort reason either way.
				let fire = null;
				const aborted = new Promise((_, reject) => {
					fire = () => reject(signal.reason ?? new DOMException("The operation was aborted", "AbortError"));
					if (signal.aborted) fire();
					else signal.addEventListener("abort", fire);
				});
				// The listener goes when the fetch is over, however it ended: a signal
				// that outlives its fetch must not keep the rejection closure alive.
				const drop = () => { if (fire) signal.removeEventListener("abort", fire); };
				// Whichever loses the race is not an unhandled rejection: one of the
				// two is always discarded.
				started.then(drop, drop);
				started.catch(() => {});
				aborted.catch(() => {});
				return Promise.race([started, aborted]);
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
						// The body's STREAM is the authority on whether the body is still
						// there: a caller that took a reader on response.body, or
						// cancelled it, has used the body up even though none of these
						// consumers ran.
						if (used || (this.body && (this.body.locked || this.body._disturbed === true))) {
							return Promise.reject(new TypeError("Body has already been consumed."));
						}
						used = true;
						// Consuming here disturbs the stream too — the bytes go to the
						// host reader, and response.body must not hand them out again.
						// Holding a reader on it is how that is said.
						if (this.body) { try { this.body.getReader(); } catch { /* already locked */ } }
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
			Object.defineProperty(res, "bodyUsed", {
				configurable: true,
				get: () => used || (res.body !== undefined && res.body !== null && res.body._disturbed === true),
			});
		} catch { /* leave the native field as-is */ }
		// Branded LAST: everything a Response answers is an own property by now,
		// so nothing the guest class's body mixin owns can be reached through the
		// chain. See nativeResponseProto.
		try { Object.setPrototypeOf(res, nativeResponseProto); } catch { /* leave unbranded */ }
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

	// ------------------------------------------------------------ fetchLater
	// The Deferred Fetch API (https://whatpr.org/fetch/1647.html): register a
	// request to be sent later — after activateAfter, or when the document is
	// destroyed. This runtime has no document teardown to hook, so a request
	// with no activateAfter simply never fires; one WITH it really is sent.
	// The 64 KiB per-reporting-origin quota is what most of the suite is about:
	// the URL, the headers and the body all count against it, and exceeding it
	// is a QuotaExceededError carrying the requested/quota pair.
	(() => {
		const QUOTA = 64 * 1024;
		const used = new Map(); // origin -> bytes reserved by pending requests
		const trustworthy = (u) => {
			if (u.protocol === "https:") return true;
			if (u.protocol !== "http:") return false;
			const h = u.hostname;
			return h === "localhost" || h === "127.0.0.1" || h === "[::1]" ||
				h.endsWith(".localhost");
		};
		globalThis.fetchLater = function fetchLater(input, init) {
			if (arguments.length < 1) {
				throw new TypeError("fetchLater: a request is required");
			}
			init = init === null || init === undefined ? {} : init;
			// activateAfter validates before anything else observable.
			const after = init.activateAfter;
			if (after !== undefined && after !== null && Number(after) < 0) {
				throw new RangeError("fetchLater: activateAfter must be non-negative");
			}
			// Request normalizes the URL, method, headers and body exactly the
			// way fetch would — and throws the same TypeErrors.
			const req = new Request(input, init);
			const url = new URL(req.url);
			if (!trustworthy(url)) {
				throw new TypeError(`fetchLater: ${url.protocol} URLs must be potentially trustworthy`);
			}
			if (req._bodyStream) {
				throw new TypeError("fetchLater: a deferred request cannot have a streaming body");
			}
			if (init.signal && init.signal.aborted) {
				throw new DOMException("The operation was aborted.", "AbortError");
			}
			// The quota: URL + headers + body + referrer, against the request
			// URL's origin.
			let cost = url.href.length;
			for (const [k, v] of req.headers) cost += k.length + v.length;
			if (req._body) cost += req._body.byteLength;
			if (typeof init.referrer === "string") cost += init.referrer.length;
			const origin = url.origin;
			const have = used.get(origin) || 0;
			if (have + cost > QUOTA) {
				throw new QuotaExceededError(
					"fetchLater: the deferred-fetch quota for " + origin + " is exhausted",
					{ requested: cost, quota: QUOTA });
			}
			used.set(origin, have + cost);
			let activated = false;
			let done = false;
			const release = () => {
				if (done) return;
				done = true;
				used.set(origin, Math.max(0, (used.get(origin) || 0) - cost));
			};
			const send = () => {
				if (done) return;
				release();
				activated = true;
				globalThis.fetch(req).catch(() => { /* fire and forget */ });
			};
			if (after !== undefined && after !== null) {
				const id = globalThis.setTimeout(send, Number(after));
				if (init.signal) {
					init.signal.addEventListener("abort", () => {
						globalThis.clearTimeout(id);
						release();
					});
				}
			} else if (init.signal) {
				init.signal.addEventListener("abort", release);
			}
			const result = {};
			Object.defineProperty(result, "activated", {
				get: () => activated, enumerable: true, configurable: true,
			});
			Object.defineProperty(result, Symbol.toStringTag, {
				value: "FetchLaterResult", configurable: true,
			});
			return result;
		};
	})();
})();
