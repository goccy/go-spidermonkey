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
	// The process working directory, tracked guest-side (see chdir).
	let __cwd = "/";
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
		// process.versions is read as a CAPABILITY MAP, not just trivia: Node's own
		// test harness decides whether crypto exists with
		// `Boolean(process.versions.openssl)`, and 413 tests skipped themselves on
		// that alone even though node:crypto and crypto.subtle are both here. Each
		// entry below names a component this runtime really provides — the crypto
		// backend is Go's standard library rather than OpenSSL, and the version is
		// reported in the field ecosystem code reads for "which crypto do you
		// have"; zlib/brotli are Go's compress and andybalholm/brotli; the ICU
		// figures are the ones compiled into the engine.
		versions: {
			node: "20.0.0",
			"go-spidermonkey": "0.2",
			openssl: "3.0.0",
			zlib: "1.3.1",
			brotli: "1.1.0",
			icu: "78.3",
			unicode: "17.0",
			cldr: "48.0",
			tz: "2026a",
		},
		pid: 1,
		ppid: 0,
		title: "node",
		exitCode: undefined,
		// The working directory is a guest-side notion: this runtime's filesystem
		// is Config.FS, whose root IS "/", so a cwd is just the prefix relative
		// paths resolve against. Refusing chdir outright made any program that
		// organizes itself by directory fail at the first call, for no reason
		// other than that nothing tracked the value.
		cwd: () => __cwd,
		chdir: (dir) => {
			if (typeof dir !== "string") {
				throw Object.assign(new TypeError("The \"directory\" argument must be of type string"), { code: "ERR_INVALID_ARG_TYPE" });
			}
			const next = dir.startsWith("/") ? dir : __cwd.replace(/\/$/, "") + "/" + dir;
			const norm = core.path.resolve(next);
			// chdir to a path that is not a directory is ENOENT/ENOTDIR, as in Node.
			let st;
			try { st = core.fs.statSync(norm); } catch (e) {
				throw Object.assign(new Error(`ENOENT: no such file or directory, chdir '${dir}'`), { code: "ENOENT", path: dir });
			}
			if (!st.isDirectory()) {
				throw Object.assign(new Error(`ENOTDIR: not a directory, chdir '${dir}'`), { code: "ENOTDIR", path: dir });
			}
			__cwd = norm;
		},
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
		// Real numbers from the Go host (runtime.MemStats); heapUsed/heapTotal
		// are the live GC figures so leak-guards and ratios work.
		memoryUsage: Object.assign(() => ops.proc_memusage(), { rss: () => ops.proc_memusage().rss }),
		// emitWarning(warning[, options]) / (warning[, type[, code]]) — normalize
		// to an Error and emit 'warning' on the next tick so process.on("warning")
		// listeners (including EventEmitter max-listeners leak detection) fire.
		// With no listener, fall back to stderr like Node's default handler.
		emitWarning(warning, options, code) {
			let type, detail;
			if (options !== null && typeof options === "object") {
				({ type, code, detail } = options);
			} else if (typeof options === "string") {
				type = options;
			}
			let w = warning;
			if (!(w instanceof Error)) {
				w = new Error(String(warning));
				w.name = type || "Warning";
			} else if (type) {
				w.name = type;
			}
			if (code !== undefined) w.code = code;
			if (detail !== undefined) w.detail = detail;
			process.nextTick(() => {
				if (typeof process.listenerCount === "function" && process.listenerCount("warning") > 0) {
					process.emit("warning", w);
				} else {
					console.error(`${w.name}: ${w.message}`);
				}
			});
		},
		execPath: "/usr/local/bin/node",
		getuid: () => 1000,
		getgid: () => 1000,
		geteuid: () => 1000,
		getegid: () => 1000,
		// cpuUsage([prev]) returns cumulative microseconds of user/system CPU;
		// with a previous reading it returns the delta (Node's contract). Values
		// are best-effort from the host (monotonic in elapsed time).
		cpuUsage: (prev) => {
			const u = ops.proc_cpuusage();
			if (prev && typeof prev === "object") {
				return { user: u.user - (prev.user || 0), system: u.system - (prev.system || 0) };
			}
			return u;
		},
		resourceUsage: () => ops.proc_resusage(),
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
				// A Uint8Array/Buffer copies its bytes; a DataView copies its backing
				// bytes; any OTHER TypedArray is copied element-wise truncated to a
				// byte (Node: Buffer.from(new Uint16Array([0x1234])) -> <34>), NOT a
				// raw reinterpretation of its backing memory.
				if (value instanceof Uint8Array) return wrap(new Uint8Array(value));
				if (value instanceof DataView) return wrap(new Uint8Array(value.buffer.slice(value.byteOffset, value.byteOffset + value.byteLength)));
				return wrap(Uint8Array.from(value));
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
			return ["utf8", "utf-8", "hex", "base64", "base64url", "latin1", "binary", "ascii",
				"utf16le", "utf-16le", "ucs2", "ucs-2"].includes(String(enc).toLowerCase());
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
		write(string, offset, length, encoding) {
			// Node overloads: write(string), (string, encoding),
			// (string, offset, encoding), (string, offset, length, encoding).
			if (typeof offset === "string") { encoding = offset; offset = 0; length = undefined; }
			else if (typeof length === "string") { encoding = length; length = undefined; }
			offset = offset || 0;
			const bytes = encodeString(String(string), encoding);
			let n = Math.min(bytes.length, this.length - offset);
			if (length !== undefined && length < n) n = length;
			if (n < 0) n = 0;
			this.set(bytes.subarray(0, n), offset);
			return n;
		}
		indexOf(value, byteOffset, encoding) {
			if (typeof byteOffset === "string") { encoding = byteOffset; byteOffset = 0; }
			byteOffset = +byteOffset || 0;
			if (byteOffset < 0) byteOffset = Math.max(this.length + byteOffset, 0);
			if (typeof value === "number") return Uint8Array.prototype.indexOf.call(this, value & 0xff, byteOffset);
			const needle = typeof value === "string" ? encodeString(value, encoding || "utf8") : value;
			if (needle.length === 0) return byteOffset <= this.length ? byteOffset : this.length;
			outer: for (let i = byteOffset; i <= this.length - needle.length; i++) {
				for (let j = 0; j < needle.length; j++) {
					if (this[i + j] !== needle[j]) continue outer;
				}
				return i;
			}
			return -1;
		}
		lastIndexOf(value, byteOffset, encoding) {
			if (typeof byteOffset === "string") { encoding = byteOffset; byteOffset = undefined; }
			if (typeof value === "number") {
				let start = byteOffset === undefined ? this.length - 1 : (byteOffset < 0 ? this.length + byteOffset : byteOffset);
				if (start >= this.length) start = this.length - 1;
				for (let i = start; i >= 0; i--) if (this[i] === (value & 0xff)) return i;
				return -1;
			}
			const needle = typeof value === "string" ? encodeString(value, encoding || "utf8") : value;
			if (needle.length === 0) return this.length;
			let start = byteOffset === undefined ? this.length - needle.length : (byteOffset < 0 ? this.length + byteOffset : byteOffset);
			if (start > this.length - needle.length) start = this.length - needle.length;
			outer: for (let i = start; i >= 0; i--) {
				for (let j = 0; j < needle.length; j++) {
					if (this[i + j] !== needle[j]) continue outer;
				}
				return i;
			}
			return -1;
		}
		includes(value, byteOffset, encoding) { return this.indexOf(value, byteOffset, encoding) !== -1; }
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
			this._ckU(v, 1);
			this[off] = v & 0xff;
			return off + 1;
		}
		// Range-check a value before a write, like Node (which throws ERR_OUT_OF_
		// RANGE rather than silently truncating a wrapped value into the buffer).
		_ckU(v, bytes) { const max = Math.pow(2, 8 * bytes) - 1; if (v < 0 || v > max) throw new RangeError(`The value of "value" is out of range. It must be >= 0 and <= ${max}. Received ${v}`); }
		_ckI(v, bytes) { const lim = Math.pow(2, 8 * bytes - 1); if (v < -lim || v > lim - 1) throw new RangeError(`The value of "value" is out of range. It must be >= ${-lim} and <= ${lim - 1}. Received ${v}`); }
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
		readFloatBE(o = 0) { return this._dv().getFloat32(o, false); }
		readFloatLE(o = 0) { return this._dv().getFloat32(o, true); }
		readDoubleBE(o = 0) { return this._dv().getFloat64(o, false); }
		readDoubleLE(o = 0) { return this._dv().getFloat64(o, true); }
		readBigUInt64BE(o = 0) { return this._dv().getBigUint64(o, false); }
		readBigUInt64LE(o = 0) { return this._dv().getBigUint64(o, true); }
		readBigInt64BE(o = 0) { return this._dv().getBigInt64(o, false); }
		readBigInt64LE(o = 0) { return this._dv().getBigInt64(o, true); }
		writeInt8(v, o = 0) { this._ckI(v, 1); this._dv().setInt8(o, v); return o + 1; }
		writeUInt16BE(v, o = 0) { this._ckU(v, 2); this._dv().setUint16(o, v, false); return o + 2; }
		writeUInt16LE(v, o = 0) { this._ckU(v, 2); this._dv().setUint16(o, v, true); return o + 2; }
		writeUInt32BE(v, o = 0) { this._ckU(v, 4); this._dv().setUint32(o, v, false); return o + 4; }
		writeUInt32LE(v, o = 0) { this._ckU(v, 4); this._dv().setUint32(o, v, true); return o + 4; }
		writeInt16BE(v, o = 0) { this._ckI(v, 2); this._dv().setInt16(o, v, false); return o + 2; }
		writeInt16LE(v, o = 0) { this._ckI(v, 2); this._dv().setInt16(o, v, true); return o + 2; }
		writeInt32BE(v, o = 0) { this._ckI(v, 4); this._dv().setInt32(o, v, false); return o + 4; }
		writeInt32LE(v, o = 0) { this._ckI(v, 4); this._dv().setInt32(o, v, true); return o + 4; }
		writeFloatBE(v, o = 0) { this._dv().setFloat32(o, v, false); return o + 4; }
		writeFloatLE(v, o = 0) { this._dv().setFloat32(o, v, true); return o + 4; }
		writeDoubleBE(v, o = 0) { this._dv().setFloat64(o, v, false); return o + 8; }
		writeDoubleLE(v, o = 0) { this._dv().setFloat64(o, v, true); return o + 8; }
		_ckBU(v) { const b = BigInt(v); if (b < 0n || b > 0xffffffffffffffffn) throw new RangeError(`The value of "value" is out of range. It must be >= 0n and <= 18446744073709551615n. Received ${b}`); return b; }
		_ckBI(v) { const b = BigInt(v); if (b < -(2n ** 63n) || b > 2n ** 63n - 1n) throw new RangeError(`The value of "value" is out of range. Received ${b}`); return b; }
		writeBigUInt64BE(v, o = 0) { this._dv().setBigUint64(o, this._ckBU(v), false); return o + 8; }
		writeBigUInt64LE(v, o = 0) { this._dv().setBigUint64(o, this._ckBU(v), true); return o + 8; }
		writeBigInt64BE(v, o = 0) { this._dv().setBigInt64(o, this._ckBI(v), false); return o + 8; }
		writeBigInt64LE(v, o = 0) { this._dv().setBigInt64(o, this._ckBI(v), true); return o + 8; }
		// Variable-width LE/BE integer accessors (1..6 bytes).
		readUIntLE(o = 0, len = 1) { let v = 0, m = 1; for (let i = 0; i < len; i++) { v += this[o + i] * m; m *= 256; } return v; }
		readUIntBE(o = 0, len = 1) { let v = 0; for (let i = 0; i < len; i++) v = v * 256 + this[o + i]; return v; }
		readIntLE(o = 0, len = 1) { let v = this.readUIntLE(o, len); const s = Math.pow(2, 8 * len - 1); if (v >= s) v -= s * 2; return v; }
		readIntBE(o = 0, len = 1) { let v = this.readUIntBE(o, len); const s = Math.pow(2, 8 * len - 1); if (v >= s) v -= s * 2; return v; }
		writeUIntLE(v, o = 0, len = 1) { this._ckU(v, len); let n = v; for (let i = 0; i < len; i++) { this[o + i] = n & 0xff; n = Math.floor(n / 256); } return o + len; }
		writeUIntBE(v, o = 0, len = 1) { this._ckU(v, len); let n = v; for (let i = len - 1; i >= 0; i--) { this[o + i] = n & 0xff; n = Math.floor(n / 256); } return o + len; }
		writeIntLE(v, o = 0, len = 1) { this._ckI(v, len); return this.writeUIntLE(v < 0 ? v + Math.pow(2, 8 * len) : v, o, len); }
		writeIntBE(v, o = 0, len = 1) { this._ckI(v, len); return this.writeUIntBE(v < 0 ? v + Math.pow(2, 8 * len) : v, o, len); }
		swap16() { if (this.length % 2) throw new RangeError("Buffer size must be a multiple of 16-bits"); for (let i = 0; i < this.length; i += 2) { const t = this[i]; this[i] = this[i + 1]; this[i + 1] = t; } return this; }
		swap32() { if (this.length % 4) throw new RangeError("Buffer size must be a multiple of 32-bits"); for (let i = 0; i < this.length; i += 4) { let a = this[i], b = this[i + 1]; this[i] = this[i + 3]; this[i + 1] = this[i + 2]; this[i + 2] = b; this[i + 3] = a; } return this; }
		swap64() { if (this.length % 8) throw new RangeError("Buffer size must be a multiple of 64-bits"); for (let i = 0; i < this.length; i += 8) { for (let j = 0; j < 4; j++) { const t = this[i + j]; this[i + j] = this[i + 7 - j]; this[i + 7 - j] = t; } } return this; }
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
				// Node stops at the first non-hex character (no zero-fill), and a
				// trailing odd nibble is dropped.
				const isHex = (c) => (c >= "0" && c <= "9") || (c >= "a" && c <= "f") || (c >= "A" && c <= "F");
				const bytes = [];
				for (let i = 0; i + 1 < s.length + 1; i += 2) {
					const hi = s[i], lo = s[i + 1];
					if (hi === undefined || lo === undefined || !isHex(hi) || !isHex(lo)) break;
					bytes.push(parseInt(hi + lo, 16));
				}
				return wrap(Uint8Array.from(bytes));
			}
			case "base64": case "base64url": {
				// Node's base64 decoder is lenient: it ignores characters outside the
				// alphabet rather than throwing (unlike atob), decoding what it can.
				const B64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
				const clean = s.replace(/-/g, "+").replace(/_/g, "/");
				const out = [];
				let buf = 0, bits = 0;
				for (let i = 0; i < clean.length; i++) {
					if (clean[i] === "=") break; // padding terminates the stream (Node)
					const v = B64.indexOf(clean[i]);
					if (v < 0) continue; // skip whitespace/other non-alphabet
					buf = (buf << 6) | v;
					bits += 6;
					if (bits >= 8) { bits -= 8; out.push((buf >> bits) & 0xff); }
				}
				return wrap(Uint8Array.from(out));
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
