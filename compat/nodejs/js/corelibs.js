// compat/nodejs core modules (pure JS over __node_ops) plus the CommonJS
// require machinery. Evaluated after runtime.js.
(() => {
	"use strict";
	const ops = globalThis.__node_ops;
	const core = globalThis.__node_core_registry; // js/http.js (last) deletes it

	// --------------------------------------------------------------- path

	function normalizeSegs(p, keepRoot) {
		const abs = p.startsWith("/");
		const out = [];
		for (const seg of p.split("/")) {
			if (seg === "" || seg === ".") continue;
			if (seg === "..") {
				if (out.length && out[out.length - 1] !== "..") out.pop();
				else if (!abs) out.push("..");
			} else out.push(seg);
		}
		let joined = out.join("/");
		if (abs) joined = "/" + joined;
		if (joined === "") joined = abs && keepRoot ? "/" : ".";
		return joined;
	}

	const path = {
		sep: "/",
		delimiter: ":",
		isAbsolute: (p) => String(p).startsWith("/"),
		normalize(p) {
			p = String(p);
			if (p === "") return ".";
			const trailing = p.length > 1 && p.endsWith("/");
			let n = normalizeSegs(p, true);
			if (trailing && !n.endsWith("/")) n += "/";
			return n;
		},
		join(...parts) {
			const joined = parts.filter((p) => p !== "").join("/");
			return joined === "" ? "." : path.normalize(joined);
		},
		resolve(...parts) {
			let resolved = "";
			for (let i = parts.length - 1; i >= 0; i--) {
				const p = String(parts[i]);
				if (p === "") continue;
				resolved = resolved === "" ? p : p + "/" + resolved;
				if (p.startsWith("/")) break;
			}
			if (!resolved.startsWith("/")) resolved = process.cwd() + "/" + resolved;
			return normalizeSegs(resolved, true);
		},
		dirname(p) {
			p = String(p);
			if (p === "") return ".";
			const trimmed = p.length > 1 ? p.replace(/\/+$/, "") : p;
			const i = trimmed.lastIndexOf("/");
			if (i < 0) return ".";
			if (i === 0) return "/";
			return trimmed.slice(0, i);
		},
		basename(p, suffix) {
			p = String(p).replace(/\/+$/, "");
			const i = p.lastIndexOf("/");
			let b = i < 0 ? p : p.slice(i + 1);
			if (suffix && b.endsWith(suffix) && b !== suffix) b = b.slice(0, -suffix.length);
			return b;
		},
		extname(p) {
			const b = path.basename(p);
			const i = b.lastIndexOf(".");
			return i <= 0 ? "" : b.slice(i);
		},
		relative(from, to) {
			const f = path.resolve(from).split("/").filter(Boolean);
			const t = path.resolve(to).split("/").filter(Boolean);
			let i = 0;
			while (i < f.length && i < t.length && f[i] === t[i]) i++;
			return [...f.slice(i).map(() => ".."), ...t.slice(i)].join("/") || "";
		},
		parse(p) {
			const dir = path.dirname(p);
			const base = path.basename(p);
			const ext = path.extname(p);
			return {
				root: String(p).startsWith("/") ? "/" : "",
				dir: dir === "." && !String(p).includes("/") ? "" : dir,
				base,
				ext,
				name: ext ? base.slice(0, -ext.length) : base,
			};
		},
		format(o) {
			const base = o.base ?? (o.name ?? "") + (o.ext ?? "");
			const dir = o.dir ?? o.root ?? "";
			if (!dir) return base;
			return dir === "/" ? "/" + base : dir + "/" + base;
		},
	};
	path.posix = path;
	path.win32 = path;
	core.path = path;

	// ------------------------------------------------------------- events

	// A FUNCTION-style constructor on purpose: the util.inherits generation
	// of npm packages calls `EventEmitter.call(this)` / `Stream.call(this)`,
	// which a class constructor rejects.
	function EventEmitter() {
		this._events = Object.create(null);
		this._maxListeners = undefined;
	}
	Object.assign(EventEmitter.prototype, {
		_list(type, create) {
			if (!this._events) this._events = Object.create(null);
			let l = this._events[type];
			if (!l && create) this._events[type] = l = [];
			return l;
		},
		_emitNewListener(type, fn) {
			// 'newListener' fires BEFORE the add (Node), and only when someone
			// is actually listening for it (avoid recursion on every add).
			if (this._events && this._events.newListener && type !== "newListener") {
				this.emit("newListener", type, fn.listener || fn);
			}
		},
		_checkMaxListeners(type, l) {
			const max = this._maxListeners ?? EventEmitter.defaultMaxListeners;
			if (max > 0 && l.length > max && !l._warned) {
				l._warned = true;
				const w = new Error(`Possible EventEmitter memory leak detected. ${l.length} ${String(type)} listeners added to [${this.constructor && this.constructor.name || "EventEmitter"}]. Use emitter.setMaxListeners() to increase limit`);
				w.name = "MaxListenersExceededWarning";
				if (typeof process !== "undefined" && process.emitWarning) process.emitWarning(w);
				else if (typeof console !== "undefined") console.error(String(w.message));
			}
		},
		on(type, fn) {
			this._emitNewListener(type, fn);
			const l = this._list(type, true);
			l.push(fn);
			this._checkMaxListeners(type, l);
			return this;
		},
		addListener(type, fn) { return this.on(type, fn); },
		prependListener(type, fn) {
			this._emitNewListener(type, fn);
			const l = this._list(type, true);
			l.unshift(fn);
			this._checkMaxListeners(type, l);
			return this;
		},
		once(type, fn) {
			const wrapper = (...args) => { this.off(type, wrapper); fn.apply(this, args); };
			wrapper.listener = fn;
			return this.on(type, wrapper);
		},
		prependOnceListener(type, fn) {
			const wrapper = (...args) => { this.off(type, wrapper); fn.apply(this, args); };
			wrapper.listener = fn;
			return this.prependListener(type, wrapper);
		},
		off(type, fn) {
			const l = this._list(type, false);
			if (!l) return this;
			const i = l.findIndex((h) => h === fn || h.listener === fn);
			if (i >= 0) {
				const removed = l[i];
				l.splice(i, 1);
				if (this._events && this._events.removeListener && type !== "removeListener") {
					this.emit("removeListener", type, removed.listener || removed);
				}
			}
			if (l.length === 0) delete this._events[type];
			return this;
		},
		removeListener(type, fn) { return this.off(type, fn); },
		removeAllListeners(type) {
			if (type === undefined) this._events = Object.create(null);
			else delete this._events[type];
			return this;
		},
		emit(type, ...args) {
			const l = this._list(type, false);
			if (!l || l.length === 0) {
				if (type === "error") {
					const err = args[0] instanceof Error ? args[0] : new Error(`Unhandled error: ${args[0]}`);
					throw err;
				}
				return false;
			}
			for (const fn of [...l]) fn.apply(this, args);
			return true;
		},
		listeners(type) { return [...(this._list(type, false) || [])].map((h) => h.listener || h); },
		rawListeners(type) { return [...(this._list(type, false) || [])]; },
		listenerCount(type) { return (this._list(type, false) || []).length; },
		eventNames() { return Object.keys(this._events || {}); },
		setMaxListeners(n) { this._maxListeners = n; return this; },
		getMaxListeners() { return this._maxListeners ?? EventEmitter.defaultMaxListeners; },
	});
	EventEmitter.defaultMaxListeners = 10;
	EventEmitter.EventEmitter = EventEmitter;
	EventEmitter.once = (emitter, type, options = {}) =>
		new Promise((resolve, reject) => {
			const signal = options.signal;
			if (signal && signal.aborted) {
				return reject(signal.reason || Object.assign(new Error("The operation was aborted"), { name: "AbortError" }));
			}
			let errHandler, onAbort;
			const cleanup = () => {
				emitter.off(type, onEvent);
				if (errHandler) emitter.off("error", errHandler);
				if (signal && onAbort) signal.removeEventListener("abort", onAbort);
			};
			const onEvent = (...args) => { cleanup(); resolve(args); };
			if (type !== "error") {
				errHandler = (e) => { cleanup(); reject(e); };
				emitter.once("error", errHandler);
			}
			if (signal) {
				onAbort = () => { cleanup(); reject(signal.reason || Object.assign(new Error("The operation was aborted"), { name: "AbortError" })); };
				signal.addEventListener("abort", onAbort, { once: true });
			}
			emitter.once(type, onEvent);
		});
	core.events = EventEmitter;

	// Graft the emitter surface onto process (replacing runtime.js stubs).
	{
		const emitter = new EventEmitter();
		for (const m of ["on", "addListener", "once", "off", "removeListener",
			"removeAllListeners", "emit", "listeners", "listenerCount", "eventNames",
			"prependListener", "prependOnceListener", "rawListeners",
			"setMaxListeners", "getMaxListeners"]) {
			process[m] = EventEmitter.prototype[m].bind(emitter);
		}
	}

	// beforeExit/exit lifecycle, driven by the host loop (rt.Wait). beforeExit
	// fires when the loop has drained (a handler may schedule more work); exit
	// fires once on termination (natural drain or process.exit). Return whether a
	// beforeExit listener exists so the host knows to drain again.
	globalThis.__node_emit_before_exit = () => {
		if (process.listenerCount && process.listenerCount("beforeExit") > 0) {
			process.emit("beforeExit", process.exitCode ?? 0);
			return true;
		}
		return false;
	};
	let __exitEmitted = false;
	globalThis.__node_emit_exit = () => {
		if (__exitEmitted) return;
		__exitEmitted = true;
		if (process.listenerCount && process.listenerCount("exit") > 0) {
			process.emit("exit", process.exitCode ?? 0);
		}
	};
	globalThis.__node_reset_exit_emitted = () => { __exitEmitted = false; };

	// The uncaughtException channel: an error escaping a process.nextTick
	// callback (or a stream 'error' with no listener) routes here. If a handler
	// is registered it runs and the error is considered handled; otherwise the
	// caller (runTicks) rethrows so it surfaces to the host instead of vanishing
	// as an unobserved rejection. Returns true iff handled.
	globalThis.__node_emit_uncaught = (e) => {
		// The process.exit() sentinel must always propagate to unwind to the host;
		// a user uncaughtException handler must not be able to swallow it.
		if (e && e.__nodeExit) return false;
		if (process.listenerCount && process.listenerCount("uncaughtException") > 0) {
			process.emit("uncaughtException", e, "uncaughtException");
			return true;
		}
		return false;
	};
	// The generic hook the shared (web-layer) timer wrapper routes a callback
	// throw to, so a throw in a setTimeout/setInterval callback reaches the
	// uncaughtException handler and does not tear down the loop. process.exit()'s
	// sentinel is never "handled" here — it must propagate to terminate the loop.
	globalThis.__emit_uncaught = (e) => {
		if (e && e.__nodeExit) return false;
		return globalThis.__node_emit_uncaught(e);
	};

	// --------------------------------------------------------------- util

	function inspect(v, opts = {}, depth = 0, seen = new Set()) {
		switch (typeof v) {
			case "string": return depth === 0 && opts.raw ? v : JSON.stringify(v);
			case "number": case "boolean": case "undefined": return String(v);
			case "bigint": return String(v) + "n";
			case "symbol": return String(v);
			case "function": return `[Function: ${v.name || "anonymous"}]`;
		}
		if (v === null) return "null";
		if (v instanceof Error) {
			const head = `${v.name}: ${v.message}`;
			return v.stack ? `${head}\n${v.stack}` : head;
		}
		if (v instanceof Date) return v.toISOString();
		if (v instanceof RegExp) return String(v);
		if (globalThis.Buffer && Buffer.isBuffer(v)) {
			const hex = [...v.subarray(0, 32)].map((b) => b.toString(16).padStart(2, "0")).join(" ");
			return `<Buffer ${hex}${v.length > 32 ? " ..." : ""}>`;
		}
		if (seen.has(v)) return "[Circular]";
		if (depth > (opts.depth ?? 4)) return Array.isArray(v) ? "[Array]" : "[Object]";
		seen.add(v);
		try {
			if (Array.isArray(v)) {
				return v.length ? `[ ${v.map((x) => inspect(x, opts, depth + 1, seen)).join(", ")} ]` : "[]";
			}
			if (v instanceof Map) {
				return `Map(${v.size}) {${[...v].map(([k, x]) => ` ${inspect(k, opts, depth + 1, seen)} => ${inspect(x, opts, depth + 1, seen)}`).join(",")} }`;
			}
			if (v instanceof Set) {
				return `Set(${v.size}) {${[...v].map((x) => " " + inspect(x, opts, depth + 1, seen)).join(",")} }`;
			}
			const keys = Object.keys(v);
			if (!keys.length) return "{}";
			return `{ ${keys.map((k) => {
				// A throwing getter must degrade to a placeholder, not abort the
				// whole inspect/console.log call (Node renders "[Getter]"/the error).
				let val;
				try { val = v[k]; } catch (e) { return `${k}: [Getter: threw ${e && e.name || "Error"}]`; }
				return `${k}: ${inspect(val, opts, depth + 1, seen)}`;
			}).join(", ")} }`;
		} finally {
			seen.delete(v);
		}
	}

	function format(f, ...args) {
		// util.format() with no args is "" (so console.log() prints a blank line,
		// not "undefined").
		if (arguments.length === 0) return "";
		if (typeof f !== "string") {
			return [f, ...args].map((a) => inspect(a, { raw: true })).join(" ");
		}
		let i = 0;
		let out = f.replace(/%[sdifjoOc%]/g, (m) => {
			if (m === "%%") return "%";
			if (i >= args.length) return m;
			const a = args[i++];
			switch (m) {
				case "%s": return typeof a === "string" ? a : inspect(a, { raw: true });
				// A Symbol can't be coerced to Number (would throw); Node yields NaN.
				case "%d": return typeof a === "bigint" ? a + "n" : typeof a === "symbol" ? "NaN" : String(Number(a));
				case "%i": return typeof a === "bigint" ? a + "n" : typeof a === "symbol" ? "NaN" : String(parseInt(a, 10));
				case "%f": return typeof a === "symbol" ? "NaN" : String(parseFloat(a));
				case "%j": try { return JSON.stringify(a); } catch { return "[Circular]"; }
				case "%o": case "%O": return inspect(a);
				case "%c": return "";
			}
			return m;
		});
		for (; i < args.length; i++) out += " " + inspect(args[i], { raw: true });
		return out;
	}

	const util = {
		format,
		inspect: (v, opts) => inspect(v, { ...(opts || {}), raw: true }),
		inherits(ctor, superCtor) {
			Object.setPrototypeOf(ctor.prototype, superCtor.prototype);
			ctor.super_ = superCtor;
		},
		promisify(fn) {
			// Honor a function's own promisified implementation (Node's
			// util.promisify.custom), e.g. timers' setTimeout.
			const custom = fn[Symbol.for("nodejs.util.promisify.custom")];
			if (custom) return custom;
			const promisified = (...args) =>
				new Promise((resolve, reject) => {
					fn(...args, (err, value) => (err ? reject(err) : resolve(value)));
				});
			promisified[Symbol.for("nodejs.util.promisify.custom")] = promisified;
			return promisified;
		},
		callbackify(fn) {
			return (...args) => {
				const cb = args.pop();
				fn(...args).then((v) => cb(null, v), (e) => cb(e));
			};
		},
		deprecate: (fn) => fn,
		debuglog: () => () => {},
		isDeepStrictEqual: (a, b) => deepEqual(a, b),
		types: {
			isPromise: (v) => v instanceof Promise,
			isDate: (v) => v instanceof Date,
			isRegExp: (v) => v instanceof RegExp,
			isNativeError: (v) => v instanceof Error,
			isTypedArray: (v) => ArrayBuffer.isView(v) && !(v instanceof DataView),
		},
		TextEncoder: globalThis.TextEncoder,
		TextDecoder: globalThis.TextDecoder,
	};
	util.promisify.custom = Symbol.for("nodejs.util.promisify.custom");
	core.util = util;

	// -------------------------------------------------------- querystring

	const qsEscape = (s) => encodeURIComponent(String(s));
	const qsUnescape = (s) => {
		try { return decodeURIComponent(String(s).replace(/\+/g, " ")); } catch { return String(s); }
	};
	core.querystring = {
		escape: qsEscape,
		unescape: qsUnescape,
		parse(str, sep = "&", eq = "=", options = {}) {
			const out = Object.create(null);
			// Node caps at 1000 keys by default (maxKeys) as a DoS guard against a
			// hostile query string; 0 means unlimited.
			const maxKeys = options.maxKeys === undefined ? 1000 : options.maxKeys;
			let pairCount = 0;
			for (const part of String(str ?? "").split(sep)) {
				if (!part) continue;
				// Node counts EVERY pair against maxKeys (duplicates included), so a
				// repeated key can't bypass the DoS guard.
				if (maxKeys > 0 && pairCount >= maxKeys) break;
				pairCount++;
				const i = part.indexOf(eq);
				const k = qsUnescape(i < 0 ? part : part.slice(0, i));
				const v = i < 0 ? "" : qsUnescape(part.slice(i + eq.length));
				if (k in out) {
					if (Array.isArray(out[k])) out[k].push(v);
					else out[k] = [out[k], v];
				} else out[k] = v;
			}
			return out;
		},
		stringify(obj, sep = "&", eq = "=") {
			const parts = [];
			for (const k of Object.keys(obj || {})) {
				const v = obj[k];
				if (Array.isArray(v)) for (const x of v) parts.push(qsEscape(k) + eq + qsEscape(x));
				else parts.push(qsEscape(k) + eq + qsEscape(v ?? ""));
			}
			return parts.join(sep);
		},
	};

	// ----------------------------------------------------------------- os

	core.os = {
		EOL: "\n",
		platform: () => process.platform,
		arch: () => process.arch,
		type: () => (process.platform === "darwin" ? "Darwin" : process.platform === "win32" ? "Windows_NT" : "Linux"),
		release: () => "0.0.0",
		homedir: () => "/",
		tmpdir: () => "/tmp",
		hostname: () => "localhost",
		cpus: () => [],
		totalmem: () => 0,
		freemem: () => 0,
		endianness: () => "LE",
		availableParallelism: () => 1,
	};

	// ---------------------------------------------------------------- url

	core.url = {
		URL: globalThis.URL,
		URLSearchParams: globalThis.URLSearchParams,
		pathToFileURL: (p) => new URL("file://" + path.resolve(p)),
		fileURLToPath: (u) => {
			const s = u instanceof URL ? u : new URL(String(u));
			if (s.protocol !== "file:") throw new TypeError("must be a file: URL");
			return decodeURIComponent(s.pathname);
		},
		domainToASCII: (d) => String(d).toLowerCase(),
		domainToUnicode: (d) => String(d),
	};

	// ------------------------------------------------------------- timers

	globalThis.setImmediate = (fn, ...args) => {
		if (typeof fn !== "function") throw new TypeError("callback is not a function");
		return ops.immediate_set(args.length ? () => fn(...args) : fn);
	};
	globalThis.clearImmediate = (id) => {
		if (id !== undefined && id !== null) ops.immediate_clear(Number(id) || 0);
	};
	// The web layer's setTimeout/setInterval already return Timeout-like
	// handles (ref/unref/refresh/close, coercing to the numeric id), so the
	// Node timers inherit them directly — no extra wrapping here.
	core.timers = {
		setTimeout: globalThis.setTimeout,
		clearTimeout: globalThis.clearTimeout,
		setInterval: globalThis.setInterval,
		clearInterval: globalThis.clearInterval,
		setImmediate: globalThis.setImmediate,
		clearImmediate: globalThis.clearImmediate,
	};
	core["timers/promises"] = {
		setTimeout: (ms, value, options = {}) => new Promise((res, rej) => {
			const signal = options.signal;
			if (signal && signal.aborted) {
				return rej(signal.reason || Object.assign(new Error("The operation was aborted"), { name: "AbortError" }));
			}
			const t = setTimeout(() => { cleanup(); res(value); }, ms);
			// Node keeps the timer ref'd by default; unref only when ref:false.
			if (options.ref === false && t && typeof t.unref === "function") t.unref();
			const onAbort = () => { clearTimeout(t); cleanup(); rej(signal.reason || Object.assign(new Error("The operation was aborted"), { name: "AbortError" })); };
			const cleanup = () => { if (signal) signal.removeEventListener("abort", onAbort); };
			if (signal) signal.addEventListener("abort", onAbort, { once: true });
		}),
		setImmediate: (value, options = {}) => new Promise((res, rej) => {
			const signal = options.signal;
			if (signal && signal.aborted) {
				return rej(signal.reason || Object.assign(new Error("The operation was aborted"), { name: "AbortError" }));
			}
			setImmediate(() => res(value));
		}),
	};

	// ------------------------------------------------------------- assert

	function deepEqual(a, b, seen = new Map()) {
		if (Object.is(a, b)) return true;
		if (typeof a !== "object" || typeof b !== "object" || a === null || b === null) return false;
		if (Object.getPrototypeOf(a) !== Object.getPrototypeOf(b)) return false;
		if (seen.get(a) === b) return true;
		seen.set(a, b);
		if (Array.isArray(a)) {
			return a.length === b.length && a.every((x, i) => deepEqual(x, b[i], seen));
		}
		if (a instanceof Date) return a.getTime() === b.getTime();
		if (a instanceof RegExp) return String(a) === String(b);
		if (ArrayBuffer.isView(a)) {
			return a.byteLength === b.byteLength && [...new Uint8Array(a.buffer, a.byteOffset, a.byteLength)]
				.every((x, i) => x === new Uint8Array(b.buffer, b.byteOffset, b.byteLength)[i]);
		}
		if (a instanceof Map) {
			// Map/Set have no own-enumerable keys, so the Object.keys fallthrough
			// would compare them equal regardless of contents. Match entries
			// STRUCTURALLY (Node's deepStrictEqual), pairing each of a's entries to
			// a distinct not-yet-used entry of b.
			if (a.size !== b.size) return false;
			const be = [...b], used = new Array(be.length).fill(false);
			outer: for (const [ak, av] of a) {
				for (let i = 0; i < be.length; i++) {
					if (!used[i] && deepEqual(ak, be[i][0], seen) && deepEqual(av, be[i][1], seen)) {
						used[i] = true;
						continue outer;
					}
				}
				return false;
			}
			return true;
		}
		if (a instanceof Set) {
			if (a.size !== b.size) return false;
			const bv = [...b], used = new Array(bv.length).fill(false);
			outer: for (const av of a) {
				for (let i = 0; i < bv.length; i++) {
					if (!used[i] && deepEqual(av, bv[i], seen)) { used[i] = true; continue outer; }
				}
				return false;
			}
			return true;
		}
		const ka = Object.keys(a), kb = Object.keys(b);
		return ka.length === kb.length && ka.every((k) => deepEqual(a[k], b[k], seen));
	}

	// matchError validates a thrown value against a Node error matcher: an error
	// constructor, a RegExp (tested against the message), a validation object
	// (own keys must deep-equal), or a predicate function.
	function matchError(err, matcher) {
		if (matcher === undefined) return true;
		if (typeof matcher === "function") {
			// Constructor (Error subclass) vs. plain predicate.
			if (matcher.prototype instanceof Error || matcher === Error) return err instanceof matcher;
			return matcher(err) === true;
		}
		if (matcher instanceof RegExp) return matcher.test(err && err.message !== undefined ? String(err.message) : String(err));
		if (matcher && typeof matcher === "object") {
			return Object.keys(matcher).every((k) => deepEqual(err ? err[k] : undefined, matcher[k]));
		}
		return true;
	}

	class AssertionError extends Error {
		constructor(opts) {
			super(opts.message || `${inspect(opts.actual)} ${opts.operator} ${inspect(opts.expected)}`);
			this.name = "AssertionError";
			this.actual = opts.actual;
			this.expected = opts.expected;
			this.operator = opts.operator;
			this.code = "ERR_ASSERTION";
		}
	}
	function assert(value, message) {
		if (!value) throw new AssertionError({ actual: value, expected: true, operator: "==", message });
	}
	Object.assign(assert, {
		AssertionError,
		ok: assert,
		fail: (message) => { throw new AssertionError({ message: message || "Failed", operator: "fail" }); },
		equal: (a, e, m) => { if (a != e) throw new AssertionError({ actual: a, expected: e, operator: "==", message: m }); },
		notEqual: (a, e, m) => { if (a == e) throw new AssertionError({ actual: a, expected: e, operator: "!=", message: m }); },
		strictEqual: (a, e, m) => { if (!Object.is(a, e)) throw new AssertionError({ actual: a, expected: e, operator: "===", message: m }); },
		notStrictEqual: (a, e, m) => { if (Object.is(a, e)) throw new AssertionError({ actual: a, expected: e, operator: "!==", message: m }); },
		deepEqual: (a, e, m) => { if (!deepEqual(a, e)) throw new AssertionError({ actual: a, expected: e, operator: "deepEqual", message: m }); },
		deepStrictEqual: (a, e, m) => { if (!deepEqual(a, e)) throw new AssertionError({ actual: a, expected: e, operator: "deepStrictEqual", message: m }); },
		throws: (fn, matcher, m) => {
			// If the 2nd arg is a plain string it's the message, not a matcher.
			if (typeof matcher === "string") { m = matcher; matcher = undefined; }
			let thrown;
			try { fn(); } catch (e) { thrown = e; }
			if (thrown === undefined) {
				throw new AssertionError({ message: m || "Missing expected exception", operator: "throws" });
			}
			if (!matchError(thrown, matcher)) {
				throw new AssertionError({ actual: thrown, expected: matcher, operator: "throws", message: m || "thrown error did not match" });
			}
		},
		doesNotThrow: (fn, matcher, m) => {
			if (typeof matcher === "string") { m = matcher; matcher = undefined; }
			try { fn(); } catch (e) {
				if (matchError(e, matcher)) throw new AssertionError({ actual: e, operator: "doesNotThrow", message: m || "Got unwanted exception" });
				throw e;
			}
		},
		async rejects(fn, matcher, m) {
			if (typeof matcher === "string") { m = matcher; matcher = undefined; }
			let thrown, threw = false;
			try { await (typeof fn === "function" ? fn() : fn); } catch (e) { thrown = e; threw = true; }
			if (!threw) throw new AssertionError({ message: m || "Missing expected rejection", operator: "rejects" });
			if (!matchError(thrown, matcher)) throw new AssertionError({ actual: thrown, expected: matcher, operator: "rejects", message: m });
		},
		match: (s, re, m) => { if (!re.test(s)) throw new AssertionError({ actual: s, expected: re, operator: "match", message: m }); },
	});
	assert.strict = assert;
	core.assert = assert;

	// ----------------------------------------------------------------- fs

	function fsError(r, syscall, p) {
		const e = new Error(`${r.code}: ${r.message}, ${syscall} '${p}'`);
		e.code = r.code;
		e.syscall = syscall;
		e.path = p;
		return e;
	}
	const isErr = (r) => r !== null && typeof r === "object" && typeof r.code === "string" && !(r instanceof Uint8Array);
	const wrapBuf = (u8) => Object.setPrototypeOf(u8, Buffer.prototype);
	const encodingOf = (opts) => (typeof opts === "string" ? opts : opts && opts.encoding);

	function statsOf(r) {
		return {
			size: r.size,
			mode: r.mode,
			// etag (and friends) duck-type fs.Stats: ino/ctime/mtime/size must
			// exist with the right types.
			ino: 0,
			dev: 0,
			nlink: 1,
			uid: 1000,
			gid: 1000,
			rdev: 0,
			blksize: 4096,
			blocks: Math.ceil(r.size / 512),
			atimeMs: r.mtimeMs,
			ctimeMs: r.mtimeMs,
			birthtimeMs: r.mtimeMs,
			mtime: new Date(r.mtimeMs),
			mtimeMs: r.mtimeMs,
			atime: new Date(r.mtimeMs),
			ctime: new Date(r.mtimeMs),
			birthtime: new Date(r.mtimeMs),
			isFile: () => !r.dir,
			isDirectory: () => r.dir,
			isSymbolicLink: () => false,
			isFIFO: () => false,
			isSocket: () => false,
			isBlockDevice: () => false,
			isCharacterDevice: () => false,
		};
	}

	const fsSync = {
		readFileSync(p, opts) {
			const r = ops.fs_read_file(String(p));
			ops.release_pending();
			if (isErr(r)) throw fsError(r, "open", p);
			const buf = wrapBuf(r);
			const enc = encodingOf(opts);
			return enc ? buf.toString(enc) : buf;
		},
		writeFileSync(p, data, opts) {
			const payload = typeof data === "string" ? data : Buffer.from(data);
			const r = ops.fs_write_file(String(p), payload, false);
			if (isErr(r)) throw fsError(r, "open", p);
		},
		appendFileSync(p, data, opts) {
			const payload = typeof data === "string" ? data : Buffer.from(data);
			const r = ops.fs_write_file(String(p), payload, true);
			if (isErr(r)) throw fsError(r, "open", p);
		},
		existsSync: (p) => ops.fs_exists(String(p)),
		statSync(p) {
			const r = ops.fs_stat(String(p));
			if (isErr(r)) throw fsError(r, "stat", p);
			return statsOf(r);
		},
		lstatSync(p) { return fsSync.statSync(p); },
		readdirSync(p, options) {
			const r = ops.fs_readdir(String(p));
			if (isErr(r)) throw fsError(r, "scandir", p);
			if (options && options.withFileTypes) {
				return r.names.map((name, i) => ({
					name,
					parentPath: String(p),
					isDirectory: () => r.dirs[i],
					isFile: () => !r.dirs[i],
					isSymbolicLink: () => false,
					isFIFO: () => false,
					isSocket: () => false,
					isBlockDevice: () => false,
					isCharacterDevice: () => false,
				}));
			}
			return r.names;
		},
		accessSync(p, mode) {
			if (!ops.fs_exists(String(p))) {
				throw fsError({ code: "ENOENT", message: "no such file or directory" }, "access", p);
			}
		},
		mkdirSync(p, opts) {
			const r = ops.fs_mkdir(String(p), !!(opts && opts.recursive));
			if (isErr(r)) throw fsError(r, "mkdir", p);
		},
		rmdirSync(p) {
			const r = ops.fs_remove(String(p));
			if (isErr(r)) throw fsError(r, "rmdir", p);
		},
		unlinkSync(p) {
			const r = ops.fs_remove(String(p));
			if (isErr(r)) throw fsError(r, "unlink", p);
		},
		renameSync(oldP, newP) {
			const r = ops.fs_rename(String(oldP), String(newP));
			if (isErr(r)) throw fsError(r, "rename", oldP);
		},
		realpathSync: (p) => path.resolve(String(p)),
		watch(p, options, listener) {
			if (typeof options === "function") { listener = options; options = {}; }
			const watcher = new EventEmitter();
			const id = ops.fs_watch(String(p), (eventType, filename) => {
				watcher.emit("change", eventType, filename);
				if (listener) listener(eventType, filename);
			});
			watcher.close = () => ops.fs_unwatch(id);
			return watcher;
		},
		watchFile(p, options, listener) {
			if (typeof options === "function") { listener = options; options = {}; }
			let prev = null;
			try { prev = fsSync.statSync(p); } catch {}
			const id = ops.fs_watch(String(p), () => {
				let cur = null;
				try { cur = fsSync.statSync(p); } catch {}
				listener(cur || { mtime: new Date(0), size: 0 }, prev || { mtime: new Date(0), size: 0 });
				prev = cur;
			});
			return { _id: id };
		},
		unwatchFile(p) {},
		copyFileSync(src, dest) {
			const r = ops.fs_copyfile(String(src), String(dest));
			if (isErr(r)) throw fsError(r, "copyfile", src);
		},
		rmSync(p, options = {}) {
			const r = ops.fs_rm(String(p), !!options.recursive, !!options.force);
			if (isErr(r)) throw fsError(r, "rm", p);
		},
		rmdirSync(p, options = {}) {
			const r = ops.fs_rm(String(p), !!(options && options.recursive), false);
			if (isErr(r)) throw fsError(r, "rmdir", p);
		},
		mkdtempSync(prefix) {
			const r = ops.fs_mkdtemp(String(prefix));
			if (isErr(r)) throw fsError(r, "mkdtemp", prefix);
			return r;
		},
		cpSync(src, dest, options = {}) {
			// Recursive directory copy over the primitive ops.
			const st = fsSync.statSync(src);
			if (st.isDirectory()) {
				fsSync.mkdirSync(dest, { recursive: true });
				for (const name of fsSync.readdirSync(src)) {
					fsSync.cpSync(path.join(String(src), name), path.join(String(dest), name), options);
				}
			} else {
				fsSync.copyFileSync(src, dest);
			}
		},
		openSync(p, flags = "r") {
			const r = ops.fs_open(String(p), String(flags));
			if (isErr(r)) throw fsError(r, "open", p);
			return r;
		},
		closeSync(fd) {
			const r = ops.fs_close_fd(fd);
			if (isErr(r)) throw fsError(r, "close", fd);
		},
		readSync(fd, buffer, offset = 0, length = buffer.length, position = null) {
			const r = ops.fs_read_fd(fd, length, position);
			ops.release_pending();
			if (isErr(r)) throw fsError(r, "read", fd);
			const data = new Uint8Array(r.data);
			buffer.set(data.subarray(0, Math.min(data.length, length)), offset);
			return r.bytesRead;
		},
		writeSync(fd, buffer, offset, length, position) {
			let data, pos;
			if (typeof buffer === "string") {
				// writeSync(fd, string[, position[, encoding]])
				data = Buffer.from(buffer, typeof length === "string" ? length : undefined);
				pos = typeof offset === "number" ? offset : null;
			} else {
				// writeSync(fd, buffer[, offset[, length[, position]]])
				data = Buffer.from(buffer.buffer ? new Uint8Array(buffer.buffer, buffer.byteOffset + (offset || 0), length ?? buffer.length) : buffer);
				pos = typeof position === "number" ? position : null;
			}
			const r = ops.fs_write_fd(fd, data, pos);
			if (isErr(r)) throw fsError(r, "write", fd);
			return r;
		},
		fstatSync(fd) {
			const r = ops.fs_fstat(fd);
			if (isErr(r)) throw fsError(r, "fstat", fd);
			return statsOf(r);
		},
		chmodSync() {}, // no-op: the FS abstraction has no mode bits to set
		chownSync() {},
		utimesSync() {},
		symlinkSync() { throw fsError({ code: "ENOSYS", message: "symlink not supported" }, "symlink", ""); },
		readlinkSync(p) { throw fsError({ code: "EINVAL", message: "not a symlink" }, "readlink", p); },
	};

	// Callback flavors run the sync op and deliver on the microtask queue.
	function callbackify1(syncFn) {
		return (...args) => {
			const cb = args.pop();
			if (typeof cb !== "function") throw new TypeError("callback required");
			queueMicrotask(() => {
				try { cb(null, syncFn(...args)); } catch (e) { cb(e); }
			});
		};
	}
	const fsMod = {
		...fsSync,
		readFile: callbackify1(fsSync.readFileSync),
		writeFile: callbackify1(fsSync.writeFileSync),
		appendFile: callbackify1(fsSync.appendFileSync),
		stat: callbackify1(fsSync.statSync),
		lstat: callbackify1(fsSync.statSync),
		readdir: callbackify1(fsSync.readdirSync),
		mkdir: callbackify1(fsSync.mkdirSync),
		rmdir: callbackify1(fsSync.rmdirSync),
		rm: callbackify1(fsSync.rmSync),
		unlink: callbackify1(fsSync.unlinkSync),
		rename: callbackify1(fsSync.renameSync),
		realpath: callbackify1(fsSync.realpathSync),
		copyFile: callbackify1(fsSync.copyFileSync),
		mkdtemp: callbackify1(fsSync.mkdtempSync),
		cp: callbackify1(fsSync.cpSync),
		chmod: (p, mode, cb) => queueMicrotask(() => (cb || mode)(null)),
		chown: (p, uid, gid, cb) => queueMicrotask(() => cb(null)),
		exists: (p, cb) => queueMicrotask(() => cb(fsSync.existsSync(p))),
		access: (p, mode, cb) => {
			const done = typeof mode === "function" ? mode : cb;
			queueMicrotask(() => {
				try { fsSync.accessSync(p); done(null); } catch (e) { done(e); }
			});
		},
		constants: { F_OK: 0, R_OK: 4, W_OK: 2, X_OK: 1, COPYFILE_EXCL: 1 },
	};
	core.fs = fsMod;

	const promisified = {};
	for (const [name, syncName] of [
		["readFile", "readFileSync"], ["writeFile", "writeFileSync"],
		["appendFile", "appendFileSync"], ["stat", "statSync"],
		["lstat", "lstatSync"], ["readdir", "readdirSync"],
		["mkdir", "mkdirSync"], ["rmdir", "rmdirSync"], ["rm", "rmSync"],
		["unlink", "unlinkSync"], ["rename", "renameSync"],
		["realpath", "realpathSync"], ["copyFile", "copyFileSync"],
		["mkdtemp", "mkdtempSync"], ["cp", "cpSync"],
	]) {
		promisified[name] = (...args) => {
			try { return Promise.resolve(fsSync[syncName](...args)); } catch (e) { return Promise.reject(e); }
		};
	}
	promisified.access = (p) => (ops.fs_exists(String(p)) ? Promise.resolve() : Promise.reject(fsError({ code: "ENOENT", message: "no such file or directory" }, "access", p)));
	core["fs/promises"] = promisified;
	fsMod.promises = promisified;

	// ------------------------------------------------------ child_process
	// Real subprocesses over the child_* host ops (Go os/exec), gated by
	// Config.Exec. Async spawns stream stdout/stderr as 'data' events and
	// fire 'exit'/'close'; the sync forms block and return the result.

	const cpErr = (r, cmd) => { const e = new Error(r.message + (cmd ? " " + cmd : "")); e.code = r.code; return e; };
	const envToArray = (env) => env ? Object.keys(env).map((k) => `${k}=${env[k]}`) : undefined;

	class ChildProcess extends EventEmitter {
		constructor() {
			super();
			this.pid = undefined;
			this.exitCode = null;
			this.signalCode = null;
			this.killed = false;
			this.stdout = new core.stream.Readable({ read() {} });
			this.stderr = new core.stream.Readable({ read() {} });
			this.stdin = new core.stream.Writable({
				write: (chunk, enc, cb) => { ops.child_stdin(this.pid, Buffer.from(chunk)); cb(); },
				final: (cb) => { if (this.pid !== undefined) ops.child_stdin(this.pid, null); cb(); },
			});
		}
		kill(signal) {
			this.killed = true;
			const sig = typeof signal === "string" ? signal : "SIGTERM";
			this.signalCode = sig;
			if (this.pid !== undefined) ops.child_kill(this.pid, sig);
			return true;
		}
	}

	function spawn(file, args, options) {
		if (!Array.isArray(args)) { options = args; args = []; }
		options = options || {};
		const cp = new ChildProcess();
		const onStdout = (chunk) => cp.stdout.push(Buffer.from(chunk));
		const onStderr = (chunk) => cp.stderr.push(Buffer.from(chunk));
		let exited = false;
		const onExit = (code, signal) => {
			exited = true;
			cp.exitCode = code;
			cp.signalCode = signal || null;
			cp.stdout.push(null);
			cp.stderr.push(null);
			cp.emit("exit", code, signal || null);
			process.nextTick(() => cp.emit("close", code, signal || null));
		};
		const onError = (msg) => cp.emit("error", new Error(String(msg)));
		const r = ops.child_spawn(
			{ file: String(file), args: (args || []).map(String), cwd: options.cwd, envArray: envToArray(options.env) },
			onStdout, onStderr, onExit, onError);
		if (isErr(r)) { process.nextTick(() => cp.emit("error", cpErr(r, file))); return cp; }
		cp.pid = r.pid;
		return cp;
	}

	function normalizeExec(command, options, callback) {
		if (typeof options === "function") { callback = options; options = {}; }
		return { options: options || {}, callback };
	}

	function exec(command, options, callback) {
		const { options: o, callback: cb } = normalizeExec(command, options, callback);
		// exec runs the command through a shell.
		const cp = spawn("/bin/sh", ["-c", String(command)], o);
		collectAndCallback(cp, cb, o);
		return cp;
	}

	function execFile(file, args, options, callback) {
		if (typeof args === "function") { callback = args; args = []; options = {}; }
		else if (typeof options === "function") { callback = options; options = {}; }
		const cp = spawn(file, args || [], options || {});
		collectAndCallback(cp, callback, options || {});
		return cp;
	}

	function collectAndCallback(cp, callback, options) {
		if (!callback) return;
		const enc = options.encoding === "buffer" ? null : (options.encoding || "utf8");
		const out = [], err = [];
		cp.stdout.on("data", (c) => out.push(c));
		cp.stderr.on("data", (c) => err.push(c));
		cp.on("error", (e) => callback(e, decodeChunks(out, enc), decodeChunks(err, enc)));
		cp.on("close", (code) => {
			const e = code === 0 ? null : Object.assign(new Error(`Command failed`), { code });
			callback(e, decodeChunks(out, enc), decodeChunks(err, enc));
		});
	}
	const decodeChunks = (chunks, enc) => {
		const buf = Buffer.concat(chunks);
		return enc ? buf.toString(enc) : buf;
	};

	function spawnSync(file, args, options) {
		if (!Array.isArray(args)) { options = args; args = []; }
		options = options || {};
		const input = options.input !== undefined ? Buffer.from(options.input) : undefined;
		const r = ops.child_spawnsync(
			{ file: String(file), args: (args || []).map(String), cwd: options.cwd, envArray: envToArray(options.env) },
			input);
		ops.release_pending();
		if (isErr(r)) return { pid: 0, status: null, signal: null, error: cpErr(r, file), stdout: Buffer.alloc(0), stderr: Buffer.alloc(0) };
		const enc = options.encoding && options.encoding !== "buffer" ? options.encoding : null;
		const stdout = Buffer.from(r.stdout), stderr = Buffer.from(r.stderr);
		return {
			pid: r.pid,
			status: r.status,
			signal: r.signal || null,
			error: r.error ? Object.assign(new Error(r.error), { code: "ENOENT" }) : undefined,
			stdout: enc ? stdout.toString(enc) : stdout,
			stderr: enc ? stderr.toString(enc) : stderr,
			output: [null, enc ? stdout.toString(enc) : stdout, enc ? stderr.toString(enc) : stderr],
		};
	}

	function execSync(command, options = {}) {
		const r = spawnSync("/bin/sh", ["-c", String(command)], options);
		if (r.error) throw r.error;
		if (r.status !== 0) {
			const e = new Error(`Command failed: ${command}\n${r.stderr}`);
			e.status = r.status;
			e.stderr = r.stderr;
			e.stdout = r.stdout;
			throw e;
		}
		return r.stdout;
	}

	function execFileSync(file, args, options) {
		if (!Array.isArray(args)) { options = args; args = []; }
		const r = spawnSync(file, args || [], options || {});
		if (r.error) throw r.error;
		if (r.status !== 0) { const e = new Error(`Command failed`); e.status = r.status; throw e; }
		return r.stdout;
	}

	core.child_process = {
		spawn,
		spawnSync,
		exec,
		execSync,
		execFile,
		execFileSync,
		fork: () => { throw new Error("child_process.fork is not supported (no node executable to re-spawn)"); },
		ChildProcess,
	};

	// ---------------------------------------------------- CommonJS require

	function requireError(spec, message) {
		const e = new Error(message || `Cannot find module '${spec}'`);
		e.code = "MODULE_NOT_FOUND";
		return e;
	}

	const requireCache = Object.create(null);

	// The Module class IS require("module")'s export, and ALL requires flow
	// through Module.prototype.require / Module._resolveFilename — so
	// monkey-patches (Next.js's require-hook aliasing) intercept everything,
	// exactly as on Node.
	function Module(id) {
		this.id = id;
		this.filename = id;
		this.path = path.dirname(id);
		this.exports = {};
		this.loaded = false;
		this.children = [];
		this.paths = [];
	}
	Module._cache = requireCache;
	Module._resolveFilename = function _resolveFilename(request, parent, isMain, options) {
		const parentPath = typeof parent === "string"
			? parent.replace(/^\//, "")
			: parent && parent.filename ? String(parent.filename).replace(/^\//, "") : "main.js";
		const r = ops.node_resolve(String(request), parentPath);
		if (isErr(r)) throw requireError(request, r.message);
		return r.core ? r.core : "/" + r.path;
	};
	Module.prototype.require = function require(request) {
		const resolved = Module._resolveFilename(request, this);
		if (!resolved.startsWith("/")) return globalThis.__node_core(resolved);
		return loadCJSPath(resolved.slice(1));
	};
	Module.createRequire = (from) => {
		const m = new Module(typeof from === "string" ? from : "/main.js");
		return makeRequireFor(m);
	};
	Module.isBuiltin = (name) => {
		try { globalThis.__node_core(name); return true; } catch { return false; }
	};
	Object.defineProperty(Module, "builtinModules", {
		get: () => Object.keys(core).concat(Object.keys(core).map((n) => "node:" + n)),
	});
	Module.syncBuiltinESMExports = () => {};
	Module.Module = Module;
	core.module = Module;

	function loadCJSPath(fsPath) {
		// Key the cache by the ABSOLUTE filename, exactly what
		// Module._resolveFilename / require.resolve return, so
		// require.cache[require.resolve(id)] hits (it was keyed slash-less).
		const absPath = "/" + fsPath;
		const cached = requireCache[absPath];
		if (cached) return cached.exports;
		const src = ops.node_read(fsPath);
		if (isErr(src)) throw requireError(fsPath, `Cannot load module '${fsPath}': ${src.message}`);
		const module = new Module(absPath);
		requireCache[absPath] = module;
		try {
			if (fsPath.endsWith(".json")) {
				module.exports = JSON.parse(src);
			} else {
				const fn = new Function(
					"exports", "require", "module", "__filename", "__dirname",
					src + "\n//# sourceURL=" + fsPath,
				);
				fn.call(module.exports, module.exports, makeRequireFor(module), module, module.filename, module.path);
			}
		} catch (e) {
			delete requireCache[absPath];
			throw e;
		}
		module.loaded = true;
		return module.exports;
	}

	function makeRequireFor(module) {
		const req = (request) => module.require(request);
		req.cache = requireCache;
		req.resolve = (request) => Module._resolveFilename(request, module);
		// The entry module, so the `if (require.main === module)` guard works
		// (true only in the top-level/entry module, false in required ones).
		req.main = rootModule;
		req.extensions = { ".js": () => {}, ".json": () => {}, ".node": () => {} };
		return req;
	}

	const rootModule = new Module("/main.js");
	// NOTE: do NOT seed requireCache with rootModule — its "/main.js" id would
	// shadow a real ./main.js on the FS (require would return the empty entry
	// exports). require.main === module still works via require.main below.
	globalThis.require = makeRequireFor(rootModule);
	globalThis.module = rootModule; // the entry module object (for require.main === module)
	globalThis.__node_require_path = loadCJSPath;
	globalThis.__dirname = "/";
	globalThis.__filename = "/main.js";
})();
