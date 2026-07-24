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

	const isErr = (r) => r !== null && typeof r === "object" && typeof r.code === "string" && !(r instanceof Uint8Array);
	const cryptoThrow = (r) => { const e = new Error(r.message); e.code = r.code; throw e; };

	// Cipheriv/Decipheriv buffer their update() input and run one host-side
	// transform at final() (no host cipher state to leak).
	function makeCipher(encrypt) {
		return class Cipher {
			constructor(algorithm, key, iv) {
				this._algo = String(algorithm).toLowerCase();
				this._key = toBuf(key);
				this._iv = iv == null ? new Uint8Array(0) : toBuf(iv);
				this._chunks = [];
				this._aad = new Uint8Array(0);
				this._authTag = new Uint8Array(0);
			}
			setAAD(aad) { this._aad = toBuf(aad); return this; }
			setAuthTag(tag) { this._authTag = toBuf(tag); return this; }
			getAuthTag() { return this._tagOut; }
			setAutoPadding() { return this; }
			update(data, inputEnc, outputEnc) {
				this._chunks.push(toBuf(data, inputEnc));
				return outputEnc ? "" : Buffer.alloc(0);
			}
			final(outputEnc) {
				const data = Buffer.concat(this._chunks);
				const r = ops.crypto_cipher(this._algo, this._key, this._iv, encrypt, data, this._aad, this._authTag);
				ops.release_pending();
				if (isErr(r)) cryptoThrow(r);
				const out = Buffer.from(r.data);
				this._tagOut = Buffer.from(r.tag);
				return outputEnc ? out.toString(outputEnc) : out;
			}
		};
	}
	const Cipheriv = makeCipher(true);
	const Decipheriv = makeCipher(false);

	class Sign {
		constructor(algorithm) { this._algo = String(algorithm).toLowerCase().replace(/^rsa-/, ""); this._chunks = []; }
		update(data, enc) { this._chunks.push(toBuf(data, enc)); return this; }
		sign(key, outputEnc) {
			const pem = typeof key === "string" ? key : key.key;
			const r = ops.crypto_sign(this._algo, pem, Buffer.concat(this._chunks));
			ops.release_pending();
			if (isErr(r)) cryptoThrow(r);
			const out = Buffer.from(r);
			return outputEnc ? out.toString(outputEnc) : out;
		}
	}
	class Verify {
		constructor(algorithm) { this._algo = String(algorithm).toLowerCase().replace(/^rsa-/, ""); this._chunks = []; }
		update(data, enc) { this._chunks.push(toBuf(data, enc)); return this; }
		verify(key, signature, sigEnc) {
			const pem = typeof key === "string" ? key : key.key;
			const sig = toBuf(signature, sigEnc);
			return ops.crypto_verify(this._algo, pem, Buffer.concat(this._chunks), sig);
		}
	}

	function pbkdf2Sync(password, salt, iterations, keylen, digest) {
		const r = ops.crypto_pbkdf2(toBuf(password), toBuf(salt), iterations, keylen, String(digest).toLowerCase());
		ops.release_pending();
		if (isErr(r)) cryptoThrow(r);
		return Buffer.from(r);
	}
	function scryptSync(password, salt, keylen, options = {}) {
		const r = ops.crypto_scrypt(toBuf(password), toBuf(salt), keylen, options);
		ops.release_pending();
		if (isErr(r)) cryptoThrow(r);
		return Buffer.from(r);
	}
	function hkdfSync(digest, ikm, salt, info, keylen) {
		const r = ops.crypto_hkdf(String(digest).toLowerCase(), toBuf(ikm), toBuf(salt), toBuf(info), keylen);
		ops.release_pending();
		if (isErr(r)) cryptoThrow(r);
		return Buffer.from(r).buffer;
	}
	const asyncify = (fn) => (...args) => {
		const cb = args.pop();
		queueMicrotask(() => { try { cb(null, fn(...args)); } catch (e) { cb(e); } });
	};

	function generateKeyPairSync(type, options = {}) {
		const r = ops.crypto_generatekey(type, options);
		if (isErr(r)) cryptoThrow(r);
		return { publicKey: r.publicKey, privateKey: r.privateKey };
	}

	core.crypto = {
		createHash: (algorithm) => new Hash(algorithm),
		createHmac: (algorithm, key) => new Hash(algorithm, toBuf(key)),
		Hash, Hmac: Hash,
		createCipheriv: (algo, key, iv) => new Cipheriv(algo, key, iv),
		createDecipheriv: (algo, key, iv) => new Decipheriv(algo, key, iv),
		Cipheriv, Decipheriv,
		createSign: (algo) => new Sign(algo),
		createVerify: (algo) => new Verify(algo),
		Sign, Verify,
		pbkdf2Sync,
		pbkdf2: asyncify(pbkdf2Sync),
		scryptSync,
		scrypt: (pw, salt, keylen, opts, cb) => {
			if (typeof opts === "function") { cb = opts; opts = {}; }
			queueMicrotask(() => { try { cb(null, scryptSync(pw, salt, keylen, opts)); } catch (e) { cb(e); } });
		},
		hkdfSync,
		hkdf: (digest, ikm, salt, info, keylen, cb) => {
			queueMicrotask(() => { try { cb(null, hkdfSync(digest, ikm, salt, info, keylen)); } catch (e) { cb(e); } });
		},
		generateKeyPairSync,
		generateKeyPair: (type, options, cb) => {
			queueMicrotask(() => { try { const kp = generateKeyPairSync(type, options); cb(null, kp.publicKey, kp.privateKey); } catch (e) { cb(e); } });
		},
		randomBytes,
		randomInt: (min, max) => {
			if (max === undefined) { max = min; min = 0; }
			const range = max - min;
			const buf = randomBytes(6);
			let n = 0;
			for (const b of buf) n = n * 256 + b;
			return min + (n % range);
		},
		pseudoRandomBytes: randomBytes,
		randomUUID: () => globalThis.crypto.randomUUID(),
		randomFillSync: (buf) => { globalThis.crypto.getRandomValues(buf); return buf; },
		randomFill: (buf, cb) => { globalThis.crypto.getRandomValues(buf); queueMicrotask(() => cb(null, buf)); },
		timingSafeEqual: (a, b) => {
			if (a.byteLength !== b.byteLength) throw new RangeError("Input buffers must have the same byte length");
			let diff = 0;
			const ua = toBuf(a), ub = toBuf(b);
			for (let i = 0; i < ua.length; i++) diff |= ua[i] ^ ub[i];
			return diff === 0;
		},
		getHashes: () => ["md5", "sha1", "sha256", "sha384", "sha512"],
		getCiphers: () => ["aes-128-gcm", "aes-192-gcm", "aes-256-gcm", "aes-128-cbc", "aes-256-cbc", "aes-128-ctr", "aes-256-ctr"],
		webcrypto: globalThis.crypto,
		subtle: globalThis.crypto.subtle,
		constants: { RSA_PKCS1_PADDING: 1, RSA_PKCS1_OAEP_PADDING: 4, RSA_PSS_SALTLEN_DIGEST: -1 },
	};

	// ----------------------------------------------------------------- tty

	core.tty = {
		isatty: () => false,
		ReadStream: class ReadStream {},
		WriteStream: class WriteStream {},
	};

	const notSupported = (what) => () => { throw new Error(`${what} is not supported yet in this runtime`); };

	// ----------------------------------------------------------------- net
	// Raw TCP over the net_* host ops (Config.Dial/Resolve/Listen gated).
	// Socket is a Duplex: writes go to the host connection; inbound bytes
	// arrive as 'data' events posted from the reader goroutine.

	const isIPv4 = (s) => {
		const parts = String(s).split(".");
		return parts.length === 4 && parts.every((p) => /^\d{1,3}$/.test(p) && Number(p) <= 255);
	};
	const isIPv6 = (s) => {
		s = String(s);
		return s.includes(":") && /^[0-9a-fA-F:.]+$/.test(s) && s.split("::").length <= 2;
	};

	function Socket() {
		core.stream.Duplex.call(this, {});
		this._id = null;
		this.connecting = false;
		this.remoteAddress = undefined;
		this.remotePort = undefined;
	}
	Object.setPrototypeOf(Socket.prototype, core.stream.Duplex.prototype);
	Object.setPrototypeOf(Socket, core.stream.Duplex);
	Socket.prototype.connect = function connect(port, host, connectListener) {
		if (typeof port === "object") { const o = port; connectListener = host; host = o.host; port = o.port; }
		if (typeof host === "function") { connectListener = host; host = undefined; }
		host = host || "127.0.0.1";
		this.connecting = true;
		if (connectListener) this.once("connect", connectListener);
		const onData = (chunk) => this.push(Buffer.from(chunk));
		const onEnd = () => this.push(null);
		const onError = (msg) => { const e = new Error(msg); e.code = "ECONNRESET"; this.emit("error", e); };
		const onConnect = () => {
			this.connecting = false;
			this.remoteAddress = host;
			this.remotePort = port;
			this.emit("connect");
			this.emit("ready");
		};
		const r = ops.net_connect(String(host), Number(port), onData, onEnd, onError, onConnect);
		if (isErr(r)) { const e = new Error(r.message); e.code = r.code; process.nextTick(() => this.emit("error", e)); return this; }
		this._id = r;
		return this;
	};
	Socket.prototype._write = function _write(chunk, encoding, callback) {
		if (this._id === null) return callback(new Error("not connected"));
		ops.net_write(this._id, chunk);
		callback();
	};
	Socket.prototype._read = function _read() {}; // pushed by the host reader
	Socket.prototype.destroy = function destroy(err) {
		if (this._id !== null) { ops.net_close(this._id); this._id = null; }
		core.stream.Duplex.prototype.destroy.call(this, err);
		return this;
	};
	Socket.prototype.setTimeout = function () { return this; };
	Socket.prototype.setNoDelay = function () { return this; };
	Socket.prototype.setKeepAlive = function () { return this; };
	Socket.prototype.address = function () { return { address: this.remoteAddress, port: this.remotePort, family: "IPv4" }; };
	Socket.prototype.ref = function () { return this; };
	Socket.prototype.unref = function () { return this; };

	function NetServer(connectionListener) {
		core.events.call(this);
		this._id = null;
		this.listening = false;
		if (connectionListener) this.on("connection", connectionListener);
	}
	Object.setPrototypeOf(NetServer.prototype, core.events.prototype);
	Object.setPrototypeOf(NetServer, core.events);
	NetServer.prototype.listen = function listen(port, host, cb) {
		if (typeof port === "object") { const o = port; cb = host; host = o.host; port = o.port; }
		if (typeof host === "function") { cb = host; host = undefined; }
		host = host || "127.0.0.1";
		const onConnection = (id, remote) => {
			const sock = new Socket();
			sock._id = id;
			const at = remote.lastIndexOf(":");
			sock.remoteAddress = remote.slice(0, at);
			sock.remotePort = Number(remote.slice(at + 1));
			const onData = (chunk) => sock.push(Buffer.from(chunk));
			const onEnd = () => sock.push(null);
			const onError = (msg) => sock.emit("error", new Error(msg));
			ops.net_attach(id, onData, onEnd, onError);
			this.emit("connection", sock);
		};
		const r = ops.net_listen(String(host), Number(port) || 0, onConnection);
		if (isErr(r)) { const e = new Error(r.message); e.code = r.code; process.nextTick(() => this.emit("error", e)); return this; }
		this._id = r.id;
		this._port = r.port;
		this._host = host;
		this.listening = true;
		if (cb) this.once("listening", cb);
		process.nextTick(() => this.emit("listening"));
		return this;
	};
	NetServer.prototype.address = function () {
		return this.listening ? { address: this._host, port: this._port, family: "IPv4" } : null;
	};
	NetServer.prototype.close = function (cb) {
		if (this._id !== null) { ops.net_close_srv(this._id); this._id = null; this.listening = false; }
		if (cb) process.nextTick(cb);
		process.nextTick(() => this.emit("close"));
		return this;
	};

	core.net = {
		isIPv4,
		isIPv6,
		isIP: (s) => (isIPv4(s) ? 4 : isIPv6(s) ? 6 : 0),
		Socket,
		Stream: Socket,
		Server: NetServer,
		createServer: (listener) => new NetServer(listener),
		createConnection: (...args) => new Socket().connect(...args),
		connect: (...args) => new Socket().connect(...args),
	};

	// --------------------------------------------------------------- dgram
	// UDP sockets over the udp_* host ops.

	function Dgram(type) {
		core.events.call(this);
		this._id = null;
		this.type = type || "udp4";
	}
	Object.setPrototypeOf(Dgram.prototype, core.events.prototype);
	Object.setPrototypeOf(Dgram, core.events);
	Dgram.prototype.bind = function bind(port, address, cb) {
		if (typeof port === "object") { const o = port; cb = address; address = o.address; port = o.port; }
		if (typeof address === "function") { cb = address; address = undefined; }
		const onMessage = (data, rinfo) => this.emit("message", Buffer.from(data), rinfo);
		const r = ops.udp_bind(String(address || ""), Number(port) || 0, onMessage);
		if (isErr(r)) { const e = new Error(r.message); e.code = r.code; process.nextTick(() => this.emit("error", e)); return this; }
		this._id = r.id;
		this._port = r.port;
		if (cb) this.once("listening", cb);
		process.nextTick(() => this.emit("listening"));
		return this;
	};
	Dgram.prototype.send = function send(msg, ...rest) {
		// send(msg, [offset, length,] port, address, [callback])
		let cb;
		if (typeof rest[rest.length - 1] === "function") cb = rest.pop();
		let port, address;
		if (rest.length >= 3) { port = rest[2]; address = rest[3]; } // offset, length ignored (whole buffer)
		else { port = rest[0]; address = rest[1]; }
		const buf = typeof msg === "string" ? Buffer.from(msg) : Buffer.from(msg);
		const r = ops.udp_send(this._id, buf, Number(port), String(address || "127.0.0.1"));
		if (cb) process.nextTick(() => cb(isErr(r) ? Object.assign(new Error(r.message), { code: r.code }) : null));
		return this;
	};
	Dgram.prototype.address = function () { return { address: "127.0.0.1", port: this._port, family: "IPv4" }; };
	Dgram.prototype.close = function (cb) {
		if (this._id !== null) { ops.udp_close(this._id); this._id = null; }
		if (cb) process.nextTick(cb);
		process.nextTick(() => this.emit("close"));
		return this;
	};
	Dgram.prototype.setBroadcast = function () { return this; };
	Dgram.prototype.ref = function () { return this; };
	Dgram.prototype.unref = function () { return this; };
	core.dgram = {
		Socket: Dgram,
		createSocket: (type, listener) => {
			const t = typeof type === "object" ? type.type : type;
			const s = new Dgram(t);
			if (typeof listener === "function") s.on("message", listener);
			else if (type && typeof type.recvBufferSize === "undefined" && typeof listener === "function") s.on("message", listener);
			return s;
		},
	};

	// ---------------------------------------------------------------- zlib
	// One-shot transforms over the zlib_transform host op; the *Sync forms
	// are direct, the async forms defer to the microtask queue, and the
	// stream forms are Transforms that buffer then emit at flush.

	function zlibRun(method, data) {
		const r = ops.zlib_transform(method, toBuf(data));
		ops.release_pending();
		if (isErr(r)) { const e = new Error(r.message); e.code = r.code; throw e; }
		return Buffer.from(r);
	}
	const zlibSync = (method) => (data) => zlibRun(method, data);
	const zlibAsync = (method) => (data, opts, cb) => {
		if (typeof opts === "function") { cb = opts; }
		queueMicrotask(() => { try { cb(null, zlibRun(method, data)); } catch (e) { cb(e); } });
	};
	function zlibStream(method) {
		const chunks = [];
		return new core.stream.Transform({
			transform(chunk, enc, callback) { chunks.push(toBuf(chunk, enc)); callback(); },
			flush(callback) {
				try { this.push(zlibRun(method, Buffer.concat(chunks))); callback(); }
				catch (e) { callback(e); }
			},
		});
	}
	core.zlib = {
		gzipSync: zlibSync("gzip"),
		gunzipSync: zlibSync("gunzip"),
		deflateSync: zlibSync("deflate"),
		inflateSync: zlibSync("inflate"),
		deflateRawSync: zlibSync("deflateRaw"),
		inflateRawSync: zlibSync("inflateRaw"),
		unzipSync: zlibSync("gunzip"),
		brotliCompressSync: zlibSync("brotliCompress"),
		brotliDecompressSync: zlibSync("brotliDecompress"),
		gzip: zlibAsync("gzip"),
		gunzip: zlibAsync("gunzip"),
		deflate: zlibAsync("deflate"),
		inflate: zlibAsync("inflate"),
		deflateRaw: zlibAsync("deflateRaw"),
		inflateRaw: zlibAsync("inflateRaw"),
		unzip: zlibAsync("gunzip"),
		brotliCompress: zlibAsync("brotliCompress"),
		brotliDecompress: zlibAsync("brotliDecompress"),
		createGzip: () => zlibStream("gzip"),
		createGunzip: () => zlibStream("gunzip"),
		createDeflate: () => zlibStream("deflate"),
		createInflate: () => zlibStream("inflate"),
		createDeflateRaw: () => zlibStream("deflateRaw"),
		createInflateRaw: () => zlibStream("inflateRaw"),
		createUnzip: () => zlibStream("gunzip"),
		createBrotliCompress: () => zlibStream("brotliCompress"),
		createBrotliDecompress: () => zlibStream("brotliDecompress"),
		constants: {
			Z_NO_FLUSH: 0, Z_SYNC_FLUSH: 2, Z_FULL_FLUSH: 3, Z_FINISH: 4,
			Z_BEST_SPEED: 1, Z_BEST_COMPRESSION: 9, Z_DEFAULT_COMPRESSION: -1,
			BROTLI_OPERATION_PROCESS: 0, BROTLI_OPERATION_FLUSH: 1, BROTLI_OPERATION_FINISH: 2,
		},
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

	// worker_threads over the engine's agent cluster (real goroutine threads,
	// separate realms, structured-clone messaging, SharedArrayBuffer sharing).
	// In a worker realm the bootstrap (js/worker.js) sets __wt_parentPort etc.
	const inWorker = globalThis.__wt_isMainThread === false;

	class Worker extends core.events {
		constructor(filename, options = {}) {
			super();
			let source;
			if (options.eval) {
				source = String(filename);
			} else {
				// Main reads the worker file; workers run as scripts in their
				// own realm (self-contained — see js/worker.js).
				const fs = core.fs;
				let p = filename;
				if (filename && typeof filename === "object" && filename.href) {
					p = decodeURIComponent(new URL(filename.href).pathname);
				}
				source = fs.readFileSync(String(p), "utf8");
			}
			const r = ops.worker_spawn(source, options.workerData ?? null, this);
			this._id = r.id;
			this.threadId = r.threadId;
		}
		postMessage(value) { ops.worker_post(this._id, value); return this; }
		terminate() { ops.worker_terminate(this._id); return Promise.resolve(0); }
		ref() { return this; }
		unref() { return this; }
		// The host pump calls this to deliver an event.
		_emit(type, value) {
			if (type === "message") this.emit("message", value);
			else if (type === "error") this.emit("error", value instanceof Error ? value : new Error(String(value)));
			else this.emit(type, value);
		}
		addEventListener(type, fn) { return this.on(type, (v) => fn({ data: v })); }
		removeEventListener() { return this; }
	}

	core.worker_threads = {
		isMainThread: !inWorker,
		threadId: inWorker ? globalThis.__wt_threadId : 0,
		workerData: inWorker ? globalThis.__wt_workerData : null,
		parentPort: inWorker ? globalThis.__wt_parentPort : null,
		resourceLimits: {},
		SHARE_ENV: Symbol.for("nodejs.worker_threads.SHARE_ENV"),
		Worker,
		MessageChannel: class MessageChannel {
			constructor() { notSupported("worker_threads.MessageChannel")(); }
		},
		MessagePort: class MessagePort {},
		BroadcastChannel: class BroadcastChannel {
			constructor() { notSupported("worker_threads.BroadcastChannel")(); }
		},
		markAsUntransferable: () => {},
		getEnvironmentData: () => undefined,
		setEnvironmentData: () => {},
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
