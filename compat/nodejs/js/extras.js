// compat/nodejs: the smaller modules the Express dependency tree pulls in —
// node:crypto (hash/hmac over Go crypto ops), node:tty, node:net (helpers;
// raw sockets come later), node:zlib (load-only stub), legacy url.parse, and
// the Error.captureStackTrace shim. Evaluated after streams.js.
(() => {
	"use strict";
	const ops = globalThis.__node_ops;
	const core = globalThis.__node_core_registry;

	// V8's Error.captureStackTrace, including the prepareStackTrace protocol
	// (depd's Node build depends on structured CallSite objects): parse the
	// SpiderMonkey "fn@file:line:col" stack into CallSite-shaped frames.
	// Installed UNCONDITIONALLY: recent SpiderMonkey ships a native
	// captureStackTrace, but without prepareStackTrace support.
	{
		const toCallSite = (line) => {
			const m = /^(.*?)@(.*?):(\d+):(\d+)$/.exec(line);
			const fn = m ? m[1] : "";
			const file = m ? m[2] : String(line);
			const ln = m ? Number(m[3]) : 0;
			const col = m ? Number(m[4]) : 0;
			return {
				getFileName: () => file,
				getScriptNameOrSourceURL: () => file,
				getLineNumber: () => ln,
				getColumnNumber: () => col,
				getFunctionName: () => fn || null,
				getMethodName: () => fn || null,
				getFunction: () => undefined,
				getTypeName: () => null,
				getThis: () => undefined,
				getEvalOrigin: () => undefined,
				getPromiseIndex: () => null,
				isEval: () => false,
				isNative: () => false,
				isConstructor: () => false,
				isToplevel: () => true,
				isAsync: () => false,
				isPromiseAll: () => false,
				toString: () => `${fn || "<anonymous>"} (${file}:${ln}:${col})`,
			};
		};
		Error.captureStackTrace = function captureStackTrace(obj) {
			const raw = String(new Error().stack || "");
			const lines = raw.split("\n").filter(Boolean).slice(1); // drop this frame
			try {
				if (typeof Error.prepareStackTrace === "function") {
					obj.stack = Error.prepareStackTrace(obj, lines.map(toCallSite));
				} else {
					obj.stack = lines.join("\n");
				}
			} catch {
				obj.stack = lines.join("\n");
			}
		};
	}

	// -------------------------------------------------------------- crypto

	const toBuf = (d, enc) => (typeof d === "string" ? Buffer.from(d, enc || "utf8") : Buffer.from(d.buffer ? new Uint8Array(d.buffer, d.byteOffset, d.byteLength) : d));

	class Hash {
		constructor(algorithm, key) {
			this._alg = String(algorithm).toLowerCase();
			this._key = key;
			this._chunks = [];
		}
		update(data, encoding) {
			this._chunks.push(toBuf(data, encoding));
			return this;
		}
		digest(encoding) {
			const data = Buffer.concat(this._chunks);
			const raw = this._key !== undefined
				? ops.crypto_hmac(this._alg, this._key, data)
				: ops.crypto_hash(this._alg, data);
			const out = Buffer.from(raw);
			return encoding ? out.toString(encoding) : out;
		}
	}

	function randomBytes(size, cb) {
		const out = Buffer.alloc(size);
		for (let off = 0; off < size; off += 65536) {
			globalThis.crypto.getRandomValues(out.subarray(off, Math.min(off + 65536, size)));
		}
		if (cb) { queueMicrotask(() => cb(null, out)); return; }
		return out;
	}

	core.crypto = {
		createHash: (algorithm) => new Hash(algorithm),
		createHmac: (algorithm, key) => new Hash(algorithm, toBuf(key)),
		randomBytes,
		pseudoRandomBytes: randomBytes,
		randomUUID: () => globalThis.crypto.randomUUID(),
		randomFillSync: (buf) => { globalThis.crypto.getRandomValues(buf); return buf; },
		timingSafeEqual: (a, b) => {
			if (a.byteLength !== b.byteLength) throw new RangeError("Input buffers must have the same byte length");
			let diff = 0;
			const ua = toBuf(a), ub = toBuf(b);
			for (let i = 0; i < ua.length; i++) diff |= ua[i] ^ ub[i];
			return diff === 0;
		},
		getHashes: () => ["md5", "sha1", "sha256", "sha384", "sha512"],
		webcrypto: globalThis.crypto,
		subtle: globalThis.crypto.subtle,
		constants: {},
	};

	// ----------------------------------------------------------------- tty

	core.tty = {
		isatty: () => false,
		ReadStream: class ReadStream {},
		WriteStream: class WriteStream {},
	};

	// ----------------------------------------------------------------- net
	// Address helpers now; real sockets arrive with the net phase.

	const isIPv4 = (s) => {
		const parts = String(s).split(".");
		return parts.length === 4 && parts.every((p) => /^\d{1,3}$/.test(p) && Number(p) <= 255);
	};
	const isIPv6 = (s) => {
		s = String(s);
		return s.includes(":") && /^[0-9a-fA-F:.]+$/.test(s) && s.split("::").length <= 2;
	};
	const notSupported = (what) => () => { throw new Error(`${what} is not supported yet in this runtime`); };
	core.net = {
		isIPv4,
		isIPv6,
		isIP: (s) => (isIPv4(s) ? 4 : isIPv6(s) ? 6 : 0),
		Socket: class Socket extends core.stream.Duplex {
			connect() { notSupported("net.Socket.connect")(); }
		},
		Server: class Server extends core.events {
			listen() { notSupported("net.Server.listen")(); }
		},
		createServer: notSupported("net.createServer"),
		createConnection: notSupported("net.createConnection"),
		connect: notSupported("net.connect"),
	};

	// ---------------------------------------------------------------- zlib
	// Load-only stub: body-parser requires it at module load but only calls
	// it for compressed request bodies.

	core.zlib = {
		createGzip: notSupported("zlib.createGzip"),
		createGunzip: notSupported("zlib.createGunzip"),
		createInflate: notSupported("zlib.createInflate"),
		createDeflate: notSupported("zlib.createDeflate"),
		createBrotliCompress: notSupported("zlib.createBrotliCompress"),
		createBrotliDecompress: notSupported("zlib.createBrotliDecompress"),
		gzipSync: notSupported("zlib.gzipSync"),
		gunzipSync: notSupported("zlib.gunzipSync"),
		constants: {},
	};

	// --------------------------------------------------------- async_hooks
	// AsyncLocalStorage without engine async-context tracking: the store is
	// a plain slot held for the duration of run() — including, when fn is
	// async, until its promise settles. Correct for the serialized
	// one-request-at-a-time execution this runtime does; NOT correct for
	// interleaved concurrent contexts.

	class AsyncLocalStorage {
		constructor() { this._store = undefined; }
		getStore() { return this._store; }
		run(store, fn, ...args) {
			const prev = this._store;
			this._store = store;
			let result;
			try {
				result = fn(...args);
			} catch (e) {
				this._store = prev;
				throw e;
			}
			if (result && typeof result.then === "function") {
				return result.finally(() => { this._store = prev; });
			}
			this._store = prev;
			return result;
		}
		exit(fn, ...args) { return this.run(undefined, fn, ...args); }
		enterWith(store) { this._store = store; }
		disable() { this._store = undefined; }
	}
	class AsyncResource {
		constructor(type) { this.type = type; }
		runInAsyncScope(fn, thisArg, ...args) { return fn.apply(thisArg, args); }
		emitDestroy() { return this; }
		bind(fn) { return fn; }
		asyncId() { return 1; }
		static bind(fn) { return fn; }
	}
	core.async_hooks = {
		AsyncLocalStorage,
		AsyncResource,
		executionAsyncId: () => 1,
		triggerAsyncId: () => 0,
		executionAsyncResource: () => ({}),
		createHook: () => ({ enable() { return this; }, disable() { return this; } }),
	};

	// ----------------------------------------------------------- perf_hooks

	Object.assign(globalThis.performance, {
		mark: () => {},
		measure: () => {},
		clearMarks: () => {},
		clearMeasures: () => {},
		getEntries: () => [],
		getEntriesByName: () => [],
		getEntriesByType: () => [],
		eventLoopUtilization: () => ({ idle: 0, active: 0, utilization: 0 }),
	});
	core.perf_hooks = {
		performance: globalThis.performance,
		PerformanceObserver: class PerformanceObserver {
			observe() {}
			disconnect() {}
			static get supportedEntryTypes() { return []; }
		},
		constants: {},
		monitorEventLoopDelay: () => ({ enable() {}, disable() {}, reset() {}, mean: 0, percentile: () => 0 }),
	};

	// ------------------------------------- small stubs the loaders require

	core.punycode = {
		version: "2.1.0",
		toASCII: (s) => String(s),
		toUnicode: (s) => String(s),
		encode: (s) => String(s),
		decode: (s) => String(s),
		ucs2: {
			encode: (arr) => String.fromCodePoint(...arr),
			decode: (s) => [...String(s)].map((c) => c.codePointAt(0)),
		},
	};

	core.vm = {
		createContext: (o = {}) => o,
		isContext: () => false,
		runInThisContext: (code) => (0, eval)(String(code)),
		runInNewContext: notSupported("vm.runInNewContext"),
		runInContext: notSupported("vm.runInContext"),
		compileFunction: (code, params = []) => new Function(...params, String(code)),
		Script: class Script {
			constructor(code) { this._code = String(code); }
			runInThisContext() { return (0, eval)(this._code); }
			runInNewContext() { notSupported("vm.Script.runInNewContext")(); }
		},
	};

	core.worker_threads = {
		isMainThread: true,
		threadId: 0,
		workerData: null,
		parentPort: null,
		resourceLimits: {},
		SHARE_ENV: Symbol.for("nodejs.worker_threads.SHARE_ENV"),
		Worker: class Worker {
			constructor() { notSupported("worker_threads.Worker")(); }
		},
		MessageChannel: class MessageChannel {
			constructor() { notSupported("worker_threads.MessageChannel")(); }
		},
		MessagePort: class MessagePort {},
		markAsUntransferable: () => {},
	};

	const dnsErr = (cb) => queueMicrotask(() => cb(Object.assign(new Error("dns is not supported yet in this runtime"), { code: "ENOTSUP" })));
	core.dns = {
		lookup: (host, opts, cb) => dnsErr(typeof opts === "function" ? opts : cb),
		resolve: (host, type, cb) => dnsErr(typeof type === "function" ? type : cb),
		promises: {
			lookup: () => Promise.reject(new Error("dns is not supported yet in this runtime")),
			resolve: () => Promise.reject(new Error("dns is not supported yet in this runtime")),
		},
	};

	core.tls = {
		connect: notSupported("tls.connect"),
		createServer: notSupported("tls.createServer"),
		createSecureContext: () => ({}),
		TLSSocket: class TLSSocket {},
		rootCertificates: [],
	};

	core.http2 = {
		connect: notSupported("http2.connect"),
		createServer: notSupported("http2.createServer"),
		createSecureServer: notSupported("http2.createSecureServer"),
		constants: {},
	};

	core.inspector = {
		open: () => {},
		close: () => {},
		url: () => undefined,
		Session: class Session {
			connect() {}
			disconnect() {}
			post(method, params, cb) { if (cb) cb(new Error("inspector is not supported")); }
			on() { return this; }
		},
	};

	core.readline = {
		createInterface: notSupported("readline.createInterface"),
		clearLine: () => {},
		cursorTo: () => {},
	};

	core.cluster = {
		isMaster: true,
		isPrimary: true,
		isWorker: false,
		workers: {},
		fork: notSupported("cluster.fork"),
	};

	core.diagnostics_channel = {
		channel: (name) => ({
			name,
			hasSubscribers: false,
			publish() {},
			subscribe() {},
			unsubscribe() {},
			bindStore() {},
			runStores(data, fn, ...args) { return fn(...args); },
		}),
		hasSubscribers: () => false,
		subscribe: () => {},
		unsubscribe: () => {},
		tracingChannel: (name) => ({
			start: { publish() {} },
			end: { publish() {} },
			traceSync: (fn, ctx, thisArg, ...args) => fn.apply(thisArg, args),
			tracePromise: (fn, ctx, thisArg, ...args) => fn.apply(thisArg, args),
		}),
	};

	// node:module is the Module class itself, defined with the require
	// machinery in corelibs.js.

	core.v8 = {
		getHeapStatistics: () => ({
			total_heap_size: 0, used_heap_size: 0, heap_size_limit: 2 ** 31,
			total_available_size: 2 ** 31, malloced_memory: 0, external_memory: 0,
		}),
		getHeapSpaceStatistics: () => [],
		setFlagsFromString: () => {},
		cachedDataVersionTag: () => 0,
		serialize: notSupported("v8.serialize"),
		deserialize: notSupported("v8.deserialize"),
		writeHeapSnapshot: notSupported("v8.writeHeapSnapshot"),
	};

	core.console = Object.assign(Object.create(null), globalThis.console, {
		Console: function Console() { return globalThis.console; },
	});

	core.constants = { os: {}, fs: {} };

	Object.assign(core.os, {
		networkInterfaces: () => ({}),
		userInfo: () => ({ username: "user", uid: 1000, gid: 1000, shell: null, homedir: "/" }),
		loadavg: () => [0, 0, 0],
		uptime: () => 0,
		version: () => "0.0.0",
		machine: () => "x86_64",
	});

	// ------------------------------------------------- legacy url.parse

	const qs = core.querystring;
	core.url.parse = function parse(str, parseQueryString) {
		str = String(str);
		const hasProtocol = /^[a-zA-Z][a-zA-Z0-9+.-]*:/.test(str);
		const u = hasProtocol ? new URL(str) : new URL(str, "http://placeholder.invalid");
		const qIndex = str.indexOf("?");
		const rawQuery = u.search ? u.search.slice(1) : qIndex >= 0 ? "" : null;
		return {
			protocol: hasProtocol ? u.protocol : null,
			slashes: hasProtocol ? true : null,
			auth: u.username ? (u.password ? `${u.username}:${u.password}` : u.username) : null,
			host: hasProtocol ? u.host : null,
			hostname: hasProtocol ? u.hostname : null,
			port: hasProtocol && u.port ? u.port : null,
			pathname: u.pathname,
			search: u.search || (qIndex >= 0 ? "?" : null),
			query: parseQueryString ? qs.parse(rawQuery ?? "") : rawQuery,
			hash: u.hash || null,
			path: u.pathname + (u.search || ""),
			href: hasProtocol ? u.href : u.pathname + (u.search || "") + (u.hash || ""),
		};
	};
	core.url.format = function format(o) {
		if (o instanceof URL) return o.href;
		if (typeof o === "string") return o;
		let s = "";
		if (o.protocol) s += o.protocol.endsWith(":") ? o.protocol : o.protocol + ":";
		if (o.slashes || o.protocol) s += "//";
		if (o.auth) s += o.auth + "@";
		s += o.host || ((o.hostname || "") + (o.port ? ":" + o.port : ""));
		s += o.pathname || "";
		const search = o.search || (o.query ? "?" + qs.stringify(o.query) : "");
		s += search || "";
		s += o.hash || "";
		return s;
	};
	core.url.resolve = (from, to) => new URL(to, new URL(from, "http://placeholder.invalid")).href;
})();
