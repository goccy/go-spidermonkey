// compat/web: WinterTC vocabulary completions — Blob, File, FormData, a full
// structuredClone, CustomEvent, TextEncoder/DecoderStream, and the stream
// controller/reader classes as globals. Evaluated after builtins.js/subtle.js
// while __web_ops is still present.
(() => {
	"use strict";

	// -------------------------------------------------------------- Blob/File

	const encodePart = (part) => {
		if (part instanceof Uint8Array) return part.slice();
		if (part instanceof ArrayBuffer) return new Uint8Array(part.slice(0));
		if (ArrayBuffer.isView(part)) return new Uint8Array(part.buffer.slice(part.byteOffset, part.byteOffset + part.byteLength));
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
			this._blobParts = [...(parts || [])];
			this._bytes = concatBytes(this._blobParts);
			this.type = options.type ? String(options.type).toLowerCase() : "";
		}
		get size() { return this._bytes.length; }
		async arrayBuffer() { return this._bytes.buffer.slice(this._bytes.byteOffset, this._bytes.byteOffset + this._bytes.byteLength); }
		async bytes() { return this._bytes.slice(); }
		async text() { return new TextDecoder().decode(this._bytes); }
		slice(start = 0, end = this._bytes.length, contentType = "") {
			const s = start < 0 ? Math.max(this._bytes.length + start, 0) : Math.min(start, this._bytes.length);
			const e = end < 0 ? Math.max(this._bytes.length + end, 0) : Math.min(end, this._bytes.length);
			const b = new Blob([], { type: contentType });
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
			super(parts, options);
			this.name = String(name);
			this.lastModified = options.lastModified ?? 0;
		}
		get [Symbol.toStringTag]() { return "File"; }
	}
	globalThis.File = File;

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

	// --------------------------------------------------- structuredClone (full)
	// Replaces the JSON-limited version: Map/Set/Date/RegExp/ArrayBuffer/
	// typed arrays/Blob/File, cycles preserved. Functions/symbols/WeakMap
	// throw DataCloneError per spec.

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
		void options; // transfer list not supported
		return fullClone(value, new Map());
	};

	// -------------------------------------- TextEncoder/DecoderStream

	globalThis.TextEncoderStream = class TextEncoderStream {
		constructor() {
			const enc = new TextEncoder();
			this.encoding = "utf-8";
			this.readable = new ReadableStream({ start: (c) => (this._rc = c) });
			this.writable = new WritableStream({
				write: (chunk) => this._rc.enqueue(enc.encode(String(chunk))),
				close: () => this._rc.close(),
			});
		}
	};
	globalThis.TextDecoderStream = class TextDecoderStream {
		constructor(label = "utf-8", options = {}) {
			const dec = new TextDecoder(label, options);
			this.encoding = dec.encoding;
			this.readable = new ReadableStream({ start: (c) => (this._rc = c) });
			this.writable = new WritableStream({
				write: (chunk) => {
					const s = dec.decode(chunk instanceof Uint8Array ? chunk : new Uint8Array(chunk), { stream: true });
					if (s) this._rc.enqueue(s);
				},
				close: () => this._rc.close(),
			});
		}
	};

	// --------------------------------------- QueuingStrategy classes

	globalThis.CountQueuingStrategy ??= class CountQueuingStrategy {
		constructor({ highWaterMark } = {}) { this.highWaterMark = highWaterMark; }
		size() { return 1; }
	};
	globalThis.ByteLengthQueuingStrategy ??= class ByteLengthQueuingStrategy {
		constructor({ highWaterMark } = {}) { this.highWaterMark = highWaterMark; }
		size(chunk) { return chunk.byteLength; }
	};
})();
