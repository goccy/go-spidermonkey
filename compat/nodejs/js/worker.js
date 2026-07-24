// compat/nodejs worker-thread bootstrap. Evaluated as its OWN script (via
// Agents.Spawn's glue slot) in the worker agent's realm BEFORE the user's
// worker source. The agent is a real OS-level thread (a goroutine) with its
// own SpiderMonkey realm and linear memory, sharing nothing with the main
// interpreter but SharedArrayBuffer memory — exactly Node's worker_threads
// model. Messages cross via the agent's structured-clone transport.
//
// This realm has no __node_ops (those are per-instance host functions on the
// main interpreter), so the worker environment here is self-contained
// pure-JS: Buffer, TextEncoder/Decoder, console (forwarded to the main
// thread), structuredClone, timers (via Atomics.waitAsync), plus
// parentPort/workerData/threadId. require of node: fs/http and friends is not
// available inside a worker — worker code should be self-contained or use
// workerData / messages.
(() => {
	"use strict";
	const A = globalThis.__agent__;

	// ---- TextEncoder / TextDecoder (pure UTF-8) ----
	function utf8Encode(str) {
		const out = [];
		for (let i = 0; i < str.length; i++) {
			let c = str.charCodeAt(i);
			if (c >= 0xd800 && c <= 0xdbff && i + 1 < str.length) {
				const lo = str.charCodeAt(i + 1);
				if (lo >= 0xdc00 && lo <= 0xdfff) { c = 0x10000 + ((c - 0xd800) << 10) + (lo - 0xdc00); i++; }
				else c = 0xfffd;
			} else if (c >= 0xd800 && c <= 0xdfff) c = 0xfffd;
			if (c < 0x80) out.push(c);
			else if (c < 0x800) out.push(0xc0 | (c >> 6), 0x80 | (c & 63));
			else if (c < 0x10000) out.push(0xe0 | (c >> 12), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
			else out.push(0xf0 | (c >> 18), 0x80 | ((c >> 12) & 63), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
		}
		return new Uint8Array(out);
	}
	globalThis.TextEncoder ??= class TextEncoder { get encoding() { return "utf-8"; } encode(s = "") { return utf8Encode(String(s)); } };
	globalThis.TextDecoder ??= class TextDecoder {
		get encoding() { return "utf-8"; }
		decode(input) {
			if (input === undefined) return "";
			const b = input instanceof Uint8Array ? input : new Uint8Array(input.buffer ? input.buffer : input);
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

	// ---- Buffer (Uint8Array subclass, compact) ----
	const wrap = (u8) => Object.setPrototypeOf(u8, Buffer.prototype);
	class Buffer extends Uint8Array {
		static from(v, enc) {
			if (typeof v === "string") {
				if (enc === "hex") { const o = wrap(new Uint8Array(v.length / 2)); for (let i = 0; i < o.length; i++) o[i] = parseInt(v.substr(i * 2, 2), 16); return o; }
				if (enc === "base64") { const bin = atob(v); const o = wrap(new Uint8Array(bin.length)); for (let i = 0; i < bin.length; i++) o[i] = bin.charCodeAt(i); return o; }
				return wrap(utf8Encode(v));
			}
			if (v instanceof ArrayBuffer) return wrap(new Uint8Array(v));
			return wrap(Uint8Array.from(v));
		}
		static alloc(n, fill) { const o = wrap(new Uint8Array(n)); if (fill) o.fill(typeof fill === "number" ? fill : fill.charCodeAt(0)); return o; }
		static concat(list) { const total = list.reduce((n, b) => n + b.length, 0); const o = wrap(new Uint8Array(total)); let off = 0; for (const b of list) { o.set(b, off); off += b.length; } return o; }
		static isBuffer(v) { return v instanceof Buffer; }
		toString(enc = "utf8") {
			if (enc === "hex") return [...this].map((b) => b.toString(16).padStart(2, "0")).join("");
			if (enc === "base64") return btoa(String.fromCharCode(...this));
			return new TextDecoder().decode(this);
		}
	}
	globalThis.Buffer ??= Buffer;

	// ---- atob / btoa ----
	const B64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
	globalThis.btoa ??= (data) => {
		const s = String(data); let out = "";
		for (let i = 0; i < s.length; i += 3) {
			const n = (s.charCodeAt(i) << 16) | ((s.charCodeAt(i + 1) || 0) << 8) | (s.charCodeAt(i + 2) || 0);
			out += B64[(n >> 18) & 63] + B64[(n >> 12) & 63] + (i + 1 < s.length ? B64[(n >> 6) & 63] : "=") + (i + 2 < s.length ? B64[n & 63] : "=");
		}
		return out;
	};
	globalThis.atob ??= (data) => {
		const s = String(data).replace(/[^A-Za-z0-9+/]/g, ""); let out = "", buf = 0, bits = 0;
		for (const ch of s) { buf = (buf << 6) | B64.indexOf(ch); bits += 6; if (bits >= 8) { bits -= 8; out += String.fromCharCode((buf >> bits) & 0xff); } }
		return out;
	};

	globalThis.structuredClone ??= (v) => (v === undefined ? undefined : JSON.parse(JSON.stringify(v)));
	globalThis.queueMicrotask ??= (fn) => Promise.resolve().then(fn);

	// ---- timers via Atomics.waitAsync (each fires as a drained job) ----
	let timerSeq = 0;
	const liveTimers = new Set();
	function scheduleTimer(fn, ms, repeat, args) {
		const id = ++timerSeq;
		liveTimers.add(id);
		const arm = () => {
			const sab = new Int32Array(new SharedArrayBuffer(4));
			Atomics.waitAsync(sab, 0, 0, Math.max(0, ms)).value.then(() => {
				if (!liveTimers.has(id)) return;
				try { fn(...args); } finally { if (repeat && liveTimers.has(id)) arm(); else liveTimers.delete(id); }
			});
		};
		arm();
		return id;
	}
	globalThis.setTimeout = (fn, ms, ...args) => scheduleTimer(fn, ms || 0, false, args);
	globalThis.setInterval = (fn, ms, ...args) => scheduleTimer(fn, ms || 0, true, args);
	globalThis.clearTimeout = globalThis.clearInterval = (id) => liveTimers.delete(id);
	globalThis.setImmediate = (fn, ...args) => scheduleTimer(fn, 0, false, args);
	globalThis.clearImmediate = (id) => liveTimers.delete(id);

	// ---- console (forwarded to the main thread) ----
	const fmt = (args) => args.map((a) => (typeof a === "string" ? a : (() => { try { return JSON.stringify(a); } catch { return String(a); } })())).join(" ");
	const consoleSend = (level, args) => A.post({ __wt_console: { level, text: fmt(args) } });
	globalThis.console = {
		log: (...a) => consoleSend(0, a), info: (...a) => consoleSend(0, a), debug: (...a) => consoleSend(0, a),
		warn: (...a) => consoleSend(1, a), error: (...a) => consoleSend(1, a),
	};

	// ---- parentPort / workerData / message plumbing ----
	// The first inbox message is the workerData handshake (main sends it right
	// after spawn); A.recv() blocks for it, so workerData is ready before the
	// user source runs.
	const init = A.recv();
	const workerData = init && init.__wt_init ? init.workerData : undefined;
	const threadId = init && init.__wt_init ? init.threadId : 0;

	function makePort() {
		const handlers = { message: [], messageerror: [], close: [] };
		return {
			postMessage: (v) => A.post({ __wt_msg: true, data: v }),
			on(type, fn) { (handlers[type] ||= []).push(fn); return this; },
			once(type, fn) { const w = (...a) => { this.off(type, w); fn(...a); }; return this.on(type, w); },
			off(type, fn) { const l = handlers[type]; if (l) { const i = l.indexOf(fn); if (i >= 0) l.splice(i, 1); } return this; },
			addEventListener(type, fn) { return this.on(type === "message" ? "message" : type, (data) => fn({ data })); },
			removeEventListener() { return this; },
			start() {}, close() { A.leaving(); },
			_deliver(data) { for (const fn of handlers.message.slice()) fn(data); },
		};
	}
	const parentPort = makePort();

	// A minimal process for worker code: exit() posts the code then leaves.
	const exitWorker = (code) => { A.post({ __wt_exit: Number(code) || 0 }); A.leaving(); };
	globalThis.process ??= {
		exit: exitWorker,
		nextTick: (fn, ...a) => queueMicrotask(() => fn(...a)),
		env: {}, argv: ["node", "worker"], platform: "linux", pid: 1,
		on() { return this; }, once() { return this; }, emit() { return false; },
	};
	parentPort.close = () => exitWorker(0);

	globalThis.__wt_parentPort = parentPort;
	globalThis.__wt_workerData = workerData;
	globalThis.__wt_threadId = threadId;
	globalThis.__wt_isMainThread = false;

	// A minimal require for worker code: worker_threads (this surface) and
	// buffer are available; other node: modules are not (their ops live on
	// the main interpreter). Worker code should be self-contained.
	const workerThreadsModule = {
		isMainThread: false,
		threadId,
		workerData,
		parentPort,
		Worker: class Worker { constructor() { throw new Error("nested workers are not supported"); } },
		MessagePort: class MessagePort {},
		SHARE_ENV: Symbol.for("nodejs.worker_threads.SHARE_ENV"),
	};
	globalThis.require = (name) => {
		const n = String(name).replace(/^node:/, "");
		if (n === "worker_threads") return workerThreadsModule;
		if (n === "buffer") return { Buffer };
		throw new Error(`Cannot require '${name}' inside a worker thread (only worker_threads and buffer are available)`);
	};

	// The agent's C++ pump calls __deliver__ with each incoming inbox message,
	// interleaved with job-queue draining — so an async message handler's
	// promises resolve without any busy poll. A throw in a handler is posted
	// to the main thread as the Worker's 'error'.
	globalThis.__deliver__ = (msg) => {
		if (msg && msg.__terminate__) { exitWorker(0); return; }
		if (msg && msg.__wt_msg) {
			try { parentPort._deliver(msg.data); }
			catch (e) { A.post({ __wt_error: e instanceof Error ? `${e.name}: ${e.message}` : String(e) }); }
		}
	};
})();
