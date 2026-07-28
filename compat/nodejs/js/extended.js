// compat/nodejs: pure-JS API completions layered onto the core modules —
// the "closing the diff against real Node" pass (docs/compat-gaps.md).
// Evaluated after http.js, while the __node_core_registry is still present.
(() => {
	"use strict";
	const core = globalThis.__node_core_registry;

	// -------------------------------------------------------- alias modules

	core["dns/promises"] = core.dns.promises;
	core.sys = core.util;
	// path.posix/path.win32 (distinct objects) are wired in corelibs.js;
	// expose them as their own modules here.
	core["path/posix"] = core.path.posix;
	core["path/win32"] = core.path.win32;
	// readline/promises: same Interface, but question() returns a Promise instead
	// of taking a callback (Node's promises variant).
	core["readline/promises"] = {
		createInterface: (opts) => {
			const rl = core.readline.createInterface(opts);
			const cbQuestion = rl.question.bind(rl);
			rl.question = (query, options) => new Promise((resolve, reject) => {
				const abortErr = () => (options.signal.reason || Object.assign(new Error("The operation was aborted"), { name: "AbortError" }));
				if (options && options.signal) {
					if (options.signal.aborted) return reject(abortErr());
					options.signal.addEventListener("abort", () => {
						// Detach the pending question, or the next input line is routed
						// into the settled resolve and swallowed instead of emitted as
						// a 'line' event.
						if (rl._questionCb === resolve) rl._questionCb = null;
						reject(abortErr());
					}, { once: true });
				}
				cbQuestion(query, resolve);
			});
			return rl;
		},
	};
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
	// Keep the raw sinks; the public log/info/warn/error/debug re-apply the
	// current group indent to every line.
	const rawLog = con.log.bind(con);
	const rawErr = con.error.bind(con);
	const indent = (args) => {
		// Route through util.format so printf specifiers (%s/%d/%j/...) substitute,
		// matching Node's console.
		const text = core.util.format(...args);
		return groupIndent ? groupIndent + text.replace(/\n/g, "\n" + groupIndent) : text;
	};
	const writeOut = (s) => rawLog(groupIndent + String(s).replace(/\n/g, "\n" + groupIndent));

	Object.assign(con, {
		log: (...a) => rawLog(indent(a)),
		info: (...a) => rawLog(indent(a)),
		debug: (...a) => rawLog(indent(a)),
		warn: (...a) => rawErr(indent(a)),
		error: (...a) => rawErr(indent(a)),
		dir: (obj, opts) => writeOut(core.util.inspect(obj, opts || {})),
		dirxml: (...a) => con.log(...a),
		trace: (...a) => con.error("Trace:", ...a),
		group: (...a) => { if (a.length) rawLog(indent(a)); groupIndent += "  "; },
		groupCollapsed: (...a) => { if (a.length) rawLog(indent(a)); groupIndent += "  "; },
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
	// errorMonitor is defined in corelibs (emit() consults it); do not replace
	// the symbol here or listeners registered against the original would be
	// orphaned.
	EventEmitter.captureRejectionSymbol = Symbol.for("nodejs.rejection");
	EventEmitter.getEventListeners = (emitter, name) => emitter.listeners(name);
	EventEmitter.getMaxListeners = (emitter) => emitter.getMaxListeners();
	EventEmitter.setMaxListeners = (n, ...emitters) => { for (const e of emitters) e.setMaxListeners(n); };
	EventEmitter.listenerCount = (emitter, name) => emitter.listenerCount(name);
	// on(emitter, name): async iterator of events.
	EventEmitter.on = function on(emitter, name, options = {}) {
		const makeAbortError = (signal) => {
			const e = new Error("The operation was aborted");
			e.name = "AbortError";
			e.code = "ABORT_ERR";
			if (signal && signal.reason !== undefined) e.cause = signal.reason;
			return e;
		};
		const signal = options.signal;
		// Node throws synchronously on an already-aborted signal.
		if (signal && signal.aborted) throw makeAbortError(signal);
		const queue = [];
		let error = null, wake = null, finished = false;
		const push = (v) => { queue.push(v); if (wake) { wake(); wake = null; } };
		const handler = (...args) => push(args);
		const errHandler = (err) => { error = err; if (wake) { wake(); wake = null; } };
		emitter.on(name, handler);
		emitter.on("error", errHandler);
		// Abort REJECTS the pending (and any subsequent) next() with an
		// AbortError — it must surface in the for-await consumer's catch, not
		// end the iteration cleanly.
		if (signal) signal.addEventListener("abort", () => {
			error = makeAbortError(signal);
			emitter.off(name, handler);
			emitter.off("error", errHandler);
			if (wake) { wake(); wake = null; }
		}, { once: true });
		return {
			async next() {
				if (queue.length) return { value: queue.shift(), done: false };
				if (error) throw error;
				if (finished) return { value: undefined, done: true };
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

	// Node's full util.inspect.colors table. A missing entry is not cosmetic:
	// styleText THROWS on an unknown style, so one gap turns a library's
	// colourized output into an exception — @babel/code-frame asks for bgRed
	// while formatting a syntax error, and the TypeError replaced the real error
	// Babel was reporting.
	const STYLES = {
		reset: [0, 0], bold: [1, 22], dim: [2, 22], italic: [3, 23],
		underline: [4, 24], blink: [5, 25], inverse: [7, 27], hidden: [8, 28],
		strikethrough: [9, 29], doubleunderline: [21, 24], framed: [51, 54],
		overlined: [53, 55],
		black: [30, 39], red: [31, 39], green: [32, 39], yellow: [33, 39],
		blue: [34, 39], magenta: [35, 39], cyan: [36, 39], white: [37, 39],
		gray: [90, 39], grey: [90, 39], blackBright: [90, 39],
		redBright: [91, 39], greenBright: [92, 39], yellowBright: [93, 39],
		blueBright: [94, 39], magentaBright: [95, 39], cyanBright: [96, 39],
		whiteBright: [97, 39],
		bgBlack: [40, 49], bgRed: [41, 49], bgGreen: [42, 49],
		bgYellow: [43, 49], bgBlue: [44, 49], bgMagenta: [45, 49],
		bgCyan: [46, 49], bgWhite: [47, 49], bgGray: [100, 49],
		bgGrey: [100, 49], bgBlackBright: [100, 49], bgRedBright: [101, 49],
		bgGreenBright: [102, 49], bgYellowBright: [103, 49],
		bgBlueBright: [104, 49], bgMagentaBright: [105, 49],
		bgCyanBright: [106, 49], bgWhiteBright: [107, 49],
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

	// util.getCallSites (Node >= 22): the current call stack as structured
	// frames. Node builds it from V8's structured API; there is no equivalent
	// here, so it is parsed out of the engine's own stack string
	// ("name@file:line:col" per frame, "@file:line:col" for an anonymous one).
	// The frame for this function itself is dropped, as Node drops its own.
	util.getCallSites = (frameCount, options) => {
		if (typeof frameCount === "object" && frameCount !== null) {
			options = frameCount;
			frameCount = undefined;
		}
		const limit = typeof frameCount === "number" && frameCount > 0 ? frameCount : 10;
		const frames = String(new Error().stack || "").split("\n").slice(1);
		const out = [];
		for (const line of frames) {
			const at = line.lastIndexOf("@");
			if (at < 0) continue;
			const m = /^(.*):(\d+):(\d+)$/.exec(line.slice(at + 1));
			if (!m) continue;
			out.push({
				functionName: line.slice(0, at),
				scriptId: "0",
				scriptName: m[1],
				lineNumber: Number(m[2]),
				columnNumber: Number(m[3]),
				column: Number(m[3]),
			});
			if (out.length >= limit) break;
		}
		return out;
	};

	// util.parseArgs — the common subset (options with type string|boolean,
	// short, multiple, default; positionals; strict).
	util.parseArgs = (config = {}) => {
		const args = config.args ?? process.argv.slice(2);
		const options = config.options ?? {};
		const strict = config.strict ?? true;
		// Node: allowPositionals defaults to true when strict is false, else false.
		const allowPositionals = config.allowPositionals ?? !strict;
		const wantTokens = config.tokens === true;
		const shortMap = {};
		for (const [name, o] of Object.entries(options)) if (o.short) shortMap[o.short] = name;
		const values = {};
		const positionals = [];
		const tokens = [];
		for (const [name, o] of Object.entries(options)) if (o.multiple) values[name] = [];

		const setValue = (name, val) => {
			const o = options[name];
			if (!o && strict) throw new Error(`Unknown option '--${name}'`);
			const v = o && o.type === "boolean" ? true : val;
			if (o && o.multiple) values[name].push(v);
			else values[name] = v;
		};
		// index is the position in args; rawName is the flag as written ("--foo"/
		// "-f"); value/inlineValue follow Node's token shape (value undefined and
		// inlineValue undefined for boolean options).
		const pushOptionToken = (index, name, rawName, value, inlineValue) => {
			tokens.push({ kind: "option", index, name, rawName, value, inlineValue });
		};
		for (let i = 0; i < args.length; i++) {
			let arg = args[i];
			if (arg === "--") {
				tokens.push({ kind: "option-terminator", index: i });
				for (let k = i + 1; k < args.length; k++) {
					positionals.push(args[k]);
					tokens.push({ kind: "positional", index: k, value: args[k] });
				}
				break;
			}
			if (arg.startsWith("--")) {
				const index = i;
				let name = arg.slice(2), val, inline = false;
				const eq = name.indexOf("=");
				if (eq >= 0) { val = name.slice(eq + 1); name = name.slice(0, eq); inline = true; }
				const o = options[name];
				if (o && o.type !== "boolean" && val === undefined) val = args[++i];
				setValue(name, val);
				if (o && o.type === "boolean") pushOptionToken(index, name, `--${name}`, undefined, undefined);
				else pushOptionToken(index, name, `--${name}`, val, inline);
			} else if (arg.startsWith("-") && arg.length > 1) {
				const index = i;
				for (let j = 1; j < arg.length; j++) {
					const name = shortMap[arg[j]];
					if (!name && strict) throw new Error(`Unknown option '-${arg[j]}'`);
					const o = options[name];
					if (o && o.type !== "boolean") {
						const rest = arg.slice(j + 1);
						const val = rest || args[++i];
						setValue(name, val);
						pushOptionToken(index, name, `-${arg[j]}`, val, rest !== "");
						break;
					}
					setValue(name || arg[j], true);
					pushOptionToken(index, name || arg[j], `-${arg[j]}`, undefined, undefined);
				}
			} else {
				if (!allowPositionals && strict) throw new Error(`Unexpected argument '${arg}'`);
				positionals.push(arg);
				tokens.push({ kind: "positional", index: i, value: arg });
			}
		}
		// Fill declared defaults.
		for (const [name, o] of Object.entries(options)) {
			if (!(name in values) && "default" in o) values[name] = o.default;
		}
		const result = { values, positionals };
		if (wantTokens) result.tokens = tokens;
		return result;
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
	core.path.win32.matchesGlob = core.path.matchesGlob;

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

	core["timers/promises"].setInterval = async function* (delay, value, options = {}) {
		const signal = options.signal;
		const abortErr = () => (signal && signal.reason) || Object.assign(new Error("The operation was aborted"), { name: "AbortError" });
		while (true) {
			// Re-check on EVERY iteration: an abort landing while the consumer's
			// for-await body runs (between ticks) has no pending wait to reject,
			// and re-adding an 'abort' listener to an already-aborted signal
			// would never fire.
			if (signal && signal.aborted) throw abortErr();
			// Honor an AbortSignal: reject the pending tick so a for-await loop exits.
			// Remove the per-tick listener when the tick resolves normally, or a
			// long-running interval accumulates one stale listener per tick.
			await new Promise((res, rej) => {
				let t;
				const onAbort = () => { clearTimeout(t); rej(abortErr()); };
				t = setTimeout(() => { if (signal) signal.removeEventListener("abort", onAbort); res(); }, delay);
				if (signal) signal.addEventListener("abort", onAbort, { once: true });
			});
			yield value;
		}
	};
	core["timers/promises"].scheduler = {
		// scheduler.wait(ms, { signal }) is abortable exactly like
		// timers/promises setTimeout (which already wires the signal).
		wait: (ms, options) => core["timers/promises"].setTimeout(ms, undefined, options),
		yield: () => new Promise((res) => setImmediate(res)),
	};
	core.timers.promises = core["timers/promises"];

	// ------------------------------------------------------------ process ++

	let refCount = 0;
	process.ref = () => { refCount++; };
	process.unref = () => { refCount--; };

	// process.stdin: a Readable backed by Config.Stdin (lazily started when a
	// consumer attaches). __node_ops is still in scope here (extended.js runs
	// before its deletion).
	{
		const ops = globalThis.__node_ops;
		const stdin = new core.stream.Readable({ read() {} });
		stdin.isTTY = false;
		let started = false;
		let stdinEnded = false;
		let stdinUnreffed = false;
		const startStdin = () => {
			if (started) return;
			started = true;
			ops.stdin_start((chunk) => stdin.push(Buffer.from(chunk)), () => {
				// Host EOF releases the stdin pending. Undo any unref offset HERE
				// (not via a stream 'end' event — a paused/never-read stdin never
				// emits 'end', which would strand the global offset and underflow
				// it). Mark stdin ended so a later pause()/unref() is a no-op.
				if (stdinUnreffed) { ops.loop_ref(true); stdinUnreffed = false; }
				stdinEnded = true;
				stdin.push(null);
			});
		};
		// Node: process.stdin.pause()/unref() release stdin's hold on the loop so
		// the process can exit even before EOF; resume()/ref()/a 'data' listener
		// re-ref it. The host stdin_start holds a pending released only on EOF, so
		// unref is an OFFSET (loop_ref(false)) applied only while stdin is started
		// and still holds that pending (not after EOF).
		const stdinUnref = () => {
			if (started && !stdinEnded && !stdinUnreffed) {
				ops.loop_ref(false);
				stdinUnreffed = true;
			}
		};
		const stdinRef = () => {
			if (stdinUnreffed) { ops.loop_ref(true); stdinUnreffed = false; }
		};
		const origOn = stdin.on.bind(stdin);
		stdin.on = (type, fn) => { if (type === "data" || type === "readable") { startStdin(); stdinRef(); } return origOn(type, fn); };
		stdin.resume = function () { startStdin(); stdinRef(); return core.stream.Readable.prototype.resume.call(this); };
		stdin.pause = function () { stdinUnref(); return core.stream.Readable.prototype.pause.call(this); };
		stdin.ref = function () { stdinRef(); return this; };
		stdin.unref = function () { stdinUnref(); return this; };
		process.stdin = stdin;

		// process.stdout/stderr: real Writable streams so `readable.pipe(process.
		// stdout)` works (pipe needs .on('drain')/.emit('pipe')) and write(data,cb)/
		// end(cb) invoke their callbacks. The runtime.js versions were plain objects
		// that broke both patterns.
		const makeStdio = (fd) => {
			const w = new core.stream.Writable({
				write(chunk, enc, cb) {
					if (typeof chunk === "string") {
						// A non-utf8 encoding means the string is an encoded form (hex/
						// base64/...); decode to bytes like Node before writing.
						ops.raw_write(fd, enc && enc !== "utf8" && enc !== "buffer" ? Buffer.from(chunk, enc) : chunk);
					} else {
						ops.raw_write(fd, chunk); // Buffer/Uint8Array: raw bytes, no UTF-8 round-trip
					}
					cb();
				},
			});
			w.isTTY = false;
			w.columns = 80;
			w.rows = 24;
			w.fd = fd;
			return w;
		};
		process.stdout = makeStdio(0);
		process.stderr = makeStdio(1);

		// process signals: install one OS handler that re-emits onto process.
		let signalWired = false;
		const wireSignals = () => {
			if (signalWired) return;
			signalWired = true;
			ops.signal_watch((name) => process.emit(name));
		};
		const procOn = process.on.bind(process);
		process.on = (type, fn) => {
			if (typeof type === "string" && type.startsWith("SIG")) wireSignals();
			return procOn(type, fn);
		};
	}
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
	bufferMod.transcode = (source, from, to) => {
		const str = Buffer.from(source).toString(from);
		// Node maps characters unrepresentable in the target single-byte
		// encoding to "?" (0x3f), not to the low byte of the code point.
		if (to === "latin1" || to === "binary" || to === "ascii") {
			const limit = to === "ascii" ? 0x7f : 0xff;
			const cps = [...str];
			const out = Buffer.alloc(cps.length);
			for (let i = 0; i < cps.length; i++) {
				const cp = cps[i].codePointAt(0);
				out[i] = cp > limit ? 0x3f : cp;
			}
			return out;
		}
		return Buffer.from(str, to);
	};

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
		let rc, cancelled = false;
		const readable = new ReadableStream({ start(c) { rc = c; }, cancel() { cancelled = true; } });
		const writable = new WritableStream({
			write(chunk) { chunks.push(chunk instanceof Uint8Array ? chunk : new Uint8Array(chunk)); },
			close() {
				// A cancelled readable no longer accepts chunks — writer.close()
				// still resolves cleanly (enqueue would TypeError).
				if (cancelled) return;
				const total = chunks.reduce((n, c) => n + c.length, 0);
				const joined = new Uint8Array(total);
				let off = 0;
				for (const c of chunks) { joined.set(c, off); off += c.length; }
				try {
					rc.enqueue(new Uint8Array(zlib[method + "Sync"] ? zlib[method + "Sync"](joined) : zlib[method](joined)));
					rc.close();
				} catch (e) {
					// A transform failure (e.g. corrupt input to DecompressionStream)
					// must ERROR the readable side so a consumer's read rejects,
					// rather than leaving it open forever (a hang).
					rc.error(e);
					throw e;
				}
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
