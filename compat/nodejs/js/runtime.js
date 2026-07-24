// compat/nodejs runtime builtins: process, Buffer, the nextTick queue, and
// the core-module registry. Evaluated after compat/web's builtins (so
// TextEncoder/TextDecoder/atob/btoa/console exist) and before corelibs.js.
// __node_ops stays global until nodejs.Install deletes it.
(() => {
	"use strict";
	const ops = globalThis.__node_ops;

	// Node's global alias.
	globalThis.global = globalThis;

	// ------------------------------------------------------ core registry

	const core = {};
	globalThis.__node_core = (name) => {
		const m = core[String(name).replace(/^node:/, "")];
		if (!m) throw new Error(`Unknown builtin module: ${name}`);
		return m;
	};
	// corelibs.js populates this; hidden under a stable key.
	globalThis.__node_core_registry = core;

	// -------------------------------------------------- process.nextTick

	// The nextTick queue drains as an ENGINE microtask scheduled the moment
	// the queue becomes non-empty. Ticks therefore run before promise jobs
	// registered after them and before any macrotask; a tick queued BY a
	// promise job runs after the current promise batch (Node proper
	// interleaves per-job — a documented deviation, see the plan).
	const tickQueue = [];
	let tickScheduled = false;
	const runTicks = () => {
		tickScheduled = false;
		let n = 0;
		let firstErr;
		let threw = false;
		while (tickQueue.length) {
			const cb = tickQueue.shift();
			n++;
			// Isolate each tick: a throw in one must not drop the ticks queued
			// after it (Node runs them all). Route it to the uncaughtException
			// channel; only if unhandled is the first error re-thrown once the
			// queue drains, so it surfaces to the host instead of vanishing.
			try {
				cb();
			} catch (e) {
				const handled = globalThis.__node_emit_uncaught && globalThis.__node_emit_uncaught(e);
				if (!handled && !threw) { firstErr = e; threw = true; }
			}
		}
		if (threw) throw firstErr;
		return n;
	};
	const scheduleTicks = () => {
		if (!tickScheduled) {
			tickScheduled = true;
			Promise.resolve().then(runTicks);
		}
	};
	globalThis.__node_run_ticks = runTicks;
	globalThis.__node_ticks_pending = () => tickQueue.length;
	globalThis.__node_schedule_ticks = scheduleTicks;

	// ------------------------------------------------------------ process

	const env = ops.node_env();
	const process = {
		env,
		argv: ops.node_argv(),
		argv0: "node",
		execArgv: [],
		platform: ops.node_platform(),
		arch: "x64",
		version: "v20.0.0",
		versions: { node: "20.0.0", "go-spidermonkey": "0.2" },
		pid: 1,
		ppid: 0,
		title: "node",
		exitCode: undefined,
		cwd: () => "/",
		chdir: () => { throw new Error("process.chdir is not supported in this runtime"); },
		umask: () => 0o022,
		nextTick(cb, ...args) {
			if (typeof cb !== "function") throw new TypeError("callback is not a function");
			tickQueue.push(args.length ? () => cb(...args) : cb);
			scheduleTicks();
		},
		exit(code) {
			if (code !== undefined) process.exitCode = Number(code);
			// Record the exit on the host so RunScript/Wait report it as a clean
			// termination, then throw an unwind sentinel to stop execution. The
			// sentinel is never treated as "handled" by uncaughtException, so it
			// can't be swallowed by a user handler.
			ops.node_exit(process.exitCode ?? 0);
			const e = new Error(`process.exit(${process.exitCode ?? 0})`);
			e.__nodeExit = true;
			throw e;
		},
		stdout: {
			isTTY: false,
			write: (s) => { ops.raw_write(0, String(s)); return true; },
			end: () => {},
			columns: 80,
		},
		stderr: {
			isTTY: false,
			write: (s) => { ops.raw_write(1, String(s)); return true; },
			end: () => {},
			columns: 80,
		},
		// stdin is lazily backed by Config.Stdin (a Readable); corelibs.js
		// grafts the real stream once node:stream exists.
		stdin: { isTTY: false },
		hrtime: Object.assign(
			(prev) => {
				const ms = performance.now();
				let s = Math.floor(ms / 1000), ns = Math.round((ms % 1000) * 1e6);
				if (prev) { s -= prev[0]; ns -= prev[1]; if (ns < 0) { s--; ns += 1e9; } }
				return [s, ns];
			},
			{ bigint: () => BigInt(Math.round(performance.now() * 1e6)) },
		),
		uptime: () => performance.now() / 1000,
		memoryUsage: () => ({ rss: 0, heapTotal: 0, heapUsed: 0, external: 0, arrayBuffers: 0 }),
		emitWarning: (w) => { console.error("Warning:", w); },
		execPath: "/usr/local/bin/node",
		getuid: () => 1000,
		getgid: () => 1000,
		geteuid: () => 1000,
		getegid: () => 1000,
		cpuUsage: () => ({ user: 0, system: 0 }),
		resourceUsage: () => ({}),
		release: { name: "node" },
		config: { variables: {} },
		features: {},
		allowedNodeEnvironmentFlags: new Set(),
		binding() { throw new Error("process.binding is not supported"); },
		dlopen() { throw new Error("process.dlopen is not supported"); },
		kill() { throw new Error("process.kill is not supported"); },
		// EventEmitter surface is grafted on by corelibs.js once node:events
		// exists; give inert fallbacks meanwhile.
		on() { return this; }, once() { return this; }, off() { return this; },
		emit() { return false; }, removeListener() { return this; },
	};
	globalThis.process = process;

	// ------------------------------------------------------------- Buffer

	function latin1Of(u8) {
		let s = "";
		for (let i = 0; i < u8.length; i += 0x8000) {
			s += String.fromCharCode.apply(null, u8.subarray(i, i + 0x8000));
		}
		return s;
	}
	const normEnc = (enc) => {
		const e = String(enc || "utf8").toLowerCase();
		if (e === "utf-8") return "utf8";
		if (e === "binary") return "latin1";
		if (e === "ucs2" || e === "ucs-2" || e === "utf-16le") return "utf16le";
		return e;
	};

	class Buffer extends Uint8Array {
		static from(value, encodingOrOffset, length) {
			if (typeof value === "string") return encodeString(value, encodingOrOffset);
			if (value instanceof ArrayBuffer) {
				return wrap(new Uint8Array(value, encodingOrOffset ?? 0, length ?? undefined));
			}
			if (ArrayBuffer.isView(value)) {
				return wrap(new Uint8Array(new Uint8Array(value.buffer, value.byteOffset, value.byteLength)));
			}
			if (Array.isArray(value) || (value && typeof value.length === "number")) {
				return wrap(Uint8Array.from(value));
			}
			throw new TypeError("Buffer.from: unsupported input");
		}
		static alloc(size, fill, encoding) {
			const b = wrap(new Uint8Array(size));
			if (fill !== undefined && fill !== 0) {
				if (typeof fill === "number") b.fill(fill);
				else {
					const f = Buffer.from(fill, encoding);
					for (let i = 0; i < b.length; i++) b[i] = f[i % f.length];
				}
			}
			return b;
		}
		static allocUnsafe(size) { return Buffer.alloc(size); }
		static allocUnsafeSlow(size) { return Buffer.alloc(size); }
		static isBuffer(v) { return v instanceof Buffer; }
		static isEncoding(enc) {
			return ["utf8", "hex", "base64", "base64url", "latin1", "ascii", "utf-8", "binary"].includes(String(enc).toLowerCase());
		}
		static byteLength(v, encoding) {
			if (typeof v === "string") return encodeString(v, encoding).length;
			return v.byteLength ?? 0;
		}
		static concat(list, totalLength) {
			let len = totalLength ?? list.reduce((n, b) => n + b.length, 0);
			const out = wrap(new Uint8Array(len));
			let off = 0;
			for (const b of list) {
				const chunk = off + b.length > len ? b.subarray(0, len - off) : b;
				out.set(chunk, off);
				off += chunk.length;
				if (off >= len) break;
			}
			return out;
		}
		static compare(a, b) { return compareBytes(a, b); }

		toString(encoding = "utf8", start = 0, end = this.length) {
			const sub = start !== 0 || end !== this.length ? this.subarray(start, end) : this;
			switch (normEnc(encoding)) {
				case "utf8": return new TextDecoder().decode(sub);
				case "hex": return [...sub].map((b) => b.toString(16).padStart(2, "0")).join("");
				case "base64": return btoa(latin1Of(sub));
				case "base64url":
					return btoa(latin1Of(sub)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
				case "latin1": return latin1Of(sub);
				case "ascii": return [...sub].map((b) => String.fromCharCode(b & 0x7f)).join("");
				case "utf16le": {
					let s = "";
					for (let i = 0; i + 1 < sub.length; i += 2) s += String.fromCharCode(sub[i] | (sub[i + 1] << 8));
					return s;
				}
			}
			throw new TypeError(`Unknown encoding: ${encoding}`);
		}
		toJSON() { return { type: "Buffer", data: [...this] }; }
		slice(start, end) { return this.subarray(start, end); } // Node slice shares memory
		equals(other) { return compareBytes(this, other) === 0; }
		compare(other) { return compareBytes(this, other); }
		copy(target, targetStart = 0, sourceStart = 0, sourceEnd = this.length) {
			// Node copies only what fits in the target's remaining space and
			// returns that count, rather than throwing when the source is larger.
			const room = target.length - targetStart;
			if (room <= 0) return 0;
			let chunk = this.subarray(sourceStart, sourceEnd);
			if (chunk.length > room) chunk = chunk.subarray(0, room);
			target.set(chunk, targetStart);
			return chunk.length;
		}
		write(string, offset = 0, encoding) {
			if (typeof offset === "string") { encoding = offset; offset = 0; }
			const bytes = encodeString(String(string), encoding);
			const n = Math.min(bytes.length, this.length - offset);
			this.set(bytes.subarray(0, n), offset);
			return n;
		}
		indexOf(value, byteOffset = 0) {
			if (typeof value === "number") return Uint8Array.prototype.indexOf.call(this, value, byteOffset);
			const needle = typeof value === "string" ? encodeString(value, "utf8") : value;
			if (needle.length === 0) return byteOffset;
			outer: for (let i = byteOffset; i <= this.length - needle.length; i++) {
				for (let j = 0; j < needle.length; j++) {
					if (this[i + j] !== needle[j]) continue outer;
				}
				return i;
			}
			return -1;
		}
		includes(value, byteOffset) { return this.indexOf(value, byteOffset) !== -1; }
		// fill(value[, offset[, end]][, encoding]). Buffer extends Uint8Array, whose
		// own fill coerces a string value to NaN -> 0 (silently zero-filling); Node
		// repeats the string/Buffer pattern, so override it.
		fill(value, offset = 0, end = this.length, encoding) {
			if (typeof offset === "string") { encoding = offset; offset = 0; end = this.length; }
			else if (typeof end === "string") { encoding = end; end = this.length; }
			offset = offset < 0 ? 0 : offset | 0;
			end = end > this.length ? this.length : end | 0;
			if (end <= offset) return this;
			if (typeof value === "number") {
				Uint8Array.prototype.fill.call(this, value & 0xff, offset, end);
				return this;
			}
			const src = typeof value === "string" ? encodeString(value, encoding)
				: (value && value.length !== undefined ? value : Buffer.from(value));
			if (!src || src.length === 0) { Uint8Array.prototype.fill.call(this, 0, offset, end); return this; }
			for (let i = offset; i < end; i++) this[i] = src[(i - offset) % src.length];
			return this;
		}
		readUInt8(off = 0) {
			if (off < 0 || off >= this.length) throw new RangeError(`The value of "offset" is out of range. It must be >= 0 and <= ${this.length - 1}. Received ${off}`);
			return this[off];
		}
		writeUInt8(v, off = 0) {
			if (off < 0 || off >= this.length) throw new RangeError(`The value of "offset" is out of range. It must be >= 0 and <= ${this.length - 1}. Received ${off}`);
			this[off] = v & 0xff;
			return off + 1;
		}
		_dv() { return new DataView(this.buffer, this.byteOffset, this.byteLength); }
		readUInt16BE(o = 0) { return this._dv().getUint16(o, false); }
		readUInt16LE(o = 0) { return this._dv().getUint16(o, true); }
		readUInt32BE(o = 0) { return this._dv().getUint32(o, false); }
		readUInt32LE(o = 0) { return this._dv().getUint32(o, true); }
		readInt8(o = 0) { return this._dv().getInt8(o); }
		readInt16BE(o = 0) { return this._dv().getInt16(o, false); }
		readInt16LE(o = 0) { return this._dv().getInt16(o, true); }
		readInt32BE(o = 0) { return this._dv().getInt32(o, false); }
		readInt32LE(o = 0) { return this._dv().getInt32(o, true); }
		readDoubleBE(o = 0) { return this._dv().getFloat64(o, false); }
		readDoubleLE(o = 0) { return this._dv().getFloat64(o, true); }
		readBigUInt64BE(o = 0) { return this._dv().getBigUint64(o, false); }
		writeUInt16BE(v, o = 0) { this._dv().setUint16(o, v, false); return o + 2; }
		writeUInt16LE(v, o = 0) { this._dv().setUint16(o, v, true); return o + 2; }
		writeUInt32BE(v, o = 0) { this._dv().setUint32(o, v, false); return o + 4; }
		writeUInt32LE(v, o = 0) { this._dv().setUint32(o, v, true); return o + 4; }
		writeInt32BE(v, o = 0) { this._dv().setInt32(o, v, false); return o + 4; }
	}

	function wrap(u8) { return Object.setPrototypeOf(u8, Buffer.prototype); }

	function compareBytes(a, b) {
		const len = Math.min(a.length, b.length);
		for (let i = 0; i < len; i++) {
			if (a[i] !== b[i]) return a[i] < b[i] ? -1 : 1;
		}
		return a.length === b.length ? 0 : a.length < b.length ? -1 : 1;
	}

	function encodeString(s, encoding) {
		switch (normEnc(encoding)) {
			case "utf8": return wrap(new TextEncoder().encode(s));
			case "hex": {
				const clean = s.length % 2 ? s.slice(0, -1) : s;
				const out = wrap(new Uint8Array(clean.length / 2));
				for (let i = 0; i < out.length; i++) out[i] = parseInt(clean.slice(i * 2, i * 2 + 2), 16) || 0;
				return out;
			}
			case "base64": case "base64url": {
				const bin = atob(s.replace(/-/g, "+").replace(/_/g, "/").replace(/=+$/, ""));
				const out = wrap(new Uint8Array(bin.length));
				for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
				return out;
			}
			case "latin1": case "ascii": {
				const out = wrap(new Uint8Array(s.length));
				for (let i = 0; i < s.length; i++) out[i] = s.charCodeAt(i) & 0xff;
				return out;
			}
			case "utf16le": {
				const out = wrap(new Uint8Array(s.length * 2));
				for (let i = 0; i < s.length; i++) {
					const c = s.charCodeAt(i);
					out[i * 2] = c & 0xff;
					out[i * 2 + 1] = c >> 8;
				}
				return out;
			}
		}
		throw new TypeError(`Unknown encoding: ${encoding}`);
	}

	globalThis.Buffer = Buffer;
	core.buffer = {
		Buffer,
		kMaxLength: 0x7fffffff,
		constants: { MAX_LENGTH: 0x7fffffff, MAX_STRING_LENGTH: 0x1fffffe8 },
		atob: globalThis.atob,
		btoa: globalThis.btoa,
	};
	core.process = process;
})();
