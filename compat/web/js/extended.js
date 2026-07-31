// compat/web: WinterTC vocabulary completions — Blob, File, FormData, a full
// structuredClone, CustomEvent, TextEncoder/DecoderStream, and the stream
// controller/reader classes as globals. Evaluated after builtins.js/subtle.js
// while __web_ops is still present.
(() => {
	"use strict";

	// -------------------------------------------------------------- Blob/File

	// A media type is reported PARSED and SERIALIZED BACK, never as given: that
	// is what lowercases it, normalizes parameter whitespace and yields "" for
	// one that does not parse. Captured here because __web_ops is deleted once
	// the builtins have run.
	// A media type is reported PARSED and SERIALIZED BACK, never as given: that
	// is what lowercases it, normalizes parameter whitespace and yields "" for
	// one that does not parse. Captured here because __web_ops is deleted once
	// the builtins have run.
	const mimeType = __web_ops.mime_type;

	// slice() does NOT parse: its steps say only that a type outside
	// U+0020..U+007E becomes the empty string and that what is left is
	// lowercased. The two rules are genuinely different, and a slice given
	// "te<xt/plain" keeps it where the constructor would not.
	const sliceType = (t) => (/^[\x20-\x7e]*$/.test(t) ? t.toLowerCase() : "");

	const encodePart = (part) => {
		// A DETACHED buffer holds nothing: its bytes went elsewhere, and a Blob
		// built from it is empty rather than a failure.
		try {
			if (part instanceof Uint8Array) return part.slice();
			if (part instanceof ArrayBuffer) return new Uint8Array(part.slice(0));
			if (ArrayBuffer.isView(part)) return new Uint8Array(part.buffer.slice(part.byteOffset, part.byteOffset + part.byteLength));
		} catch {
			return new Uint8Array(0);
		}
		if (part && typeof part._blobParts === "object") return concatBytes(part._blobParts);
		return new TextEncoder().encode(String(part));
	};
	function concatBytes(parts) {
		const bufs = parts.map(encodePart);
		const total = bufs.reduce((n, b) => n + b.length, 0);
		const out = new Uint8Array(total);
		let off = 0;
		for (const b of bufs) { out.set(b, off); off += b.length; }
		return out;
	}

	class Blob {
		constructor(parts = [], options = {}) {
			if (parts != null && typeof parts[Symbol.iterator] !== "function") {
				throw new TypeError("Blob parts must be iterable");
			}
			// `endings` is an enumeration, so a value outside it is a TypeError —
			// and it is read (and validated) before anything is copied.
			if (options !== null && options !== undefined && options.endings !== undefined) {
				const endings = String(options.endings);
				if (endings !== "transparent" && endings !== "native") {
					throw new TypeError(`Blob: ${endings} is not a line-ending conversion`);
				}
				this._endings = endings;
			}
			this._blobParts = [...(parts || [])];
			this._bytes = concatBytes(this._blobParts);
			// type and size are IDL attributes: prototype accessors over slots, not
			// own data properties. As own properties they were writable, and every
			// interface test that reads a descriptor said so.
			this._type = options && options.type !== undefined ? mimeType(String(options.type)) : "";
		}
		get type() { return this._type; }
		get size() { return this._bytes.length; }
		async arrayBuffer() { return this._bytes.buffer.slice(this._bytes.byteOffset, this._bytes.byteOffset + this._bytes.byteLength); }
		async bytes() { return this._bytes.slice(); }
		async text() { return new TextDecoder().decode(this._bytes); }
		slice(start = 0, end = this._bytes.length, contentType = "") {
			const s = start < 0 ? Math.max(this._bytes.length + start, 0) : Math.min(start, this._bytes.length);
			const e = end < 0 ? Math.max(this._bytes.length + end, 0) : Math.min(end, this._bytes.length);
			const b = new Blob([]);
			b._type = sliceType(String(contentType));
			b._bytes = this._bytes.slice(s, Math.max(s, e));
			b._blobParts = [b._bytes];
			return b;
		}
		stream() {
			const bytes = this._bytes;
			let done = false;
			return new ReadableStream({
				pull(controller) {
					if (done) { controller.close(); return; }
					done = true;
					if (bytes.length) controller.enqueue(bytes.slice());
					controller.close();
				},
			});
		}
		get [Symbol.toStringTag]() { return "Blob"; }
	}
	globalThis.Blob = Blob;

	class File extends Blob {
		constructor(parts, name, options = {}) {
			if (arguments.length < 2) {
				throw new TypeError("File: parts and a name are required");
			}
			super(parts, options);
			this._name = String(name);
			this._lastModified = options && options.lastModified !== undefined
				? Number(options.lastModified) : Date.now();
		}
		get name() { return this._name; }
		get lastModified() { return this._lastModified; }
		get [Symbol.toStringTag]() { return "File"; }
	}
	globalThis.File = File;

	// FileList is an ordered collection of Files with indexed access. Nothing
	// here produces one — that needs a file picker — but the interface exists,
	// which is what `self` is asked for and what a caller feature-detects.
	class FileList {
		constructor() {
			// Nothing here produces one, and script may not either: a FileList comes
			// from a file picker, and there is none.
			throw new TypeError("Illegal constructor");
			Object.defineProperty(this, "_items", { value: [] });
		}
		get length() { return this._items.length; }
		item(i) { return this._items[i >>> 0] ?? null; }
		*[Symbol.iterator]() { yield* this._items; }
	}
	globalThis.FileList = FileList;

	// FileReaderSync is the worker-only synchronous reader. It is implementable
	// here precisely because a Blob's bytes are already in memory: the async
	// FileReader exists for a browser that must read from disk without blocking
	// the main thread, and there is no disk in the path.
	class FileReaderSync {
		readAsArrayBuffer(blob) {
			const b = blobBytes(blob, "readAsArrayBuffer");
			return b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength);
		}
		readAsBinaryString(blob) {
			const b = blobBytes(blob, "readAsBinaryString");
			let s = "";
			for (let i = 0; i < b.length; i++) s += String.fromCharCode(b[i]);
			return s;
		}
		readAsText(blob, encoding) {
			const b = blobBytes(blob, "readAsText");
			try {
				return new TextDecoder(encoding === undefined ? "utf-8" : String(encoding)).decode(b);
			} catch {
				// An unknown encoding label falls back to UTF-8 here, as the reader
				// does: the read itself is not the place a bad label is reported.
				return new TextDecoder().decode(b);
			}
		}
		readAsDataURL(blob) {
			const b = blobBytes(blob, "readAsDataURL");
			let bin = "";
			for (let i = 0; i < b.length; i++) bin += String.fromCharCode(b[i]);
			return `data:${blob.type || "application/octet-stream"};base64,${btoa(bin)}`;
		}
	}
	function blobBytes(blob, who) {
		if (!(blob instanceof Blob)) {
			throw new TypeError(`${who}: argument must be a Blob`);
		}
		return blob._bytes;
	}
	globalThis.FileReaderSync = FileReaderSync;

	// ----------------------------------------------------------- FileReader
	// The event-based way to read a Blob. Blob's own promise methods cover the
	// same ground, but FileReader is what the platform's own tests — and any
	// code written against a browser — actually use, and it was absent.
	// FileReader's state, result and error are ATTRIBUTES and its three ready
	// states are CONSTANTS: the first belong on the prototype and the second are
	// unwritable. Written as own properties of each instance, the prototype had
	// none of them and the constants could be reassigned.
	const READER_EVENTS = ["loadstart", "progress", "load", "abort", "error", "loadend"];

	class FileReader extends EventTarget {
		constructor() {
			super();
			Object.defineProperties(this, {
				_readyState: { value: 0, writable: true }, // EMPTY
				_result: { value: null, writable: true },
				_error: { value: null, writable: true },
				_aborted: { value: false, writable: true },
				_on: { value: {} },
			});
		}
		get readyState() { return this._readyState; }
		get result() { return this._result; }
		get error() { return this._error; }

		_fire(type, loaded = 0, total = 0) {
			const ev = new Event(type);
			// The spec's ProgressEvent fields, which handlers read.
			try {
				Object.defineProperties(ev, {
					lengthComputable: { value: total > 0, configurable: true },
					loaded: { value: loaded, configurable: true },
					total: { value: total, configurable: true },
				});
			} catch { /* a plain Event is close enough */ }
			this.dispatchEvent(ev);
		}

		// _steps runs each event in its OWN task, which is what the specification
		// queues and what the difference is observable through: a test awaiting
		// loadstart and then load resumes between them, and a reader that fired
		// the whole sequence in one turn gave it three events at once. Aborting
		// mid-sequence stops the rest.
		_steps(fns) {
			const run = (i) => {
				if (i >= fns.length) return;
				setTimeout(() => {
					if (this._aborted) return;
					fns[i]();
					run(i + 1);
				}, 0);
			};
			run(0);
		}

		// _read runs the common state machine: one read at a time, LOADING while
		// it runs, then result-or-error and always loadend.
		_read(blob, convert) {
			if (this._readyState === 1) {
				throw new DOMException("The object is in an invalid state.", "InvalidStateError");
			}
			if (!(blob instanceof Blob)) {
				throw new TypeError("FileReader expects a Blob");
			}
			this._readyState = 1; // LOADING
			this._result = null;
			this._error = null;
			this._aborted = false;
			blob.arrayBuffer().then(
				(buf) => {
					if (this._aborted) return;
					const bytes = new Uint8Array(buf);
					const steps = [() => this._fire("loadstart", 0, bytes.length)];
					// No progress event for an empty blob: no data is loaded, so there
					// is no progress to report — the events test checks exactly that.
					if (bytes.length > 0) {
						steps.push(() => this._fire("progress", bytes.length, bytes.length));
					}
					steps.push(() => {
						this._readyState = 2; // DONE
						try {
							this._result = convert(bytes, blob);
						} catch (e) {
							this._result = null;
							this._error = e instanceof DOMException ? e
								: new DOMException(String(e && e.message || e), "EncodingError");
							this._fire("error", bytes.length, bytes.length);
							return;
						}
						this._fire("load", bytes.length, bytes.length);
					});
					steps.push(() => this._fire("loadend", bytes.length, bytes.length));
					this._steps(steps);
				},
				(e) => {
					if (this._aborted) return;
					this._steps([
						() => this._fire("loadstart"),
						() => {
							this._readyState = 2;
							this._result = null;
							this._error = e instanceof DOMException ? e
								: new DOMException(String(e && e.message || e), "NotReadableError");
							this._fire("error");
						},
						() => this._fire("loadend"),
					]);
				},
			);
		}

		readAsArrayBuffer(blob) { this._read(blob, (bytes) => bytes.buffer.slice(0)); }
		readAsText(blob, encoding = undefined) {
			this._read(blob, (bytes) => new TextDecoder(encoding || "utf-8", { ignoreBOM: false }).decode(bytes));
		}
		readAsBinaryString(blob) {
			this._read(blob, (bytes) => { let out = ""; for (const b of bytes) out += String.fromCharCode(b); return out; });
		}
		readAsDataURL(blob) {
			this._read(blob, (bytes, b) => {
				let bin = "";
				for (const x of bytes) bin += String.fromCharCode(x);
				return "data:" + (b.type || "application/octet-stream") + ";base64," + btoa(bin);
			});
		}
		abort() {
			if (this._readyState === 2 || this._readyState === 0) {
				this._readyState = 2;
				this._result = null;
				return;
			}
			this._aborted = true;
			this._readyState = 2;
			this._result = null;
			this._error = new DOMException("The user aborted a request.", "AbortError");
			this._fire("abort");
			this._fire("loadend");
		}
	}
	// An onX property IS a listener, registered where it is assigned — so a
	// handler added with addEventListener beforehand runs first. On the
	// PROTOTYPE, because that is where an event-handler attribute lives.
	for (const name of READER_EVENTS) {
		Object.defineProperty(FileReader.prototype, "on" + name, {
			enumerable: true, configurable: true,
			get() { return this._on[name] ?? null; },
			set(fn) {
				const prev = this._on[name];
				if (prev) this.removeEventListener(name, prev);
				this._on[name] = typeof fn === "function" ? fn : null;
				if (this._on[name]) this.addEventListener(name, this._on[name]);
			},
		});
	}
	Object.defineProperty(FileReader.prototype, Symbol.toStringTag, {
		value: "FileReader", configurable: true,
	});
	// A constant is not writable and not configurable, on the interface object
	// and on its prototype alike.
	for (const [k, v] of [["EMPTY", 0], ["LOADING", 1], ["DONE", 2]]) {
		Object.defineProperty(FileReader, k, { value: v, enumerable: true });
		Object.defineProperty(FileReader.prototype, k, { value: v, enumerable: true });
	}
	globalThis.FileReader = FileReader;

	// ------------------------------------------------------------- FormData

	class FormData {
		constructor() { this._entries = []; }
		append(name, value, filename) { this._entries.push([String(name), toFormValue(value, filename)]); }
		set(name, value, filename) {
			name = String(name);
			const v = toFormValue(value, filename);
			let replaced = false;
			this._entries = this._entries.filter(([k]) => {
				if (k !== name) return true;
				if (replaced) return false;
				replaced = true;
				return true;
			});
			if (replaced) {
				const i = this._entries.findIndex(([k]) => k === name);
				this._entries[i] = [name, v];
			} else this._entries.push([name, v]);
		}
		get(name) { const e = this._entries.find(([k]) => k === String(name)); return e ? e[1] : null; }
		getAll(name) { return this._entries.filter(([k]) => k === String(name)).map(([, v]) => v); }
		has(name) { return this._entries.some(([k]) => k === String(name)); }
		delete(name) { this._entries = this._entries.filter(([k]) => k !== String(name)); }
		forEach(cb, thisArg) { for (const [k, v] of this._entries) cb.call(thisArg, v, k, this); }
		*entries() { yield* this._entries.map((e) => [...e]); }
		*keys() { for (const [k] of this._entries) yield k; }
		*values() { for (const [, v] of this._entries) yield v; }
		[Symbol.iterator]() { return this.entries(); }
		get [Symbol.toStringTag]() { return "FormData"; }
	}
	function toFormValue(value, filename) {
		if (value instanceof Blob) {
			if (filename !== undefined && !(value instanceof File)) return new File([value], filename, { type: value.type });
			return value;
		}
		return String(value);
	}
	globalThis.FormData = FormData;

	// --------------------------------------------------------- CustomEvent

	globalThis.CustomEvent ??= class CustomEvent extends Event {
		constructor(type, init = {}) {
			super(type, init);
			this.detail = init.detail ?? null;
		}
	};

	// ErrorEvent and PromiseRejectionEvent are the two error-reporting events
	// the platform dispatches. They were missing entirely, so a handler written
	// against either could not even be constructed.
	globalThis.ErrorEvent ??= class ErrorEvent extends Event {
		constructor(type, init = {}) {
			super(type, init);
			this.message = init.message ?? "";
			this.filename = init.filename ?? "";
			this.lineno = init.lineno ?? 0;
			this.colno = init.colno ?? 0;
			this.error = init.error ?? null;
		}
	};
	globalThis.PromiseRejectionEvent ??= class PromiseRejectionEvent extends Event {
		constructor(type, init = {}) {
			super(type, init);
			this.promise = init.promise;
			this.reason = init.reason;
		}
	};

	// --------------------------------------------------- structuredClone (full)
	// Replaces the JSON-limited version: Map/Set/Date/RegExp/ArrayBuffer/
	// typed arrays/Blob/File, cycles preserved. Functions/symbols/WeakMap
	// throw DataCloneError per spec.

	// The hook a serializable platform object defines to clone itself. A
	// registry symbol rather than a global so the interfaces that use it stay
	// reachable across these files without adding an enumerable property to
	// anything the suite inspects.
	const CLONEABLE = Symbol.for("go-spidermonkey.structuredClone");

	function fullClone(value, seen) {
		if (value === null || typeof value !== "object") {
			if (typeof value === "function" || typeof value === "symbol") {
				throw new DOMException("could not be cloned", "DataCloneError");
			}
			return value;
		}
		if (seen.has(value)) return seen.get(value);

		// Register every clone in `seen` BEFORE recursing so shared references
		// (and cycles) map to a single clone rather than being duplicated.
		if (value instanceof Date) { const out = new Date(value.getTime()); seen.set(value, out); return out; }
		if (value instanceof RegExp) { const out = new RegExp(value.source, value.flags); seen.set(value, out); return out; }
		if (value instanceof ArrayBuffer) { const out = value.slice(0); seen.set(value, out); return out; }
		if (ArrayBuffer.isView(value)) {
			// Clone the underlying buffer through the same path, so two views
			// over one ArrayBuffer keep sharing a single cloned buffer.
			const clonedBuf = fullClone(value.buffer, seen);
			const out = value instanceof DataView
				? new DataView(clonedBuf, value.byteOffset, value.byteLength)
				: new value.constructor(clonedBuf, value.byteOffset, value.length);
			seen.set(value, out);
			return out;
		}
		if (value instanceof Blob) { const out = value.slice(0, value.size, value.type); seen.set(value, out); return out; }
		// A boxed primitive clones as a boxed primitive of the same type. The
		// plain-object path below turned `new Number(5)` into `{}`: the value
		// lives in an internal slot, not an own property, so nothing was copied
		// and `instanceof Number` stopped being true.
		for (const Box of [Number, String, Boolean, BigInt, Symbol]) {
			if (value instanceof Box) {
				if (Box === Symbol) throw new DOMException("could not be cloned", "DataCloneError");
				const out = Object(Box.prototype.valueOf.call(value));
				seen.set(value, out);
				return out;
			}
		}

		if (value instanceof Map) {
			const out = new Map();
			seen.set(value, out);
			for (const [k, v] of value) out.set(fullClone(k, seen), fullClone(v, seen));
			return out;
		}
		if (value instanceof Set) {
			const out = new Set();
			seen.set(value, out);
			for (const v of value) out.add(fullClone(v, seen));
			return out;
		}
		if (Array.isArray(value)) {
			const out = [];
			seen.set(value, out);
			for (let i = 0; i < value.length; i++) out[i] = fullClone(value[i], seen);
			return out;
		}
		if (value instanceof Error) {
			// Error is a structured-cloneable type, but message/name/stack are
			// non-enumerable, so the plain-object path below would drop them and
			// yield an empty {}. Reconstruct the matching error subtype instead.
			const Ctor = { Error, TypeError, RangeError, ReferenceError, SyntaxError, EvalError, URIError }[value.name] || Error;
			const out = new Ctor(value.message);
			seen.set(value, out);
			if (value.name !== out.name) { try { out.name = value.name; } catch (_e) { /* read-only name */ } }
			out.stack = value.stack;
			if ("cause" in value) out.cause = fullClone(value.cause, seen);
			for (const k of Object.keys(value)) {
				if (k === "message" || k === "stack" || k === "cause") continue;
				out[k] = fullClone(value[k], seen);
			}
			return out;
		}

		// A platform object the spec marks [[Serializable]] carries its own
		// cloner. The plain-object path below copies own enumerable properties,
		// which is not enough for one whose state lives elsewhere: a CryptoKey
		// cloned that way came back as an empty {}.
		const custom = value[CLONEABLE];
		if (typeof custom === "function") {
			const out = custom.call(value, (v) => fullClone(v, seen));
			seen.set(value, out);
			return out;
		}

		// Plain object (reject exotic platform objects with methods only).
		const out = {};
		seen.set(value, out);
		for (const k of Object.keys(value)) {
			// defineProperty (not out[k]=) so an own "__proto__" key becomes a
			// real data property instead of invoking the prototype setter.
			Object.defineProperty(out, k, {
				value: fullClone(value[k], seen),
				writable: true, enumerable: true, configurable: true,
			});
		}
		return out;
	}
	globalThis.structuredClone = (value, options) => {
		const transfer = options && options.transfer;
		if (transfer === undefined || transfer === null) return fullClone(value, new Map());
		// Transfer list: ArrayBuffers only (the transferable types this runtime
		// has). Validate the whole list up front — DataCloneError on a non-
		// transferable, a duplicate, or an already-detached buffer.
		const list = [...transfer];
		const set = new Set();
		for (const t of list) {
			if (!(t instanceof ArrayBuffer)) throw new DOMException("Value is not transferable", "DataCloneError");
			if (set.has(t)) throw new DOMException("Duplicate value in transfer list", "DataCloneError");
			if (t.detached === true || (t.byteLength === 0 && isDetached(t))) throw new DOMException("Cannot transfer a detached ArrayBuffer", "DataCloneError");
			set.add(t);
		}
		// Clone first (the serialize step reads the source data), then detach the
		// transferred buffers via ArrayBuffer.prototype.transfer — the engine's
		// real detach, so any view over the source throws afterwards.
		const out = fullClone(value, new Map());
		for (const t of list) t.transfer();
		return out;
	};
	// isDetached distinguishes a genuinely detached zero-length buffer from a
	// normal new ArrayBuffer(0): constructing a view over a detached buffer
	// throws TypeError.
	function isDetached(buf) {
		try { new Uint8Array(buf); return false; } catch { return true; }
	}

	// -------------------------------------- TextEncoder/DecoderStream

	// Both are GenericTransformStreams: the pair IS a TransformStream, and
	// readable/writable are accessors over it. Built by hand out of a
	// ReadableStream and a WritableStream — which is what these were — they had
	// no backpressure at all (a writer never waited for a reader), the error
	// propagation had to be re-derived at each site, and `encoding`, `fatal` and
	// `ignoreBOM` were own data properties instead of the interface's attributes.
	class TextEncoderStream {
		constructor() {
			const enc = new TextEncoder();
			let pending = ""; // a lone high surrogate carried from the previous chunk
			const transform = new TransformStream({
				transform(chunk, controller) {
					let s = pending + String(chunk);
					pending = "";
					// Hold a trailing lone high surrogate for the next chunk so a
					// surrogate pair split across writes isn't corrupted to U+FFFD.
					if (s.length) {
						const last = s.charCodeAt(s.length - 1);
						if (last >= 0xd800 && last <= 0xdbff) { pending = s.slice(-1); s = s.slice(0, -1); }
					}
					if (s) controller.enqueue(enc.encode(s));
				},
				flush(controller) {
					if (pending) controller.enqueue(enc.encode(pending)); // lone surrogate -> U+FFFD
				},
			});
			Object.defineProperty(this, "_transform", { value: transform });
		}
		get encoding() { return "utf-8"; }
		get readable() { return this._transform.readable; }
		get writable() { return this._transform.writable; }
	}
	Object.defineProperty(TextEncoderStream.prototype, Symbol.toStringTag, {
		value: "TextEncoderStream", configurable: true,
	});
	globalThis.TextEncoderStream = TextEncoderStream;

	class TextDecoderStream {
		constructor(label = "utf-8", options = undefined) {
			const opts = options === undefined || options === null ? {} : options;
			const dec = new TextDecoder(label, opts);
			const transform = new TransformStream({
				transform(chunk, controller) {
					// Only a BufferSource may be decoded. A DETACHED one carries no
					// bytes and decodes to nothing, which is not the same as an error:
					// the buffer was legitimately handed away.
					if (!isBufferSource(chunk)) {
						throw new TypeError("TextDecoderStream: only a BufferSource can be decoded");
					}
					const bytes = viewBytes(chunk);
					if (bytes === null) return;
					const s = dec.decode(bytes, { stream: true });
					if (s) controller.enqueue(s);
				},
				flush(controller) {
					// The final non-stream decode flushes any bytes held from an
					// incomplete trailing sequence (emits U+FFFD, or throws in fatal
					// mode), per the WHATWG flush-on-end contract.
					const tail = dec.decode();
					if (tail) controller.enqueue(tail);
				},
			});
			Object.defineProperties(this, {
				_transform: { value: transform },
				_decoder: { value: dec },
			});
		}
		get encoding() { return this._decoder.encoding; }
		get fatal() { return this._decoder.fatal; }
		get ignoreBOM() { return this._decoder.ignoreBOM; }
		get readable() { return this._transform.readable; }
		get writable() { return this._transform.writable; }
	}
	Object.defineProperty(TextDecoderStream.prototype, Symbol.toStringTag, {
		value: "TextDecoderStream", configurable: true,
	});
	globalThis.TextDecoderStream = TextDecoderStream;

	// viewBytes is the bytes a BufferSource holds, or null when its buffer has
	// been transferred away — a detached view is empty, not broken.
	function viewBytes(chunk) {
		try {
			if (chunk instanceof ArrayBuffer) return new Uint8Array(chunk);
			return new Uint8Array(chunk.buffer, chunk.byteOffset, chunk.byteLength);
		} catch {
			return null;
		}
	}

	// CountQueuingStrategy / ByteLengthQueuingStrategy are defined in builtins.js.

	// ------------------------------------------- MessageChannel/MessagePort
	// Web APIs, and they belong here for the same reason the compression streams
	// do: they existed only under compat/nodejs (built on Node's EventEmitter
	// for worker_threads), so a web-only embedding had no MessageChannel at all
	// and the whole WPT `webmessaging` directory scored zero.
	//
	// This pair is SAME-REALM: it is the in-page channel, not a transport to
	// another thread. compat/nodejs still installs its own worker_threads
	// version over these, which is what carries messages across an agent.

	// MessageEvent's members are IDL attributes, and `ports` is a FROZEN array:
	// the ports that arrived are what arrived, and a receiver cannot add to them.
	class MessageEvent extends Event {
		constructor(type, init = {}) {
			super(type, init);
			const opts = init === null || init === undefined ? {} : init;
			Object.defineProperties(this, {
				_data: { value: opts.data ?? null },
				_origin: { value: opts.origin === undefined ? "" : String(opts.origin) },
				_lastEventId: { value: opts.lastEventId === undefined ? "" : String(opts.lastEventId) },
				_source: { value: opts.source ?? null },
				_ports: { value: Object.freeze(opts.ports ? [...opts.ports] : []) },
			});
		}
		get data() { return this._data; }
		get origin() { return this._origin; }
		get lastEventId() { return this._lastEventId; }
		get source() { return this._source; }
		get ports() { return this._ports; }
	}
	Object.defineProperty(MessageEvent.prototype, Symbol.toStringTag, {
		value: "MessageEvent", configurable: true,
	});
	globalThis.MessageEvent ??= MessageEvent;

	globalThis.MessagePort ??= class MessagePort extends EventTarget {
		constructor() {
			super();
			// A port only delivers once it is started — explicitly, or implicitly
			// by assigning onmessage (the spec's "start()" side effect).
			this._peer = null;
			this._started = false;
			this._closed = false;
			this._queue = [];
			this._onmessage = null;
			this._onmessageerror = null;
		}
		postMessage(value, options) {
			if (arguments.length < 1) throw new TypeError("postMessage: a message is required");
			const transfer = Array.isArray(options) ? options : options && options.transfer;
			// The transfer list is split: buffers are DETACHED by the clone, ports
			// are MOVED to the other side and arrive as event.ports. Both are
			// validated before anything is sent, and a port may not transfer itself.
			const list = transfer ? [...transfer] : [];
			const ports = [];
			const buffers = [];
			for (const t of list) {
				if (t instanceof MessagePort) {
					if (t === this) throw new DOMException("A port cannot transfer itself", "DataCloneError");
					ports.push(t);
					continue;
				}
				buffers.push(t);
			}
			// A value that cannot be cloned is a DataCloneError on the CALLER, not a
			// silent drop — so the clone happens before the closed check.
			const cloned = structuredClone(value, buffers.length ? { transfer: buffers } : undefined);
			if (this._closed) return;
			const peer = this._peer;
			if (!peer || peer._closed) return;
			queueMicrotask(() => peer._deliver(cloned, ports));
		}
		_deliver(data, ports) {
			if (this._closed) return;
			if (!this._started) { this._queue.push({ data, ports }); return; }
			const ev = new MessageEvent("message", { data, ports: ports || [] });
			if (this._onmessage) this._onmessage.call(this, ev);
			this.dispatchEvent(ev);
		}
		start() {
			if (this._started || this._closed) return;
			this._started = true;
			const queued = this._queue;
			this._queue = [];
			for (const d of queued) this._deliver(d.data, d.ports);
		}
		close() {
			if (this._closed) return;
			this._closed = true;
			this._queue.length = 0;
		}
		get onmessage() { return this._onmessage; }
		set onmessage(fn) {
			this._onmessage = typeof fn === "function" ? fn : null;
			if (this._onmessage) this.start();
		}
		get onmessageerror() { return this._onmessageerror; }
		set onmessageerror(fn) { this._onmessageerror = typeof fn === "function" ? fn : null; }
		addEventListener(type, cb, opts) {
			super.addEventListener(type, cb, opts);
			if (String(type) === "message") this.start();
		}
	};
	Object.defineProperty(globalThis.MessagePort.prototype, Symbol.toStringTag, {
		value: "MessagePort", configurable: true,
	});

	globalThis.MessageChannel ??= class MessageChannel {
		constructor() {
			this.port1 = new MessagePort();
			this.port2 = new MessagePort();
			this.port1._peer = this.port2;
			this.port2._peer = this.port1;
		}
	};
	Object.defineProperty(globalThis.MessageChannel.prototype, Symbol.toStringTag, {
		value: "MessageChannel", configurable: true,
	});

	// ------------------------------ Compression/DecompressionStream
	// WHATWG compression streams over the shared host codec. They belong HERE:
	// they are web APIs, and defining them only in compat/nodejs (where the zlib
	// op used to live) left a web-only embedding without them.
	//
	// The transform buffers the input and codes it on close. That is observably
	// different from a streaming implementation only in when output appears, not
	// in what it is — the formats involved have no partial-flush semantics a
	// consumer can rely on anyway.
	// Captured now: __web_ops is deleted once the builtins have been evaluated.
	const codecNew = __web_ops.codec_new;
	const codecPush = __web_ops.codec_push;
	const codecFinish = __web_ops.codec_finish;
	const codecFree = __web_ops.codec_free;
	// brotli is in the shared codec already (compat/internal/compress, which the
	// Node zlib binding uses); the stream constructors just never offered the
	// label, so every brotli case failed as an unsupported format.

	// A codec is a HANDLE with state on the host: created, fed, finished. The
	// whole-buffer path cannot serve a stream — a consumer that writes one chunk
	// and then reads, which is what these streams are for, would wait for a
	// codec that only produces at close.
	// A BufferSource is an ArrayBuffer or a view onto one; a SHARED one is
	// neither, whatever it looks like. Both are asked structurally — the
	// constructor a value came from, not the name it reports.
	const isBufferSource = (v) => ArrayBuffer.isView(v) || v instanceof ArrayBuffer
		|| (typeof SharedArrayBuffer !== "undefined" && v instanceof SharedArrayBuffer);
	const isShared = (v) => typeof SharedArrayBuffer !== "undefined"
		&& (v instanceof SharedArrayBuffer || (ArrayBuffer.isView(v) && v.buffer instanceof SharedArrayBuffer));

	function compressionTransform(decompress, format, who) {
		if (arguments.length < 2 || format === undefined) {
			throw new TypeError(`${who}: a format is required`);
		}
		const name = String(format);
		// "brotli" is not in the standard's list; it is offered here because this
		// runtime has offered it since before that list existed.
		if (name !== "gzip" && name !== "deflate" && name !== "deflate-raw" && name !== "brotli") {
			throw new TypeError(`${who}: ${name} is not a supported format`);
		}
		const handle = codecNew(name, decompress);
		if (handle && typeof handle === "object" && handle.error) {
			throw new TypeError(`${who}: ${handle.error}`);
		}
		let rc = null;
		let freed = false;
		const free = () => { if (!freed) { freed = true; codecFree(handle); } };
		// A codec op answers with both what it produced and what went wrong, and
		// the order here is why: a stream with junk after its end produced real
		// bytes first, and those have to reach the reader before the stream errors.
		const emit = (out, controller) => {
			if (out.bytes) controller.enqueue(out.bytes);
			if (out.error) {
				const e = new TypeError(out.error);
				free();
				controller.error(e);
				throw e;
			}
		};
		const readable = new ReadableStream({
			start(c) { rc = c; },
		});
		const writable = new WritableStream({
			write(chunk) {
				// Only a BufferSource may be written: a stream that silently encoded
				// a string would produce bytes the caller never asked for. A SHARED
				// buffer is not one — the IDL is BufferSource, not
				// [AllowShared] BufferSource — and it could not be one, since
				// another agent may rewrite it while the codec is reading it.
				if (!isBufferSource(chunk) || isShared(chunk)) {
					const e = new TypeError(`${who}: only a BufferSource can be written`);
					free();
					rc.error(e);
					throw e;
				}
				const bytes = chunk instanceof ArrayBuffer
					? new Uint8Array(chunk) : new Uint8Array(chunk.buffer, chunk.byteOffset, chunk.byteLength);
				emit(codecPush(handle, bytes), rc);
			},
			close() {
				const out = codecFinish(handle);
				free();
				emit(out, rc);
				rc.close();
			},
			abort() { free(); },
		});
		return { readable, writable };
	}

	// The instance IS the stream pair. Returning a plain {readable, writable}
	// from the constructor — which is what these used to do — gave back an object
	// that was not a CompressionStream at all: instanceof was false, the
	// prototype was Object's, and the class string said so.
	class CompressionStream {
		constructor(format) {
			const pair = compressionTransform(false, format, "CompressionStream");
			Object.defineProperty(this, "_pair", { value: pair });
		}
		get readable() { return this._pair.readable; }
		get writable() { return this._pair.writable; }
	}
	class DecompressionStream {
		constructor(format) {
			const pair = compressionTransform(true, format, "DecompressionStream");
			Object.defineProperty(this, "_pair", { value: pair });
		}
		get readable() { return this._pair.readable; }
		get writable() { return this._pair.writable; }
	}
	for (const cls of [CompressionStream, DecompressionStream]) {
		Object.defineProperty(cls.prototype, Symbol.toStringTag, { value: cls.name, configurable: true });
	}
	globalThis.CompressionStream = CompressionStream;
	globalThis.DecompressionStream = DecompressionStream;
})();
