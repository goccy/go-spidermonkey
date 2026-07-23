// compat/nodejs: the smaller modules the Express dependency tree pulls in —
// node:crypto (hash/hmac over Go crypto ops), node:tty, node:net (helpers;
// raw sockets come later), node:zlib (load-only stub), legacy url.parse, and
// the Error.captureStackTrace shim. Evaluated after streams.js.
(() => {
	"use strict";
	const ops = globalThis.__node_ops;
	const core = globalThis.__node_core_registry;

	// Guarded users (http-errors, ...) just want err.stack filled in. V8
	// semantics (prepareStackTrace structured frames) are NOT provided.
	if (typeof Error.captureStackTrace !== "function") {
		Error.captureStackTrace = function captureStackTrace(obj) {
			const stack = new Error().stack;
			try { obj.stack = typeof stack === "string" ? stack : ""; } catch {}
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
