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
			try {
				process.emit("exit", process.exitCode ?? 0);
			} catch (e) {
				// An 'exit' handler calling process.exit() is ordinary — it is how
				// a failed assertion sets the code from inside the handler — and
				// its unwind sentinel has nowhere left to go. Record the code and
				// let the process finish exiting; rethrowing made the sentinel
				// itself the reported failure, hiding what the test actually said.
				if (!(e && e.__nodeExit)) throw e;
			}
		}
	};
	globalThis.__node_reset_exit_emitted = () => { __exitEmitted = false; };

	// The CHILD end of a fork() channel. `__node_has_ipc` is set by the host on
	// a runtime that was forked; without it process.send is absent, which is
	// exactly how Node tells a forked child from a plain one.
	if (globalThis.__node_has_ipc) {
		process.connected = true;
		// The channel starts UNREF'd, as Node's does, and only counts as a
		// reason to stay alive once the child is actually listening. Holding it
		// ref'd unconditionally deadlocked every well-behaved fork: the child
		// waited on a channel the parent would not close until the child
		// finished, and the parent would not see it finish until it did.
		let channelRefs = 0;
		const refChannel = (on) => {
			// Once disconnected the channel can never deliver again, so listening
			// on it is not a reason to stay alive.
			if (on && !process.connected) return;
			const before = channelRefs;
			channelRefs = Math.max(0, channelRefs + (on ? 1 : -1));
			if ((before > 0) !== (channelRefs > 0)) ops.ipc_ref(0, channelRefs > 0);
		};
		process.channel = {
			ref() { refChannel(true); },
			unref() { refChannel(false); },
		};
		// Listening for a message is what makes the channel load-bearing.
		// Listening for a message is what makes the channel load-bearing.
		process.on("newListener", (event) => { if (event === "message") refChannel(true); });
		process.on("removeListener", (event) => { if (event === "message") refChannel(false); });
		process.send = function send(message, sendHandle, options, callback) {
			if (typeof sendHandle === "function") { callback = sendHandle; sendHandle = undefined; }
			else if (typeof options === "function") { callback = options; options = undefined; }
			if (!process.connected) {
				const e = Object.assign(new Error("channel closed"), { code: "ERR_IPC_CHANNEL_CLOSED" });
				if (callback) { process.nextTick(() => callback(e)); return false; }
				throw e;
			}
			const ok = ops.ipc_send(0, JSON.stringify(message === undefined ? null : message));
			if (callback) process.nextTick(() => callback(ok ? null : new Error("channel closed")));
			return ok;
		};
		process.disconnect = function disconnect() {
			if (!process.connected) return;
			process.connected = false;
			ops.ipc_disconnect(0);
			process.nextTick(() => process.emit("disconnect"));
		};
		ops.ipc_start(0,
			(json) => { try { process.emit("message", JSON.parse(json)); } catch { /* not JSON: drop */ } },
			() => { if (process.connected) { process.connected = false; process.emit("disconnect"); } },
			false); // the child end does not hold the loop open by itself
	}

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
		// No handler: this is a FATAL exception, and Node's contract is that the
		// process prints it and dies with code 1. Reporting it and carrying on
		// leaves every handle the program had opened — a listening server, a
		// socket, a timer — holding the loop open forever, so a program that
		// threw simply never finishes. That is the single largest cause of
		// non-terminating runs in the Node test suite.
		// SpiderMonkey's .stack omits the name/message line that Node prints.
		try { console.error(e instanceof Error ? `${e.name}: ${e.message}\n${e.stack}` : String(e)); } catch { /* ignore */ }
		process.exitCode = 1;
		process.exit(1);
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
		// The exit sentinel travels out through the tick queue's microtask, so it
		// arrives here as a rejection. It is a control-flow signal, not an error
		// to report.
		if (reason && reason.__nodeExit) return;
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
		// Past the depth limit the value is named, not shown — and the name is
		// its CLASS where it has one, since "[BlockList]" tells the reader what
		// was elided and "[Object]" does not.
		if (depth > maxDepth) {
			if (Array.isArray(v)) return "[Array]";
			const ctor = v.constructor && v.constructor.name;
			return ctor && ctor !== "Object" ? `[${ctor}]` : "[Object]";
		}
		seen.add(v);
		try {
			if (Array.isArray(v)) {
				if (!v.length) return "[]";
				const cap = inspectCap(opts);
				const shown = v.slice(0, cap).map((x) => inspect(x, opts, depth + 1, seen));
				return `[ ${shown.join(", ")}${inspectMore(v.length, cap)} ]`;
			}
			if (v instanceof Map) {
				const cap = inspectCap(opts);
				const items = inspectTake(v, cap).map(([k, x]) => ` ${inspect(k, opts, depth + 1, seen)} => ${inspect(x, opts, depth + 1, seen)}`);
				return `Map(${v.size}) {${items.join(",")}${inspectMore(v.size, cap)} }`;
			}
			if (v instanceof Set) {
				const cap = inspectCap(opts);
				const items = inspectTake(v, cap).map((x) => " " + inspect(x, opts, depth + 1, seen));
				return `Set(${v.size}) {${items.join(",")}${inspectMore(v.size, cap)} }`;
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

	// Node's util.inspect renders at most maxArrayLength (default 100) entries of
	// an array, Map or Set and reports the rest as "... N more items". Formatting
	// all of them builds an intermediate proportional to the collection, which is
	// how console.log of a ten-million-element array exhausted the interpreter's
	// whole memory budget instead of printing a line. null means "no limit", as
	// in Node.
	function inspectCap(opts) {
		const n = opts && opts.maxArrayLength;
		if (n === null) return Infinity;
		return typeof n === "number" && n >= 0 ? n : 100;
	}

	function inspectMore(total, cap) {
		const n = total - cap;
		return n > 0 ? `, ... ${n} more item${n === 1 ? "" : "s"}` : "";
	}

	function inspectTake(it, cap) {
		const out = [];
		for (const x of it) {
			if (out.length >= cap) break;
			out.push(x);
		}
		return out;
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

	// Outstanding immediates, so process.getActiveResourcesInfo() can name them.
	// An Immediate stops counting the moment it is about to run, which is what
	// Node reports from inside the callback.
	const liveImmediates = new Set();
	globalThis.__active_immediates = () => [...liveImmediates].map(() => "Immediate");
	globalThis.setImmediate = (fn, ...args) => {
		if (typeof fn !== "function") throw new TypeError("callback is not a function");
		let id;
		const run = args.length ? () => fn(...args) : fn;
		id = ops.immediate_set(() => { liveImmediates.delete(id); run(); });
		liveImmediates.add(id);
		return id;
	};
	globalThis.clearImmediate = (id) => {
		if (id === undefined || id === null) return;
		const n = Number(id) || 0;
		liveImmediates.delete(n);
		ops.immediate_clear(n);
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
		// A RegExp matcher runs against the STRING REPRESENTATION of the error
		// ("TypeError: bad thing"), not its message alone — which is what makes
		// an anchored /^Error: toString$/ meaningful. A coded error also answers
		// to the form its stack header takes, "TypeError [ERR_X]: bad thing",
		// which is how a test matches on the code with a bare /ERR_X/.
		if (matcher instanceof RegExp) {
			if (matcher.test(String(err))) return true;
			return !!(err && err.code) && matcher.test(`${err.name} [${err.code}]: ${err.message}`);
		}
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

	// fsResolve turns a path argument into the absolute path the host ops take.
	// A RELATIVE path means "relative to process.cwd()", exactly as in Node —
	// without this, process.chdir() would move the reported directory while
	// every relative read kept resolving against the filesystem root, which is
	// worse than not having chdir at all.
	// Symbolic links live HERE, in the runtime, not in the filesystem: this
	// embedding's FS is an abstract fs.FS with no link concept, and inventing
	// one in the host would mean every backing store had to grow it. A link
	// created by a program is therefore visible to that program (which is what
	// symlink tests, build tools and package layouts need) and to nothing else.
	// That limitation is the honest shape of the feature here, not a bug.
	const symlinks = new Map(); // absolute link path -> target as given

	// followLinks resolves a path through the link table, one component at a
	// time so a link to a DIRECTORY works for paths beneath it. The hop cap is
	// what turns a link cycle into ELOOP instead of a hang.
	function followLinks(abs) {
		if (symlinks.size === 0) return abs;
		let path = abs;
		for (let hop = 0; hop < 40; hop++) {
			if (symlinks.has(path)) {
				path = absoluteAgainst(symlinks.get(path), parentOf(path));
				continue;
			}
			// A prefix of the path may itself be a link.
			let replaced = false;
			for (const [link, target] of symlinks) {
				if (path.startsWith(link + "/")) {
					path = absoluteAgainst(target, parentOf(link)) + path.slice(link.length);
					replaced = true;
					break;
				}
			}
			if (!replaced) return path;
		}
		throw fsError({ code: "ELOOP", message: "too many symbolic links" }, "stat", abs);
	}
	const parentOf = (p) => p.slice(0, Math.max(1, p.lastIndexOf("/")));
	const absoluteAgainst = (target, dir) => {
		if (target.startsWith("/")) return normalizePath(target);
		return normalizePath((dir === "/" ? "" : dir) + "/" + target);
	};
	function normalizePath(p) {
		const out = [];
		for (const part of p.split("/")) {
			if (part === "" || part === ".") continue;
			if (part === "..") { out.pop(); continue; }
			out.push(part);
		}
		return "/" + out.join("/");
	}

	// Node validates a path before doing anything with it, and reports the
	// failure with a CODE that callers (and its own suite) match on. Accepting
	// anything and stringifying it turned "assert.throws(…, ERR_INVALID_ARG_TYPE)"
	// into a silent success.
	function fsResolveNoFollow(p) {
		if (p instanceof URL) {
			if (p.protocol !== "file:") {
				throw Object.assign(new TypeError(`The URL must be of scheme file`), { code: "ERR_INVALID_URL_SCHEME" });
			}
			// A URL pathname is percent-ENCODED. Handing it to the filesystem raw
			// makes a directory literally named "copy_%251" out of "copy_%1", so
			// every pathToFileURL round-trip missed its own target.
			return decodeURIComponent(p.pathname);
		}
		if (typeof p !== "string" && !(p instanceof Uint8Array)) {
			throw Object.assign(
				new TypeError(`The "path" argument must be of type string or an instance of Buffer or URL. Received ${p === null ? "null" : typeof p}`),
				{ code: "ERR_INVALID_ARG_TYPE" });
		}
		const s = typeof p === "string" ? p : new TextDecoder().decode(p);
		// A NUL byte cannot appear in a path; Node rejects it explicitly rather
		// than letting it truncate somewhere below.
		if (s.includes("\u0000")) {
			throw Object.assign(
				new TypeError(`The argument 'path' must be a string, Uint8Array, or URL without null bytes. Received '${s}'`),
				{ code: "ERR_INVALID_ARG_VALUE" });
		}
		if (s.startsWith("/")) return s;
		const cwd = globalThis.process ? globalThis.process.cwd() : "/";
		return (cwd === "/" ? "/" : cwd + "/") + s;
	}
	// Every path-taking API resolves THROUGH links; lstat, readlink and symlink
	// itself use the raw form, because they are about the link and not what it
	// points at.
	function fsResolve(p) { return followLinks(fsResolveNoFollow(p)); }

	// ------------------------------------------------------ argument checks
	// Node rejects a bad argument before it touches anything, and its own suite
	// asserts on the { code, name } pair the rejection carries. Two details make
	// or break those assertions. The callback flavours validate SYNCHRONOUSLY —
	// fs.readFile(p, "bogus-encoding", cb) throws at the call, it does not report
	// through cb — so a check cannot live inside the deferred operation. And a
	// malformed fd or offset is an ERR_OUT_OF_RANGE RangeError, while a wrong
	// TYPE is an ERR_INVALID_ARG_TYPE TypeError; the two are not interchangeable.
	function receivedOf(v) {
		if (v === null || v === undefined) return ` Received ${v}`;
		if (typeof v === "function") return ` Received function ${v.name}`;
		if (typeof v === "object") {
			const n = v.constructor && v.constructor.name;
			return n ? ` Received an instance of ${n}` : ` Received ${String(v)}`;
		}
		let s = typeof v === "string" ? `'${v}'` : String(v);
		if (s.length > 28) s = s.slice(0, 25) + "...";
		return ` Received type ${typeof v} (${s})`;
	}
	const errArgType = (name, expected, v) => Object.assign(
		new TypeError(`The "${name}" argument must be ${expected}.${receivedOf(v)}`),
		{ code: "ERR_INVALID_ARG_TYPE" });
	const errArgValue = (name, v, reason) => Object.assign(
		new TypeError(`The argument '${name}' ${reason}. Received ${typeof v === "string" ? `'${v}'` : String(v)}`),
		{ code: "ERR_INVALID_ARG_VALUE" });
	const errRange = (name, range, v) => Object.assign(
		new RangeError(`The value of "${name}" is out of range. It must be ${range}. Received ${v}`),
		{ code: "ERR_OUT_OF_RANGE" });

	function validateFunction(v, name) {
		if (typeof v !== "function") throw errArgType(name, "of type function", v);
	}
	function validateInteger(v, name, min, max) {
		if (typeof v !== "number") throw errArgType(name, "of type number", v);
		if (!Number.isInteger(v)) throw errRange(name, "an integer", v);
		if (v < min || v > max) throw errRange(name, `>= ${min} && <= ${max}`, v);
	}
	const validateFd = (fd) => validateInteger(fd, "fd", 0, 0x7fffffff);
	function validateBuffer(v, name) {
		if (!ArrayBuffer.isView(v)) throw errArgType(name, "an instance of Buffer, TypedArray, or DataView", v);
	}
	// Buffer's encoding names. An unknown one is a VALUE error, not a type error,
	// and undefined means "the default" rather than "invalid".
	const KNOWN_ENCODINGS = new Set([
		"utf8", "utf-8", "ascii", "latin1", "binary", "base64", "base64url",
		"hex", "ucs2", "ucs-2", "utf16le", "utf-16le", "buffer",
	]);
	function validateEncoding(enc) {
		if (enc === undefined || enc === null) return;
		if (typeof enc !== "string" || !KNOWN_ENCODINGS.has(enc.toLowerCase())) {
			throw errArgValue("encoding", enc, "is invalid encoding");
		}
	}
	// The trailing options argument of most path APIs: a string encoding, an
	// object bag, or absent. A function there is the callback, checked elsewhere.
	function validateEncodingOpt(opts) {
		if (opts === undefined || opts === null || typeof opts === "function") return;
		if (typeof opts === "string") return validateEncoding(opts);
		if (typeof opts !== "object") throw errArgType("options", "of type object", opts);
		validateEncoding(opts.encoding);
	}
	// fsResolveNoFollow already raises the right error for a bad path, so calling
	// it for the check alone keeps one definition of "what is a path".
	const validatePath = (p) => { fsResolveNoFollow(p); };

	// Per-API checks, shared by the sync and the callback flavour so both reject
	// the same inputs at the same moment. Keyed by the callback-flavour name.
	const fsCheck = {
		readFile: (p, o) => { validatePath(p); validateEncodingOpt(o); },
		writeFile: (p, d, o) => { validatePath(p); validateEncodingOpt(o); },
		appendFile: (p, d, o) => { validatePath(p); validateEncodingOpt(o); },
		readdir: (p, o) => { validatePath(p); validateEncodingOpt(o); },
		readlink: (p, o) => { validatePath(p); validateEncodingOpt(o); },
		realpath: (p, o) => { validatePath(p); validateEncodingOpt(o); },
		mkdtemp: (p, o) => { validatePath(p); validateEncodingOpt(o); },
		stat: validatePath, lstat: validatePath, unlink: validatePath,
		rmdir: validatePath, rm: validatePath, mkdir: validatePath,
		access: validatePath, truncate: validatePath, open: validatePath,
		rename: (a, b) => { validatePath(a); validatePath(b); },
		link: (a, b) => { validatePath(a); validatePath(b); },
		copyFile: (a, b) => { validatePath(a); validatePath(b); },
		cp: (a, b, o) => { validatePath(a); validatePath(b); validateCpOptions(o); },
		symlink: (t, p) => { validatePath(t); validatePath(p); },
		chmod: (p, m) => { validatePath(p); validateMode(m); },
		lchmod: (p, m) => { validatePath(p); validateMode(m); },
		chown: (p, u, g) => { validatePath(p); validateUidGid(u, g); },
		lchown: (p, u, g) => { validatePath(p); validateUidGid(u, g); },
		close: validateFd,
		fstat: validateFd,
		ftruncate: (fd) => validateFd(fd),
	};
	// An octal string ("755") is as good as a number; anything else is not a mode.
	function validateMode(m) {
		if (m === undefined || m === null) return;
		if (typeof m === "string") {
			if (!/^[0-7]+$/.test(m)) throw errArgValue("mode", m, "must be a 32-bit unsigned integer or an octal string");
			return;
		}
		validateInteger(m, "mode", 0, 0o777);
	}
	// -1 is Node's "leave it alone" sentinel, so the range starts below zero.
	function validateUidGid(uid, gid) {
		validateInteger(uid, "uid", -1, 0xffffffff);
		validateInteger(gid, "gid", -1, 0xffffffff);
	}
	// The later-evaluated modules (streams.js, extras.js) are separate closures
	// and need the same checks, so they travel on the shared registry.
	core.__validate = {
		errArgType, errArgValue, errRange,
		validateFunction, validateInteger, validateFd, validateBuffer,
		validateEncoding, validateEncodingOpt, validatePath,
	};


	// Open flags come as a string ("w+", "ax") or as the O_* bit set from
	// fs.constants. The host op speaks the string form, so a numeric flag has to
	// be translated rather than stringified — String(66) is "66", which is not a
	// mode at all and reached the host as one.
	function flagsToString(flags) {
		if (typeof flags === "string") return flags;
		if (typeof flags !== "number") throw errArgType("flags", "of type string or number", flags);
		const { O_WRONLY = 1, O_RDWR = 2, O_CREAT = 0o100, O_EXCL = 0o200, O_TRUNC = 0o1000, O_APPEND = 0o2000 } = OPEN_FLAGS;
		const rw = (flags & O_RDWR) === O_RDWR;
		let base;
		if (flags & O_APPEND) base = rw ? "a+" : "a";
		else if (flags & O_TRUNC || flags & O_CREAT) base = rw ? "w+" : (flags & O_WRONLY ? "w" : "r+");
		else base = rw ? "r+" : "r";
		if (!(flags & O_CREAT) && !(flags & O_TRUNC) && !(flags & O_APPEND)) base = rw ? "r+" : (flags & O_WRONLY ? "w" : "r");
		return (flags & O_EXCL) ? base + "x" : base;
	}
	const OPEN_FLAGS = {
		O_RDONLY: 0, O_WRONLY: 1, O_RDWR: 2, O_CREAT: 0o100, O_EXCL: 0o200,
		O_TRUNC: 0o1000, O_APPEND: 0o2000,
	};

	// flagOf extracts the string `flag` option ("w", "a", "wx", ...) from a
	// writeFile/appendFile options bag; string opts are encodings, not flags.
	const flagOf = (opts, dflt) =>
		(opts && typeof opts === "object" && typeof opts.flag === "string" ? opts.flag : dflt);
	// Node's stringToFlags: 'x' anywhere means O_EXCL (wx/xw/ax/xa/wx+/...),
	// 'a' anywhere (in a valid flag) means O_APPEND.
	function checkExclusive(p, flag) {
		if (flag.includes("x") && ops.fs_exists(fsResolve(p))) {
			throw fsError({ code: "EEXIST", message: "file already exists" }, "open", p);
		}
	}

	// ---------------------------------------------------------------- fs.cp
	// cp() is the one fs API with a whole error family of its own: every way a
	// copy can be ill-formed gets its own ERR_FS_CP_* code, because "EINVAL"
	// alone tells a caller nothing about which of the two paths was wrong.
	// Ignoring the options bag entirely — as this did — made force:false
	// overwrite, errorOnExist silent, and filter dead.
	const cpError = (kind, message) => Object.assign(
		new Error(message), { code: `ERR_FS_CP_${kind}`, name: "SystemError" });
	function validateCpOptions(options) {
		if (options === undefined || options === null) return {};
		if (typeof options !== "object") throw errArgType("options", "of type object", options);
		for (const k of ["dereference", "errorOnExist", "force", "preserveTimestamps", "recursive", "verbatimSymlinks"]) {
			if (options[k] !== undefined && typeof options[k] !== "boolean") {
				throw errArgType(`options.${k}`, "of type boolean", options[k]);
			}
		}
		if (options.filter !== undefined) validateFunction(options.filter, "options.filter");
		// `mode` is copyFile's flag set, not a permission mask.
		if (options.mode !== undefined) validateInteger(options.mode, "mode", 0, 7);
		if (options.dereference && options.verbatimSymlinks) {
			throw Object.assign(new TypeError("Option pair dereference and verbatimSymlinks is incompatible"),
				{ code: "ERR_INCOMPATIBLE_OPTION_PAIR" });
		}
		return { force: true, ...options };
	}
	// The filter may be async, but only for the callback and promise forms —
	// cpSync has nowhere to await, so a promise there is a return-value error
	// rather than something to coerce to "truthy, therefore copy everything".
	function syncFilter(opts, from, to) {
		if (!opts.filter) return true;
		const r = opts.filter(from, to);
		if (r && typeof r.then === "function") {
			throw Object.assign(new TypeError('Expected a boolean from the "filter" function but got an instance of Promise.'),
				{ code: "ERR_INVALID_RETURN_VALUE" });
		}
		return r;
	}

	// One source entry to one destination entry, recursing for directories.
	function copyEntry(from, to, opts) {
		if (!syncFilter(opts, from, to)) return;
		const st = fsSync.lstatSync(from);
		let destSt = null;
		try { destSt = fsSync.lstatSync(to); } catch { /* absent is the normal case */ }
		if (st.isDirectory()) {
			if (!opts.recursive) {
				throw Object.assign(new Error(`Recursive option not enabled, current size of ${from} exceeds`),
					{ code: "ERR_FS_EISDIR", name: "SystemError" });
			}
			if (destSt && !destSt.isDirectory()) {
				throw cpError("DIR_TO_NON_DIR", `cannot overwrite non-directory ${to} with directory ${from}`);
			}
			fsSync.mkdirSync(to, { recursive: true });
			for (const name of fsSync.readdirSync(from)) {
				copyEntry(path.join(from, name), path.join(to, name), opts);
			}
			return;
		}
		if (destSt && destSt.isDirectory()) {
			throw cpError("NON_DIR_TO_DIR", `cannot overwrite directory ${to} with non-directory ${from}`);
		}
		if (destSt) {
			// force defaults to true (overwrite). With it off the entry is either
			// an error or silently left alone — never overwritten.
			if (opts.errorOnExist && !opts.force) throw cpError("EEXIST", `${to} already exists`);
			if (!opts.force) return;
		}
		if (st.isSymbolicLink() && !opts.dereference) {
			const target = fsSync.readlinkSync(from);
			// A link that points into the source tree would, once copied, point
			// back out of the destination — Node refuses rather than produce it.
			if (!opts.verbatimSymlinks && path.resolve(path.dirname(from), target).startsWith(to)) {
				throw cpError("SYMLINK_TO_SUBDIRECTORY", `cannot overwrite ${to} with ${from}`);
			}
			try { fsSync.unlinkSync(to); } catch { /* nothing to replace */ }
			fsSync.symlinkSync(target, to);
			return;
		}
		fsSync.copyFileSync(from, to);
	}

	// The async walk. It differs from the sync one in exactly one place — it
	// AWAITS the filter — which is the whole reason cp() cannot just be cpSync()
	// on a microtask.
	async function copyEntryAsync(from, to, opts) {
		if (opts.filter && !(await opts.filter(from, to))) return;
		const st = fsSync.lstatSync(from);
		let destSt = null;
		try { destSt = fsSync.lstatSync(to); } catch { /* absent is the normal case */ }
		if (st.isDirectory()) {
			if (!opts.recursive) {
				throw Object.assign(new Error(`Recursive option not enabled, current size of ${from} exceeds`),
					{ code: "ERR_FS_EISDIR", name: "SystemError" });
			}
			if (destSt && !destSt.isDirectory()) {
				throw cpError("DIR_TO_NON_DIR", `cannot overwrite non-directory ${to} with directory ${from}`);
			}
			fsSync.mkdirSync(to, { recursive: true });
			for (const name of fsSync.readdirSync(from)) {
				await copyEntryAsync(path.join(from, name), path.join(to, name), opts);
			}
			return;
		}
		if (destSt && destSt.isDirectory()) {
			throw cpError("NON_DIR_TO_DIR", `cannot overwrite directory ${to} with non-directory ${from}`);
		}
		if (destSt) {
			if (opts.errorOnExist && !opts.force) throw cpError("EEXIST", `${to} already exists`);
			if (!opts.force) return;
		}
		if (st.isSymbolicLink() && !opts.dereference) {
			const target = fsSync.readlinkSync(from);
			if (!opts.verbatimSymlinks && path.resolve(path.dirname(from), target).startsWith(to)) {
				throw cpError("SYMLINK_TO_SUBDIRECTORY", `cannot overwrite ${to} with ${from}`);
			}
			try { fsSync.unlinkSync(to); } catch { /* nothing to replace */ }
			fsSync.symlinkSync(target, to);
			return;
		}
		fsSync.copyFileSync(from, to);
	}

	// The shared front half of every cp form: validate, resolve, and refuse the
	// two copies that would never terminate.
	function cpPrepare(src, dest, options) {
		const opts = validateCpOptions(options);
		const from = fsResolve(src), to = fsResolve(dest);
		if (from === to) throw cpError("EINVAL", `src and dest cannot be the same ${from}`);
		if (to.startsWith(from.endsWith("/") ? from : from + "/")) {
			throw cpError("EINVAL", `cannot copy ${from} to a subdirectory of self ${to}`);
		}
		return { from, to, opts };
	}

	// fs.watch() hands back an FSWatcher, and callers treat it as a handle, not
	// just an emitter: they close it, they unref() it so a watch alone does not
	// hold the process open, and they wait on it with once(). A bare
	// EventEmitter with a close() method answered none of that.
	class FSWatcher extends EventEmitter {
		constructor() { super(); this._id = null; this._closed = false; }
		close() {
			if (this._closed) return;
			this._closed = true;
			if (this._id !== null) ops.fs_unwatch(this._id);
			this.emit("close");
		}
		// The host op holds a loop pending; releasing it is exactly what unref
		// means here, and re-taking it is ref.
		unref() { if (!this._closed && this._id !== null && !this._unrefed) { this._unrefed = true; ops.fs_watch_unref(this._id, true); } return this; }
		ref() { if (this._unrefed) { this._unrefed = false; ops.fs_watch_unref(this._id, false); } return this; }
	}

	// Active watchFile() host watchers keyed by path, so unwatchFile() can stop
	// them — each fs_watch holds a host loop-pending, and never calling
	// fs_unwatch would keep the event loop alive forever.
	const watchFileWatchers = new Map();

	const fsSync = {
		readFileSync(p, opts) {
			validateEncodingOpt(opts);
			const r = ops.fs_read_file(fsResolve(p));
			ops.release_pending();
			if (isErr(r)) throw fsError(r, "open", p);
			const buf = wrapBuf(r);
			const enc = encodingOf(opts);
			return enc ? buf.toString(enc) : buf;
		},
		writeFileSync(p, data, opts) {
			validateEncodingOpt(opts);
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
			const r = ops.fs_write_file(fsResolve(p), payload, append);
			if (isErr(r)) throw fsError(r, "open", p);
		},
		appendFileSync(p, data, opts) {
			validateEncodingOpt(opts);
			const payload = typeof data === "string" ? Buffer.from(data, encodingOf(opts) || "utf8") : Buffer.from(data);
			checkExclusive(p, flagOf(opts, "a"));
			const r = ops.fs_write_file(fsResolve(p), payload, true);
			if (isErr(r)) throw fsError(r, "open", p);
		},
		existsSync: (p) => ops.fs_exists(fsResolve(p)),
		statSync(p, opts) {
			const r = ops.fs_stat(fsResolve(p));
			if (isErr(r)) throw fsError(r, "stat", p);
			return statsOf(r, !!(opts && opts.bigint));
		},
		lstatSync(p, opts) {
			const abs = fsResolveNoFollow(p);
			if (symlinks.has(abs)) {
				// A link's own stat: it IS a link, and its size is the target's
				// length, as on a real filesystem.
				const target = symlinks.get(abs);
				const st = statsOf({ size: target.length, dir: false, mtimeMs: Date.now() }, !!(opts && opts.bigint));
				st.isSymbolicLink = () => true;
				st.isFile = () => false;
				st.mode = 0o120777;
				return st;
			}
			return fsSync.statSync(p, opts);
		},
		readdirSync(p, options) {
			validateEncodingOpt(options);
			const base = fsResolve(p);
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
			if (!ops.fs_exists(fsResolve(p))) {
				throw fsError({ code: "ENOENT", message: "no such file or directory" }, "access", p);
			}
		},
		mkdirSync(p, opts) {
			// `recursive` decides whether a missing parent is an error, so a
			// truthy string quietly changing that behaviour is exactly the kind
			// of mistake worth naming.
			if (opts && opts.recursive !== undefined && typeof opts.recursive !== "boolean") {
				throw errArgType("options.recursive", "of type boolean", opts.recursive);
			}
			if (opts && opts.mode !== undefined) validateMode(opts.mode);
			const r = ops.fs_mkdir(fsResolve(p), !!(opts && opts.recursive));
			if (isErr(r)) throw fsError(r, "mkdir", p);
		},
		rmdirSync(p) {
			// rmdir on a non-directory is ENOTDIR, not a silent removal — the
			// caller asked to remove a DIRECTORY.
			const r = ops.fs_remove(fsResolve(p));
			if (isErr(r)) throw fsError(r, "rmdir", p);
		},
		unlinkSync(p) {
			const r = ops.fs_remove(fsResolve(p));
			if (isErr(r)) throw fsError(r, "unlink", p);
		},
		renameSync(oldP, newP) {
			const r = ops.fs_rename(fsResolve(oldP), fsResolve(newP));
			if (isErr(r)) throw fsError(r, "rename", oldP);
		},
		realpathSync(p, options) {
			// No symlinks exist in this FS (memfs/host mounts have none), so the
			// canonical path is just the resolved one — but Node's realpath still
			// requires the target to EXIST (ENOENT otherwise; syscall "lstat").
			validateEncodingOpt(options);
			// Canonical means normalized too: "/dir/../dir/f" is "/dir/f".
			const resolved = normalizePath(fsResolve(p));
			if (!ops.fs_exists(resolved)) {
				throw fsError({ code: "ENOENT", message: "no such file or directory" }, "lstat", String(p));
			}
			return resolved;
		},
		watch(p, options, listener) {
			if (typeof options === "function") { listener = options; options = {}; }
			validatePath(p);
			validateEncodingOpt(options);
			if (listener !== undefined && listener !== null) validateFunction(listener, "listener");
			const watcher = new FSWatcher();
			const id = ops.fs_watch(fsResolve(p), (eventType, filename) => {
				watcher.emit("change", eventType, filename);
			});
			watcher._id = id;
			if (listener) watcher.on("change", listener);
			return watcher;
		},
		watchFile(p, options, listener) {
			if (typeof options === "function") { listener = options; options = {}; }
			validatePath(p);
			validateFunction(listener, "listener");
			let prev = null;
			try { prev = fsSync.statSync(p); } catch {}
			const id = ops.fs_watch(fsResolve(p), () => {
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
			const r = ops.fs_copyfile(fsResolve(src), fsResolve(dest));
			if (isErr(r)) throw fsError(r, "copyfile", src);
		},
		rmSync(p, options = {}) {
			const r = ops.fs_rm(fsResolve(p), !!options.recursive, !!options.force);
			if (isErr(r)) throw fsError(r, "rm", p);
		},
		rmdirSync(p, options = {}) {
			// rmdir on a non-directory is ENOTDIR: the caller asked to remove a
			// DIRECTORY, and removing a file instead is not a helpful reading.
			const abs = fsResolve(p);
			let st = null;
			try { st = fsSync.statSync(p); } catch { /* let the op report ENOENT */ }
			if (st && !st.isDirectory()) throw fsError({ code: "ENOTDIR", message: "not a directory" }, "rmdir", p);
			const r = ops.fs_rm(abs, !!(options && options.recursive), false);
			if (isErr(r)) throw fsError(r, "rmdir", p);
		},
		mkdtempSync(prefix, options) {
			validateEncodingOpt(options);
			const r = ops.fs_mkdtemp(fsResolve(prefix));
			if (isErr(r)) throw fsError(r, "mkdtemp", prefix);
			return r;
		},
		cpSync(src, dest, options) {
			const { from, to, opts } = cpPrepare(src, dest, options);
			copyEntry(from, to, opts);
		},
		openSync(p, flags = "r") {
			const f = flagsToString(flags);
			// Exclusive ('x') fails on an existing file; the host op then gets the
			// base flag ('x' and the sync 's' modifier stripped: "wx" -> "w").
			checkExclusive(p, f);
			const r = ops.fs_open(fsResolve(p), f.replace(/[xs]/g, "") || "r");
			if (isErr(r)) throw fsError(r, "open", p);
			return r;
		},
		closeSync(fd) {
			validateFd(fd);
			const r = ops.fs_close_fd(fd);
			if (isErr(r)) throw fsError(r, "close", fd);
		},
		readSync(fd, buffer, offset = 0, length = buffer.length, position = null) {
			validateFd(fd);
			validateBuffer(buffer, "buffer");
			validateInteger(offset, "offset", 0, buffer.byteLength);
			if (position !== null && position !== undefined && typeof position !== "bigint") {
				validateInteger(position, "position", 0, Number.MAX_SAFE_INTEGER);
			}
			const r = ops.fs_read_fd(fd, length, position);
			ops.release_pending();
			if (isErr(r)) throw fsError(r, "read", fd);
			const data = new Uint8Array(r.data);
			buffer.set(data.subarray(0, Math.min(data.length, length)), offset);
			return r.bytesRead;
		},
		writeSync(fd, buffer, offset, length, position) {
			validateFd(fd);
			if (typeof buffer !== "string") validateBuffer(buffer, "buffer");
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
		// The fd-addressed twins of the path operations. There is nothing behind
		// them here — the host filesystem has no owners, and every write is
		// already durable — but they have to EXIST and validate their fd, since
		// that is the whole of their observable contract.
		fsyncSync(fd) { validateFd(fd); },
		fdatasyncSync(fd) { validateFd(fd); },
		fchmodSync(fd, mode) { validateFd(fd); validateMode(mode); },
		fchownSync(fd, uid, gid) { validateFd(fd); validateUidGid(uid, gid); },
		futimesSync(fd, atime, mtime) { validateFd(fd); },
		fstatSync(fd, opts) {
			validateFd(fd);
			const r = ops.fs_fstat(fd);
			if (isErr(r)) throw fsError(r, "fstat", fd);
			return statsOf(r, !!(opts && opts.bigint));
		},
		ftruncateSync(fd, len = 0) {
			validateFd(fd);
			const r = ops.fs_ftruncate(fd, Number(len) || 0);
			if (isErr(r)) throw fsError(r, "ftruncate", fd);
		},
		truncateSync(p, len = 0) {
			const fd = fsSync.openSync(p, "r+");
			try { fsSync.ftruncateSync(fd, len); } finally { fsSync.closeSync(fd); }
		},
		chmodSync(p, mode) {
			validateMode(mode);
			// Node accepts an octal string ("755") or a number.
			const m = typeof mode === "string" ? parseInt(mode, 8) : Number(mode);
			const r = ops.fs_chmod(fsResolve(p), m & 0o7777);
			if (isErr(r)) throw fsError(r, "chmod", p);
		},
		lchmodSync(p, mode) { return fsSync.chmodSync(p, mode); }, // no symlinks
		// There are no owners in this filesystem, so a chown that VALIDATES is
		// the whole of it — but it has to validate, because that is the only
		// observable half.
		chownSync(p, uid, gid) { validatePath(p); validateUidGid(uid, gid); },
		lchownSync(p, uid, gid) { validatePath(p); validateUidGid(uid, gid); },
		utimesSync(p, atime, mtime) {
			validatePath(p);
			// Node: numbers are epoch SECONDS; Dates and numeric strings work too.
			// Anything that is not one of those three is a type error rather than
			// a NaN timestamp written to the file.
			const toMs = (t) => {
				if (t instanceof Date) return t.getTime();
				if (typeof t === "number" || typeof t === "bigint") return Number(t) * 1000;
				if (typeof t === "string" && t.trim() !== "" && Number.isFinite(Number(t))) return Number(t) * 1000;
				throw errArgType("time", "a number, a string convertible to a number, or a Date", t);
			};
			const r = ops.fs_utimes(fsResolve(p), toMs(atime), toMs(mtime));
			if (isErr(r)) throw fsError(r, "utime", p);
		},
		lutimesSync(p, atime, mtime) { return fsSync.utimesSync(p, atime, mtime); },
		symlinkSync(target, linkPath) {
			const abs = fsResolveNoFollow(linkPath);
			if (symlinks.has(abs)) throw fsError({ code: "EEXIST", message: "file already exists" }, "symlink", linkPath);
			symlinks.set(abs, typeof target === "string" ? target : String(target));
		},
		readlinkSync(p, options) {
			validateEncodingOpt(options);
			const abs = fsResolveNoFollow(p);
			if (!symlinks.has(abs)) throw fsError({ code: "EINVAL", message: "invalid argument" }, "readlink", p);
			return symlinks.get(abs);
		},
		linkSync(existing, newPath) {
			// A hard link is indistinguishable from a copy for a store with no
			// inodes, so it is one — with the same visibility caveat as above.
			const data = fsSync.readFileSync(existing);
			fsSync.writeFileSync(newPath, data);
		},
		unlinkLink(p) { return symlinks.delete(fsResolveNoFollow(p)); },
	};

	// Callback flavors validate synchronously, then run the sync op and deliver
	// on the microtask queue. The split matters: Node THROWS on a bad argument
	// and REPORTS a failed operation through the callback, and its suite asserts
	// on both halves.
	function callbackify1(syncFn, check) {
		return (...args) => {
			const cb = args.pop();
			validateFunction(cb, "cb");
			if (check) check(...args);
			queueMicrotask(() => {
				try { cb(null, syncFn(...args)); } catch (e) { cb(e); }
			});
		};
	}
	const fsMod = {
		...fsSync,
		readFile: callbackify1(fsSync.readFileSync, fsCheck.readFile),
		writeFile: callbackify1(fsSync.writeFileSync, fsCheck.writeFile),
		appendFile: callbackify1(fsSync.appendFileSync, fsCheck.appendFile),
		stat: callbackify1(fsSync.statSync, fsCheck.stat),
		lstat: callbackify1(fsSync.statSync, fsCheck.lstat),
		readdir: callbackify1(fsSync.readdirSync, fsCheck.readdir),
		mkdir: callbackify1(fsSync.mkdirSync, fsCheck.mkdir),
		rmdir: callbackify1(fsSync.rmdirSync, fsCheck.rmdir),
		rm: callbackify1(fsSync.rmSync, fsCheck.rm),
		unlink: callbackify1(fsSync.unlinkSync, fsCheck.unlink),
		rename: callbackify1(fsSync.renameSync, fsCheck.rename),
		realpath: callbackify1(fsSync.realpathSync, fsCheck.realpath),
		symlink: callbackify1(fsSync.symlinkSync, fsCheck.symlink),
		readlink: callbackify1(fsSync.readlinkSync, fsCheck.readlink),
		link: callbackify1(fsSync.linkSync, fsCheck.link),
		copyFile: callbackify1(fsSync.copyFileSync, fsCheck.copyFile),
		mkdtemp: callbackify1(fsSync.mkdtempSync, fsCheck.mkdtemp),
		// cp() is not cpSync() on a microtask: its filter may be async, and the
		// walk has to wait for each answer before deciding what to copy.
		cp: (src, dest, options, cb) => {
			if (typeof options === "function") { cb = options; options = undefined; }
			validateFunction(cb, "cb");
			validatePath(src);
			validatePath(dest);
			// A bad OPTION is an argument error and throws here; a malformed pair
			// of PATHS is an operation failure and reaches the callback.
			validateCpOptions(options);
			let prepared;
			try { prepared = cpPrepare(src, dest, options); }
			catch (e) { queueMicrotask(() => cb(e)); return; }
			copyEntryAsync(prepared.from, prepared.to, prepared.opts).then(() => cb(null), (e) => cb(e));
		},
		chmod: callbackify1(fsSync.chmodSync, fsCheck.chmod),
		lchmod: callbackify1(fsSync.chmodSync, fsCheck.lchmod),
		utimes: callbackify1(fsSync.utimesSync, fsCheck.stat),
		lutimes: callbackify1(fsSync.utimesSync, fsCheck.stat),
		truncate: callbackify1(fsSync.truncateSync, fsCheck.truncate),
		ftruncate: callbackify1(fsSync.ftruncateSync, fsCheck.ftruncate),
		chown: callbackify1(fsSync.chownSync, fsCheck.chown),
		lchown: callbackify1(fsSync.lchownSync, fsCheck.lchown),
		fsync: callbackify1(fsSync.fsyncSync, fsCheck.close),
		fdatasync: callbackify1(fsSync.fdatasyncSync, fsCheck.close),
		fchmod: callbackify1(fsSync.fchmodSync, (fd, m) => { validateFd(fd); validateMode(m); }),
		fchown: callbackify1(fsSync.fchownSync, (fd, u, g) => { validateFd(fd); validateUidGid(u, g); }),
		futimes: callbackify1(fsSync.futimesSync, fsCheck.close),
		exists: (p, cb) => {
			// exists() predates Node's error convention: its callback takes the
			// boolean alone. The callback is still required, and a bad path
			// answers false rather than throwing.
			validateFunction(cb, "cb");
			queueMicrotask(() => {
				let ok = false;
				try { ok = fsSync.existsSync(p); } catch { ok = false; }
				cb(ok);
			});
		},
		access: (p, mode, cb) => {
			const done = typeof mode === "function" ? mode : cb;
			validateFunction(done, "cb");
			validatePath(p);
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
			validateFunction(cb, "cb");
			validatePath(p);
			queueMicrotask(() => { try { cb(null, fsSync.openSync(p, flags)); } catch (e) { cb(e); } });
		},
		close: (fd, cb) => {
			validateFd(fd);
			if (cb !== undefined) validateFunction(cb, "cb");
			queueMicrotask(() => { try { fsSync.closeSync(fd); if (cb) cb(null); } catch (e) { if (cb) cb(e); } });
		},
		read: (fd, buffer, offset, length, position, cb) => {
			validateFd(fd);
			validateBuffer(buffer, "buffer");
			if (offset !== undefined && offset !== null) validateInteger(offset, "offset", 0, buffer.byteLength);
			if (length !== undefined && length !== null) validateInteger(length, "length", 0, buffer.byteLength - (offset || 0));
			validateFunction(cb, "cb");
			queueMicrotask(() => { try { const n = fsSync.readSync(fd, buffer, offset, length, position); cb(null, n, buffer); } catch (e) { cb(e); } });
		},
		write: (fd, buffer, offset, length, position, cb) => {
			if (typeof offset === "function") { cb = offset; offset = length = position = undefined; }
			else if (typeof length === "function") { cb = length; length = position = undefined; }
			else if (typeof position === "function") { cb = position; position = undefined; }
			validateFd(fd);
			if (typeof buffer !== "string") validateBuffer(buffer, "buffer");
			validateFunction(cb, "cb");
			queueMicrotask(() => { try { const n = fsSync.writeSync(fd, buffer, offset, length, position); cb(null, n, buffer); } catch (e) { cb(e); } });
		},
		fstat: (fd, opts, cb) => {
			if (typeof opts === "function") cb = opts;
			validateFd(fd);
			validateFunction(cb, "cb");
			queueMicrotask(() => { try { cb(null, fsSync.fstatSync(fd)); } catch (e) { cb(e); } });
		},
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
	// Utf8Stream (node:fs) is a write-only stream over a file descriptor built
	// for logging: append-only, UTF-8, with a synchronous mode. It is a small
	// surface over the fd primitives already here, and 16 suite files use it.
	class Utf8Stream extends core.events {
		constructor(options = {}) {
			super();
			const opts = typeof options === "string" ? { dest: options } : (options || {});
			this.sync = !!opts.sync;
			this.append = opts.append !== false;
			this.mode = opts.mode;
			this.destroyed = false;
			this.writing = false;
			this.minLength = opts.minLength || 0;
			this.maxLength = opts.maxLength || 0;
			this.maxWrite = opts.maxWrite || 16 * 1024;
			this._buf = "";
			this.file = opts.dest ?? opts.file ?? null;
			if (opts.fd !== undefined && opts.fd !== null) {
				this.fd = opts.fd;
			} else if (this.file) {
				const flags = this.append ? "a" : "w";
				this.fd = fsMod.openSync(this.file, flags, this.mode);
			} else {
				throw Object.assign(new TypeError("Utf8Stream needs a dest or an fd"), { code: "ERR_INVALID_ARG_TYPE" });
			}
			// Opening for append creates the file; make it exist now rather than
			// at the first write, which is what a reader checking for the log
			// expects.
			try { fsMod.writeSync(this.fd, ""); } catch { /* the fd may be a pipe */ }
			// Node emits 'ready' once the destination is usable.
			process.nextTick(() => { if (!this.destroyed) this.emit("ready"); });
		}
		write(data) {
			if (this.destroyed) {
				throw Object.assign(new Error("the stream has been destroyed"), { code: "ERR_STREAM_DESTROYED" });
			}
			if (typeof data !== "string") {
				throw Object.assign(new TypeError("expected a string"), { code: "ERR_INVALID_ARG_TYPE" });
			}
			this._buf += data;
			if (this.maxLength && this._buf.length > this.maxLength) {
				this._buf = "";
				this.emit("drop", data);
				return true;
			}
			// minLength decides, not `sync`: a stream told to buffer until 4 KiB
			// buffers in sync mode too. `sync` says HOW the flush happens, not
			// when.
			if (this._buf.length >= this.minLength) this.flushSync();
			return true;
		}
		flushSync() {
			if (this.destroyed || this._buf === "") return;
			const chunk = this._buf;
			this._buf = "";
			fsMod.writeSync(this.fd, chunk);
		}
		flush(cb) {
			try { this.flushSync(); } catch (e) { if (cb) return cb(e); throw e; }
			if (cb) process.nextTick(() => cb(null));
		}
		reopen(file) {
			if (this.destroyed) return;
			this.flushSync();
			try { fsMod.closeSync(this.fd); } catch { /* already gone */ }
			this.file = file ?? this.file;
			this.fd = fsMod.openSync(this.file, this.append ? "a" : "w", this.mode);
			this.emit("ready");
		}
		end() {
			// A stream that has already been destroyed cannot be ended again.
			if (this.destroyed) return;
			try { this.flushSync(); } catch { /* report through 'error' below */ }
			this.destroy();
			this.emit("finish");
		}
		destroy() {
			if (this.destroyed) return;
			try { this.flushSync(); } catch { /* the fd may already be gone */ }
			this.destroyed = true;
			try { fsMod.closeSync(this.fd); } catch { /* already closed */ }
			process.nextTick(() => this.emit("close"));
		}
	}
	fsMod.Utf8Stream = Utf8Stream;

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
		["mkdtemp", "mkdtempSync"],
		["chmod", "chmodSync"], ["lchmod", "lchmodSync"],
		["utimes", "utimesSync"], ["lutimes", "lutimesSync"],
		["truncate", "truncateSync"],
		["symlink", "symlinkSync"], ["readlink", "readlinkSync"], ["link", "linkSync"],
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
	// fs.promises.watch is an async ITERATOR of {eventType, filename}, not an
	// emitter: `for await (const e of watch(dir))`. Events that arrive while
	// nothing is awaiting queue up, so none is lost between turns of the loop.
	promisified.watch = (p, options) => {
		const watcher = fsMod.watch(p, options);
		const queued = [], waiting = [];
		let done = false;
		watcher.on("change", (eventType, filename) => {
			const ev = { eventType, filename };
			if (waiting.length) waiting.shift()({ value: ev, done: false });
			else queued.push(ev);
		});
		const stop = () => {
			done = true;
			watcher.close();
			while (waiting.length) waiting.shift()({ value: undefined, done: true });
			return Promise.resolve({ value: undefined, done: true });
		};
		if (options && options.signal) {
			if (options.signal.aborted) stop();
			else options.signal.addEventListener("abort", stop, { once: true });
		}
		return {
			[Symbol.asyncIterator]() { return this; },
			next() {
				if (queued.length) return Promise.resolve({ value: queued.shift(), done: false });
				if (done) return Promise.resolve({ value: undefined, done: true });
				return new Promise((resolve) => waiting.push(resolve));
			},
			return: stop,
		};
	};
	// The promise form shares the async walk, so it accepts an async filter too.
	promisified.cp = (src, dest, options) => {
		try {
			const { from, to, opts } = cpPrepare(src, dest, options);
			return copyEntryAsync(from, to, opts);
		} catch (e) { return Promise.reject(e); }
	};
	promisified.access = (p) => (ops.fs_exists(fsResolve(p)) ? Promise.resolve() : Promise.reject(fsError({ code: "ENOENT", message: "no such file or directory" }, "access", p)));
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
			{ file: String(file), args: (args || []).map(String), cwd: options.cwd,
				envArray: envToArray(options.env), ipc: !!options.ipc },
			onStdout, onStderr, onExit, onError);
		if (isErr(r)) { process.nextTick(() => cp.emit("error", cpErr(r, file))); return cp; }
		cp.pid = r.pid;
		if (options.ipc) attachIPC(cp, r.pid);
		return cp;
	}

	// attachIPC wires the parent end of a fork() channel onto a ChildProcess:
	// send/message/disconnect, and `connected` so a caller can tell whether the
	// channel is still there. Node serializes IPC messages as JSON, and so does
	// this — a value that will not survive JSON does not survive fork either.
	function attachIPC(cp, id) {
		cp.connected = true;
		cp.channel = { ref() {}, unref() {} };
		cp.send = function send(message, sendHandle, options, callback) {
			if (typeof sendHandle === "function") { callback = sendHandle; sendHandle = undefined; }
			else if (typeof options === "function") { callback = options; options = undefined; }
			if (!cp.connected) {
				const e = Object.assign(new Error("channel closed"), { code: "ERR_IPC_CHANNEL_CLOSED" });
				if (callback) { process.nextTick(() => callback(e)); return false; }
				cp.emit("error", e);
				return false;
			}
			const ok = ops.ipc_send(id, JSON.stringify(message === undefined ? null : message));
			if (callback) process.nextTick(() => callback(ok ? null : new Error("channel closed")));
			return ok;
		};
		cp.disconnect = function disconnect() {
			if (!cp.connected) return;
			cp.connected = false;
			ops.ipc_disconnect(id);
			process.nextTick(() => cp.emit("disconnect"));
		};
		ops.ipc_start(id,
			(json) => { try { cp.emit("message", JSON.parse(json)); } catch { /* not JSON: drop */ } },
			() => { if (cp.connected) { cp.connected = false; cp.emit("disconnect"); } });
	}

	// fork() is spawn() of THIS runtime with a message channel attached. The
	// "binary" is the runtime itself (see nested.go), so the module path is the
	// script argument.
	function fork(modulePath, args, options) {
		if (!Array.isArray(args)) { options = args; args = []; }
		options = options || {};
		const argv = [...(options.execArgv || []), String(modulePath), ...(args || []).map(String)];
		return spawn(options.execPath || process.execPath, argv, { ...options, ipc: true });
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
		fork,
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
	// Node returns undefined when a file has no source map, and callers are
	// required to handle that. Nothing here records source maps, so undefined is
	// the honest answer — and the honest answer is also the working one: Next.js
	// calls this while formatting an error, and its absence turned every such
	// format into a second error about findSourceMap not being a function.
	Module.findSourceMap = () => undefined;
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
