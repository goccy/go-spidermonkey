// compat/nodejs: pure-JS API completions layered onto the core modules —
// the "closing the diff against real Node" pass (docs/compat-gaps.md).
// Evaluated after http.js, while the __node_core_registry is still present.
(() => {
	"use strict";
	const core = globalThis.__node_core_registry;

	// -------------------------------------------------------- alias modules

	core["assert/strict"] = core.assert.strict;
	core["dns/promises"] = core.dns.promises;
	core.sys = core.util;
	core.path.posix = core.path;
	core.path.win32 = core.path;
	core["path/posix"] = core.path;
	core["path/win32"] = core.path;
	core["readline/promises"] = { createInterface: core.readline.createInterface };
	core["inspector/promises"] = core.inspector;

	// util/types is already core.util.types; expose it as a module too.
	core["util/types"] = core.util.types;

	// stream/consumers: drain a stream (or async iterable) to a whole value.
	async function collect(stream) {
		const chunks = [];
		for await (const chunk of stream) chunks.push(typeof chunk === "string" ? Buffer.from(chunk) : chunk);
		return Buffer.concat(chunks);
	}
	core["stream/consumers"] = {
		buffer: (s) => collect(s),
		arrayBuffer: async (s) => { const b = await collect(s); return b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength); },
		text: async (s) => (await collect(s)).toString("utf8"),
		json: async (s) => JSON.parse((await collect(s)).toString("utf8")),
		blob: async (s) => new Blob([await collect(s)]),
	};

	// --------------------------------------------------------------- console
	// Augment the plain console with the grouping/timing/table surface.

	const con = globalThis.console;
	const counts = new Map();
	const timers = new Map();
	let groupIndent = "";
	const writeOut = (s) => con.log(groupIndent + String(s).replace(/\n/g, "\n" + groupIndent));

	Object.assign(con, {
		dir: (obj, opts) => writeOut(core.util.inspect(obj, opts || {})),
		dirxml: (...a) => con.log(...a),
		trace: (...a) => con.error("Trace:", ...a),
		group: (...a) => { if (a.length) con.log(...a); groupIndent += "  "; },
		groupCollapsed: (...a) => { if (a.length) con.log(...a); groupIndent += "  "; },
		groupEnd: () => { groupIndent = groupIndent.slice(0, -2); },
		count: (label = "default") => {
			const n = (counts.get(label) || 0) + 1;
			counts.set(label, n);
			writeOut(`${label}: ${n}`);
		},
		countReset: (label = "default") => counts.delete(label),
		time: (label = "default") => timers.set(label, performance.now()),
		timeEnd: (label = "default") => {
			const t = timers.get(label);
			if (t !== undefined) { writeOut(`${label}: ${(performance.now() - t).toFixed(3)}ms`); timers.delete(label); }
		},
		timeLog: (label = "default", ...a) => {
			const t = timers.get(label);
			if (t !== undefined) writeOut(`${label}: ${(performance.now() - t).toFixed(3)}ms ${a.join(" ")}`);
		},
		timeStamp: () => {},
		clear: () => {},
		table: (data) => {
			// Minimal renderer: rows as objects/arrays -> aligned columns.
			if (data === null || typeof data !== "object") return writeOut(String(data));
			const rows = Array.isArray(data) ? data.map((v, i) => [String(i), v]) : Object.entries(data);
			const cols = new Set();
			for (const [, v] of rows) {
				if (v && typeof v === "object") for (const k of Object.keys(v)) cols.add(k);
			}
			const header = ["(index)", ...cols, ...(rows.some(([, v]) => v === null || typeof v !== "object") ? ["Values"] : [])];
			const lines = [header.join(" | ")];
			for (const [idx, v] of rows) {
				const cells = [idx];
				for (const c of cols) cells.push(v && typeof v === "object" && c in v ? core.util.inspect(v[c]) : "");
				if (header.includes("Values")) cells.push(v && typeof v === "object" ? "" : core.util.inspect(v));
				lines.push(cells.join(" | "));
			}
			writeOut(lines.join("\n"));
		},
	});
	con.Console = function Console() { return con; };
	core.console = con;

	// ---------------------------------------------------------------- events

	const EventEmitter = core.events;
	EventEmitter.errorMonitor = Symbol("events.errorMonitor");
	EventEmitter.captureRejectionSymbol = Symbol.for("nodejs.rejection");
	EventEmitter.getEventListeners = (emitter, name) => emitter.listeners(name);
	EventEmitter.getMaxListeners = (emitter) => emitter.getMaxListeners();
	EventEmitter.setMaxListeners = (n, ...emitters) => { for (const e of emitters) e.setMaxListeners(n); };
	EventEmitter.listenerCount = (emitter, name) => emitter.listenerCount(name);
	// on(emitter, name): async iterator of events.
	EventEmitter.on = function on(emitter, name, options = {}) {
		const queue = [];
		let error = null, wake = null, finished = false;
		const push = (v) => { queue.push(v); if (wake) { wake(); wake = null; } };
		const handler = (...args) => push(args);
		const errHandler = (err) => { error = err; if (wake) { wake(); wake = null; } };
		emitter.on(name, handler);
		emitter.on("error", errHandler);
		const signal = options.signal;
		if (signal) signal.addEventListener("abort", () => { finished = true; if (wake) { wake(); wake = null; } });
		return {
			async next() {
				if (queue.length) return { value: queue.shift(), done: false };
				if (error) throw error;
				if (finished || (signal && signal.aborted)) return { value: undefined, done: true };
				await new Promise((res) => { wake = res; });
				return this.next();
			},
			return() {
				emitter.off(name, handler);
				emitter.off("error", errHandler);
				finished = true;
				return Promise.resolve({ value: undefined, done: true });
			},
			[Symbol.asyncIterator]() { return this; },
		};
	};

	// ------------------------------------------------------------------ util

	const util = core.util;
	util.isArray = Array.isArray;
	util.toUSVString = (s) => String(s);
	util.stripVTControlCharacters = (s) => String(s).replace(/\x1b\[[0-9;]*m/g, "");
	util.formatWithOptions = (opts, ...args) => util.format(...args);
	util.debug = util.debuglog;
	util.getSystemErrorName = (n) => `Unknown system error ${n}`;

	const STYLES = {
		reset: [0, 0], bold: [1, 22], italic: [3, 23], underline: [4, 24],
		inverse: [7, 27], red: [31, 39], green: [32, 39], yellow: [33, 39],
		blue: [34, 39], magenta: [35, 39], cyan: [36, 39], white: [37, 39],
		gray: [90, 39], grey: [90, 39],
	};
	util.styleText = (format, text) => {
		const list = Array.isArray(format) ? format : [format];
		let open = "", close = "";
		for (const f of list) {
			const s = STYLES[f];
			if (!s) throw new TypeError(`Invalid style: ${f}`);
			open += `\x1b[${s[0]}m`;
			close = `\x1b[${s[1]}m` + close;
		}
		return open + text + close;
	};

	// util.parseArgs — the common subset (options with type string|boolean,
	// short, multiple, default; positionals; strict).
	util.parseArgs = (config = {}) => {
		const args = config.args ?? process.argv.slice(2);
		const options = config.options ?? {};
		const allowPositionals = config.allowPositionals ?? !config.strict === false ? config.allowPositionals ?? false : false;
		const strict = config.strict ?? true;
		const shortMap = {};
		for (const [name, o] of Object.entries(options)) if (o.short) shortMap[o.short] = name;
		const values = {};
		const positionals = [];
		for (const [name, o] of Object.entries(options)) if (o.multiple) values[name] = [];

		const setValue = (name, val) => {
			const o = options[name];
			if (!o && strict) throw new Error(`Unknown option '--${name}'`);
			const v = o && o.type === "boolean" ? true : val;
			if (o && o.multiple) values[name].push(v);
			else values[name] = v;
		};
		for (let i = 0; i < args.length; i++) {
			let arg = args[i];
			if (arg === "--") { positionals.push(...args.slice(i + 1)); break; }
			if (arg.startsWith("--")) {
				let name = arg.slice(2), val;
				const eq = name.indexOf("=");
				if (eq >= 0) { val = name.slice(eq + 1); name = name.slice(0, eq); }
				const o = options[name];
				if (o && o.type !== "boolean" && val === undefined) val = args[++i];
				setValue(name, val);
			} else if (arg.startsWith("-") && arg.length > 1) {
				for (let j = 1; j < arg.length; j++) {
					const name = shortMap[arg[j]];
					if (!name && strict) throw new Error(`Unknown option '-${arg[j]}'`);
					const o = options[name];
					if (o && o.type !== "boolean") { setValue(name, arg.slice(j + 1) || args[++i]); break; }
					setValue(name || arg[j], true);
				}
			} else {
				if (!allowPositionals && strict) throw new Error(`Unexpected argument '${arg}'`);
				positionals.push(arg);
			}
		}
		// Fill declared defaults.
		for (const [name, o] of Object.entries(options)) {
			if (!(name in values) && "default" in o) values[name] = o.default;
		}
		return { values, positionals };
	};

	class MIMEParams {
		constructor() { this._map = new Map(); }
		get(k) { return this._map.has(k) ? this._map.get(k) : null; }
		set(k, v) { this._map.set(k, String(v)); }
		has(k) { return this._map.has(k); }
		delete(k) { this._map.delete(k); }
		entries() { return this._map.entries(); }
		keys() { return this._map.keys(); }
		values() { return this._map.values(); }
		[Symbol.iterator]() { return this._map.entries(); }
		toString() { return [...this._map].map(([k, v]) => `;${k}=${v}`).join(""); }
	}
	class MIMEType {
		constructor(input) {
			const s = String(input);
			const semi = s.indexOf(";");
			const essence = (semi < 0 ? s : s.slice(0, semi)).trim().toLowerCase();
			const slash = essence.indexOf("/");
			if (slash < 0) throw new TypeError(`Invalid MIME type: ${input}`);
			this.type = essence.slice(0, slash);
			this.subtype = essence.slice(slash + 1);
			this.params = new MIMEParams();
			if (semi >= 0) {
				for (const part of s.slice(semi + 1).split(";")) {
					const eq = part.indexOf("=");
					if (eq >= 0) this.params.set(part.slice(0, eq).trim().toLowerCase(), part.slice(eq + 1).trim().replace(/^"|"$/g, ""));
				}
			}
		}
		get essence() { return `${this.type}/${this.subtype}`; }
		toString() { return this.essence + this.params.toString(); }
	}
	util.MIMEType = MIMEType;
	util.MIMEParams = MIMEParams;

	// ------------------------------------------------------------------ path

	core.path.matchesGlob = (p, pattern) => {
		// Minimal glob: * (not /), ** (any), ? (one). Enough for basic checks.
		const re = "^" + String(pattern)
			.replace(/[.+^${}()|[\]\\]/g, "\\$&")
			.replace(/\*\*/g, "\0")
			.replace(/\*/g, "[^/]*")
			.replace(/\0/g, ".*")
			.replace(/\?/g, "[^/]") + "$";
		return new RegExp(re).test(String(p));
	};
	core.path.toNamespacedPath = (p) => p;

	// ----------------------------------------------------------- querystring

	core.querystring.decode = core.querystring.parse;
	core.querystring.encode = core.querystring.stringify;

	// -------------------------------------------------------------------- os

	core.os.devNull = "/dev/null";
	core.os.getPriority = () => 0;
	core.os.setPriority = () => {};
	core.os.constants = {
		signals: { SIGHUP: 1, SIGINT: 2, SIGQUIT: 3, SIGKILL: 9, SIGTERM: 15 },
		errno: {},
		priority: { PRIORITY_LOW: 19, PRIORITY_NORMAL: 0, PRIORITY_HIGH: -14 },
	};

	// ---------------------------------------------------------------- stream

	const stream = core.stream;
	const isStreamLike = (o, kind) => o && typeof o === "object" && typeof o[kind] === "function";
	stream.isReadable = (o) => !!(o && o.readable && (o._rs ? !o._rs.endEmitted : true));
	stream.isWritable = (o) => !!(o && o.writable);
	stream.isErrored = (o) => !!(o && o.errored);
	stream.isDestroyed = (o) => !!(o && o.destroyed);
	stream.isDisturbed = (o) => !!(o && (o._rs ? o._rs.consumed || o._rs.flowing !== null : false));
	stream.getDefaultHighWaterMark = () => 16384;
	stream.setDefaultHighWaterMark = () => {};
	stream.addAbortSignal = (signal, s) => {
		if (signal.aborted) s.destroy(new Error("The operation was aborted"));
		else signal.addEventListener("abort", () => s.destroy(new Error("The operation was aborted")));
		return s;
	};
	stream.promises = core["stream/promises"];

	// -------------------------------------------------------- timers/promises

	core["timers/promises"].setInterval = async function* (delay, value) {
		while (true) {
			await new Promise((res) => setTimeout(res, delay));
			yield value;
		}
	};
	core["timers/promises"].scheduler = {
		wait: (ms) => new Promise((res) => setTimeout(res, ms)),
		yield: () => new Promise((res) => setImmediate(res)),
	};
	core.timers.promises = core["timers/promises"];

	// ------------------------------------------------------------ process ++

	let refCount = 0;
	process.ref = () => { refCount++; };
	process.unref = () => { refCount--; };
	process.getBuiltinModule = (name) => {
		try { return globalThis.__node_core(name); } catch { return undefined; }
	};
	process.availableMemory = () => 0;
	process.constrainedMemory = () => 0;
	process.getActiveResourcesInfo = () => [];
	process.abort = () => { throw new Error("process.abort() called"); };
	process.setSourceMapsEnabled = () => {};

	// ------------------------------------------------------------- buffer ++

	const Buffer = globalThis.Buffer;
	Buffer.prototype.toString; // ensure prototype touched
	const bufferMod = core.buffer;
	bufferMod.INSPECT_MAX_BYTES = 50;
	bufferMod.isAscii = (input) => {
		const u8 = input instanceof Uint8Array ? input : new Uint8Array(input);
		for (const b of u8) if (b > 0x7f) return false;
		return true;
	};
	bufferMod.isUtf8 = (input) => {
		const u8 = input instanceof Uint8Array ? input : new Uint8Array(input);
		try {
			new TextDecoder("utf-8", { fatal: true }).decode(u8);
			return true;
		} catch { return false; }
	};
	bufferMod.transcode = (source, from, to) => Buffer.from(Buffer.from(source).toString(from), to);

	// Blob/File live on globalThis (compat/web); mirror onto node:buffer.
	if (globalThis.Blob) bufferMod.Blob = globalThis.Blob;
	if (globalThis.File) bufferMod.File = globalThis.File;

	// ----------------------------- Compression/DecompressionStream
	// WHATWG streams backed by node:zlib (needs the host op, so defined here
	// rather than in compat/web). gzip/deflate/deflate-raw.

	const zlib = core.zlib;
	const zlibFor = { gzip: "gzip", deflate: "deflate", "deflate-raw": "deflateRaw" };
	const unzlibFor = { gzip: "gunzip", deflate: "inflate", "deflate-raw": "inflateRaw" };
	function makeCompressionStream(map, format) {
		const method = map[format];
		if (!method) throw new TypeError(`Unsupported format: ${format}`);
		const chunks = [];
		let rc;
		const readable = new ReadableStream({ start(c) { rc = c; } });
		const writable = new WritableStream({
			write(chunk) { chunks.push(chunk instanceof Uint8Array ? chunk : new Uint8Array(chunk)); },
			close() {
				const total = chunks.reduce((n, c) => n + c.length, 0);
				const joined = new Uint8Array(total);
				let off = 0;
				for (const c of chunks) { joined.set(c, off); off += c.length; }
				rc.enqueue(new Uint8Array(zlib[method + "Sync"] ? zlib[method + "Sync"](joined) : zlib[method](joined)));
				rc.close();
			},
		});
		return { readable, writable };
	}
	globalThis.CompressionStream = class CompressionStream {
		constructor(format) { return makeCompressionStream(zlibFor, format); }
	};
	globalThis.DecompressionStream = class DecompressionStream {
		constructor(format) { return makeCompressionStream(unzlibFor, format); }
	};

	delete globalThis.__node_core_registry;
})();
