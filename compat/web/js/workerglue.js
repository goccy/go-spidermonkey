// compat/web: the environment inside a Worker.
//
// Evaluated in the worker agent's own realm, BEFORE the worker's own source.
// The agent is a real thread with its own SpiderMonkey realm and linear memory,
// sharing nothing with the parent but SharedArrayBuffer memory — the model the
// standard describes.
//
// It is pure JavaScript because it has to be: an agent's realm has no host
// functions at all, so nothing implemented in Go on the parent side is reachable
// from here (docs/engine-followups.md item 11 records why, and what the engine
// would need to provide). That bounds the surface honestly:
//
//   here:    self, WorkerGlobalScope, DedicatedWorkerGlobalScope, close,
//            postMessage/onmessage, location (parsed by the parent at spawn),
//            navigator, Event/EventTarget/CustomEvent/MessageEvent/ErrorEvent,
//            atob/btoa, TextEncoder/TextDecoder, structuredClone, timers,
//            console (forwarded to the parent), queueMicrotask, performance.now
//
//   NOT here: fetch, WebSocket, EventSource, crypto.subtle, URL, URLPattern,
//            streams, Blob/File, WebAssembly. Every one of those is Go on the
//            parent side. A worker that needs them should be given its data
//            through messages instead — which is what the parent is for.
//
// Anything absent is absent, not stubbed. A stub that answers wrongly is worse
// than a ReferenceError, which at least says what happened.
(() => {
	"use strict";
	const A = globalThis.__agent__;

	// ------------------------------------------------------------ events
	class Event {
		constructor(type, init = {}) {
			this.type = String(type);
			this.target = null;
			this.currentTarget = null;
			this.defaultPrevented = false;
			this.bubbles = !!init.bubbles;
			this.cancelable = !!init.cancelable;
			this._trusted = false;
			this._stopImmediate = false;
		}
		get isTrusted() { return this._trusted; }
		preventDefault() { if (this.cancelable) this.defaultPrevented = true; }
		stopPropagation() {}
		stopImmediatePropagation() { this._stopImmediate = true; }
	}
	class EventTarget {
		constructor() { Object.defineProperty(this, "_ls", { value: new Map() }); }
		addEventListener(type, cb, options = {}) {
			if (cb === null || cb === undefined) return;
			type = String(type);
			let list = this._ls.get(type);
			if (!list) this._ls.set(type, list = []);
			if (list.some((l) => l.cb === cb)) return;
			list.push({ cb, once: !!(options && options.once) });
		}
		removeEventListener(type, cb) {
			const list = this._ls.get(String(type));
			if (!list) return;
			const i = list.findIndex((l) => l.cb === cb);
			if (i >= 0) list.splice(i, 1);
		}
		dispatchEvent(event) {
			event._trusted = false;
			event.target = event.currentTarget = this;
			const list = this._ls.get(event.type);
			if (list) {
				for (const l of [...list]) {
					if (l.once) this.removeEventListener(event.type, l.cb);
					if (typeof l.cb === "function") l.cb.call(this, event);
					else if (l.cb && typeof l.cb.handleEvent === "function") l.cb.handleEvent(event);
					if (event._stopImmediate) break;
				}
			}
			return !event.defaultPrevented;
		}
	}
	class CustomEvent extends Event {
		constructor(type, init = {}) { super(type, init); this.detail = init.detail ?? null; }
	}
	class MessageEvent extends Event {
		constructor(type, init = {}) {
			super(type, init);
			this.data = init.data ?? null;
			this.origin = init.origin ?? "";
			this.lastEventId = init.lastEventId ?? "";
			this.source = init.source ?? null;
			this.ports = init.ports ?? [];
		}
	}
	class ErrorEvent extends Event {
		constructor(type, init = {}) {
			super(type, init);
			this.message = init.message ?? "";
			this.filename = init.filename ?? "";
			this.lineno = init.lineno ?? 0;
			this.colno = init.colno ?? 0;
			this.error = init.error ?? null;
		}
	}
	for (const cls of [Event, EventTarget, CustomEvent, MessageEvent, ErrorEvent]) {
		globalThis[cls.name] = cls;
	}

	// The global is the event target, the way a worker's global is.
	const target = new EventTarget();
	globalThis.addEventListener = (...a) => target.addEventListener(...a);
	globalThis.removeEventListener = (...a) => target.removeEventListener(...a);
	globalThis.dispatchEvent = (e) => target.dispatchEvent(e);

	// ------------------------------------------------------------ base64
	globalThis.atob ??= (data) => {
		const s = String(data).replace(/[\t\n\f\r ]/g, "");
		if (s.length % 4 === 1 || /[^A-Za-z0-9+/=]/.test(s)) {
			throw new Error("atob: the string is not correctly encoded");
		}
		const T = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
		let out = "", bits = 0, acc = 0;
		for (const ch of s.replace(/=+$/, "")) {
			acc = (acc << 6) | T.indexOf(ch);
			bits += 6;
			if (bits >= 8) { bits -= 8; out += String.fromCharCode((acc >> bits) & 0xff); }
		}
		return out;
	};
	globalThis.btoa ??= (data) => {
		const s = String(data);
		const T = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
		let out = "";
		for (let i = 0; i < s.length; i += 3) {
			const a = s.charCodeAt(i), b = s.charCodeAt(i + 1), c = s.charCodeAt(i + 2);
			if (a > 0xff || b > 0xff || c > 0xff) {
				throw new Error("btoa: the string contains a character outside latin1");
			}
			out += T[a >> 2] + T[((a & 3) << 4) | ((b || 0) >> 4)];
			out += Number.isNaN(b) ? "==" : T[((b & 15) << 2) | ((c || 0) >> 6)] + (Number.isNaN(c) ? "=" : T[c & 63]);
		}
		return out;
	};

	// ------------------------------------------------------------ text codecs
	function utf8Encode(str) {
		const out = [];
		for (let i = 0; i < str.length; i++) {
			let c = str.codePointAt(i);
			if (c > 0xffff) i++;
			else if (c >= 0xd800 && c <= 0xdfff) c = 0xfffd;
			if (c < 0x80) out.push(c);
			else if (c < 0x800) out.push(0xc0 | (c >> 6), 0x80 | (c & 63));
			else if (c < 0x10000) out.push(0xe0 | (c >> 12), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
			else out.push(0xf0 | (c >> 18), 0x80 | ((c >> 12) & 63), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
		}
		return new Uint8Array(out);
	}
	globalThis.TextEncoder ??= class TextEncoder {
		get encoding() { return "utf-8"; }
		encode(s = "") { return utf8Encode(String(s)); }
	};
	globalThis.TextDecoder ??= class TextDecoder {
		constructor(label = "utf-8") {
			// Only UTF-8 lives here: the legacy encodings are table-driven and those
			// tables are on the parent side. Asking for one is refused rather than
			// answered with UTF-8, which would decode wrongly and silently.
			if (String(label).toLowerCase().replace(/[\t\n\f\r ]/g, "") !== "utf-8") {
				throw new RangeError(`TextDecoder: ${label} is not available inside a worker`);
			}
		}
		get encoding() { return "utf-8"; }
		decode(input) {
			if (input === undefined) return "";
			const b = input instanceof Uint8Array ? input
				: ArrayBuffer.isView(input) ? new Uint8Array(input.buffer, input.byteOffset, input.byteLength)
				: new Uint8Array(input);
			let out = "";
			for (let i = 0; i < b.length;) {
				const c = b[i];
				let cp, len;
				if (c < 0x80) { cp = c; len = 1; }
				else if ((c & 0xe0) === 0xc0) { cp = c & 0x1f; len = 2; }
				else if ((c & 0xf0) === 0xe0) { cp = c & 0x0f; len = 3; }
				else if ((c & 0xf8) === 0xf0) { cp = c & 0x07; len = 4; }
				else { out += "�"; i++; continue; }
				if (i + len > b.length) { out += "�"; break; }
				for (let j = 1; j < len; j++) cp = (cp << 6) | (b[i + j] & 0x3f);
				out += String.fromCodePoint(cp);
				i += len;
			}
			return out;
		}
	};

	// ------------------------------------------------------------ clone
	// structuredClone over the values a clone can hold. It is a real deep clone
	// with cycle preservation, not JSON: a Map stays a Map and a cycle stays a
	// cycle, which is the whole difference the API exists for.
	globalThis.structuredClone ??= function structuredClone(value) {
		const seen = new Map();
		const walk = (v) => {
			if (v === null || typeof v !== "object") {
				if (typeof v === "function" || typeof v === "symbol") {
					throw new Error("structuredClone: value could not be cloned");
				}
				return v;
			}
			if (seen.has(v)) return seen.get(v);
			let out;
			if (Array.isArray(v)) { out = []; seen.set(v, out); for (const x of v) out.push(walk(x)); return out; }
			if (v instanceof Date) { out = new Date(v.getTime()); seen.set(v, out); return out; }
			if (v instanceof RegExp) { out = new RegExp(v.source, v.flags); seen.set(v, out); return out; }
			if (v instanceof Map) { out = new Map(); seen.set(v, out); for (const [k, x] of v) out.set(walk(k), walk(x)); return out; }
			if (v instanceof Set) { out = new Set(); seen.set(v, out); for (const x of v) out.add(walk(x)); return out; }
			if (v instanceof ArrayBuffer) { out = v.slice(0); seen.set(v, out); return out; }
			if (ArrayBuffer.isView(v)) {
				out = new v.constructor(walk(v.buffer), v.byteOffset, v.length ?? v.byteLength);
				seen.set(v, out);
				return out;
			}
			if (v instanceof Error) {
				out = new v.constructor(v.message);
				seen.set(v, out);
				return out;
			}
			out = {};
			seen.set(v, out);
			for (const k of Object.keys(v)) out[k] = walk(v[k]);
			return out;
		};
		return walk(value);
	};

	// ------------------------------------------------------------ timers
	// A worker's timers run on the agent's own thread. The agent sleeps for the
	// shortest pending delay and then fires whatever is due — which is what a
	// single-threaded event loop with no other work to do amounts to. Between
	// sleeps the job queue drains, so a promise chain inside a timer runs.
	let nextTimer = 1;
	const timers = new Map(); // id -> {at, fn, args, interval}
	globalThis.setTimeout = (fn, delay, ...args) => {
		const id = nextTimer++;
		timers.set(id, { at: A.monotonicNow() + (Number(delay) || 0), fn, args, interval: -1 });
		return id;
	};
	globalThis.setInterval = (fn, delay, ...args) => {
		const id = nextTimer++;
		const d = Number(delay) || 0;
		timers.set(id, { at: A.monotonicNow() + d, fn, args, interval: d });
		return id;
	};
	globalThis.clearTimeout = (id) => { timers.delete(Number(id)); };
	globalThis.clearInterval = globalThis.clearTimeout;
	globalThis.queueMicrotask ??= (fn) => { Promise.resolve().then(fn); };
	globalThis.performance ??= { timeOrigin: 0, now: () => A.monotonicNow() };

	function runDueTimers() {
		const now = A.monotonicNow();
		for (const [id, t] of [...timers]) {
			if (t.at > now) continue;
			if (t.interval >= 0) t.at = now + t.interval;
			else timers.delete(id);
			try {
				if (typeof t.fn === "function") t.fn(...t.args);
			} catch (e) {
				reportFatal(e);
			}
		}
	}
	function nextTimerDelay() {
		let soonest = Infinity;
		const now = A.monotonicNow();
		for (const t of timers.values()) soonest = Math.min(soonest, Math.max(0, t.at - now));
		return soonest;
	}

	// ------------------------------------------------------------ console
	// Forwarded to the parent, which owns the streams.
	function fmt(args) {
		return args.map((a) => {
			if (typeof a === "string") return a;
			try { return JSON.stringify(a) ?? String(a); } catch { return String(a); }
		}).join(" ");
	}
	const post = (v) => { try { A.post(v); } catch { /* the parent is gone */ } };
	const consoleOut = (level) => (...args) => post({ __web_worker_console: { level, text: fmt(args) } });
	globalThis.console ??= {
		log: consoleOut(0), info: consoleOut(0), debug: consoleOut(0),
		warn: consoleOut(1), error: consoleOut(1), trace: consoleOut(1),
		dir: consoleOut(0), group: consoleOut(0), groupEnd: () => {}, table: consoleOut(0),
		assert: (c, ...rest) => { if (!c) consoleOut(1)("Assertion failed:", ...rest); },
		count: () => {}, time: () => {}, timeEnd: () => {},
	};

	// ------------------------------------------------------------ the scope
	// A global-scope interface's one instance is the global.
	function scopeClass(name, parent) {
		const cls = parent ? class extends parent {} : class {};
		Object.defineProperty(cls, "name", { value: name, configurable: true });
		Object.defineProperty(cls, Symbol.hasInstance, { value: (v) => v === globalThis, configurable: true });
		Object.defineProperty(globalThis, name, { value: cls, writable: true, configurable: true });
		return cls;
	}
	const WorkerGlobalScope = scopeClass("WorkerGlobalScope");
	scopeClass("DedicatedWorkerGlobalScope", WorkerGlobalScope);

	// self is NOT replaceable: assigning to it must leave the global in place.
	Object.defineProperty(globalThis, "self", {
		value: globalThis, writable: false, enumerable: true, configurable: true,
	});

	let closed = false;
	Object.defineProperty(globalThis, "close", {
		value: function close() { closed = true; }, writable: true, configurable: true,
	});

	globalThis.postMessage = function postMessage(message) {
		if (arguments.length < 1) throw new TypeError("postMessage: a message is required");
		post({ __web_worker_msg: true, data: message });
	};

	const handlers = {};
	for (const type of ["message", "messageerror", "error"]) {
		Object.defineProperty(globalThis, "on" + type, {
			get() { return handlers[type] ?? null; },
			set(fn) {
				const prev = handlers[type];
				if (prev) globalThis.removeEventListener(type, prev);
				handlers[type] = typeof fn === "function" ? fn : null;
				if (handlers[type]) globalThis.addEventListener(type, handlers[type]);
			},
			configurable: true, enumerable: true,
		});
	}

	// __web_worker_fatal is what the source wrapper calls for a top-level throw;
	// reportFatal is the same path for an error inside a timer or a message
	// handler. Either way the parent gets an 'error' event rather than a thread
	// that vanished.
	function reportFatal(e) {
		post({ __web_worker_error: (e && e.message) ? String(e.message) : String(e) });
	}
	globalThis.__web_worker_fatal = reportFatal;

	// ------------------------------------------------------------ init
	// The parent's first message carries what only it can compute: the worker's
	// name, and location's components from the real URL parser.
	const init = A.recv();
	if (init && init.__web_worker_terminate) {
		A.leaving();
		return;
	}
	globalThis.name = init && init.name ? init.name : "";
	if (init && init.location) {
		const parts = init.location;
		const WorkerLocation = class {};
		Object.defineProperty(WorkerLocation, "name", { value: "WorkerLocation", configurable: true });
		for (const k of Object.keys(parts)) {
			Object.defineProperty(WorkerLocation.prototype, k, {
				get() { return parts[k]; }, enumerable: true, configurable: true,
			});
		}
		WorkerLocation.prototype.toString = function () { return parts.href; };
		globalThis.WorkerLocation = WorkerLocation;
		const loc = new WorkerLocation();
		Object.defineProperty(globalThis, "location", { get: () => loc, configurable: true });
	}
	const navProto = {};
	Object.defineProperty(navProto, "userAgent", { get: () => "go-spidermonkey", enumerable: true, configurable: true });
	Object.defineProperty(navProto, "hardwareConcurrency", { get: () => 1, enumerable: true, configurable: true });
	const WorkerNavigator = function () { throw new TypeError("Illegal constructor"); };
	Object.defineProperty(WorkerNavigator, "name", { value: "WorkerNavigator", configurable: true });
	WorkerNavigator.prototype = navProto;
	globalThis.WorkerNavigator = WorkerNavigator;
	globalThis.navigator = Object.create(navProto);
	globalThis.isSecureContext = true;

	// ------------------------------------------------------------ the loop
	// Installed as a global so the wrapper can hand control here after the
	// worker's own source has run: from then on the worker is its messages and
	// its timers, exactly like any other event loop.
	globalThis.__web_worker_loop = () => {
		for (;;) {
			if (closed) break;
			let msg = A.tryRecv();
			while (msg !== A.NO_MSG) {
				if (msg && msg.__web_worker_terminate) return;
				if (msg && msg.__web_worker_msg) {
					try {
						globalThis.dispatchEvent(new MessageEvent("message", { data: msg.data }));
					} catch (e) {
						reportFatal(e);
					}
				}
				if (closed) break;
				msg = A.tryRecv();
			}
			runDueTimers();
			if (closed) break;
			// Nothing left to do and nothing to wait for: the worker is finished.
			// A worker with no timers still waits for messages, because its parent
			// may send one — that is what makes a worker outlive its script.
			const delay = nextTimerDelay();
			A.sleep(delay === Infinity ? 5 : Math.min(delay, 5));
		}
		post({ __web_worker_exit: true });
	};
})();
