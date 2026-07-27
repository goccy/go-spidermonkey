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
	// ---- path.win32: real Windows semantics (backslash+slash separators,
	// drive letters, UNC roots), not an alias of the posix object.

	const winSepStart = /^[\\/]/;
	// Split a Windows path into { device, isAbs, rest }: device is "C:" or
	// "\\server\share" (no trailing separator), isAbs whether a root separator
	// follows, rest the remainder with leading separators stripped.
	function win32Root(p) {
		p = String(p);
		let device = "", isAbs = false, rest = p;
		if (/^[\\/]{2}(?![\\/])/.test(p)) {
			// UNC: \\server\share\... (also \\?\ and \\.\ device paths).
			const m = p.match(/^[\\/]{2}([^\\/]+)(?:[\\/]+([^\\/]+))?/);
			device = "\\\\" + m[1] + (m[2] !== undefined ? "\\" + m[2] : "");
			isAbs = true;
			rest = p.slice(m[0].length).replace(/^[\\/]+/, "");
		} else if (/^[a-zA-Z]:/.test(p)) {
			device = p[0] + ":";
			rest = p.slice(2);
			if (winSepStart.test(rest)) { isAbs = true; rest = rest.replace(/^[\\/]+/, ""); }
		} else if (winSepStart.test(p)) {
			isAbs = true;
			rest = p.replace(/^[\\/]+/, "");
		}
		return { device, isAbs, rest };
	}
	function win32Segs(rest, isAbs) {
		const out = [];
		for (const seg of rest.split(/[\\/]+/)) {
			if (seg === "" || seg === ".") continue;
			if (seg === "..") {
				if (out.length && out[out.length - 1] !== "..") out.pop();
				else if (!isAbs) out.push("..");
			} else out.push(seg);
		}
		return out;
	}

	const win32 = {
		sep: "\\",
		delimiter: ";",
		isAbsolute(p) {
			p = String(p);
			// "\foo" and "\\server\share" are absolute; "C:foo" (drive-relative)
			// is not, "C:\foo" is.
			return winSepStart.test(p) || /^[a-zA-Z]:[\\/]/.test(p);
		},
		normalize(p) {
			p = String(p);
			if (p === "") return ".";
			const { device, isAbs, rest } = win32Root(p);
			let tail = win32Segs(rest, isAbs).join("\\");
			if (tail && /[\\/]$/.test(p)) tail += "\\";
			if (isAbs) return device + "\\" + tail;
			return device + tail || ".";
		},
		join(...parts) {
			const strs = parts.map(String).filter((p) => p !== "");
			if (!strs.length) return ".";
			let joined = strs.join("\\");
			// Only the FIRST argument may introduce a UNC root: joining "\" and
			// "\foo" must not fabricate "\\foo" (a \\server root). Node applies
			// the same guard.
			if (!/^[\\/]{2}(?![\\/])/.test(strs[0])) joined = joined.replace(/^[\\/]+/, "\\");
			return win32.normalize(joined);
		},
		resolve(...parts) {
			let device = "", tail = "", abs = false;
			for (let i = parts.length - 1; i >= 0; i--) {
				const p = String(parts[i]);
				if (p === "") continue;
				const r = win32Root(p);
				if (abs) {
					// Already rooted; keep scanning left only for the drive.
					if (r.device) { if (!device) device = r.device; break; }
					continue;
				}
				// A path on ANOTHER drive can't contribute to this resolution.
				if (device && r.device && r.device.toLowerCase() !== device.toLowerCase()) continue;
				if (!device && r.device) device = r.device;
				tail = r.rest && tail ? r.rest + "\\" + tail : (r.rest || tail);
				abs = r.isAbs;
				if (abs && device) break;
			}
			if (!abs) {
				// Fall back to the (posix-shaped) cwd, translated to backslashes.
				const r = win32Root(String(process.cwd()).replace(/\//g, "\\"));
				if (!device) device = r.device;
				tail = r.rest && tail ? r.rest + "\\" + tail : (r.rest || tail);
				abs = r.isAbs;
			}
			const t = win32Segs(tail, abs).join("\\");
			return (device + (abs ? "\\" : "") + t) || ".";
		},
		dirname(p) {
			p = String(p);
			const { device, isAbs, rest } = win32Root(p);
			const root = device + (isAbs ? "\\" : "");
			const trimmed = rest.replace(/[\\/]+$/, "");
			const i = Math.max(trimmed.lastIndexOf("\\"), trimmed.lastIndexOf("/"));
			if (i < 0) return root || ".";
			const head = trimmed.slice(0, i).replace(/[\\/]+$/, "");
			return head ? root + head : (root || ".");
		},
		basename(p, suffix) {
			const { rest } = win32Root(p);
			const trimmed = rest.replace(/[\\/]+$/, "");
			const segs = trimmed.split(/[\\/]+/);
			let b = segs[segs.length - 1] || "";
			if (suffix && b.endsWith(suffix) && b !== suffix) b = b.slice(0, -suffix.length);
			return b;
		},
		extname(p) {
			const b = win32.basename(p);
			const i = b.lastIndexOf(".");
			return i <= 0 ? "" : b.slice(i);
		},
		relative(from, to) {
			const rFrom = win32Root(win32.resolve(from));
			const rTo = win32Root(win32.resolve(to));
			if (rFrom.device.toLowerCase() !== rTo.device.toLowerCase()) {
				return win32.resolve(to); // different drives: no relative path
			}
			const f = rFrom.rest.split(/[\\/]+/).filter(Boolean);
			const t = rTo.rest.split(/[\\/]+/).filter(Boolean);
			let i = 0;
			// Windows paths compare case-insensitively.
			while (i < f.length && i < t.length && f[i].toLowerCase() === t[i].toLowerCase()) i++;
			return [...f.slice(i).map(() => ".."), ...t.slice(i)].join("\\") || "";
		},
		parse(p) {
			p = String(p);
			const { device, isAbs, rest } = win32Root(p);
			const root = device + (isAbs ? "\\" : "");
			const base = win32.basename(p);
			const ext = win32.extname(p);
			let dir = win32.dirname(p);
			if (dir === "." && !root && !/[\\/]/.test(p)) dir = "";
			return { root, dir, base, ext, name: ext ? base.slice(0, -ext.length) : base };
		},
		format(o) {
			const base = o.base ?? (o.name ?? "") + (o.ext ?? "");
			const dir = o.dir ?? o.root ?? "";
			if (!dir) return base;
			if (dir === o.root) return /[\\/]$/.test(dir) || /^[a-zA-Z]:$/.test(dir) ? dir + base : dir + "\\" + base;
			return /[\\/]$/.test(dir) ? dir + base : dir + "\\" + base;
		},
		toNamespacedPath(p) {
			p = String(p);
			if (!win32.isAbsolute(p)) return p;
			const resolved = win32.resolve(p);
			if (/^\\\\[^?.]/.test(resolved)) return "\\\\?\\UNC\\" + resolved.slice(2);
			if (/^[a-zA-Z]:\\/.test(resolved)) return "\\\\?\\" + resolved;
			return p;
		},
	};

	// Cross-references like Node: both flavors expose .posix and .win32.
	path.posix = path;
	path.win32 = win32;
	win32.posix = path;
	win32.win32 = win32;
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
			if (type === "error") {
				// events.errorMonitor listeners observe every 'error' emit FIRST,
				// but do not count as handling it (Node: monitoring only).
				const ml = this._list(EventEmitter.errorMonitor, false);
				if (ml) for (const fn of [...ml]) fn.apply(this, args);
			}
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
	// Defined here (not just re-exported in the extended layer) because emit()
	// above consults it on every 'error' emission.
	EventEmitter.errorMonitor = Symbol("events.errorMonitor");
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
	// The unhandled-rejection channel. The host calls this once per promise
	// rejection that reached a microtask checkpoint with nothing to handle it;
	// only the engine can see those (an async function's promise is made by the
	// engine, so no Promise wrapper here observes it). This REPLACES the web
	// layer's `unhandledrejection` event dispatch: Node reports through
	// process, not through a global event.
	//
	// Node routes a rejection with no 'unhandledRejection' listener to
	// 'uncaughtException' with origin 'unhandledRejection'. With neither
	// listener Node terminates the process; here it is reported on stderr and
	// the loop continues, so an unhandled rejection is loud but never turns a
	// working embedding into a crashing one.
	globalThis.__unhandled_rejection = (reason, promise) => {
		if (process.listenerCount && process.listenerCount("unhandledRejection") > 0) {
			process.emit("unhandledRejection", reason, promise);
			return;
		}
		const err = reason instanceof Error ? reason : Object.assign(
			new Error("This error originated either by throwing inside of an async " +
				"function without a catch block, or by rejecting a promise which was " +
				"not handled with .catch(). The reason " + inspect(reason) + "."),
			{ code: "ERR_UNHANDLED_REJECTION" });
		if (process.listenerCount && process.listenerCount("uncaughtException") > 0) {
			process.emit("uncaughtException", err, "unhandledRejection");
			return;
		}
		console.error(err);
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

	// Node-style string quoting: single quotes, escaping backslash/quote and
	// the common control characters.
	function quoteString(s) {
		return "'" + s.replace(/\\/g, "\\\\").replace(/'/g, "\\'")
			.replace(/\n/g, "\\n").replace(/\r/g, "\\r").replace(/\t/g, "\\t") + "'";
	}

	// Node lets an object customize its util.inspect / console.log rendering via
	// a [Symbol.for('nodejs.util.inspect.custom')] method; pino, ORMs, and many
	// custom-error classes rely on it (without it they log as "{}").
	const customInspectSymbol = Symbol.for("nodejs.util.inspect.custom");

	function inspect(v, opts = {}, depth = 0, seen = new Set()) {
		switch (typeof v) {
			case "string": return depth === 0 && opts.raw ? v : quoteString(v);
			case "number": case "boolean": case "undefined": return String(v);
			case "bigint": return String(v) + "n";
			case "symbol": return String(v);
			case "function": return `[Function: ${v.name || "anonymous"}]`;
		}
		if (v === null) return "null";
		// Honor the custom-inspect hook before any built-in shape handling. It is
		// called with (depth, options, inspect): depth is the REMAINING recursion
		// budget (Node's convention), options is forwarded, and the third arg is a
		// re-entrant inspect the hook can use for nested values. A string result is
		// used verbatim; any other value is inspected in turn.
		if ((typeof v === "object" || typeof v === "function") && opts.customInspect !== false) {
			let hook;
			try { hook = v[customInspectSymbol]; } catch { hook = undefined; }
			if (typeof hook === "function" && opts.customInspect !== false) {
				const maxDepth = opts.depth === null ? Infinity : (opts.depth === undefined ? 2 : opts.depth);
				const nested = (val, o) => inspect(val, o || opts, depth + 1, seen);
				let r;
				try { r = hook.call(v, maxDepth - depth, opts, nested); } catch (e) { r = undefined; }
				if (typeof r === "string") return r;
				if (r !== undefined && r !== v) return inspect(r, opts, depth + 1, seen);
			}
		}
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
		// A non-Buffer TypedArray renders as `Uint8Array(3) [ 1, 2, 3 ]`, not a
		// plain {0:1,1:2,...} object.
		if (ArrayBuffer.isView(v) && !(v instanceof DataView)) {
			return `${v.constructor.name}(${v.length}) [ ${[...v].join(", ")} ]`;
		}
		if (seen.has(v)) return "[Circular]";
		// Node: depth defaults to 2; null means unlimited (undefined means default).
		const maxDepth = opts.depth === null ? Infinity : (opts.depth === undefined ? 2 : opts.depth);
		if (depth > maxDepth) return Array.isArray(v) ? "[Array]" : "[Object]";
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

	// util.types predicates: prefer internal-slot brand checks (a prototype
	// getter/method throws for the wrong receiver) over Object.prototype.
	// toString, which a Symbol.toStringTag can spoof.
	const brandCheck = (fn) => (v) => {
		if (v === null || (typeof v !== "object" && typeof v !== "function")) return false;
		try { fn(v); return true; } catch { return false; }
	};
	const protoGetter = (C, prop) => Object.getOwnPropertyDescriptor(C.prototype, prop).get;
	const mapSize = protoGetter(Map, "size");
	const setSize = protoGetter(Set, "size");
	const abByteLength = protoGetter(ArrayBuffer, "byteLength");
	const sabByteLength = typeof SharedArrayBuffer !== "undefined" ? protoGetter(SharedArrayBuffer, "byteLength") : null;
	const dvByteLength = protoGetter(DataView, "byteLength");
	// %TypedArray%.prototype[Symbol.toStringTag] getter: returns the concrete
	// constructor name for any typed array, undefined for everything else.
	const taTag = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(Uint8Array.prototype), Symbol.toStringTag).get;
	const typedArrayTag = (v) => {
		if (v === null || typeof v !== "object") return undefined;
		try { return taTag.call(v); } catch { return undefined; }
	};
	const isDateCheck = brandCheck((v) => Date.prototype.getTime.call(v));
	// The `source` getter brand-checks [[OriginalSource]] (side-effect free,
	// unlike exec() which would touch lastIndex).
	const reSource = protoGetter(RegExp, "source");
	const isRegExpCheck = brandCheck((v) => reSource.call(v));
	const isMapCheck = brandCheck((v) => mapSize.call(v));
	const isSetCheck = brandCheck((v) => setSize.call(v));
	const isWeakMapCheck = brandCheck((v) => WeakMap.prototype.has.call(v, Object.prototype));
	const isWeakSetCheck = brandCheck((v) => WeakSet.prototype.has.call(v, Object.prototype));
	const isArrayBufferCheck = brandCheck((v) => abByteLength.call(v));
	const isSharedArrayBufferCheck = sabByteLength ? brandCheck((v) => sabByteLength.call(v)) : () => false;
	const isDataViewCheck = brandCheck((v) => dvByteLength.call(v));
	const isNumberObjectCheck = brandCheck((v) => Number.prototype.valueOf.call(v));
	const isStringObjectCheck = brandCheck((v) => String.prototype.valueOf.call(v));
	const isBooleanObjectCheck = brandCheck((v) => Boolean.prototype.valueOf.call(v));
	const isSymbolObjectCheck = brandCheck((v) => Symbol.prototype.valueOf.call(v));
	const isBigIntObjectCheck = typeof BigInt !== "undefined" ? brandCheck((v) => BigInt.prototype.valueOf.call(v)) : () => false;
	const utilTypes = {
		isDate: isDateCheck,
		isRegExp: isRegExpCheck,
		isMap: isMapCheck,
		isSet: isSetCheck,
		isWeakMap: isWeakMapCheck,
		isWeakSet: isWeakSetCheck,
		isArrayBuffer: isArrayBufferCheck,
		isSharedArrayBuffer: isSharedArrayBufferCheck,
		isAnyArrayBuffer: (v) => isArrayBufferCheck(v) || isSharedArrayBufferCheck(v),
		isArrayBufferView: (v) => ArrayBuffer.isView(v),
		isTypedArray: (v) => typedArrayTag(v) !== undefined,
		isUint8Array: (v) => typedArrayTag(v) === "Uint8Array",
		isUint8ClampedArray: (v) => typedArrayTag(v) === "Uint8ClampedArray",
		isInt8Array: (v) => typedArrayTag(v) === "Int8Array",
		isUint16Array: (v) => typedArrayTag(v) === "Uint16Array",
		isInt16Array: (v) => typedArrayTag(v) === "Int16Array",
		isUint32Array: (v) => typedArrayTag(v) === "Uint32Array",
		isInt32Array: (v) => typedArrayTag(v) === "Int32Array",
		isFloat32Array: (v) => typedArrayTag(v) === "Float32Array",
		isFloat64Array: (v) => typedArrayTag(v) === "Float64Array",
		isBigInt64Array: (v) => typedArrayTag(v) === "BigInt64Array",
		isBigUint64Array: (v) => typedArrayTag(v) === "BigUint64Array",
		isDataView: isDataViewCheck,
		isPromise: (v) => v instanceof Promise,
		isAsyncFunction: (v) => typeof v === "function" && Object.prototype.toString.call(v) === "[object AsyncFunction]",
		isGeneratorFunction: (v) => typeof v === "function" && Object.prototype.toString.call(v) === "[object GeneratorFunction]",
		isGeneratorObject: (v) => v !== null && typeof v === "object" && Object.prototype.toString.call(v) === "[object Generator]",
		isNativeError: (v) => v instanceof Error,
		isNumberObject: isNumberObjectCheck,
		isStringObject: isStringObjectCheck,
		isBooleanObject: isBooleanObjectCheck,
		isSymbolObject: isSymbolObjectCheck,
		isBigIntObject: isBigIntObjectCheck,
		isBoxedPrimitive: (v) => isNumberObjectCheck(v) || isStringObjectCheck(v) || isBooleanObjectCheck(v) || isSymbolObjectCheck(v) || isBigIntObjectCheck(v),
		// A Proxy is transparent to JS code by design; detecting one needs engine
		// support, so report false ("not detectable") rather than throwing.
		isProxy: () => false,
	};

	const util = {
		format,
		inspect: (v, opts) => inspect(v, opts || {}),
		inherits(ctor, superCtor) {
			Object.setPrototypeOf(ctor.prototype, superCtor.prototype);
			ctor.super_ = superCtor;
		},
		promisify(fn) {
			// Honor a function's own promisified implementation (Node's
			// util.promisify.custom), e.g. timers' setTimeout.
			const custom = fn[Symbol.for("nodejs.util.promisify.custom")];
			if (custom) return custom;
			// A normal function (not an arrow) so the receiver is forwarded: a
			// promisified METHOD called as obj.pf() must run fn with this===obj.
			const promisified = function (...args) {
				return new Promise((resolve, reject) => {
					fn.call(this, ...args, (err, value) => (err ? reject(err) : resolve(value)));
				});
			};
			promisified[Symbol.for("nodejs.util.promisify.custom")] = promisified;
			return promisified;
		},
		callbackify(fn) {
			// A regular function so the receiver is forwarded: a callbackified
			// METHOD called as obj.fn(cb) must run fn with this===obj.
			return function (...args) {
				const cb = args.pop();
				if (typeof cb !== "function") {
					const e = new TypeError("The last argument must be of type function");
					e.code = "ERR_INVALID_ARG_TYPE";
					throw e;
				}
				Reflect.apply(fn, this, args).then(
					(v) => cb(null, v),
					(e) => {
						// Node wraps a FALSY rejection reason so the callback's
						// `if (err)` convention still detects the failure.
						if (!e) {
							const wrapped = new Error("Promise was rejected with falsy value");
							wrapped.code = "ERR_FALSY_VALUE_REJECTION";
							wrapped.reason = e;
							e = wrapped;
						}
						cb(e);
					});
			};
		},
		// deprecate(fn, msg[, code]) wraps fn so the first call emits a
		// DeprecationWarning (via process.emitWarning) exactly once, then delegates.
		// process.noDeprecation suppresses it; process.throwDeprecation makes the
		// first call throw instead (Node semantics).
		deprecate(fn, msg, code) {
			let warned = false;
			const deprecated = function (...args) {
				if (!warned) {
					warned = true;
					if (globalThis.process && process.throwDeprecation) {
						const e = new Error(msg);
						e.name = "DeprecationWarning";
						if (code) e.code = code;
						throw e;
					} else if (!(globalThis.process && process.noDeprecation)) {
						if (globalThis.process && typeof process.emitWarning === "function") {
							process.emitWarning(msg, { type: "DeprecationWarning", code });
						}
					}
				}
				return Reflect.apply(fn, this, args);
			};
			// Preserve the wrapped function's prototype so `new deprecated()` works.
			if (fn && fn.prototype) deprecated.prototype = fn.prototype;
			return deprecated;
		},
		debuglog: () => () => {},
		isDeepStrictEqual: (a, b) => deepEqual(a, b),
		types: utilTypes,
		TextEncoder: globalThis.TextEncoder,
		TextDecoder: globalThis.TextDecoder,
	};
	util.promisify.custom = Symbol.for("nodejs.util.promisify.custom");
	util.inspect.custom = customInspectSymbol;
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
			// Node coerces any non-primitive (and null/undefined/object/Date) value
			// to "" via stringifyPrimitive; only string/number/boolean pass through.
			const prim = (x) => (typeof x === "string" || typeof x === "number" || typeof x === "boolean") ? String(x) : "";
			const parts = [];
			for (const k of Object.keys(obj || {})) {
				const v = obj[k];
				if (Array.isArray(v)) for (const x of v) parts.push(qsEscape(k) + eq + qsEscape(prim(x)));
				else parts.push(qsEscape(k) + eq + qsEscape(prim(v)));
			}
			return parts.join(sep);
		},
	};

	// ----------------------------------------------------------------- os

	// cpus()/totalmem()/freemem() are backed by the Go host (runtime.NumCPU,
	// runtime.MemStats, the linear-memory ceiling) so worker-pool sizing
	// (os.cpus().length) and memory ratios (freemem()/totalmem()) return real,
	// non-zero values instead of the old []/0 stubs that broke callers.
	core.os = {
		EOL: "\n",
		platform: () => process.platform,
		arch: () => process.arch,
		type: () => (process.platform === "darwin" ? "Darwin" : process.platform === "win32" ? "Windows_NT" : "Linux"),
		release: () => "0.0.0",
		homedir: () => "/",
		tmpdir: () => "/tmp",
		hostname: () => "localhost",
		cpus: () => ops.os_cpus(),
		totalmem: () => ops.os_meminfo().total,
		freemem: () => ops.os_meminfo().free,
		endianness: () => "LE",
		availableParallelism: () => ops.os_cpus().length,
	};

	// ---------------------------------------------------------------- url

	core.url = {
		URL: globalThis.URL,
		URLSearchParams: globalThis.URLSearchParams,
		pathToFileURL: (p) => {
			// Node semantics: pre-encode the characters the URL pathname setter
			// would misinterpret ("%" first so later escapes survive), then let
			// the setter percent-encode the rest (space, "#", "?", controls) so
			// the path round-trips through fileURLToPath.
			const resolved = path.resolve(String(p))
				.replace(/%/g, "%25")
				.replace(/\\/g, "%5C")
				.replace(/\n/g, "%0A")
				.replace(/\r/g, "%0D")
				.replace(/\t/g, "%09");
			const u = new URL("file://");
			u.pathname = resolved;
			return u;
		},
		fileURLToPath: (u) => {
			const s = u instanceof URL ? u : new URL(String(u));
			if (s.protocol !== "file:") {
				const e = new TypeError('The URL must be of scheme file');
				e.code = "ERR_INVALID_URL_SCHEME";
				throw e;
			}
			if (s.hostname !== "" && s.hostname !== "localhost") {
				const e = new TypeError(`File URL host must be "localhost" or empty on this platform`);
				e.code = "ERR_INVALID_FILE_URL_HOST";
				throw e;
			}
			const pathname = s.pathname;
			// An encoded "/" would silently change the directory structure of the
			// decoded path — Node rejects it (posix; win32 also rejects %5C).
			if (/%2f/i.test(pathname)) {
				const e = new TypeError("File URL path must not include encoded / characters");
				e.code = "ERR_INVALID_FILE_URL_PATH";
				throw e;
			}
			return decodeURIComponent(pathname); // decode exactly once
		},
		domainToASCII: (d) => {
			// IDNA-lite ToASCII (lowercase + NFC + RFC 3492 punycode), shared
			// with the WHATWG URL host parser. "" signals an invalid domain.
			const ascii = globalThis.__url_domain_to_ascii(String(d));
			if (ascii === null || ascii === "") return "";
			if (/[\x00-\x20#%/:<>?@[\\\]^|\x7f]/.test(ascii)) return "";
			const labels = ascii.split(".");
			for (let i = 0; i < labels.length; i++) {
				if (labels[i].length > 63) return "";
				// Only a single trailing empty label (root dot) is acceptable.
				if (labels[i] === "" && i !== labels.length - 1) return "";
			}
			return ascii;
		},
		domainToUnicode: (d) => String(d).toLowerCase().split(".").map((label) => {
			if (!label.startsWith("xn--")) return label;
			try { return globalThis.__url_punycode_decode(label.slice(4)); } catch { return label; }
		}).join("."),
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
			const abortErr = () => (signal && signal.reason) || Object.assign(new Error("The operation was aborted"), { name: "AbortError" });
			if (signal && signal.aborted) return rej(abortErr());
			// An abort BEFORE the immediate fires must cancel it and reject.
			const id = setImmediate(() => {
				if (signal) signal.removeEventListener("abort", onAbort);
				res(value);
			});
			const onAbort = () => { clearImmediate(id); rej(abortErr()); };
			if (signal) signal.addEventListener("abort", onAbort, { once: true });
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

	// looseDeepEqual implements assert.deepEqual's LOOSE semantics (Node):
	// primitives via ==, NaN equal to NaN, prototypes NOT compared (a
	// null-prototype object equals a plain one with the same keys), and
	// Date/RegExp/TypedArray/Map/Set contents compared structurally with the
	// loose comparator for values.
	function looseDeepEqual(a, b, seen = new Map()) {
		if (a === b) return true;
		if (typeof a === "number" && typeof b === "number" && Number.isNaN(a) && Number.isNaN(b)) return true;
		if (a === null || b === null) return a == b; // null == undefined
		if (typeof a !== "object" && typeof b !== "object") return a == b;
		// An object never loosely equals a primitive (new Number(1) != 1 here —
		// Node compares boxed primitives as objects).
		if (typeof a !== "object" || typeof b !== "object") return false;
		// Node compares type tags even in loose mode: an Array never equals a
		// plain object, a Date never equals {}, Uint8Array never Uint16Array.
		const tag = Object.prototype.toString.call(a);
		if (tag !== Object.prototype.toString.call(b)) return false;
		// Boxed primitives compare by their unwrapped value (loosely).
		if (tag === "[object Number]" || tag === "[object String]" ||
			tag === "[object Boolean]" || tag === "[object BigInt]") {
			if (a.valueOf() != b.valueOf()) return false;
		}
		// Error names and messages are always compared, even though they are
		// non-enumerable (Node's documented rule).
		if (a instanceof Error && b instanceof Error &&
			(a.name !== b.name || a.message !== b.message)) return false;
		if (seen.get(a) === b) return true;
		seen.set(a, b);
		if (a instanceof Date && b instanceof Date) return a.getTime() === b.getTime();
		if (a instanceof RegExp && b instanceof RegExp) return String(a) === String(b);
		if (ArrayBuffer.isView(a) && ArrayBuffer.isView(b)) {
			if (a.byteLength !== b.byteLength) return false;
			const ua = new Uint8Array(a.buffer, a.byteOffset, a.byteLength);
			const ub = new Uint8Array(b.buffer, b.byteOffset, b.byteLength);
			return ua.every((x, i) => x === ub[i]);
		}
		if (a instanceof Map && b instanceof Map) {
			if (a.size !== b.size) return false;
			const be = [...b], used = new Array(be.length).fill(false);
			outer: for (const [ak, av] of a) {
				for (let i = 0; i < be.length; i++) {
					if (!used[i] && looseDeepEqual(ak, be[i][0], seen) && looseDeepEqual(av, be[i][1], seen)) {
						used[i] = true;
						continue outer;
					}
				}
				return false;
			}
			return true;
		}
		if (a instanceof Set && b instanceof Set) {
			if (a.size !== b.size) return false;
			const bv = [...b], used = new Array(bv.length).fill(false);
			outer: for (const av of a) {
				for (let i = 0; i < bv.length; i++) {
					if (!used[i] && looseDeepEqual(av, bv[i], seen)) { used[i] = true; continue outer; }
				}
				return false;
			}
			return true;
		}
		const ka = Object.keys(a), kb = Object.keys(b);
		return ka.length === kb.length && ka.every((k) => Object.prototype.hasOwnProperty.call(b, k) && looseDeepEqual(a[k], b[k], seen));
	}

	// matchError validates a thrown value against a Node error matcher: an error
	// constructor, a RegExp (tested against the message), a validation object,
	// or a predicate function. In a validation object a RegExp property value
	// is applied with .test() against the actual STRING property (Node does
	// this for every key, not just message); other values must deep-equal.
	function matchError(err, matcher) {
		if (matcher === undefined) return true;
		if (typeof matcher === "function") {
			// Constructor (Error subclass) vs. plain predicate.
			if (matcher.prototype instanceof Error || matcher === Error) return err instanceof matcher;
			return matcher(err) === true;
		}
		if (matcher instanceof RegExp) return matcher.test(err && err.message !== undefined ? String(err.message) : String(err));
		if (matcher && typeof matcher === "object") {
			return Object.keys(matcher).every((k) => {
				const want = matcher[k];
				const got = err ? err[k] : undefined;
				if (want instanceof RegExp && typeof got === "string") return want.test(got);
				return deepEqual(got, want);
			});
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
		deepEqual: (a, e, m) => { if (!looseDeepEqual(a, e)) throw new AssertionError({ actual: a, expected: e, operator: "deepEqual", message: m }); },
		notDeepEqual: (a, e, m) => { if (looseDeepEqual(a, e)) throw new AssertionError({ actual: a, expected: e, operator: "notDeepEqual", message: m }); },
		deepStrictEqual: (a, e, m) => { if (!deepEqual(a, e)) throw new AssertionError({ actual: a, expected: e, operator: "deepStrictEqual", message: m }); },
		notDeepStrictEqual: (a, e, m) => { if (deepEqual(a, e)) throw new AssertionError({ actual: a, expected: e, operator: "notDeepStrictEqual", message: m }); },
		ifError: (v) => { if (v !== null && v !== undefined) throw v instanceof Error ? v : new AssertionError({ actual: v, operator: "ifError", message: "ifError got unwanted exception: " + v }); },
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
		async doesNotReject(fn, matcher, m) {
			if (typeof matcher === "string") { m = matcher; matcher = undefined; }
			try { await (typeof fn === "function" ? fn() : fn); } catch (e) {
				if (matchError(e, matcher)) throw new AssertionError({ actual: e, operator: "doesNotReject", message: m || "Got unwanted rejection" });
				throw e;
			}
		},
		match: (s, re, m) => { if (!re.test(s)) throw new AssertionError({ actual: s, expected: re, operator: "match", message: m }); },
	});
	// Strict mode (assert.strict / node:assert/strict) aliases the loose
	// comparators to their strict forms, so assert/strict.equal(0, '') THROWS
	// (Object.is) instead of passing under loose ==.
	const strict = Object.assign(function strict(v, m) { return assert(v, m); }, assert, {
		equal: assert.strictEqual,
		notEqual: assert.notStrictEqual,
		deepEqual: assert.deepStrictEqual,
		notDeepEqual: assert.notDeepStrictEqual,
	});
	strict.strict = strict;
	assert.strict = strict;
	core.assert = assert;
	core["assert/strict"] = strict;

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

	function statsOf(r, bigint) {
		const typers = {
			isFile: () => !r.dir,
			isDirectory: () => r.dir,
			isSymbolicLink: () => false,
			isFIFO: () => false,
			isSocket: () => false,
			isBlockDevice: () => false,
			isCharacterDevice: () => false,
		};
		if (bigint) {
			// { bigint: true }: every numeric field is a BigInt, plus the *Ns
			// nanosecond fields Node adds only in bigint mode.
			const ms = BigInt(Math.trunc(r.mtimeMs));
			const ns = ms * 1000000n;
			return {
				size: BigInt(r.size),
				mode: BigInt(r.mode),
				ino: 0n, dev: 0n, nlink: 1n, uid: 1000n, gid: 1000n, rdev: 0n,
				blksize: 4096n,
				blocks: BigInt(Math.ceil(r.size / 512)),
				atimeMs: ms, ctimeMs: ms, birthtimeMs: ms, mtimeMs: ms,
				atimeNs: ns, ctimeNs: ns, birthtimeNs: ns, mtimeNs: ns,
				mtime: new Date(r.mtimeMs),
				atime: new Date(r.mtimeMs),
				ctime: new Date(r.mtimeMs),
				birthtime: new Date(r.mtimeMs),
				...typers,
			};
		}
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
			...typers,
		};
	}

	// flagOf extracts the string `flag` option ("w", "a", "wx", ...) from a
	// writeFile/appendFile options bag; string opts are encodings, not flags.
	const flagOf = (opts, dflt) =>
		(opts && typeof opts === "object" && typeof opts.flag === "string" ? opts.flag : dflt);
	// Node's stringToFlags: 'x' anywhere means O_EXCL (wx/xw/ax/xa/wx+/...),
	// 'a' anywhere (in a valid flag) means O_APPEND.
	function checkExclusive(p, flag) {
		if (flag.includes("x") && ops.fs_exists(String(p))) {
			throw fsError({ code: "EEXIST", message: "file already exists" }, "open", p);
		}
	}

	// Active watchFile() host watchers keyed by path, so unwatchFile() can stop
	// them — each fs_watch holds a host loop-pending, and never calling
	// fs_unwatch would keep the event loop alive forever.
	const watchFileWatchers = new Map();

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
			// A string is decoded per the encoding option (default utf8), so
			// writeFileSync(p, "deadbeef", "hex") writes the 4 decoded bytes, not
			// the ASCII of the string.
			const payload = typeof data === "string" ? Buffer.from(data, encodingOf(opts) || "utf8") : Buffer.from(data);
			// Honor the `flag` option: an append flag ("a"/"a+"/"as"/"ax"...) must
			// append, not truncate. Ignoring it silently discarded a file's existing
			// contents on writeFileSync(p, line, { flag: "a" }) — the common
			// append-a-log-line pattern. An exclusive flag ("wx"/"xw"/"ax"/...)
			// must fail with EEXIST when the file already exists, not overwrite.
			const flag = flagOf(opts, "w");
			checkExclusive(p, flag);
			const append = flag.includes("a");
			const r = ops.fs_write_file(String(p), payload, append);
			if (isErr(r)) throw fsError(r, "open", p);
		},
		appendFileSync(p, data, opts) {
			const payload = typeof data === "string" ? Buffer.from(data, encodingOf(opts) || "utf8") : Buffer.from(data);
			checkExclusive(p, flagOf(opts, "a"));
			const r = ops.fs_write_file(String(p), payload, true);
			if (isErr(r)) throw fsError(r, "open", p);
		},
		existsSync: (p) => ops.fs_exists(String(p)),
		statSync(p, opts) {
			const r = ops.fs_stat(String(p));
			if (isErr(r)) throw fsError(r, "stat", p);
			return statsOf(r, !!(opts && opts.bigint));
		},
		lstatSync(p, opts) { return fsSync.statSync(p, opts); },
		readdirSync(p, options) {
			const base = String(p);
			const dirent = (name, parentPath, isDir) => ({
				name,
				parentPath,
				isDirectory: () => isDir,
				isFile: () => !isDir,
				isSymbolicLink: () => false,
				isFIFO: () => false,
				isSocket: () => false,
				isBlockDevice: () => false,
				isCharacterDevice: () => false,
			});
			const withTypes = !!(options && options.withFileTypes);
			if (!options || !options.recursive) {
				const r = ops.fs_readdir(base);
				if (isErr(r)) throw fsError(r, "scandir", p);
				if (withTypes) return r.names.map((name, i) => dirent(name, base, r.dirs[i]));
				return r.names;
			}
			// { recursive: true }: breadth-first like Node — a directory's own
			// entries first, then descendants, names joined with "/" relative to
			// the starting path ("sub/b.txt"). Dirents keep the bare name and get
			// parentPath = base joined with the relative directory.
			const out = [];
			const queue = [""]; // relative dir paths ("" = the root of the walk)
			while (queue.length) {
				const rel = queue.shift();
				const dirPath = rel === "" ? base : path.join(base, rel);
				const r = ops.fs_readdir(dirPath);
				if (isErr(r)) throw fsError(r, "scandir", dirPath);
				r.names.forEach((name, i) => {
					const relName = rel === "" ? name : rel + "/" + name;
					out.push(withTypes ? dirent(name, dirPath, r.dirs[i]) : relName);
					if (r.dirs[i]) queue.push(relName);
				});
			}
			return out;
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
		realpathSync(p) {
			// No symlinks exist in this FS (memfs/host mounts have none), so the
			// canonical path is just the resolved one — but Node's realpath still
			// requires the target to EXIST (ENOENT otherwise; syscall "lstat").
			const resolved = path.resolve(String(p));
			if (!ops.fs_exists(resolved)) {
				throw fsError({ code: "ENOENT", message: "no such file or directory" }, "lstat", String(p));
			}
			return resolved;
		},
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
			const key = String(p);
			let set = watchFileWatchers.get(key);
			if (!set) { set = new Set(); watchFileWatchers.set(key, set); }
			set.add(id);
			return { _id: id };
		},
		unwatchFile(p) {
			// Stop every watcher on this path (releasing each host loop-pending);
			// without this the fs_watch pending never drops and the loop hangs.
			const key = String(p);
			const set = watchFileWatchers.get(key);
			if (set) {
				for (const id of set) ops.fs_unwatch(id);
				watchFileWatchers.delete(key);
			}
		},
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
			const f = String(flags);
			// Exclusive ('x') fails on an existing file; the host op then gets the
			// base flag ('x' and the sync 's' modifier stripped: "wx" -> "w").
			checkExclusive(p, f);
			const r = ops.fs_open(String(p), f.replace(/[xs]/g, "") || "r");
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
		fstatSync(fd, opts) {
			const r = ops.fs_fstat(fd);
			if (isErr(r)) throw fsError(r, "fstat", fd);
			return statsOf(r, !!(opts && opts.bigint));
		},
		ftruncateSync(fd, len = 0) {
			const r = ops.fs_ftruncate(fd, Number(len) || 0);
			if (isErr(r)) throw fsError(r, "ftruncate", fd);
		},
		truncateSync(p, len = 0) {
			const fd = fsSync.openSync(p, "r+");
			try { fsSync.ftruncateSync(fd, len); } finally { fsSync.closeSync(fd); }
		},
		chmodSync(p, mode) {
			// Node accepts an octal string ("755") or a number.
			const m = typeof mode === "string" ? parseInt(mode, 8) : Number(mode);
			const r = ops.fs_chmod(String(p), m & 0o7777);
			if (isErr(r)) throw fsError(r, "chmod", p);
		},
		lchmodSync(p, mode) { return fsSync.chmodSync(p, mode); }, // no symlinks
		chownSync() {},
		lchownSync() {},
		utimesSync(p, atime, mtime) {
			// Node: numbers are epoch SECONDS; Dates and numeric strings work too.
			const toMs = (t) => (t instanceof Date ? t.getTime() : Number(t) * 1000);
			const r = ops.fs_utimes(String(p), toMs(atime), toMs(mtime));
			if (isErr(r)) throw fsError(r, "utime", p);
		},
		lutimesSync(p, atime, mtime) { return fsSync.utimesSync(p, atime, mtime); },
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
		chmod: callbackify1(fsSync.chmodSync),
		lchmod: callbackify1(fsSync.chmodSync),
		utimes: callbackify1(fsSync.utimesSync),
		lutimes: callbackify1(fsSync.utimesSync),
		truncate: callbackify1(fsSync.truncateSync),
		ftruncate: callbackify1(fsSync.ftruncateSync),
		chown: (p, uid, gid, cb) => queueMicrotask(() => cb(null)),
		exists: (p, cb) => queueMicrotask(() => cb(fsSync.existsSync(p))),
		access: (p, mode, cb) => {
			const done = typeof mode === "function" ? mode : cb;
			queueMicrotask(() => {
				try { fsSync.accessSync(p); done(null); } catch (e) { done(e); }
			});
		},
		// fd-callback API (graceful-fs — a transitive dep of fs-extra/rimraf/mkdirp/
		// much npm tooling — monkey-patches and calls these): open/close/read/write/
		// fstat over the existing *Sync fd ops.
		open: (p, flags, mode, cb) => {
			if (typeof flags === "function") { cb = flags; flags = "r"; }
			else if (typeof mode === "function") { cb = mode; }
			queueMicrotask(() => { try { cb(null, fsSync.openSync(p, flags)); } catch (e) { cb(e); } });
		},
		close: (fd, cb) => queueMicrotask(() => { try { fsSync.closeSync(fd); if (cb) cb(null); } catch (e) { if (cb) cb(e); } }),
		read: (fd, buffer, offset, length, position, cb) => {
			queueMicrotask(() => { try { const n = fsSync.readSync(fd, buffer, offset, length, position); cb(null, n, buffer); } catch (e) { cb(e); } });
		},
		write: (fd, buffer, offset, length, position, cb) => {
			if (typeof offset === "function") { cb = offset; offset = length = position = undefined; }
			else if (typeof length === "function") { cb = length; length = position = undefined; }
			else if (typeof position === "function") { cb = position; position = undefined; }
			queueMicrotask(() => { try { const n = fsSync.writeSync(fd, buffer, offset, length, position); cb(null, n, buffer); } catch (e) { cb(e); } });
		},
		fstat: (fd, opts, cb) => { if (typeof opts === "function") cb = opts; queueMicrotask(() => { try { cb(null, fsSync.fstatSync(fd)); } catch (e) { cb(e); } }); },
		constants: {
			F_OK: 0, R_OK: 4, W_OK: 2, X_OK: 1, COPYFILE_EXCL: 1,
			// File-type bits so `(stats.mode & S_IFMT) === S_IFDIR` works.
			S_IFMT: 0o170000, S_IFREG: 0o100000, S_IFDIR: 0o040000, S_IFLNK: 0o120000,
			S_IFCHR: 0o020000, S_IFBLK: 0o060000, S_IFIFO: 0o010000, S_IFSOCK: 0o140000,
			// Common open flags.
			O_RDONLY: 0, O_WRONLY: 1, O_RDWR: 2, O_CREAT: 0o100, O_EXCL: 0o200,
			O_TRUNC: 0o1000, O_APPEND: 0o2000,
		},
	};
	core.fs = fsMod;

	// A FileHandle for fs.promises.open (read/write/close/stat/readFile/writeFile
	// over the fd ops).
	const makeFileHandle = (fd) => ({
		fd,
		read(buffer, offset, length, position) { try { const n = fsSync.readSync(fd, buffer, offset, length, position); return Promise.resolve({ bytesRead: n, buffer }); } catch (e) { return Promise.reject(e); } },
		write(buffer, offset, length, position) { try { const n = fsSync.writeSync(fd, buffer, offset, length, position); return Promise.resolve({ bytesWritten: n, buffer }); } catch (e) { return Promise.reject(e); } },
		close() { try { fsSync.closeSync(fd); return Promise.resolve(); } catch (e) { return Promise.reject(e); } },
		stat(opts) { try { return Promise.resolve(fsSync.fstatSync(fd, opts)); } catch (e) { return Promise.reject(e); } },
		truncate(len = 0) { try { fsSync.ftruncateSync(fd, len); return Promise.resolve(); } catch (e) { return Promise.reject(e); } },
		readFile(opts) {
			try {
				const st = fsSync.fstatSync(fd);
				const buf = Buffer.alloc(st.size);
				if (st.size) fsSync.readSync(fd, buf, 0, st.size, 0);
				const enc = typeof opts === "string" ? opts : opts && opts.encoding;
				return Promise.resolve(enc ? buf.toString(enc) : buf);
			} catch (e) { return Promise.reject(e); }
		},
		writeFile(data, opts) {
			try {
				const enc = typeof opts === "string" ? opts : (opts && opts.encoding) || "utf8";
				const buf = typeof data === "string" ? Buffer.from(data, enc) : Buffer.from(data);
				fsSync.writeSync(fd, buf, 0, buf.length, 0);
				return Promise.resolve();
			} catch (e) { return Promise.reject(e); }
		},
	});

	const promisified = {};
	for (const [name, syncName] of [
		["readFile", "readFileSync"], ["writeFile", "writeFileSync"],
		["appendFile", "appendFileSync"], ["stat", "statSync"],
		["lstat", "lstatSync"], ["readdir", "readdirSync"],
		["mkdir", "mkdirSync"], ["rmdir", "rmdirSync"], ["rm", "rmSync"],
		["unlink", "unlinkSync"], ["rename", "renameSync"],
		["realpath", "realpathSync"], ["copyFile", "copyFileSync"],
		["mkdtemp", "mkdtempSync"], ["cp", "cpSync"],
		["chmod", "chmodSync"], ["lchmod", "lchmodSync"],
		["utimes", "utimesSync"], ["lutimes", "lutimesSync"],
		["truncate", "truncateSync"],
	]) {
		promisified[name] = (...args) => {
			try { return Promise.resolve(fsSync[syncName](...args)); } catch (e) { return Promise.reject(e); }
		};
	}
	// fs.promises.readFile/writeFile accept { signal }: a pre-aborted signal must
	// reject with the AbortError (the ops themselves complete synchronously, so
	// pre-abort is the only observable abort window).
	const fsAbortErr = (signal) => {
		const e = new Error("The operation was aborted");
		e.name = "AbortError";
		e.code = "ABORT_ERR";
		if (signal && signal.reason !== undefined) e.cause = signal.reason;
		return e;
	};
	promisified.readFile = (p, opts) => {
		const signal = opts && typeof opts === "object" ? opts.signal : undefined;
		if (signal && signal.aborted) return Promise.reject(fsAbortErr(signal));
		try { return Promise.resolve(fsSync.readFileSync(p, opts)); } catch (e) { return Promise.reject(e); }
	};
	promisified.writeFile = (p, data, opts) => {
		const signal = opts && typeof opts === "object" ? opts.signal : undefined;
		if (signal && signal.aborted) return Promise.reject(fsAbortErr(signal));
		try { return Promise.resolve(fsSync.writeFileSync(p, data, opts)); } catch (e) { return Promise.reject(e); }
	};
	promisified.access = (p) => (ops.fs_exists(String(p)) ? Promise.resolve() : Promise.reject(fsError({ code: "ENOENT", message: "no such file or directory" }, "access", p)));
	promisified.open = (p, flags) => { try { return Promise.resolve(makeFileHandle(fsSync.openSync(p, flags))); } catch (e) { return Promise.reject(e); } };
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
			// A signal death crosses from Go as undefined code; Node reports null.
			code = code === undefined ? null : code;
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
		// Node accepts a path, a file: URL string, or a URL object
		// (createRequire(import.meta.url) is the canonical use).
		let f = typeof from === "string" ? from
			: from && typeof from.href === "string" ? from.href
			: "/main.js";
		if (f.startsWith("file:")) {
			f = "/" + f.slice(5).replace(/^\/+/, "");
			// import.meta.url is percent-encoded ("file:///my%20dir/main.mjs");
			// the fs path needs the decoded spelling. A "%" that is not a valid
			// escape keeps the string as-is (raw paths pass through unchanged).
			try { f = decodeURIComponent(f); } catch { /* not percent-encoded */ }
		}
		const m = new Module(f);
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
				// A leading #! line is legal in a Node entry file but lands
				// mid-source inside the Function wrapper, where it is a
				// SyntaxError — neutralize it as a comment of identical length
				// so line numbers are preserved.
				let text = src;
				if (text.charCodeAt(0) === 0x23 /* # */ && text.charCodeAt(1) === 0x21 /* ! */) {
					text = "//" + text.slice(2);
				}
				const fn = new Function(
					"exports", "require", "module", "__filename", "__dirname",
					text + "\n//# sourceURL=" + fsPath,
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
