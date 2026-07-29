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

	// process.env is not a plain object: it is a view of the real environment,
	// where every name and value is a STRING. `process.env.PORT = 8080` stores
	// "8080", reading a name that was never set gives undefined rather than
	// something inherited from Object.prototype, and a symbol is refused
	// outright because an environment has no symbols in it. Handing out a plain
	// object meant env.PORT came back as the number 8080, and
	// `env.hasOwnProperty` resolved to a function.
	const envStore = ops.node_env();
	const env = new Proxy(envStore, {
		get(target, prop) {
			if (typeof prop === "symbol") return undefined;
			return Object.prototype.hasOwnProperty.call(target, prop) ? target[prop] : undefined;
		},
		set(target, prop, value) {
			if (typeof prop === "symbol" || typeof value === "symbol") {
				throw new TypeError("Cannot convert a Symbol value to a string");
			}
			target[String(prop)] = String(value);
			return true;
		},
		has(target, prop) {
			if (typeof prop === "symbol") return false;
			return Object.prototype.hasOwnProperty.call(target, prop);
		},
		deleteProperty(target, prop) {
			if (typeof prop === "symbol") return true;
			delete target[prop];
			return true;
		},
		ownKeys(target) { return Object.keys(target); },
		getOwnPropertyDescriptor(target, prop) {
			if (typeof prop === "symbol" || !Object.prototype.hasOwnProperty.call(target, prop)) return undefined;
			return { value: target[prop], writable: true, enumerable: true, configurable: true };
		},
		defineProperty(target, prop, desc) {
			if (typeof prop === "symbol") throw new TypeError("Cannot convert a Symbol value to a string");
			target[String(prop)] = String(desc.value);
			return true;
		},
	});
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

	// Node reports a bad argument with a CODE, and callers — including every
	// assert.throws in its own suite — match on that code rather than on the
	// message. Throwing a bare TypeError, or not throwing at all, is what makes
	// "Missing expected exception" the second-largest failure group.
	function argTypeError(name, expected, actual) {
		const got = actual === null ? "null" : Array.isArray(actual) ? "an instance of Array" : typeof actual;
		return Object.assign(
			new TypeError(`The "${name}" argument must be of type ${expected}. Received ${got}`),
			{ code: "ERR_INVALID_ARG_TYPE" });
	}
	// Past 2**32 Node groups the digits of the offending value ("1_099_511_627_776"),
	// which is the only readable way to show a number that wide.
	function numericSeparator(v) {
		const s = String(v);
		if (typeof v !== "number" || !Number.isInteger(v) || (v <= 2 ** 32 && v >= -(2 ** 32))) return s;
		return s.replace(/(\d)(?=(\d\d\d)+(?!\d))/g, "$1_");
	}
	function rangeError(name, range, actual) {
		return Object.assign(
			new RangeError(`The value of "${name}" is out of range. It must be ${range}. Received ${numericSeparator(actual)}`),
			{ code: "ERR_OUT_OF_RANGE" });
	}
	// requireIndex validates one of Buffer's numeric offset/length arguments.
	function requireIndex(name, value, max) {
		if (value === undefined) return undefined;
		if (typeof value !== "number") throw argTypeError(name, "number", value);
		if (!Number.isInteger(value)) throw rangeError(name, "an integer", value);
		if (value < 0 || (max !== undefined && value > max)) {
			throw rangeError(name, `>= 0 and <= ${max ?? "the buffer length"}`, value);
		}
		return value;
	}

	// Node's numeric accessors reject a bad argument with a CODE, and the code
	// says which kind of mistake it was: a non-number offset is an
	// ERR_INVALID_ARG_TYPE TypeError, while a number that is out of bounds —
	// or not an integer at all — is an ERR_OUT_OF_RANGE RangeError. The bare
	// RangeError a DataView raises carries neither, and reading past the end
	// through the index accessors did not throw at all, so the suite's whole
	// read*/write* coverage saw the wrong error or none.
	function ckOff(buf, o, bytes) {
		if (typeof o !== "number") throw argTypeError("offset", "number", o);
		// Node tests integer-ness with floor(v) !== v, which lets Infinity through
		// to the range message and sends only NaN and fractions to this one.
		if (Math.floor(o) !== o) throw rangeError("offset", "an integer", o);
		const max = buf.length - bytes;
		// A buffer too small to hold the value AT ANY offset is a different
		// failure from an offset that merely sits past the end, and Node gives
		// it its own code.
		if (max < 0) {
			throw Object.assign(new RangeError("Attempt to access memory outside buffer bounds"),
				{ code: "ERR_BUFFER_OUT_OF_BOUNDS" });
		}
		if (o < 0 || o > max) throw rangeError("offset", `>= 0 and <= ${max}`, o);
		return o;
	}
	// The variable-width accessors carry a byteLength of 1..6 as well.
	function ckLen(len) {
		if (typeof len !== "number") throw argTypeError("byteLength", "number", len);
		if (Math.floor(len) !== len) throw rangeError("byteLength", "an integer", len);
		if (len < 1 || len > 6) throw rangeError("byteLength", ">= 1 and <= 6", len);
		return len;
	}
	// Range-check a value before a write, like Node (which throws ERR_OUT_OF_
	// RANGE rather than silently truncating a wrapped value into the buffer).
	// Beyond 4 bytes the bound is not exactly representable as a decimal literal,
	// so Node states it as a power of two instead of a rounded number.
	function ckU(v, bytes) {
		if (typeof v !== "number") throw argTypeError("value", "number", v);
		const max = Math.pow(2, 8 * bytes) - 1;
		if (v >= 0 && v <= max) return;
		throw rangeError("value", bytes > 4 ? `>= 0 and < 2 ** ${8 * bytes}` : `>= 0 and <= ${max}`, v);
	}
	function ckI(v, bytes) {
		if (typeof v !== "number") throw argTypeError("value", "number", v);
		const lim = Math.pow(2, 8 * bytes - 1);
		if (v >= -lim && v <= lim - 1) return;
		throw rangeError("value", bytes > 4 ? `>= -(2 ** ${8 * bytes - 1}) and < 2 ** ${8 * bytes - 1}` : `>= ${-lim} and <= ${lim - 1}`, v);
	}
	const dv = (buf) => new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
	// A BigInt accessor takes a BigInt: passing a Number is a type error, not
	// something to coerce, because BigInt(1.5) would throw the wrong error.
	function ckBU(v) { if (typeof v !== "bigint") throw argTypeError("value", "bigint", v); if (v < 0n || v > 0xffffffffffffffffn) throw rangeError("value", ">= 0n and <= 18446744073709551615n", v); return v; }
	function ckBI(v) { if (typeof v !== "bigint") throw argTypeError("value", "bigint", v); if (v < -(2n ** 63n) || v > 2n ** 63n - 1n) throw rangeError("value", ">= -9223372036854775808n and <= 9223372036854775807n", v); return v; }

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
			throw argTypeError("first argument", "string, Buffer, ArrayBuffer, Array, or Array-like Object", value);
		}
		static alloc(size, fill, encoding) {
			if (typeof size !== "number") throw argTypeError("size", "number", size);
			if (!Number.isInteger(size) || size < 0) throw rangeError("size", ">= 0 and <= 2**32-1", size);
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
			if (!ArrayBuffer.isView(v) && !(v instanceof ArrayBuffer)) {
				throw argTypeError("string", "string, Buffer, or ArrayBuffer", v);
			}
			return v.byteLength ?? 0;
		}
		static concat(list, totalLength) {
			if (!Array.isArray(list)) throw argTypeError("list", "Array", list);
			for (const b of list) {
				if (!ArrayBuffer.isView(b)) throw argTypeError("list[i]", "Buffer or Uint8Array", b);
			}
			if (totalLength !== undefined) requireIndex("totalLength", totalLength);
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
		static compare(a, b) {
			if (!ArrayBuffer.isView(a)) throw argTypeError("buf1", "Buffer or Uint8Array", a);
			if (!ArrayBuffer.isView(b)) throw argTypeError("buf2", "Buffer or Uint8Array", b);
			return compareBytes(a, b);
		}

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
		compare(other, targetStart, targetEnd, sourceStart, sourceEnd) {
			if (!ArrayBuffer.isView(other)) throw argTypeError("target", "Buffer or Uint8Array", other);
			targetStart = requireIndex("targetStart", targetStart, other.length) ?? 0;
			targetEnd = requireIndex("targetEnd", targetEnd, other.length) ?? other.length;
			sourceStart = requireIndex("sourceStart", sourceStart, this.length) ?? 0;
			sourceEnd = requireIndex("sourceEnd", sourceEnd, this.length) ?? this.length;
			return compareBytes(this.subarray(sourceStart, sourceEnd), other.subarray(targetStart, targetEnd));
		}
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
		// subarray shares the bytes; Node hands back a Buffer, so the result has
		// to carry Buffer's prototype rather than the plain Uint8Array one.
		subarray(start, end) { return wrap(Uint8Array.prototype.subarray.call(this, start, end)); }
		toLocaleString(encoding, start, end) { return this.toString(encoding, start, end); }
		// How a Buffer prints. util.inspect() defers to this, and it truncates at
		// buffer.INSPECT_MAX_BYTES so dumping a large payload cannot flood a log.
		inspect() {
			const max = globalThis.__node_inspect_max_bytes ?? 50;
			const shown = Math.min(this.length, max);
			let s = "";
			for (let i = 0; i < shown; i++) s += (i ? " " : "") + this[i].toString(16).padStart(2, "0");
			const rest = this.length - shown;
			if (rest > 0) s += `${shown ? " " : ""}... ${rest} more byte${rest > 1 ? "s" : ""}`;
			return `<Buffer ${s}>`;
		}
		[Symbol.for("nodejs.util.inspect.custom")]() { return this.inspect(); }
		// Per-encoding slice/write pairs. Node exposes one of each on the
		// prototype as the primitive that toString()/write() dispatch to, and
		// enough published code reaches for them directly (and its own suite
		// enumerates them) that leaving them off is a hole in the surface.
		asciiSlice(start, end) { return this.toString("ascii", start, end); }
		base64Slice(start, end) { return this.toString("base64", start, end); }
		base64urlSlice(start, end) { return this.toString("base64url", start, end); }
		latin1Slice(start, end) { return this.toString("latin1", start, end); }
		hexSlice(start, end) { return this.toString("hex", start, end); }
		ucs2Slice(start, end) { return this.toString("ucs2", start, end); }
		utf8Slice(start, end) { return this.toString("utf8", start, end); }
		asciiWrite(string, offset, length) { return this.write(string, offset, length, "ascii"); }
		base64Write(string, offset, length) { return this.write(string, offset, length, "base64"); }
		base64urlWrite(string, offset, length) { return this.write(string, offset, length, "base64url"); }
		latin1Write(string, offset, length) { return this.write(string, offset, length, "latin1"); }
		hexWrite(string, offset, length) { return this.write(string, offset, length, "hex"); }
		ucs2Write(string, offset, length) { return this.write(string, offset, length, "ucs2"); }
		utf8Write(string, offset, length) { return this.write(string, offset, length, "utf8"); }
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
		readUInt8(off = 0) { return this[ckOff(this, off, 1)]; }
		writeUInt8(v, off = 0) {
			ckU(v, 1);
			this[ckOff(this, off, 1)] = v & 0xff;
			return off + 1;
		}
		readUInt16BE(o = 0) { return dv(this).getUint16(ckOff(this, o, 2), false); }
		readUInt16LE(o = 0) { return dv(this).getUint16(ckOff(this, o, 2), true); }
		readUInt32BE(o = 0) { return dv(this).getUint32(ckOff(this, o, 4), false); }
		readUInt32LE(o = 0) { return dv(this).getUint32(ckOff(this, o, 4), true); }
		readInt8(o = 0) { return dv(this).getInt8(ckOff(this, o, 1)); }
		readInt16BE(o = 0) { return dv(this).getInt16(ckOff(this, o, 2), false); }
		readInt16LE(o = 0) { return dv(this).getInt16(ckOff(this, o, 2), true); }
		readInt32BE(o = 0) { return dv(this).getInt32(ckOff(this, o, 4), false); }
		readInt32LE(o = 0) { return dv(this).getInt32(ckOff(this, o, 4), true); }
		readFloatBE(o = 0) { return dv(this).getFloat32(ckOff(this, o, 4), false); }
		readFloatLE(o = 0) { return dv(this).getFloat32(ckOff(this, o, 4), true); }
		readDoubleBE(o = 0) { return dv(this).getFloat64(ckOff(this, o, 8), false); }
		readDoubleLE(o = 0) { return dv(this).getFloat64(ckOff(this, o, 8), true); }
		readBigUInt64BE(o = 0) { return dv(this).getBigUint64(ckOff(this, o, 8), false); }
		readBigUInt64LE(o = 0) { return dv(this).getBigUint64(ckOff(this, o, 8), true); }
		readBigInt64BE(o = 0) { return dv(this).getBigInt64(ckOff(this, o, 8), false); }
		readBigInt64LE(o = 0) { return dv(this).getBigInt64(ckOff(this, o, 8), true); }
		writeInt8(v, o = 0) { ckI(v, 1); dv(this).setInt8(ckOff(this, o, 1), v); return o + 1; }
		writeUInt16BE(v, o = 0) { ckU(v, 2); dv(this).setUint16(ckOff(this, o, 2), v, false); return o + 2; }
		writeUInt16LE(v, o = 0) { ckU(v, 2); dv(this).setUint16(ckOff(this, o, 2), v, true); return o + 2; }
		writeUInt32BE(v, o = 0) { ckU(v, 4); dv(this).setUint32(ckOff(this, o, 4), v, false); return o + 4; }
		writeUInt32LE(v, o = 0) { ckU(v, 4); dv(this).setUint32(ckOff(this, o, 4), v, true); return o + 4; }
		writeInt16BE(v, o = 0) { ckI(v, 2); dv(this).setInt16(ckOff(this, o, 2), v, false); return o + 2; }
		writeInt16LE(v, o = 0) { ckI(v, 2); dv(this).setInt16(ckOff(this, o, 2), v, true); return o + 2; }
		writeInt32BE(v, o = 0) { ckI(v, 4); dv(this).setInt32(ckOff(this, o, 4), v, false); return o + 4; }
		writeInt32LE(v, o = 0) { ckI(v, 4); dv(this).setInt32(ckOff(this, o, 4), v, true); return o + 4; }
		writeFloatBE(v, o = 0) { dv(this).setFloat32(ckOff(this, o, 4), v, false); return o + 4; }
		writeFloatLE(v, o = 0) { dv(this).setFloat32(ckOff(this, o, 4), v, true); return o + 4; }
		writeDoubleBE(v, o = 0) { dv(this).setFloat64(ckOff(this, o, 8), v, false); return o + 8; }
		writeDoubleLE(v, o = 0) { dv(this).setFloat64(ckOff(this, o, 8), v, true); return o + 8; }
		writeBigUInt64BE(v, o = 0) { const b = ckBU(v); dv(this).setBigUint64(ckOff(this, o, 8), b, false); return o + 8; }
		writeBigUInt64LE(v, o = 0) { const b = ckBU(v); dv(this).setBigUint64(ckOff(this, o, 8), b, true); return o + 8; }
		writeBigInt64BE(v, o = 0) { const b = ckBI(v); dv(this).setBigInt64(ckOff(this, o, 8), b, false); return o + 8; }
		writeBigInt64LE(v, o = 0) { const b = ckBI(v); dv(this).setBigInt64(ckOff(this, o, 8), b, true); return o + 8; }
		// Variable-width LE/BE integer accessors. Neither offset nor byteLength
		// has a default: Node requires both, and defaulting them turned
		// readIntBE(undefined, 1) into a silent read at zero rather than the
		// type error it owes.
		readUIntLE(o, len) { ckLen(len); ckOff(this, o, len); let v = 0, m = 1; for (let i = 0; i < len; i++) { v += this[o + i] * m; m *= 256; } return v; }
		readUIntBE(o, len) { ckLen(len); ckOff(this, o, len); let v = 0; for (let i = 0; i < len; i++) v = v * 256 + this[o + i]; return v; }
		readIntLE(o, len) { ckLen(len); let v = this.readUIntLE(o, len); const s = Math.pow(2, 8 * len - 1); if (v >= s) v -= s * 2; return v; }
		readIntBE(o, len) { ckLen(len); let v = this.readUIntBE(o, len); const s = Math.pow(2, 8 * len - 1); if (v >= s) v -= s * 2; return v; }
		writeUIntLE(v, o, len) { ckLen(len); ckU(v, len); ckOff(this, o, len); let n = v; for (let i = 0; i < len; i++) { this[o + i] = n & 0xff; n = Math.floor(n / 256); } return o + len; }
		writeUIntBE(v, o, len) { ckLen(len); ckU(v, len); ckOff(this, o, len); let n = v; for (let i = len - 1; i >= 0; i--) { this[o + i] = n & 0xff; n = Math.floor(n / 256); } return o + len; }
		writeIntLE(v, o, len) { ckLen(len); ckI(v, len); ckOff(this, o, len); return this.writeUIntLE(v < 0 ? v + Math.pow(2, 8 * len) : v, o, len); }
		writeIntBE(v, o, len) { ckLen(len); ckI(v, len); ckOff(this, o, len); return this.writeUIntBE(v < 0 ? v + Math.pow(2, 8 * len) : v, o, len); }
		swap16() { if (this.length % 2) throw new RangeError("Buffer size must be a multiple of 16-bits"); for (let i = 0; i < this.length; i += 2) { const t = this[i]; this[i] = this[i + 1]; this[i + 1] = t; } return this; }
		swap32() { if (this.length % 4) throw new RangeError("Buffer size must be a multiple of 32-bits"); for (let i = 0; i < this.length; i += 4) { let a = this[i], b = this[i + 1]; this[i] = this[i + 3]; this[i + 1] = this[i + 2]; this[i + 2] = b; this[i + 3] = a; } return this; }
		swap64() { if (this.length % 8) throw new RangeError("Buffer size must be a multiple of 64-bits"); for (let i = 0; i < this.length; i += 8) { for (let j = 0; j < 4; j++) { const t = this[i + j]; this[i + j] = this[i + 7 - j]; this[i + 7 - j] = t; } } return this; }
	}

	// Node spells the unsigned accessors both ways — readUInt8 and readUint8 —
	// and the two are the SAME function object, which its suite checks. The
	// lowercase spelling is the one newer code tends to use.
	for (const name of Object.getOwnPropertyNames(Buffer.prototype)) {
		const alias = name.replace(/UInt/, "Uint");
		if (alias !== name) Buffer.prototype[alias] = Buffer.prototype[name];
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
