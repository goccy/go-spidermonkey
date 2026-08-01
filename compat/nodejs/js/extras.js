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

	const toBuf = (d, enc) => {
		if (typeof d === "string") return Buffer.from(d, enc || "utf8");
		if (ArrayBuffer.isView(d)) return Buffer.from(new Uint8Array(d.buffer, d.byteOffset, d.byteLength));
		// ArrayBuffer and SharedArrayBuffer both copy through a view; Buffer.from
		// takes neither shared memory nor a raw SAB.
		if (typeof SharedArrayBuffer !== "undefined" && d instanceof SharedArrayBuffer) {
			return Buffer.from(new Uint8Array(new Uint8Array(d)));
		}
		return Buffer.from(d);
	};

	// The names every crypto entry point uses for a bad argument. Reaching the
	// host with a non-string algorithm produced whatever the Go side made of
	// "null" — a message about an unknown digest, not about the argument.
	// The received-suffix in Node's exact shapes: "Received null",
	// "Received type string ('20')", "Received an instance of Object".
	function cryptoReceived(v) {
		if (v === null || v === undefined) return `Received ${v}`;
		if (typeof v === "function") return `Received function ${v.name}`;
		if (typeof v === "object") {
			const n = v.constructor && v.constructor.name;
			return n ? `Received an instance of ${n}` : `Received ${String(v)}`;
		}
		let sv = typeof v === "string" ? `'${v}'` : String(v);
		if (sv.length > 28) sv = sv.slice(0, 25) + "...";
		return `Received type ${typeof v} (${sv})`;
	}
	const cryptoArgType = (name, expected, v) => Object.assign(
		new TypeError(`The "${name}" argument must be ${expected}. ${cryptoReceived(v)}`),
		{ code: "ERR_INVALID_ARG_TYPE" });
	const cryptoRange = (name, range, v) => Object.assign(
		new RangeError(`The value of "${name}" is out of range. It must be ${range}. Received ${v}`),
		{ code: "ERR_OUT_OF_RANGE" });
	function requireAlgorithm(algorithm, name = "algorithm") {
		if (typeof algorithm !== "string") throw cryptoArgType(name, "of type string", algorithm);
		return algorithm.toLowerCase();
	}
	// The digests the host implements; a name outside this set is Node's
	// synchronous ERR_CRYPTO_INVALID_DIGEST, never an async host error.
	const SUPPORTED_DIGESTS = new Set(["md5", "sha1", "sha224", "sha256", "sha384", "sha512"]);
	function requireDigest(digest, name = "digest") {
		const d = requireAlgorithm(digest, name);
		if (!SUPPORTED_DIGESTS.has(d)) {
			throw Object.assign(new TypeError(`Invalid digest: ${digest}`), { code: "ERR_CRYPTO_INVALID_DIGEST" });
		}
		return d;
	}
	// A count that must be a positive integer: iterations, key lengths, sizes.
	function requireCount(v, name, min = 0, max = Number.MAX_SAFE_INTEGER) {
		if (typeof v !== "number") throw cryptoArgType(name, "of type number", v);
		if (!Number.isInteger(v)) throw cryptoRange(name, "an integer", v);
		if (v < min || v > max) throw cryptoRange(name, `>= ${min} && <= ${max}`, v);
		return v;
	}
	const requireBufferLike = (v, name) => {
		if (typeof v !== "string" && !ArrayBuffer.isView(v) && !(v instanceof ArrayBuffer) &&
			!(typeof SharedArrayBuffer !== "undefined" && v instanceof SharedArrayBuffer)) {
			throw cryptoArgType(name, "of type string or an instance of Buffer, TypedArray, or DataView", v);
		}
		return v;
	};

	class Hash {
		constructor(algorithm, key) {
			this._alg = requireAlgorithm(algorithm);
			this._key = key;
			this._chunks = [];
		}
		update(data, encoding) {
			if (this._done) throw new Error("Digest already called");
			this._chunks.push(toBuf(data, encoding));
			return this;
		}
		digest(encoding) {
			if (this._done) throw new Error("Digest already called");
			this._done = true;
			const data = Buffer.concat(this._chunks);
			const raw = this._key !== undefined
				? ops.crypto_hmac(this._alg, this._key, data)
				: ops.crypto_hash(this._alg, data);
			const out = Buffer.from(raw);
			// Node: an encoding the Buffer does not know ("buffer" included)
			// returns the raw Buffer rather than throwing.
			if (!encoding || encoding === "buffer") return out;
			try { return out.toString(encoding); } catch { return out; }
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

	// randomFillInto fills buf[offset, offset+size) with random bytes, honoring the
	// (buf[, offset[, size]]) overload and chunking the getRandomValues call so a
	// buffer larger than 64 KiB doesn't hit its per-call QuotaExceededError.
	function randomFillInto(buf, offset, size) {
		if (!ArrayBuffer.isView(buf)) {
			throw Object.assign(new TypeError('The "buf" argument must be an instance of ArrayBuffer, Buffer, TypedArray, or DataView.'),
				{ code: "ERR_INVALID_ARG_TYPE" });
		}
		const total = buf.byteLength;
		// Node counts offset/size in ELEMENTS for a typed array (bytes for a
		// DataView, which has no BYTES_PER_ELEMENT). Scale to bytes before bounds
		// checks so randomFillSync(new Uint32Array(4), 1, 1) fills element 1
		// (bytes 4-7), not byte 1.
		const elem = buf.BYTES_PER_ELEMENT || 1;
		offset = offset === undefined ? 0 : (offset >>> 0) * elem;
		size = size === undefined ? total - offset : (size >>> 0) * elem;
		if (offset > total) throw Object.assign(new RangeError('The value of "offset" is out of range.'), { code: "ERR_OUT_OF_RANGE" });
		if (offset + size > total) throw Object.assign(new RangeError('The value of "size" is out of range.'), { code: "ERR_OUT_OF_RANGE" });
		// View the exact target window as bytes so offset/size are respected.
		const view = new Uint8Array(buf.buffer, buf.byteOffset + offset, size);
		for (let off = 0; off < size; off += 65536) {
			globalThis.crypto.getRandomValues(view.subarray(off, Math.min(off + 65536, size)));
		}
		return buf;
	}

	const isErr = (r) => r !== null && typeof r === "object" && typeof r.code === "string" && !(r instanceof Uint8Array);
	const cryptoThrow = (r) => { const e = new Error(r.message); e.code = r.code; throw e; };

	// Cipheriv/Decipheriv buffer their update() input and run one host-side
	// transform at final() (no host cipher state to leak).
	function makeCipher(encrypt) {
		return class Cipher {
			constructor(algorithm, key, iv) {
				this._algo = String(algorithm).toLowerCase();
				this._key = toBuf(keyMaterial(key)); // accepts a secret KeyObject
				this._iv = iv == null ? new Uint8Array(0) : toBuf(iv);
				this._chunks = [];
				this._aad = new Uint8Array(0);
				this._authTag = new Uint8Array(0);
				this._autoPad = true;
			}
			setAAD(aad) { this._aad = toBuf(aad); return this; }
			setAuthTag(tag) { this._authTag = toBuf(tag); return this; }
			getAuthTag() {
				if (!this._done) throw new Error("Attempting to get auth tag in unsupported state");
				return this._tagOut;
			}
			// Real Node semantics for block modes (CBC): encrypt with
			// autopadding off requires block-aligned input and appends no
			// PKCS#7 block; decrypt returns the raw plaintext including the
			// padding bytes. Stream modes (CTR/GCM) ignore it, as in Node.
			setAutoPadding(autoPadding) { this._autoPad = autoPadding === undefined ? true : !!autoPadding; return this; }
			update(data, inputEnc, outputEnc) {
				if (this._done) throw new Error("Trying to add data in unsupported state");
				// Node pins the DECODER once a string update chose one; a later
				// update may not switch it mid-stream.
				if (typeof data === "string") {
					const enc = (inputEnc || "utf8").toLowerCase().replace("utf-8", "utf8");
					if (!Buffer.isEncoding(enc)) {
						throw Object.assign(new TypeError(`Unknown encoding: ${inputEnc}`), { code: "ERR_UNKNOWN_ENCODING" });
					}
					if (this._inputEnc !== undefined && this._inputEnc !== enc) {
						throw Object.assign(new Error("Cannot change encoding"), { code: "ERR_CRYPTO_INVALID_STATE" });
					}
					this._inputEnc = enc;
				}
				// The OUTPUT encoding pins the same way.
				if (outputEnc) {
					const oe = String(outputEnc).toLowerCase().replace("utf-8", "utf8");
					if (oe !== "buffer" && !Buffer.isEncoding(oe)) {
						throw Object.assign(new TypeError(`Unknown encoding: ${outputEnc}`), { code: "ERR_UNKNOWN_ENCODING" });
					}
					if (this._outputEnc !== undefined && this._outputEnc !== oe) {
						throw Object.assign(new Error("Cannot change encoding"), { code: "ERR_CRYPTO_INVALID_STATE" });
					}
					this._outputEnc = oe;
				}
				this._chunks.push(toBuf(data, inputEnc));
				return outputEnc ? "" : Buffer.alloc(0);
			}
			final(outputEnc) {
				if (this._done) throw new Error("Unsupported state or unable to authenticate data");
				if (outputEnc) {
					const oe = String(outputEnc).toLowerCase().replace("utf-8", "utf8");
					if (oe !== "buffer" && !Buffer.isEncoding(oe)) {
						throw Object.assign(new TypeError(`Unknown encoding: ${outputEnc}`), { code: "ERR_UNKNOWN_ENCODING" });
					}
					if (this._outputEnc !== undefined && this._outputEnc !== oe) {
						throw Object.assign(new Error("Cannot change encoding"), { code: "ERR_CRYPTO_INVALID_STATE" });
					}
				}
				this._done = true;
				const data = Buffer.concat(this._chunks);
				const r = ops.crypto_cipher(this._algo, this._key, this._iv, encrypt, data, this._aad, this._authTag, this._autoPad);
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
			const pem = keyMaterial(key); // string/Buffer PEM, KeyObject, or { key, ... }
			// Honor the RSA padding scheme; default is PKCS#1 v1.5. Silently
			// downgrading a requested RSA-PSS to v1.5 would break interop with a
			// PSS verifier without any error.
			const padding = (typeof key === "object" && key.padding) || 1;
			const saltLength = (typeof key === "object" && key.saltLength !== undefined) ? key.saltLength : -2;
			const r = ops.crypto_sign(this._algo, pem, Buffer.concat(this._chunks), padding, saltLength);
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
			const pem = keyMaterial(key); // string/Buffer PEM, KeyObject, or { key, ... }
			const sig = toBuf(signature, sigEnc);
			const padding = (typeof key === "object" && key.padding) || 1;
			const saltLength = (typeof key === "object" && key.saltLength !== undefined) ? key.saltLength : -2;
			const r = ops.crypto_verify(this._algo, pem, Buffer.concat(this._chunks), sig, padding, saltLength);
			ops.release_pending();
			// An unsupported digest makes the host return an error OBJECT, which is
			// truthy — without this guard a defensive `if (verify.verify(...))`
			// would treat it as a successful verification (signature bypass).
			if (isErr(r)) cryptoThrow(r);
			return r;
		}
	}

	// ------------------------------- KeyObject / one-shot sign & verify
	// KeyObjects wrap the underlying key material (raw bytes for 'secret',
	// a canonical PEM string for 'public'/'private'); every key-consuming
	// path unwraps them via keyMaterial(), so jsonwebtoken-style
	// createSecretKey/createPrivateKey-on-every-call works everywhere a raw
	// key does.

	const kKeyData = Symbol("kKeyData");
	class KeyObject {
		constructor(type, data, asymmetricKeyType) {
			this.type = type; // 'secret' | 'public' | 'private'
			this[kKeyData] = data;
			if (type === "secret") this.symmetricKeySize = data.length;
			else if (asymmetricKeyType) this.asymmetricKeyType = asymmetricKeyType;
		}
		export(options = {}) {
			if (this.type === "secret") {
				if (options.format === "jwk") return { kty: "oct", k: Buffer.from(this[kKeyData]).toString("base64url") };
				return Buffer.from(this[kKeyData]);
			}
			const format = options.format || "pem";
			const type = options.type || (this.type === "private" ? "pkcs8" : "spki");
			const r = ops.crypto_key_export(this[kKeyData], this.type === "private", format, type);
			ops.release_pending();
			if (isErr(r)) cryptoThrow(r);
			if (format === "jwk") return r;
			return format === "der" ? Buffer.from(r) : r;
		}
	}

	// keyMaterial unwraps whatever a Node API accepts as a key — string /
	// Buffer PEM, KeyObject, or a { key, ... } options bag (possibly holding a
	// KeyObject) — down to the raw material the host ops take.
	function keyMaterial(key) {
		if (key instanceof KeyObject) return key[kKeyData];
		if (key !== null && typeof key === "object" && !Buffer.isBuffer(key) && !(key instanceof Uint8Array) && key.key !== undefined) {
			return keyMaterial(key.key);
		}
		return key;
	}

	// derToPem re-wraps DER key input ({ key, format: 'der', type }) as the
	// labeled PEM the host parser consumes.
	function derToPem(der, label) {
		const b64 = Buffer.from(der).toString("base64");
		let body = "";
		for (let i = 0; i < b64.length; i += 64) body += b64.slice(i, i + 64) + "\n";
		return `-----BEGIN ${label}-----\n${body}-----END ${label}-----\n`;
	}

	// asymKeyInput normalizes createPublicKey/createPrivateKey input to PEM
	// bytes for crypto_key_parse.
	function asymKeyInput(key, wantPrivate) {
		if (key instanceof KeyObject) {
			if (key.type === "secret") throw new TypeError("Invalid key object type secret");
			return key[kKeyData];
		}
		if (key !== null && typeof key === "object" && !Buffer.isBuffer(key) && !(key instanceof Uint8Array) && key.key !== undefined) {
			const inner = key.key;
			if (inner instanceof KeyObject) return inner[kKeyData];
			if ((key.format || "pem") === "der") {
				const t = key.type;
				const label = t === "pkcs1" ? (wantPrivate ? "RSA PRIVATE KEY" : "RSA PUBLIC KEY")
					: t === "sec1" ? "EC PRIVATE KEY"
					: wantPrivate ? "PRIVATE KEY" : "PUBLIC KEY";
				return derToPem(toBuf(inner), label);
			}
			return inner;
		}
		return key;
	}

	function createSecretKey(key, encoding) {
		return new KeyObject("secret", toBuf(key, encoding));
	}
	function createPrivateKey(key) {
		const r = ops.crypto_key_parse(asymKeyInput(key, true));
		if (isErr(r)) cryptoThrow(r);
		if (r.keyType !== "private") { const e = new Error("Invalid private key material"); e.code = "ERR_CRYPTO_INVALID_KEY_OBJECT_TYPE"; throw e; }
		return new KeyObject("private", r.privatePem, r.asymmetricKeyType);
	}
	function createPublicKey(key) {
		// Node derives the public key when handed private material.
		const r = ops.crypto_key_parse(asymKeyInput(key, false));
		if (isErr(r)) cryptoThrow(r);
		return new KeyObject("public", r.publicPem, r.asymmetricKeyType);
	}

	// One-shot crypto.sign/crypto.verify. algorithm may be null/undefined for
	// Ed25519 (the host signs the message directly) or a digest name.
	const oneShotAlgo = (algorithm) => algorithm == null ? "" : String(algorithm).toLowerCase().replace(/^rsa-/, "");
	const keyOptPadding = (key) => (key !== null && typeof key === "object" && !Buffer.isBuffer(key) && !(key instanceof Uint8Array) && key.padding !== undefined) ? key.padding : 1;
	const keyOptSaltLength = (key) => (key !== null && typeof key === "object" && !Buffer.isBuffer(key) && !(key instanceof Uint8Array) && key.saltLength !== undefined) ? key.saltLength : -2;
	function signOneShot(algorithm, data, key, callback) {
		if (typeof callback === "function") {
			queueMicrotask(() => { try { callback(null, signOneShot(algorithm, data, key)); } catch (e) { callback(e); } });
			return;
		}
		const r = ops.crypto_sign(oneShotAlgo(algorithm), keyMaterial(key), toBuf(data), keyOptPadding(key), keyOptSaltLength(key));
		ops.release_pending();
		if (isErr(r)) cryptoThrow(r);
		return Buffer.from(r);
	}
	function verifyOneShot(algorithm, data, key, signature, callback) {
		if (typeof callback === "function") {
			queueMicrotask(() => { try { callback(null, verifyOneShot(algorithm, data, key, signature)); } catch (e) { callback(e); } });
			return;
		}
		const r = ops.crypto_verify(oneShotAlgo(algorithm), keyMaterial(key), toBuf(data), toBuf(signature), keyOptPadding(key), keyOptSaltLength(key));
		ops.release_pending();
		if (isErr(r)) cryptoThrow(r);
		return r;
	}

	function pbkdf2Sync(password, salt, iterations, keylen, digest) {
		requireBufferLike(password, "password");
		requireBufferLike(salt, "salt");
		// int32 bounds: the host op takes C ints, and Node ranges them here.
		requireCount(iterations, "iterations", 1, 2147483647);
		requireCount(keylen, "keylen", 0, 2147483647);
		requireDigest(digest);
		const r = ops.crypto_pbkdf2(toBuf(password), toBuf(salt), iterations, keylen, String(digest).toLowerCase());
		ops.release_pending();
		if (isErr(r)) cryptoThrow(r);
		return Buffer.from(r);
	}
	function scryptSync(password, salt, keylen, options = {}) {
		requireBufferLike(password, "password");
		requireBufferLike(salt, "salt");
		requireCount(keylen, "keylen", 0);
		const r = ops.crypto_scrypt(toBuf(password), toBuf(salt), keylen, options);
		ops.release_pending();
		if (isErr(r)) cryptoThrow(r);
		return Buffer.from(r);
	}
	const HKDF_HASH_LEN = { md5: 16, sha1: 20, sha224: 28, sha256: 32, sha384: 48, sha512: 64 };
	function hkdfSync(digest, ikm, salt, info, keylen) {
		// Node's order: types first, then the info size, then the length,
		// and only THEN whether the digest exists — the suite pairs an
		// unknown digest with an oversized info to check exactly this.
		requireAlgorithm(digest, "digest");
		if (ikm instanceof KeyObject) ikm = keyMaterial(ikm);
		requireBufferLike(ikm, "ikm");
		requireBufferLike(salt, "salt");
		requireBufferLike(info, "info");
		const infoLen = typeof info === "string" ? Buffer.byteLength(info) : info.byteLength;
		if (infoLen > 1024) {
			throw Object.assign(
				new RangeError(`The value of "info" is out of range. It must be <= 1024. Received ${infoLen}`),
				{ code: "ERR_OUT_OF_RANGE" });
		}
		// Node names hkdf's fifth argument "length".
		requireCount(keylen, "length", 0, 2147483647);
		requireDigest(digest);
		const hashLen = HKDF_HASH_LEN[String(digest).toLowerCase()];
		if (hashLen && keylen > 255 * hashLen) {
			throw Object.assign(new RangeError(`Invalid key length: ${keylen}`), { code: "ERR_CRYPTO_INVALID_KEYLEN" });
		}
		const r = ops.crypto_hkdf(String(digest).toLowerCase(), toBuf(ikm), toBuf(salt), toBuf(info), keylen);
		ops.release_pending();
		if (isErr(r)) cryptoThrow(r);
		return Buffer.from(r).buffer;
	}
	// Node's async crypto shape: argument VALIDATION throws synchronously,
	// the operation's own failures arrive through the callback. Everything
	// here is one thread either way, so computing eagerly and delivering on a
	// microtask is observably Node's ordering.
	const VALIDATION_CODES = new Set(["ERR_INVALID_ARG_TYPE", "ERR_OUT_OF_RANGE",
		"ERR_INVALID_ARG_VALUE", "ERR_CRYPTO_INVALID_DIGEST", "ERR_CRYPTO_INVALID_KEYLEN"]);
	const asyncify = (fn) => (...args) => {
		// The callback is the argument PAST the operation's own: popping the
		// last argument unconditionally turned hkdf(d, ikm, salt, info, -1)
		// into a four-argument call and misnamed every later validation.
		let cb;
		if (args.length > fn.length || typeof args[args.length - 1] === "function") {
			cb = args.pop();
		}
		if (typeof cb !== "function") {
			// Node validates the ARGUMENTS before it validates the callback:
			// hkdf() with nothing at all names the digest, not the callback.
			try {
				fn(...args);
			} catch (e) {
				if (e && VALIDATION_CODES.has(e.code)) throw e;
			}
			throw Object.assign(new TypeError(`The "callback" argument must be of type function. ${cryptoReceived(cb)}`),
				{ code: "ERR_INVALID_ARG_TYPE" });
		}
		let result;
		try {
			result = fn(...args);
		} catch (e) {
			if (e && VALIDATION_CODES.has(e.code)) throw e;
			queueMicrotask(() => cb(e));
			return;
		}
		queueMicrotask(() => cb(null, result));
	};

	// generateKeyPairSync honors publicKeyEncoding/privateKeyEncoding: PEM
	// encodings return strings, DER encodings return Buffers with the DER
	// bytes, and with no encoding at all Node returns KeyObjects.
	function generateKeyPairSync(type, options = {}) {
		const r = ops.crypto_generatekey(type, options);
		if (isErr(r)) cryptoThrow(r);
		const publicKey = r.hasPublicEncoding
			? (r.publicIsDer ? Buffer.from(r.publicKey, "base64") : r.publicKey)
			: new KeyObject("public", r.publicKey, r.keyType);
		const privateKey = r.hasPrivateEncoding
			? (r.privateIsDer ? Buffer.from(r.privateKey, "base64") : r.privateKey)
			: new KeyObject("private", r.privateKey, r.keyType);
		return { publicKey, privateKey };
	}

	// RSA public/private encryption. key may be a PEM string, a KeyObject, or
	// { key, padding, oaepHash }. Node padding constants: 4 = OAEP, 1 = PKCS1.
	const PAD = { 4: "oaep", 1: "pkcs1" };
	function keyPEMof(key) { return keyMaterial(key); }
	function keyPadding(key, def) {
		if (typeof key === "object" && key.padding !== undefined) return PAD[key.padding] || def;
		return def;
	}
	function oaepHashOf(key) {
		return typeof key === "object" && key.oaepHash ? String(key.oaepHash).toLowerCase() : "sha1";
	}
	function publicEncrypt(key, buffer) {
		const r = ops.crypto_rsa_public(keyPEMof(key), toBuf(buffer), keyPadding(key, "oaep"), oaepHashOf(key));
		ops.release_pending();
		if (isErr(r)) cryptoThrow(r);
		return Buffer.from(r);
	}
	function privateDecrypt(key, buffer) {
		const r = ops.crypto_rsa_private(keyPEMof(key), toBuf(buffer), keyPadding(key, "oaep"), oaepHashOf(key));
		ops.release_pending();
		if (isErr(r)) cryptoThrow(r);
		return Buffer.from(r);
	}
	// privateEncrypt / publicDecrypt are the PKCS#1 type-1 private/public
	// primitives — distinct from public-encrypt / private-decrypt above.
	function privateEncrypt(key, buffer) {
		const r = ops.crypto_rsa_private_encrypt(keyPEMof(key), toBuf(buffer));
		ops.release_pending();
		if (isErr(r)) cryptoThrow(r);
		return Buffer.from(r);
	}
	function publicDecrypt(key, buffer) {
		const r = ops.crypto_rsa_public_decrypt(keyPEMof(key), toBuf(buffer));
		ops.release_pending();
		if (isErr(r)) cryptoThrow(r);
		return Buffer.from(r);
	}

	// Diffie-Hellman (modp) over the crypto_dh_* ops.
	// The named MODP groups. These primes are fixed by RFC 2409 and RFC 3526, so
	// asking for a group by name is how a caller skips prime generation
	// entirely — which is the whole reason the named groups exist.
	const MODP_PRIMES = {
		modp1: "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A63A3620FFFFFFFFFFFFFFFF",
		modp2: "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE65381FFFFFFFFFFFFFFFF",
		modp5: "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3DC2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F83655D23DCA3AD961C62F356208552BB9ED529077096966D670C354E4ABC9804F1746C08CA237327FFFFFFFFFFFFFFFF",
		modp14: "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3DC2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F83655D23DCA3AD961C62F356208552BB9ED529077096966D670C354E4ABC9804F1746C08CA18217C32905E462E36CE3BE39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9DE2BCBF6955817183995497CEA956AE515D2261898FA051015728E5A8AACAA68FFFFFFFFFFFFFFFF",
	};
	class DiffieHellmanGroup {
		constructor(name) {
			const prime = MODP_PRIMES[String(name)];
			if (!prime) {
				throw Object.assign(new Error(`Unknown DH group: ${name}`), { code: "ERR_CRYPTO_UNKNOWN_DH_GROUP" });
			}
			const dh = new DiffieHellman(prime.toLowerCase(), "02");
			// A group's prime and generator are fixed, so unlike DiffieHellman it
			// exposes no setters for them.
			for (const m of ["generateKeys", "computeSecret", "getPrime", "getGenerator", "getPublicKey", "getPrivateKey"]) {
				this[m] = (...a) => dh[m](...a);
			}
			Object.defineProperty(this, "verifyError", { get: () => dh.verifyError ?? 0 });
		}
	}

	class DiffieHellman {
		constructor(primeHexOrBits, generator) {
			// Node accepts a Buffer/Uint8Array prime (and generator) — e.g. from a
			// peer's getPrime(). The host op parses a string as hex, and a Buffer's
			// String() is a UTF-8 decode, not hex, so normalize to hex first.
			const toHex = (v) => (Buffer.isBuffer(v) || v instanceof Uint8Array) ? Buffer.from(v).toString("hex") : v;
			const r = ops.crypto_dh_generate(toHex(primeHexOrBits), toHex(generator));
			if (isErr(r)) cryptoThrow(r);
			this._prime = r.prime; this._gen = r.generator; this._priv = r.priv; this._pub = r.pub;
		}
		generateKeys() { return Buffer.from(this._pub, "hex"); }
		getPublicKey(enc) { const b = Buffer.from(this._pub, "hex"); return enc ? b.toString(enc) : b; }
		getPrivateKey(enc) { const b = Buffer.from(this._priv, "hex"); return enc ? b.toString(enc) : b; }
		getPrime(enc) { const b = Buffer.from(this._prime, "hex"); return enc ? b.toString(enc) : b; }
		getGenerator(enc) { const b = Buffer.from(this._gen, "hex"); return enc ? b.toString(enc) : b; }
		computeSecret(otherPub, inEnc, outEnc) {
			const hex = Buffer.isBuffer(otherPub) || otherPub instanceof Uint8Array
				? Buffer.from(otherPub).toString("hex") : Buffer.from(otherPub, inEnc).toString("hex");
			const r = ops.crypto_dh_compute(this._prime, this._priv, hex);
			if (isErr(r)) cryptoThrow(r);
			const b = Buffer.from(r, "hex");
			return outEnc ? b.toString(outEnc) : b;
		}
	}

	// ECDH over NIST curves (crypto.createECDH). Keys ride as hex; public points
	// use the uncompressed 0x04||X||Y form Node exposes.
	class ECDH {
		constructor(curve) { this._curve = curve; }
		generateKeys(enc) {
			const r = ops.crypto_ecdh_generate(this._curve);
			ops.release_pending();
			if (isErr(r)) cryptoThrow(r);
			this._priv = r.priv; this._pub = r.pub;
			const b = Buffer.from(this._pub, "hex");
			return enc ? b.toString(enc) : b;
		}
		getPublicKey(enc) { const b = Buffer.from(this._pub, "hex"); return enc ? b.toString(enc) : b; }
		getPrivateKey(enc) { const b = Buffer.from(this._priv, "hex"); return enc ? b.toString(enc) : b; }
		computeSecret(otherPub, inEnc, outEnc) {
			const hex = (Buffer.isBuffer(otherPub) || otherPub instanceof Uint8Array)
				? Buffer.from(otherPub).toString("hex") : Buffer.from(otherPub, inEnc).toString("hex");
			const r = ops.crypto_ecdh_compute(this._curve, this._priv, hex);
			ops.release_pending();
			if (isErr(r)) cryptoThrow(r);
			const b = Buffer.from(r, "hex");
			return outEnc ? b.toString(outEnc) : b;
		}
	}

	// ChaCha20-Poly1305 via createCipheriv("chacha20-poly1305", ...).
	class ChaChaCipher {
		constructor(encrypt, key, iv) { this._enc = encrypt; this._key = toBuf(keyMaterial(key)); this._iv = toBuf(iv); this._chunks = []; this._aad = new Uint8Array(0); this._authTag = new Uint8Array(0); }
		setAutoPadding() { return this; } // stream cipher: ignored, as in Node
		setAAD(aad) { this._aad = toBuf(aad); return this; }
		setAuthTag(tag) { this._authTag = toBuf(tag); return this; }
		getAuthTag() { return this._tagOut; }
		update(data, ie, oe) { this._chunks.push(toBuf(data, ie)); return oe ? "" : Buffer.alloc(0); }
		final(oe) {
			const r = ops.crypto_chacha(this._enc, this._key, this._iv, Buffer.concat(this._chunks), this._aad, this._authTag);
			ops.release_pending();
			if (isErr(r)) cryptoThrow(r);
			this._tagOut = Buffer.from(r.tag);
			const out = Buffer.from(r.data);
			return oe ? out.toString(oe) : out;
		}
	}

	class X509Certificate {
		constructor(pem) {
			const r = ops.crypto_x509(toBuf(pem));
			if (isErr(r)) cryptoThrow(r);
			this.subject = r.subject;
			this.issuer = r.issuer;
			this.validFrom = r.validFrom;
			this.validTo = r.validTo;
			this.serialNumber = r.serialNumber.toUpperCase();
			this.fingerprint256 = r.fingerprint256;
			this.ca = r.ca;
			this._publicKeyPEM = r.publicKey;
		}
		get publicKey() { return { export: () => this._publicKeyPEM }; }
	}

	core.crypto = {
		// FIPS mode is not a thing here: the primitives are Go's crypto, which
		// has no FIPS switch to report. Saying so plainly is what the tests that
		// branch on it need — they only ask whether it is on.
		getFips: () => 0,
		setFips: (v) => {
			if (v) throw Object.assign(new Error("FIPS mode is not available"), { code: "ERR_CRYPTO_FIPS_UNAVAILABLE" });
		},
		get fips() { return false; },
		createHash: (algorithm) => new Hash(algorithm),
		// The ALGORITHM is checked before the key: createHmac(null) is a mistake
		// about the digest, and reporting it as a bad key sends the caller to
		// the wrong argument.
		createHmac: (algorithm, key) => {
			requireAlgorithm(algorithm);
			return new Hash(algorithm, toBuf(keyMaterial(key)));
		},
		// crypto.hash(alg, data[, encoding]) — the one-shot form, which exists
		// because hashing a single buffer through a stream object is all
		// ceremony and no benefit.
		hash: (algorithm, data, outputEncoding = "hex") => {
			if (typeof algorithm !== "string") {
				throw Object.assign(new TypeError('The "algorithm" argument must be of type string.'), { code: "ERR_INVALID_ARG_TYPE" });
			}
			if (typeof data !== "string" && !ArrayBuffer.isView(data) && !(data instanceof ArrayBuffer)) {
				throw Object.assign(new TypeError('The "data" argument must be of type string or an instance of Buffer, TypedArray, or DataView.'), { code: "ERR_INVALID_ARG_TYPE" });
			}
			if (typeof outputEncoding !== "string") {
				throw Object.assign(new TypeError('The "outputEncoding" argument must be of type string.'), { code: "ERR_INVALID_ARG_TYPE" });
			}
			const known = ["hex", "base64", "base64url", "latin1", "binary", "buffer", "utf8", "utf-8", "ascii", "ucs2", "utf16le"];
			if (!known.includes(outputEncoding.toLowerCase())) {
				throw Object.assign(new TypeError(`The argument 'outputEncoding' is invalid. Received '${outputEncoding}'`), { code: "ERR_INVALID_ARG_VALUE" });
			}
			const h = new Hash(algorithm);
			h.update(data);
			return outputEncoding === "buffer" ? h.digest() : h.digest(outputEncoding);
		},
		// Node's crypto constructors are callable without `new` — legacy, but
		// published code and its own suite both do it.
		Hash: callableClass(Hash), Hmac: callableClass(Hash),
		KeyObject,
		createSecretKey, createPublicKey, createPrivateKey,
		sign: signOneShot,
		verify: verifyOneShot,
		createCipheriv: (algo, key, iv) =>
			requireAlgorithm(algo) === "chacha20-poly1305" ? new ChaChaCipher(true, key, iv) : new Cipheriv(algo, key, iv),
		createDecipheriv: (algo, key, iv) =>
			requireAlgorithm(algo) === "chacha20-poly1305" ? new ChaChaCipher(false, key, iv) : new Decipheriv(algo, key, iv),
		Cipheriv: callableClass(Cipheriv), Decipheriv: callableClass(Decipheriv),
		publicEncrypt, privateDecrypt,
		publicDecrypt, privateEncrypt,
		createDiffieHellman: (prime, gen) => new DiffieHellman(prime, gen),
		DiffieHellman,
		// The named MODP groups of RFC 2409/3526. They are fixed primes, so a
		// group is just a DiffieHellman over a known one — and callers ask for
		// them by name precisely to avoid generating a prime.
		createDiffieHellmanGroup: (name) => new DiffieHellmanGroup(name),
		getDiffieHellman: (name) => new DiffieHellmanGroup(name),
		DiffieHellmanGroup,
		createECDH: (curve) => new ECDH(curve),
		ECDH,
		// One-shot X25519/ECDH from two KeyObjects.
		diffieHellman: ({ privateKey, publicKey }) => {
			const r = ops.crypto_dh_keyobject(keyMaterial(privateKey), keyMaterial(publicKey));
			ops.release_pending();
			if (isErr(r)) cryptoThrow(r);
			return Buffer.from(r);
		},
		X509Certificate,
		createSign: (algo) => new Sign(algo),
		createVerify: (algo) => new Verify(algo),
		Sign: callableClass(Sign), Verify: callableClass(Verify),
		pbkdf2Sync,
		pbkdf2: asyncify(pbkdf2Sync),
		scryptSync,
		scrypt: (pw, salt, keylen, opts, cb) => {
			if (typeof opts === "function") { cb = opts; opts = {}; }
			queueMicrotask(() => { try { cb(null, scryptSync(pw, salt, keylen, opts)); } catch (e) { cb(e); } });
		},
		hkdfSync,
		hkdf: asyncify((digest, ikm, salt, info, keylen) => hkdfSync(digest, ikm, salt, info, keylen)),
		generateKeyPairSync,
		generateKeyPair: (type, options, cb) => {
			queueMicrotask(() => { try { const kp = generateKeyPairSync(type, options); cb(null, kp.publicKey, kp.privateKey); } catch (e) { cb(e); } });
		},
		randomBytes,
		randomInt: (min, max) => {
			if (max === undefined) { max = min; min = 0; }
			// Node requires safe integers with max > min; a degenerate range must
			// throw ERR_OUT_OF_RANGE, not silently return NaN or a wrong number.
			if (typeof min !== "number" || typeof max !== "number") {
				throw cryptoArgType(typeof min !== "number" ? "min" : "max", "of type number", typeof min !== "number" ? min : max);
			}
			if (!Number.isSafeInteger(min) || !Number.isSafeInteger(max) || max <= min) {
				throw Object.assign(
					new RangeError(`The value of "max" is out of range. It must be greater than the value of "min" (${min}). Received ${max}`),
					{ code: "ERR_OUT_OF_RANGE" });
			}
			const range = max - min;
			const buf = randomBytes(6);
			let n = 0;
			for (const b of buf) n = n * 256 + b;
			return min + (n % range);
		},
		pseudoRandomBytes: randomBytes,
		randomUUID: (options) => {
			if (options !== undefined && (options === null || typeof options !== "object")) {
				throw Object.assign(new TypeError('The "options" argument must be of type object.'), { code: "ERR_INVALID_ARG_TYPE" });
			}
			if (options && options.disableEntropyCache !== undefined && typeof options.disableEntropyCache !== "boolean") {
				throw Object.assign(new TypeError('The "options.disableEntropyCache" property must be of type boolean.'), { code: "ERR_INVALID_ARG_TYPE" });
			}
			return globalThis.crypto.randomUUID();
		},
		randomFillSync: (buf, offset, size) => randomFillInto(buf, offset, size),
		randomFill: (buf, offset, size, cb) => {
			// Node overloads: randomFill(buf, cb) / (buf, offset, cb) /
			// (buf, offset, size, cb). The callback is always the last argument.
			if (typeof offset === "function") { cb = offset; offset = undefined; size = undefined; }
			else if (typeof size === "function") { cb = size; size = undefined; }
			let out, err = null;
			try { out = randomFillInto(buf, offset, size); }
			catch (e) { err = e; }
			queueMicrotask(() => cb(err, err ? buf : out));
		},
		timingSafeEqual: (a, b) => {
			// Both sides must be buffers: comparing two STRINGS in constant time
			// is not what this function does, and silently coercing them would
			// give a caller a false sense of what it protected.
			for (const [v, n] of [[a, "a"], [b, "b"]]) {
				if (!ArrayBuffer.isView(v) && !(v instanceof ArrayBuffer)) {
					throw cryptoArgType(n, "an instance of Buffer, TypedArray, or DataView", v);
				}
			}
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
		constants: { RSA_PKCS1_PADDING: 1, RSA_PKCS1_OAEP_PADDING: 4, RSA_PKCS1_PSS_PADDING: 6, RSA_PSS_SALTLEN_DIGEST: -1, RSA_PSS_SALTLEN_AUTO: -2, RSA_PSS_SALTLEN_MAX_SIGN: -2 },
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

	// --------------------------- socket idle timeout (Socket#setTimeout)
	// Node semantics: setTimeout(ms[, cb]) arms an idle timer that is reset by
	// ANY activity (data received, write performed, connect); after ms of
	// inactivity the socket emits 'timeout' (plus the one-shot cb registered by
	// the call). The socket is NOT destroyed — Node leaves that to the app.
	// setTimeout(0) disables. The timer is unref'd so an idle-timeout alone
	// never keeps the event loop alive.
	function socketTimeoutArm(sock) {
		const ms = sock._timeoutMs;
		if (!ms || sock.destroyed) return;
		const t = sock._timeoutTimer;
		if (t && typeof t.refresh === "function") { t.refresh(); return; } // restart in place, keeps unref state
		if (t) clearTimeout(t);
		const timer = setTimeout(() => { sock._timeoutTimer = null; sock.emit("timeout"); }, ms);
		if (timer && typeof timer.unref === "function") timer.unref();
		sock._timeoutTimer = timer;
	}
	function socketTimeoutClear(sock) {
		if (sock._timeoutTimer) { clearTimeout(sock._timeoutTimer); sock._timeoutTimer = null; }
	}
	// Activity on the socket: restart the idle countdown if one is configured.
	function socketTimeoutTouch(sock) { if (sock._timeoutMs) socketTimeoutArm(sock); }
	function socketSetTimeout(ms, cb) {
		ms = Number(ms) || 0;
		if (typeof cb === "function") this.once("timeout", cb);
		socketTimeoutClear(this);
		this.timeout = this._timeoutMs = ms;
		if (ms > 0) {
			socketTimeoutArm(this);
			if (!this._timeoutCloseHooked) {
				this._timeoutCloseHooked = true;
				this.once("close", () => socketTimeoutClear(this)); // no stray timer after teardown
			}
		}
		return this;
	}

	function Socket(options) {
		core.stream.Duplex.call(this, {});
		this._id = null;
		this.connecting = false;
		this.remoteAddress = undefined;
		this.remotePort = undefined;
		this._timeoutMs = 0;
		this._timeoutTimer = null;
		// Node defaults allowHalfOpen:false — when the peer half-closes (FIN), the
		// writable side is auto-ended so 'finish'/'close' fire (libraries key their
		// reconnect/cleanup on 'close').
		this.allowHalfOpen = !!(options && options.allowHalfOpen);
		// A socket owns a host connection, so it takes Node's autoDestroy: once
		// both halves are done the socket is destroyed, which is what releases
		// the connection and the event-loop handle with it.
		this.autoDestroy = true;
	}
	// endOnPeerFin ends the writable half when the readable half ends, unless the
	// socket is in half-open mode — the missing transition that left 'close' unfired.
	function endOnPeerFin(sock) {
		sock.push(null);
		// Defer the auto-end until AFTER the 'end' event so a server that writes its
		// response inside the 'end' handler (read-until-FIN, then reply) still flushes
		// it — Node runs user 'end' listeners before auto-ending the writable half.
		if (!sock.allowHalfOpen) sock.once("end", () => { if (sock._ws && !sock._ws.ending && !sock._ws.destroyed) sock.end(); });
	}
	Object.setPrototypeOf(Socket.prototype, core.stream.Duplex.prototype);
	Object.setPrototypeOf(Socket, core.stream.Duplex);
	Socket.prototype.connect = function connect(port, host, connectListener) {
		if (typeof port === "object") { const o = port; connectListener = host; host = o.host; port = o.port; if (o.allowHalfOpen !== undefined) this.allowHalfOpen = !!o.allowHalfOpen; }
		if (typeof host === "function") { connectListener = host; host = undefined; }
		// A string port is a path on a real Node (a unix socket); here only a
		// numeric port can be dialled, so it is validated as one.
		if (typeof port !== "string" || !port.startsWith("/")) port = validatePort(port);
		host = host || "127.0.0.1";
		this.connecting = true;
		if (connectListener) this.once("connect", connectListener);
		const onData = (chunk) => { socketTimeoutTouch(this); this.push(Buffer.from(chunk)); };
		const onEnd = () => endOnPeerFin(this);
		const onError = (info) => {
			// info is a plain string (read errors) or a {code, message} object
			// (async connect failures — carries EACCES/ECONNREFUSED).
			const obj = info && typeof info === "object";
			const e = new Error(obj ? info.message : info);
			e.code = (obj && info.code) || "ECONNRESET";
			this.emit("error", e);
		};
		const onConnect = () => {
			// The dial completes off-loop, so its callback can be posted after the
			// guest has already destroyed the socket — the guest destroys in the
			// same tick, but the host goroutine may have passed its own check
			// first. A destroyed socket never emits 'connect'.
			if (this.destroyed || this._id === null) return;
			this.connecting = false;
			this.remoteAddress = host;
			this.remotePort = port;
			socketTimeoutTouch(this); // connect counts as activity for the idle timer
			this.emit("connect");
			this.emit("ready");
			startReading(this);
		};
		const r = ops.net_connect(String(host), Number(port), onData, onEnd, onError, onConnect);
		if (isErr(r)) { const e = new Error(r.message); e.code = r.code; process.nextTick(() => this.emit("error", e)); return this; }
		this._id = r;
		return this;
	};
	Socket.prototype._write = function _write(chunk, encoding, callback) {
		if (this._id === null) return callback(new Error("not connected"));
		socketTimeoutTouch(this); // a write is activity for the idle timer
		// The op fires our callback once the chunk is flushed off-loop, so a
		// slow peer backpressures the socket instead of an unbounded queue.
		ops.net_write(this._id, chunk, callback);
	};
	// _read is the read-backpressure signal: the Readable calls it when its buffer
	// drains, releasing one more host read (net_read_resume). Without this a fast
	// peer would stream unbounded data into the host regardless of guest demand.
	Socket.prototype._read = function _read() { if (this._id !== null) ops.net_read_resume(this._id); };
	// startReading kicks the read side once the socket is live. read(0) consumes
	// nothing but makes the Readable call _read, which is what tells the host to
	// start reading — the guest equivalent of libuv's readStart. A socket the
	// caller explicitly paused is left alone.
	function startReading(sock) {
		if (sock._id === null || sock.destroyed || sock.isPaused()) return;
		sock.read(0);
	}
	// _final runs on socket.end(): half-close the write side (send FIN) so an
	// EOF-delimited peer sees end-of-request; the read side stays open for its
	// reply. Without this, end() never sent a FIN and such peers hung.
	Socket.prototype._final = function _final(callback) {
		if (this._id !== null) ops.net_end(this._id);
		callback();
	};
	Socket.prototype._destroy = function _destroy(err, callback) {
		socketTimeoutClear(this);
		if (this._id !== null) { ops.net_close(this._id); this._id = null; }
		this.__handle = null;
		if (callback) callback(err);
	};
	Socket.prototype.destroy = function destroy(err) {
		core.stream.Duplex.prototype.destroy.call(this, err);
		return this;
	};
	Object.defineProperty(Socket.prototype, "_handle", {
		configurable: true,
		get() { return this._id === null ? null : (this.__handle ||= { fd: this._id }); },
		set(v) { if (v === null) this.__handle = null; },
	});
	Socket.prototype.setTimeout = socketSetTimeout;
	Socket.prototype.setNoDelay = function () { return this; };
	Socket.prototype.setKeepAlive = function () { return this; };
	Socket.prototype.address = function () { return { address: this.remoteAddress, port: this.remotePort, family: "IPv4" }; };
	// ref()/unref() toggle whether this socket's host pending (held while the
	// connection is open) keeps the loop alive — Node's socket.ref()/unref().
	// unref() only offsets the loop while the socket actually holds a pending
	// (connected/attached, i.e. _id !== null); the pending's own release still
	// happens host-side on close. A lazily-armed 'close' listener rebalances the
	// offset so an unref'd-then-closed socket leaves no stray loop_ref behind
	// (the same rebalance covers destroy(), which emits 'close').
	Socket.prototype.ref = function () {
		if (this._unreffed) { ops.loop_ref(true); this._unreffed = false; }
		return this;
	};
	Socket.prototype.unref = function () {
		if (!this._unreffed && this._id !== null) {
			ops.loop_ref(false);
			this._unreffed = true;
			if (!this._unrefCloseHooked) {
				this._unrefCloseHooked = true;
				this.once("close", () => { if (this._unreffed) { ops.loop_ref(true); this._unreffed = false; } });
			}
		}
		return this;
	};

	// --------------------------- server 'close' after all connections end
	// Node: server.close() stops accepting immediately, but 'close' (and the
	// callback, which Node registers as a once('close') listener) fire only
	// after every tracked server-side socket has closed. trackServerConn wires a
	// socket into that accounting; maybeEmitServerClose fires 'close' once the
	// count reaches zero after close() was requested (immediately if already 0).
	function trackServerConn(server, sock) {
		server._connections.add(sock);
		sock.once("close", () => {
			server._connections.delete(sock);
			maybeEmitServerClose(server);
		});
	}
	function maybeEmitServerClose(server) {
		if (!server._closePending || server._connections.size > 0) return;
		server._closePending = false;
		process.nextTick(() => server.emit("close")); // emit 'close' exactly once
	}

	function NetServer(options, connectionListener) {
		core.events.call(this);
		// Node: net.createServer([options][, connectionListener]) — the options
		// object is optional, so (listener) and (options, listener) are both
		// valid. Dropping the listener when options is present would silently
		// discard every connection handler.
		if (typeof options === "function") { connectionListener = options; options = undefined; }
		this._id = null;
		this.listening = false;
		this._connections = new Set();
		this._closePending = false;
		this._allowHalfOpen = !!(options && options.allowHalfOpen);
		if (connectionListener) this.on("connection", connectionListener);
	}
	Object.setPrototypeOf(NetServer.prototype, core.events.prototype);
	Object.setPrototypeOf(NetServer, core.events);
	NetServer.prototype.listen = function listen(port, host, cb) {
		if (typeof port === "object") { const o = port; cb = host; host = o.host; port = o.port; }
		if (typeof host === "function") { cb = host; host = undefined; }
		if (typeof port !== "string" || !port.startsWith("/")) port = validatePort(port);
		host = host || "127.0.0.1";
		const onConnection = (id, remote) => {
			const sock = new Socket({ allowHalfOpen: this._allowHalfOpen });
			sock._id = id;
			const at = remote.lastIndexOf(":");
			sock.remoteAddress = remote.slice(0, at);
			sock.remotePort = Number(remote.slice(at + 1));
			const onData = (chunk) => { socketTimeoutTouch(sock); sock.push(Buffer.from(chunk)); };
			const onEnd = () => endOnPeerFin(sock);
			const onError = (msg) => sock.emit("error", new Error(msg));
			ops.net_attach(id, onData, onEnd, onError);
			trackServerConn(this, sock);
			this.emit("connection", sock);
			startReading(sock);
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
	// ref()/unref() mirror http.Server: toggle whether the listen pending keeps
	// the loop alive while the server is listening. close() rebalances the offset
	// before dropping the listen pending, so an unref'd-then-closed server leaves
	// no stray loop_ref behind.
	NetServer.prototype.ref = function () {
		if (this._unreffed) { ops.loop_ref(true); this._unreffed = false; }
		return this;
	};
	NetServer.prototype.unref = function () {
		if (!this._unreffed && this._id !== null) { ops.loop_ref(false); this._unreffed = true; }
		return this;
	};
	NetServer.prototype.close = function (cb) {
		if (this._id !== null) {
			// Rebalance the loop ref accounting before dropping the listen pending.
			if (this._unreffed) { ops.loop_ref(true); this._unreffed = false; }
			ops.net_close_srv(this._id); // stop accepting NOW
			this._id = null;
			this.listening = false;
			if (cb) this.once("close", cb);
			this._closePending = true;
			maybeEmitServerClose(this); // immediate only if no live connections
		} else if (cb) {
			process.nextTick(() => cb(Object.assign(new Error("Server is not running."), { code: "ERR_SERVER_NOT_RUNNING" })));
		}
		return this;
	};

	// Happy-eyeballs settings. Address selection happens host-side (one dial per
	// connect), so these hold the configured value and report it back rather
	// than steering a race the guest does not run.
	let autoSelectFamily = true;
	let autoSelectFamilyAttemptTimeout = 250;

	// Node's constructors are ES5 functions, so `net.Server(...)` works as well
	// as `new net.Server(...)`, and its suite uses the bare form. A class throws
	// "class constructors must be invoked with 'new'". Reflect.construct with
	// new.target keeps the façade usable as a BASE CLASS too — returning a fresh
	// object from a constructor would discard the subclass instance super() was
	// initialising.
	function callableClass(Cls) {
		const f = function (...args) { return Reflect.construct(Cls, args, new.target || Cls); };
		f.prototype = Cls.prototype;
		Object.setPrototypeOf(f, Cls);
		Object.defineProperty(f, "name", { value: Cls.name, configurable: true });
		return f;
	}

	// ------------------------------------------------- SocketAddress/BlockList
	// A BlockList is an address filter: a set of single addresses, ranges and
	// subnets that check() answers against. Comparison is done on the numeric
	// form (a BigInt), because "is 1.1.1.5 between 1.1.1.1 and 1.1.1.10" is a
	// question about numbers, and string ordering answers it wrong.

	const netArgType = (name, expected, v) => Object.assign(
		new TypeError(`The "${name}" argument must be ${expected}. Received ${v === null ? "null" : typeof v}`),
		{ code: "ERR_INVALID_ARG_TYPE" });
	const netArgValue = (name, v) => Object.assign(
		new TypeError(`The argument '${name}' must be one of: 'ipv4', 'ipv6'. Received ${JSON.stringify(v)}`),
		{ code: "ERR_INVALID_ARG_VALUE" });

	// Every address is compared in ONE numeric form: the 128-bit IPv6 value,
	// with IPv4 mapped into ::ffff:a.b.c.d. That is what makes "1.1.1.2" and
	// "::ffff:1.1.1.2" compare equal — they name the same host, and a rule added
	// as either has to match a query in the other.
	function ipToBigInt(addr, family) {
		if (family === "ipv4") {
			const v4 = addr.split(".").reduce((n, o) => (n << 8n) + BigInt(Number(o)), 0n);
			return (0xffffn << 32n) | v4;
		}
		let s = addr;
		const mapped = /^::ffff:(\d+\.\d+\.\d+\.\d+)$/i.exec(s);
		if (mapped) s = "::ffff:" + ipv4ToHextets(mapped[1]);
		const [head, tail = ""] = s.split("::");
		const headParts = head ? head.split(":") : [];
		const tailParts = tail ? tail.split(":") : [];
		const fill = 8 - headParts.length - tailParts.length;
		const parts = [...headParts, ...Array(Math.max(0, fill)).fill("0"), ...tailParts];
		return parts.reduce((n, p) => (n << 16n) + BigInt(parseInt(p || "0", 16)), 0n);
	}
	const ipv4ToHextets = (v4) => {
		const o = v4.split(".").map(Number);
		return ((o[0] << 8) | o[1]).toString(16) + ":" + ((o[2] << 8) | o[3]).toString(16);
	};
	const normalizeFamily = (f, argName) => {
		if (f === undefined) return "ipv4";
		if (typeof f !== "string") throw netArgType(argName, "of type string", f);
		const lower = f.toLowerCase();
		if (lower !== "ipv4" && lower !== "ipv6") throw netArgValue(argName, f);
		return lower;
	};

	class SocketAddress {
		constructor(options = {}) {
			if (options === null || typeof options !== "object") throw netArgType("options", "of type object", options);
			this.family = normalizeFamily(options.family, "options.family");
			this.address = options.address ?? (this.family === "ipv4" ? "127.0.0.1" : "::");
			if (typeof this.address !== "string") throw netArgType("options.address", "of type string", options.address);
			this.port = options.port ?? 0;
			this.flowlabel = options.flowlabel ?? 0;
		}
		static parse(input) {
			const m6 = /^\[([^\]]+)\](?::(\d+))?$/.exec(String(input));
			if (m6) return new SocketAddress({ address: m6[1], family: "ipv6", port: m6[2] ? Number(m6[2]) : 0 });
			const [addr, port] = String(input).split(":");
			return isIPv4(addr) ? new SocketAddress({ address: addr, port: port ? Number(port) : 0 }) : undefined;
		}
	}

	// Format the 128-bit value back as an address, compressing the longest run
	// of zero hextets, so a subnet reads as "8592:757c:efae:4e45::/64" rather
	// than as a number.
	function bigIntToIP(n, family) {
		if (family === "ipv4") {
			const v = n & 0xffffffffn;
			return [24n, 16n, 8n, 0n].map((sh) => Number((v >> sh) & 0xffn)).join(".");
		}
		const parts = [];
		for (let i = 7n; i >= 0n; i--) parts.push(Number((n >> (i * 16n)) & 0xffffn).toString(16));
		let best = { at: -1, len: 0 }, run = { at: -1, len: 0 };
		parts.forEach((p, i) => {
			if (p === "0") {
				if (run.at < 0) run = { at: i, len: 0 };
				run.len++;
				if (run.len > best.len) best = { ...run };
			} else run = { at: -1, len: 0 };
		});
		if (best.len < 2) return parts.join(":");
		return parts.slice(0, best.at).join(":") + "::" + parts.slice(best.at + best.len).join(":");
	}
	const familyLabel = (f) => (f === "ipv4" ? "IPv4" : "IPv6");

	class BlockList {
		constructor() { this._rules = []; }
		// Node presents the rules as human-readable strings, newest first — it is
		// a description of the list, not its internal form.
		get rules() { return this._rules.map((r) => r.text).reverse(); }
		static isBlockList(v) { return v instanceof BlockList; }
		// The rule strings ARE the serialized form, so a list round-trips through
		// JSON without a private format.
		toJSON() { return this.rules; }
		fromJSON(json) {
			let list = json;
			if (typeof list === "string") {
				try { list = JSON.parse(list); } catch { list = undefined; }
			}
			if (!Array.isArray(list) || list.some((r) => typeof r !== "string")) {
				throw netArgType("json", "an array of strings or a JSON string", json);
			}
			for (const rule of list) this._fromRule(rule);
			return this;
		}
		// A rule that does not parse is ignored rather than fatal: the list is a
		// filter, and a line nobody can interpret filters nothing.
		_fromRule(rule) {
			let m = /^Address: (IPv4|IPv6) (.+)$/.exec(rule);
			if (m) { try { this.addAddress(m[2], m[1].toLowerCase()); } catch { /* ignored */ } return; }
			m = /^Range: (IPv4|IPv6) (.+)-(.+)$/.exec(rule);
			if (m) { try { this.addRange(m[2], m[3], m[1].toLowerCase()); } catch { /* ignored */ } return; }
			m = /^Subnet: (IPv4|IPv6) (.+)\/(\d+)$/.exec(rule);
			if (m) { try { this.addSubnet(m[2], Number(m[3]), m[1].toLowerCase()); } catch { /* ignored */ } }
		}
		// An address argument is either a SocketAddress or a string plus family.
		_addr(value, family, argName) {
			if (value instanceof SocketAddress) return { addr: value.address, family: value.family };
			if (typeof value !== "string") throw netArgType(argName, "a string or an instance of SocketAddress", value);
			return { addr: value, family: normalizeFamily(family, "family") };
		}
		addAddress(value, family) {
			const { addr, family: f } = this._addr(value, family, "address");
			this._rules.push({ kind: "address", family: f, n: ipToBigInt(addr, f), text: `Address: ${familyLabel(f)} ${addr}` });
			return this;
		}
		addRange(start, end, family) {
			const a = this._addr(start, family, "start");
			const b = this._addr(end, family, "end");
			// An inverted range would match nothing, which is never what the
			// caller meant, so Node names it rather than silently accepting it.
			if (ipToBigInt(a.addr, a.family) > ipToBigInt(b.addr, b.family)) {
				throw Object.assign(new TypeError(`The argument 'start' must be less than or equal to 'end'. Received ${JSON.stringify(a.addr)}`),
					{ code: "ERR_INVALID_ARG_VALUE" });
			}
			this._rules.push({ kind: "range", family: a.family, lo: ipToBigInt(a.addr, a.family), hi: ipToBigInt(b.addr, b.family),
				text: `Range: ${familyLabel(a.family)} ${a.addr}-${b.addr}` });
			return this;
		}
		addSubnet(net, prefix, family) {
			const a = this._addr(net, family, "net");
			if (typeof prefix !== "number") throw netArgType("prefix", "of type number", prefix);
			const width = a.family === "ipv4" ? 32 : 128;
			if (!Number.isInteger(prefix) || prefix < 0 || prefix > width) {
				throw Object.assign(new RangeError(`The value of "prefix" is out of range. It must be >= 0 and <= ${width}. Received ${prefix}`),
					{ code: "ERR_OUT_OF_RANGE" });
			}
			// The prefix counts from the address's OWN width, but the comparison
			// happens in the 128-bit space, so an IPv4 prefix leaves the mapped
			// ::ffff: header intact.
			const hostBits = BigInt((a.family === "ipv4" ? 32 : 128) - prefix);
			const base = ipToBigInt(a.addr, a.family);
			const span = (1n << hostBits) - 1n;
			const lo = base & ~span & ((1n << 128n) - 1n);
			this._rules.push({ kind: "range", family: a.family, lo, hi: lo | span,
				text: `Subnet: ${familyLabel(a.family)} ${bigIntToIP(lo, a.family)}/${prefix}` });
			return this;
		}
		check(value, family) {
			// A malformed FAMILY is an argument error; an address that simply does
			// not parse is just an address the list does not contain.
			const { addr, family: f } = this._addr(value, family, "address");
			if ((f === "ipv4" && !isIPv4(addr)) || (f === "ipv6" && !isIPv6(addr))) return false;
			const n = ipToBigInt(addr, f);
			for (const r of this._rules) {
				if (r.kind === "address" ? n === r.n : n >= r.lo && n <= r.hi) return true;
			}
			return false;
		}
	}

	core.net = {
		isIPv4,
		isIPv6,
		isIP: (s) => (isIPv4(s) ? 4 : isIPv6(s) ? 6 : 0),
		BlockList,
		SocketAddress,
		Socket: callableClass(Socket),
		Stream: callableClass(Socket),
		Server: callableClass(NetServer),
		createServer: (options, listener) => new NetServer(options, listener),
		createConnection: (...args) => new Socket().connect(...args),
		connect: (...args) => new Socket().connect(...args),
		getDefaultAutoSelectFamily: () => autoSelectFamily,
		setDefaultAutoSelectFamily: (v) => { autoSelectFamily = !!v; },
		getDefaultAutoSelectFamilyAttemptTimeout: () => autoSelectFamilyAttemptTimeout,
		setDefaultAutoSelectFamilyAttemptTimeout: (v) => {
			v = Number(v);
			if (!Number.isInteger(v) || v <= 0) {
				throw Object.assign(new RangeError(`The value of "value" is out of range. Received ${v}`), { code: "ERR_OUT_OF_RANGE" });
			}
			autoSelectFamilyAttemptTimeout = Math.max(10, v);
		},
	};

	// --------------------------------------------------------------- dgram
	// UDP sockets over the udp_* host ops.

	// A port is a 16-bit number. Accepting anything else and handing it to the
	// host produced "address 999999: invalid port" from deep inside the dial,
	// with no indication of which argument the caller got wrong.
	function validatePort(port, name = "port") {
		if (port === undefined || port === null) return 0;
		const n = typeof port === "number" ? port : Number(port);
		if (typeof port === "boolean" || Number.isNaN(n) || !Number.isInteger(n) || n < 0 || n > 65535) {
			throw Object.assign(new RangeError(`${name} should be >= 0 and < 65536. Received type ${typeof port} (${port}).`),
				{ code: "ERR_SOCKET_BAD_PORT" });
		}
		return n;
	}

	function Dgram(type) {
		// udp4 and udp6 are the only socket types there are; anything else names
		// a protocol this cannot open, and finding that out at bind time is too
		// late to be useful.
		const t = typeof type === "object" && type !== null ? type.type : type;
		if (t !== undefined && t !== "udp4" && t !== "udp6") {
			throw Object.assign(new TypeError(`Bad socket type specified. Valid types are: udp4, udp6`),
				{ code: "ERR_SOCKET_BAD_TYPE" });
		}
		core.events.call(this);
		this._id = null;
		this._remote = null; // set by connect(): the default send destination
		this.type = type || "udp4";
	}
	Object.setPrototypeOf(Dgram.prototype, core.events.prototype);
	Object.setPrototypeOf(Dgram, core.events);
	// _ensureBound opens an ephemeral socket if none is bound yet. Node's send()
	// and connect() implicitly bind to a random port when called on an unbound
	// socket; doing the bind synchronously here means _id is set before we send.
	Dgram.prototype._ensureBound = function () {
		if (this._id === null || this._id === undefined) this.bind();
		return this._id !== null && this._id !== undefined;
	};
	Dgram.prototype.bind = function bind(port, address, cb) {
		if (typeof port === "object" && port !== null) { const o = port; cb = address; address = o.address; port = o.port; }
		if (typeof address === "function") { cb = address; address = undefined; }
		if (typeof port === "function") { cb = port; port = undefined; }
		port = validatePort(port);
		const onMessage = (data, rinfo) => this.emit("message", Buffer.from(data), rinfo);
		// The socket TYPE goes to the host: a udp4 socket must resolve and bind in
		// the IPv4 family. Without it "localhost" resolved to ::1 for a udp4
		// socket and every send failed with "non-IPv4 address" — the message never
		// arrived, so nothing closed the socket and the loop never went idle.
		const r = ops.udp_bind(String(address || ""), Number(port) || 0, onMessage, this.type || "udp4");
		if (isErr(r)) { const e = new Error(r.message); e.code = r.code; process.nextTick(() => this.emit("error", e)); return this; }
		this._id = r.id;
		this._port = r.port;
		this._address = r.address;
		this._family = r.family;
		if (cb) this.once("listening", cb);
		process.nextTick(() => this.emit("listening"));
		return this;
	};
	Dgram.prototype.send = function send(msg, ...rest) {
		// send(msg, [offset, length,] [port, address,] [callback])
		let cb;
		if (typeof rest[rest.length - 1] === "function") cb = rest.pop();
		// Node decides the overload by TYPE, not by how many arguments are left:
		// a leading pair of numbers is (offset, length). Counting arguments made
		// send(buf, 0, 0, cb) on a CONNECTED socket read the offset and length as
		// the port and address, so the slice was never taken and the callback
		// never ran — every connected-send test hung on it.
		let offset, length;
		if (!Array.isArray(msg) && rest.length >= 2 &&
			typeof rest[0] === "number" && typeof rest[1] === "number") {
			offset = rest.shift();
			length = rest.shift();
		}
		let port = rest[0], address = rest[1];
		// A connected socket sends to its default destination; passing an explicit
		// address on a connected socket is an error in Node.
		if (this._remote) {
			if (port !== undefined && address !== undefined) {
				const e = Object.assign(new Error("Already connected"), { code: "ERR_SOCKET_DGRAM_IS_CONNECTED" });
				if (cb) { process.nextTick(() => cb(e)); return this; }
				throw e;
			}
			port = this._remote.port; address = this._remote.address;
		}
		// Node accepts a LIST of message parts and sends their concatenation as one
		// datagram (send(['foo','bar'], …) is one "foobar" packet). Buffer.from of
		// an array of strings produced an empty buffer instead, so the datagram
		// arrived with no payload.
		let buf = Array.isArray(msg)
			? Buffer.concat(msg.map((part) => Buffer.from(part)))
			: Buffer.from(msg);
		// The (msg, offset, length, ...) overload sends only that slice.
		if (offset !== undefined) {
			const o = offset || 0;
			buf = buf.subarray(o, o + (length ?? buf.length - o));
		}
		// Node implicitly binds an unbound socket to an ephemeral port before the
		// first send rather than failing.
		this._ensureBound();
		// Async: the host resolves+sends off the loop and invokes our callback with
		// an error object (or nothing) — so a hostname destination's DNS lookup
		// never blocks the event loop. On the normal path we report the result
		// ONLY through the callback; a bare 'error' emit (no callback) matches Node
		// for send failures with no cb, but a successful send emits nothing.
		const self = this;
		const sent = buf.length;
		ops.udp_send(this._id, buf, Number(port), String(address || "127.0.0.1"), (err) => {
			const e = err ? Object.assign(new Error(err.message), { code: err.code }) : null;
			// Node's send callback is (error, bytes); omitting the count made
			// every test that asserts on it fail on `undefined`.
			if (cb) cb(e, e ? 0 : sent);
			else if (e) self.emit("error", e);
		});
		return this;
	};
	// connect() associates a default destination so subsequent send(msg[, cb])
	// needs no address. Implemented guest-side over the same datagram write path
	// (no kernel connect), which is transparent for the common request/response
	// use of connected UDP.
	Dgram.prototype.connect = function connect(port, address, cb) {
		if (typeof address === "function") { cb = address; address = undefined; }
		this._ensureBound();
		this._remote = { port: Number(port), address: String(address || "127.0.0.1"), family: this.type === "udp6" ? "IPv6" : "IPv4" };
		if (cb) this.once("connect", cb);
		process.nextTick(() => this.emit("connect"));
		return this;
	};
	Dgram.prototype.disconnect = function disconnect() {
		if (!this._remote) throw Object.assign(new Error("Not connected"), { code: "ERR_SOCKET_DGRAM_NOT_CONNECTED" });
		this._remote = null;
		return this;
	};
	Dgram.prototype.remoteAddress = function remoteAddress() {
		if (!this._remote) throw Object.assign(new Error("Not connected"), { code: "ERR_SOCKET_DGRAM_NOT_CONNECTED" });
		return { address: this._remote.address, port: this._remote.port, family: this._remote.family };
	};
	Dgram.prototype.address = function () {
		if (this._id === null || this._id === undefined) throw Object.assign(new Error("Not running"), { code: "ERR_SOCKET_DGRAM_NOT_RUNNING" });
		return { address: this._address || "127.0.0.1", port: this._port, family: this._family || "IPv4" };
	};
	Dgram.prototype.close = function (cb) {
		if (this._id !== null && this._id !== undefined) {
			ops.udp_close(this._id); this._id = null; this._remote = null;
			if (cb) process.nextTick(cb);
			process.nextTick(() => this.emit("close")); // emit 'close' exactly once
		} else if (cb) {
			process.nextTick(() => cb(Object.assign(new Error("Not running"), { code: "ERR_SOCKET_DGRAM_NOT_RUNNING" })));
		}
		return this;
	};
	// Socket-option and multicast methods. The host UDP bridge does not expose
	// the underlying fd, so TTL/multicast controls can't be plumbed to the
	// kernel; they are accepted as no-ops (returning this / a plausible value)
	// rather than throwing TypeError, so multicast-configuring code keeps working
	// against a unicast-only socket. Buffer-size getters report a typical default.
	Dgram.prototype.setBroadcast = function () { return this; };
	Dgram.prototype.setTTL = function (n) { return Number(n); };
	Dgram.prototype.setMulticastTTL = function (n) { return Number(n); };
	Dgram.prototype.setMulticastLoopback = function (b) { return Boolean(b); };
	Dgram.prototype.setMulticastInterface = function () { return this; };
	Dgram.prototype.addMembership = function () { return this; };
	Dgram.prototype.dropMembership = function () { return this; };
	Dgram.prototype.addSourceSpecificMembership = function () { return this; };
	Dgram.prototype.dropSourceSpecificMembership = function () { return this; };
	Dgram.prototype.setRecvBufferSize = function () { return this; };
	Dgram.prototype.setSendBufferSize = function () { return this; };
	Dgram.prototype.getRecvBufferSize = function () { return 65536; };
	Dgram.prototype.getSendBufferSize = function () { return 65536; };
	// ref()/unref() toggle whether the bound socket's host pending keeps the loop
	// alive — Node's socket.ref()/unref(). unref() only offsets while the socket
	// actually holds a pending (bound, i.e. _id set); a lazily-armed 'close'
	// listener rebalances the offset so an unref'd-then-closed socket leaves no
	// stray loop_ref behind (the host bind pending is released on close).
	Dgram.prototype.ref = function () {
		if (this._unreffed) { ops.loop_ref(true); this._unreffed = false; }
		return this;
	};
	Dgram.prototype.unref = function () {
		if (!this._unreffed && this._id != null) {
			ops.loop_ref(false);
			this._unreffed = true;
			if (!this._unrefCloseHooked) {
				this._unrefCloseHooked = true;
				this.once("close", () => { if (this._unreffed) { ops.loop_ref(true); this._unreffed = false; } });
			}
		}
		return this;
	};
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
	// The *Sync/callback forms are one-shot transforms over the zlib_transform
	// host op. The stream forms are INCREMENTAL: each chunk feeds a live host
	// compressor/decompressor (zlib_stream_* ops), so output appears as input
	// arrives and .flush() (Z_SYNC_FLUSH) makes everything written so far
	// decodable mid-stream — what Express's compression middleware needs for SSE.

	function zlibRun(method, data) {
		const r = ops.zlib_transform(method, toBuf(data));
		ops.release_pending();
		if (isErr(r)) { const e = new Error(r.message); e.code = r.code; throw e; }
		return Buffer.from(r);
	}
	// zlib checks its options before it compresses anything, and its own suite
	// asserts on the codes. Accepting a chunkSize of 0 or a level of 42 and then
	// ignoring them is not leniency — it silently builds a stream to settings
	// the caller never asked for.
	const Z_MIN_CHUNK = 64;
	const zlibRange = (name, min, max, v) => Object.assign(
		new RangeError(`The value of "options.${name}" is out of range. It must be >= ${min} and <= ${max}. Received ${v}`),
		{ code: "ERR_OUT_OF_RANGE" });
	function checkZlibOption(opts, name, min, max) {
		const v = opts[name];
		if (v === undefined || v === null) return;
		if (typeof v !== "number") {
			throw Object.assign(new TypeError(`The "options.${name}" property must be of type number. Received ${typeof v}`),
				{ code: "ERR_INVALID_ARG_TYPE" });
		}
		if (!Number.isInteger(v) || v < min || v > max) throw zlibRange(name, min, max, v);
	}
	function validateZlibOptions(opts) {
		if (opts === undefined || opts === null) return {};
		if (typeof opts !== "object") {
			throw Object.assign(new TypeError(`The "options" argument must be of type object. Received ${typeof opts}`),
				{ code: "ERR_INVALID_ARG_TYPE" });
		}
		checkZlibOption(opts, "chunkSize", Z_MIN_CHUNK, 0x7fffffff);
		checkZlibOption(opts, "level", -1, 9);
		checkZlibOption(opts, "windowBits", 8, 15);
		checkZlibOption(opts, "memLevel", 1, 9);
		checkZlibOption(opts, "strategy", 0, 4);
		checkZlibOption(opts, "maxOutputLength", 0, Number.MAX_SAFE_INTEGER);
		return opts;
	}
	// A decompression that would exceed maxOutputLength fails rather than
	// allocating whatever the compressed input claims it expands to.
	const capZlibOutput = (buf, opts) => {
		const max = opts && opts.maxOutputLength;
		if (typeof max === "number" && buf.length > max) {
			throw Object.assign(new RangeError(`Cannot create a Buffer larger than ${max} bytes`),
				{ code: "ERR_BUFFER_TOO_LARGE" });
		}
		return buf;
	};
	const zlibSync = (method) => (data, opts) => capZlibOutput(zlibRun(method, data), validateZlibOptions(opts));
	const zlibAsync = (method) => (data, opts, cb) => {
		if (typeof opts === "function") { cb = opts; opts = undefined; }
		if (typeof cb !== "function") {
			throw Object.assign(new TypeError('The "callback" argument must be of type function.'),
				{ code: "ERR_INVALID_ARG_TYPE" });
		}
		validateZlibOptions(opts);
		queueMicrotask(() => { try { cb(null, capZlibOutput(zlibRun(method, data), opts)); } catch (e) { cb(e); } });
	};
	function zlibStream(method) {
		let id = null;
		let ended = false;
		const ensure = () => {
			if (id === null) {
				const r = ops.zlib_stream_new(method);
				if (isErr(r)) { const e = new Error(r.message); e.code = r.code; throw e; }
				id = r;
			}
			return id;
		};
		const pushOut = (ts, r) => {
			ops.release_pending();
			if (isErr(r)) { const e = new Error(r.message); e.code = r.code; throw e; }
			if (r && r.length) ts.push(Buffer.from(r));
		};
		const ts = new core.stream.Transform({
			transform(chunk, enc, callback) {
				try { pushOut(this, ops.zlib_stream_write(ensure(), toBuf(chunk, enc))); callback(); }
				catch (e) { callback(e); }
			},
			flush(callback) { // end-of-stream finalization (writer Close)
				try {
					ended = true;
					const r = ops.zlib_stream_end(ensure());
					id = null; // the host freed it
					pushOut(this, r);
					callback();
				} catch (e) { id = null; callback(e); }
			},
			destroy(err, cb) {
				if (id !== null) { ops.zlib_stream_free(id); id = null; }
				cb(err);
			},
		});
		// Public zlib .flush([kind,] cb): emit all output the data written so
		// far can produce (Z_SYNC_FLUSH semantics; the kind argument is
		// accepted but sync flush is always what the host performs). Writes are
		// transformed synchronously in this runtime, so everything written
		// before flush() has already reached the host stream.
		ts.flush = function (kind, cb) {
			if (typeof kind === "function") { cb = kind; kind = undefined; }
			const done = () => { if (typeof cb === "function") process.nextTick(cb); };
			if (ended || this.destroyed || this.writableEnded) return done();
			try { pushOut(this, ops.zlib_stream_flush(ensure())); }
			catch (e) { this.destroy(e); }
			done();
		};
		return ts;
	}
	core.zlib = {
		gzipSync: zlibSync("gzip"),
		gunzipSync: zlibSync("gunzip"),
		deflateSync: zlibSync("deflate"),
		inflateSync: zlibSync("inflate"),
		deflateRawSync: zlibSync("deflateRaw"),
		inflateRawSync: zlibSync("inflateRaw"),
		unzipSync: zlibSync("unzip"),
		brotliCompressSync: zlibSync("brotliCompress"),
		brotliDecompressSync: zlibSync("brotliDecompress"),
		gzip: zlibAsync("gzip"),
		gunzip: zlibAsync("gunzip"),
		deflate: zlibAsync("deflate"),
		inflate: zlibAsync("inflate"),
		deflateRaw: zlibAsync("deflateRaw"),
		inflateRaw: zlibAsync("inflateRaw"),
		unzip: zlibAsync("unzip"),
		brotliCompress: zlibAsync("brotliCompress"),
		brotliDecompress: zlibAsync("brotliDecompress"),
		createGzip: (o) => (validateZlibOptions(o), zlibStream("gzip")),
		createGunzip: (o) => (validateZlibOptions(o), zlibStream("gunzip")),
		createDeflate: (o) => (validateZlibOptions(o), zlibStream("deflate")),
		createInflate: (o) => (validateZlibOptions(o), zlibStream("inflate")),
		createDeflateRaw: (o) => (validateZlibOptions(o), zlibStream("deflateRaw")),
		createInflateRaw: (o) => (validateZlibOptions(o), zlibStream("inflateRaw")),
		createUnzip: (o) => (validateZlibOptions(o), zlibStream("unzip")),
		createBrotliCompress: (o) => (validateZlibOptions(o), zlibStream("brotliCompress")),
		createBrotliDecompress: (o) => (validateZlibOptions(o), zlibStream("brotliDecompress")),
		// crc32 over the standard IEEE polynomial. Node exposes it because the
		// checksum is part of the gzip container callers assemble by hand.
		crc32: (data, value = 0) => {
			if (typeof data !== "string" && !ArrayBuffer.isView(data)) {
				throw Object.assign(new TypeError('The "data" argument must be of type string or an instance of Buffer, TypedArray, or DataView.'),
					{ code: "ERR_INVALID_ARG_TYPE" });
			}
			const bytes = typeof data === "string" ? Buffer.from(data, "utf8") : data;
			let crc = (~value) >>> 0;
			for (let i = 0; i < bytes.length; i++) {
				crc ^= bytes[i];
				for (let b = 0; b < 8; b++) crc = (crc >>> 1) ^ (0xedb88320 & -(crc & 1));
			}
			return (~crc) >>> 0;
		},
		constants: {
			Z_NO_FLUSH: 0, Z_PARTIAL_FLUSH: 1, Z_SYNC_FLUSH: 2, Z_FULL_FLUSH: 3,
			Z_FINISH: 4, Z_BLOCK: 5, Z_TREES: 6,
			Z_OK: 0, Z_STREAM_END: 1, Z_NEED_DICT: 2, Z_ERRNO: -1, Z_STREAM_ERROR: -2,
			Z_DATA_ERROR: -3, Z_MEM_ERROR: -4, Z_BUF_ERROR: -5, Z_VERSION_ERROR: -6,
			Z_NO_COMPRESSION: 0, Z_BEST_SPEED: 1, Z_BEST_COMPRESSION: 9,
			Z_DEFAULT_COMPRESSION: -1,
			Z_FILTERED: 1, Z_HUFFMAN_ONLY: 2, Z_RLE: 3, Z_FIXED: 4, Z_DEFAULT_STRATEGY: 0,
			Z_BINARY: 0, Z_TEXT: 1, Z_ASCII: 1, Z_UNKNOWN: 2, Z_DEFLATED: 8,
			Z_MIN_WINDOWBITS: 8, Z_MAX_WINDOWBITS: 15, Z_DEFAULT_WINDOWBITS: 15,
			Z_MIN_CHUNK: 64, Z_MAX_CHUNK: Infinity, Z_DEFAULT_CHUNK: 16 * 1024,
			Z_MIN_MEMLEVEL: 1, Z_MAX_MEMLEVEL: 9, Z_DEFAULT_MEMLEVEL: 8,
			Z_MIN_LEVEL: -1, Z_MAX_LEVEL: 9, Z_DEFAULT_LEVEL: -1,
			BROTLI_OPERATION_PROCESS: 0, BROTLI_OPERATION_FLUSH: 1,
			BROTLI_OPERATION_FINISH: 2, BROTLI_OPERATION_EMIT_METADATA: 3,
			BROTLI_MODE_GENERIC: 0, BROTLI_MODE_TEXT: 1, BROTLI_MODE_FONT: 2,
			BROTLI_DEFAULT_MODE: 0, BROTLI_DEFAULT_QUALITY: 11, BROTLI_MAX_QUALITY: 11,
			BROTLI_MIN_QUALITY: 0, BROTLI_DEFAULT_WINDOW: 22, BROTLI_MAX_WINDOW_BITS: 24,
			BROTLI_MIN_WINDOW_BITS: 10,
			BROTLI_PARAM_MODE: 0, BROTLI_PARAM_QUALITY: 1, BROTLI_PARAM_LGWIN: 2,
			BROTLI_PARAM_LGBLOCK: 3, BROTLI_PARAM_SIZE_HINT: 5,
		},
	};
	// Each transform is a CONSTRUCTOR as well as a factory, and published code
	// reaches for both — `new zlib.Gzip()` and `instanceof zlib.BrotliDecompress`
	// alike.
	for (const [name, method] of [
		["Gzip", "gzip"], ["Gunzip", "gunzip"], ["Deflate", "deflate"],
		["Inflate", "inflate"], ["DeflateRaw", "deflateRaw"], ["InflateRaw", "inflateRaw"],
		["Unzip", "unzip"], ["BrotliCompress", "brotliCompress"],
		["BrotliDecompress", "brotliDecompress"],
	]) {
		const ctor = function (options) { validateZlibOptions(options); return zlibStream(method); };
		Object.defineProperty(ctor, "name", { value: name });
		core.zlib[name] = ctor;
	}

	// --------------------------------------------------------- async_hooks
	// AsyncLocalStorage without engine async-context tracking: the store is
	// a plain slot held for the duration of run() — including, when fn is
	// async, until its promise settles. Correct for the serialized
	// one-request-at-a-time execution this runtime does; NOT correct for
	// interleaved concurrent contexts.

	// All live AsyncLocalStorage instances, so a context snapshot can capture
	// every store at once (the basis for AsyncResource propagation).
	const allStores = new Set();
	function snapshotStores() {
		const snap = new Map();
		for (const als of allStores) snap.set(als, als._store);
		return snap;
	}
	function withSnapshot(snap, fn, thisArg, args) {
		const prev = snapshotStores();
		for (const [als, v] of snap) als._store = v;
		try { return fn.apply(thisArg, args); }
		finally { for (const [als, v] of prev) als._store = v; }
	}

	class AsyncLocalStorage {
		constructor() { this._store = undefined; allStores.add(this); }
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
		disable() { this._store = undefined; allStores.delete(this); }
		static bind(fn) { const snap = snapshotStores(); return (...a) => withSnapshot(snap, fn, this, a); }
		static snapshot() { const snap = snapshotStores(); return (fn, ...a) => withSnapshot(snap, fn, undefined, a); }
	}
	// A snapshot-carrying resource: bind() captures the current stores so the
	// callback later runs under them (correct explicit context propagation,
	// the pattern APM/tracing libraries use). Bare await interleaving without
	// AsyncResource still cannot be tracked without engine async-context hooks.
	class AsyncResource {
		constructor(type) { this.type = type; this._snap = snapshotStores(); }
		runInAsyncScope(fn, thisArg, ...args) { return withSnapshot(this._snap, fn, thisArg, args); }
		emitDestroy() { return this; }
		bind(fn) { const snap = this._snap; return (...a) => withSnapshot(snap, fn, this, a); }
		asyncId() { return 1; }
		static bind(fn) { const snap = snapshotStores(); return (...a) => withSnapshot(snap, fn, undefined, a); }
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

	// The User Timing API (performance.mark/measure/getEntries*) and
	// PerformanceObserver are installed by the compat/web layer on
	// globalThis.performance / globalThis.PerformanceObserver — one shared entry
	// buffer for both layers. perf_hooks only adds the Node-specific extras that
	// the web layer has no notion of (event-loop utilization/delay).
	if (typeof globalThis.performance.eventLoopUtilization !== "function") {
		globalThis.performance.eventLoopUtilization = () => ({ idle: 0, active: 0, utilization: 0 });
	}
	core.perf_hooks = {
		performance: globalThis.performance,
		PerformanceObserver: globalThis.PerformanceObserver,
		PerformanceEntry: globalThis.PerformanceEntry,
		PerformanceObserverEntryList: globalThis.PerformanceObserverEntryList,
		constants: {},
		monitorEventLoopDelay: () => ({ enable() {}, disable() {}, reset() {}, mean: 0, percentile: () => 0 }),
	};

	// ------------------------------------- small stubs the loaders require

	// Real RFC 3492 punycode + IDNA toASCII/toUnicode.
	const puny = (() => {
		const base = 36, tMin = 1, tMax = 26, skew = 38, damp = 700, initialBias = 72, initialN = 128, delimiter = "-";
		const adapt = (delta, numPoints, firstTime) => {
			delta = firstTime ? Math.floor(delta / damp) : delta >> 1;
			delta += Math.floor(delta / numPoints);
			let k = 0;
			for (; delta > ((base - tMin) * tMax) >> 1; k += base) delta = Math.floor(delta / (base - tMin));
			return Math.floor(k + ((base - tMin + 1) * delta) / (delta + skew));
		};
		const ucs2decode = (s) => [...s].map((c) => c.codePointAt(0));
		const digitToBasic = (d) => d + 22 + 75 * (d < 26 ? 1 : 0);
		const basicToDigit = (cp) => {
			if (cp - 48 < 10) return cp - 22;
			if (cp - 65 < 26) return cp - 65;
			if (cp - 97 < 26) return cp - 97;
			return base;
		};
		function encode(input) {
			const cps = ucs2decode(input);
			const output = [];
			let n = initialN, delta = 0, bias = initialBias;
			for (const cp of cps) if (cp < 0x80) output.push(String.fromCharCode(cp));
			let basicLength = output.length, handled = basicLength;
			if (basicLength) output.push(delimiter);
			while (handled < cps.length) {
				let m = Infinity;
				for (const cp of cps) if (cp >= n && cp < m) m = cp;
				delta += (m - n) * (handled + 1);
				n = m;
				for (const cp of cps) {
					if (cp < n) delta++;
					if (cp === n) {
						let q = delta;
						for (let k = base; ; k += base) {
							const t = k <= bias ? tMin : k >= bias + tMax ? tMax : k - bias;
							if (q < t) break;
							output.push(String.fromCharCode(digitToBasic(t + ((q - t) % (base - t)))));
							q = Math.floor((q - t) / (base - t));
						}
						output.push(String.fromCharCode(digitToBasic(q)));
						bias = adapt(delta, handled + 1, handled === basicLength);
						delta = 0;
						handled++;
					}
				}
				delta++;
				n++;
			}
			return output.join("");
		}
		function decode(input) {
			const output = [];
			let n = initialN, i = 0, bias = initialBias;
			let basic = input.lastIndexOf(delimiter);
			if (basic < 0) basic = 0;
			for (let j = 0; j < basic; j++) output.push(input.charCodeAt(j));
			for (let index = basic > 0 ? basic + 1 : 0; index < input.length;) {
				let oldi = i;
				for (let w = 1, k = base; ; k += base) {
					const digit = basicToDigit(input.charCodeAt(index++));
					i += digit * w;
					const t = k <= bias ? tMin : k >= bias + tMax ? tMax : k - bias;
					if (digit < t) break;
					w *= base - t;
				}
				const out = output.length + 1;
				bias = adapt(i - oldi, out, oldi === 0);
				n += Math.floor(i / out);
				i %= out;
				output.splice(i++, 0, n);
			}
			return String.fromCodePoint(...output);
		}
		const toASCII = (domain) => String(domain).split(".").map((l) => /[^\x00-\x7f]/.test(l) ? "xn--" + encode(l) : l).join(".");
		const toUnicode = (domain) => String(domain).split(".").map((l) => l.startsWith("xn--") ? decode(l.slice(4)) : l).join(".");
		return { encode, decode, toASCII, toUnicode };
	})();
	core.punycode = {
		version: "2.3.1",
		encode: puny.encode,
		decode: puny.decode,
		toASCII: puny.toASCII,
		toUnicode: puny.toUnicode,
		ucs2: {
			encode: (arr) => String.fromCodePoint(...arr),
			decode: (s) => [...String(s)].map((c) => c.codePointAt(0)),
		},
	};
	globalThis.__node_punycode = core.punycode;

	// vm without true realm isolation: runInNewContext/runInContext run the
	// code with globalThis/self/global bound to the supplied sandbox, so code
	// that assigns to globals (e.g. Next's App Router manifest files, which do
	// `globalThis.__RSC_MANIFEST = ...`) writes into the sandbox and Next reads
	// it back. This is NOT a security boundary — the code can still reach the
	// real global through other means — but it makes the common
	// eval-a-manifest / evaluate-config pattern work.
	function runInSandbox(code, sandbox) {
		const ctx = sandbox || {};
		// `with (this)` routes free identifier reads/writes to the sandbox, and
		// the direct eval returns the code's COMPLETION VALUE — so
		// runInNewContext("1+1") yields 2, not the sandbox object.
		// globalThis/self/global are also bound to the sandbox so an explicit
		// `globalThis.x = ...` (e.g. Next's App Router manifest) lands there.
		// Caveat: a top-level `var`/`function` DECLARATION binds to this wrapper,
		// not the sandbox — true per-context binding needs a realm we don't have.
		// The source param name is deliberately obscure so a sandbox property
		// can't shadow it through `with (this)` and substitute the code.
		const runner = new Function(
			"globalThis", "self", "global", "exports", "module", "__sm$vmSrc__",
			"with (this) { return eval(__sm$vmSrc__); }",
		);
		return runner.call(ctx, ctx, ctx, ctx, ctx.exports, ctx.module, String(code));
	}
	// vm rejects its arguments before compiling anything, and its own suite
	// asserts on the codes. A string handed to createContext is a mistake worth
	// naming, not something to treat as an empty sandbox.
	const vmArgType = (name, expected, v) => Object.assign(
		new TypeError(`The "${name}" argument must be ${expected}. Received ${v === null ? "null" : typeof v}`),
		{ code: "ERR_INVALID_ARG_TYPE" });
	const vmRange = (name, v) => Object.assign(
		new RangeError(`The value of "${name}" is out of range. It must be an integer. Received ${v}`),
		{ code: "ERR_OUT_OF_RANGE" });
	function vmOffset(opts, name) {
		const v = opts[name];
		if (v === undefined) return;
		if (typeof v !== "number") throw vmArgType(`options.${name}`, "of type number", v);
		if (!Number.isInteger(v) || v < -(2 ** 31) || v > 2 ** 31 - 1) throw vmRange(`options.${name}`, v);
	}
	function validateVMOptions(options, argName = "options") {
		if (options === undefined || options === null) return {};
		if (typeof options === "string") return { filename: options };
		if (typeof options !== "object") throw vmArgType(argName, "of type object", options);
		if (options.filename !== undefined && typeof options.filename !== "string") {
			throw vmArgType("options.filename", "of type string", options.filename);
		}
		vmOffset(options, "lineOffset");
		vmOffset(options, "columnOffset");
		if (options.timeout !== undefined) {
			if (typeof options.timeout !== "number") throw vmArgType("options.timeout", "of type number", options.timeout);
			if (!Number.isInteger(options.timeout) || options.timeout <= 0) {
				throw Object.assign(new RangeError(`The value of "options.timeout" is out of range. It must be a positive integer. Received ${options.timeout}`),
					{ code: "ERR_OUT_OF_RANGE" });
			}
		}
		return options;
	}
	// Which objects have been made into contexts. Node answers isContext() from
	// the object itself; a flat "false" made every round-trip check fail.
	const contextified = new WeakSet();
	const asContext = (o) => {
		if (o === undefined) o = {};
		if (o === null || (typeof o !== "object" && typeof o !== "function")) {
			throw vmArgType("contextObject", "of type object", o);
		}
		contextified.add(o);
		return o;
	};
	core.vm = {
		createContext: (o, options) => { validateVMOptions(options); return asContext(o); },
		isContext: (o) => {
			if (o === null || (typeof o !== "object" && typeof o !== "function")) {
				throw vmArgType("object", "of type object", o);
			}
			return contextified.has(o);
		},
		runInThisContext: (code, options) => { validateVMOptions(options); return (0, eval)(String(code)); },
		runInNewContext: (code, sandbox, options) => { validateVMOptions(options); return runInSandbox(code, asContext(sandbox)); },
		runInContext: (code, contextifiedObject, options) => { validateVMOptions(options); return runInSandbox(code, contextifiedObject); },
		compileFunction: (code, params = [], options) => { validateVMOptions(options); return new Function(...params, String(code)); },
		Script: class Script {
			constructor(code, options) {
				validateVMOptions(options);
				this._code = String(code);
			}
			runInThisContext(options) { validateVMOptions(options); return (0, eval)(this._code); }
			runInNewContext(sandbox, options) { validateVMOptions(options); return runInSandbox(this._code, asContext(sandbox)); }
			runInContext(contextifiedObject, options) { validateVMOptions(options); return runInSandbox(this._code, contextifiedObject); }
		},
		constants: {
			// The DONT_CONTEXTIFY sentinel and the import-module-dynamically modes
			// callers pass through to the options bag.
			DONT_CONTEXTIFY: Symbol("vm_dont_contextify"),
			USE_MAIN_CONTEXT_DEFAULT_LOADER: Symbol("vm_dynamic_import_main_context_default"),
		},
	};

	// worker_threads over the engine's agent cluster (real goroutine threads,
	// separate realms, structured-clone messaging, SharedArrayBuffer sharing).
	// In a worker realm the bootstrap (js/worker.js) sets __wt_parentPort etc.
	const inWorker = globalThis.__wt_isMainThread === false;

	// Detach every ArrayBuffer in a postMessage transfer list on the sender side,
	// using the engine's real ArrayBuffer.prototype.transfer (same primitive
	// compat/web's structuredClone uses). Called AFTER the message has been cloned,
	// so the receiver keeps the bytes while the sender's buffer.byteLength drops to
	// 0 and views over it throw — the observable Node transfer contract.
	const detachTransferList = (transferList) => {
		if (!Array.isArray(transferList)) return;
		for (const t of transferList) {
			if (t instanceof ArrayBuffer && typeof t.transfer === "function" && t.byteLength > 0) {
				try { t.transfer(); } catch { /* already detached */ }
			}
		}
	};

	// Reconstruct an Error from a worker's structured {name, message, stack} error
	// report (see js/worker.js __wt_reportError), preserving the original message,
	// name and stack — rather than collapsing it to String(value).
	const reviveWorkerError = (value) => {
		if (value instanceof Error) return value;
		if (value && typeof value === "object" && ("message" in value || "name" in value)) {
			const err = new Error(value.message != null ? String(value.message) : "");
			if (value.name != null) err.name = String(value.name);
			if (value.stack != null) err.stack = String(value.stack);
			return err;
		}
		return new Error(String(value));
	};

	// The sentinel that asks a worker to share the parent's environment rather
	// than be given one; declared here because the Worker constructor checks
	// against it.
	const SHARE_ENV = Symbol.for("nodejs.worker_threads.SHARE_ENV");

	class Worker extends core.events {
		constructor(filename, options = {}) {
			super();
			// Every one of these reached the filesystem and came back as ENOENT,
			// which says nothing about which argument was wrong — a Worker built
			// with `{ env: 1 }` is not a missing file.
			if (options === null || typeof options !== "object") {
				throw Object.assign(new TypeError(`The "options" argument must be of type object. Received ${typeof options}`),
					{ code: "ERR_INVALID_ARG_TYPE" });
			}
			if (!options.eval && typeof filename !== "string" && !(filename instanceof URL) && !(filename && filename.href)) {
				throw Object.assign(new TypeError(`The "filename" argument must be of type string or an instance of URL. Received ${filename === null ? "null" : typeof filename}`),
					{ code: "ERR_INVALID_ARG_TYPE" });
			}
			for (const [key, kind] of [["argv", "Array"], ["execArgv", "Array"], ["transferList", "Array"]]) {
				if (options[key] !== undefined && options[key] !== null && !Array.isArray(options[key])) {
					throw Object.assign(new TypeError(`The "options.${key}" property must be an instance of ${kind}. Received ${typeof options[key]}`),
						{ code: "ERR_INVALID_ARG_TYPE" });
				}
			}
			// SHARE_ENV is a sentinel, not an object of variables.
			if (options.env !== undefined && options.env !== null
				&& options.env !== SHARE_ENV && typeof options.env !== "object") {
				throw Object.assign(new TypeError(`The "options.env" property must be an instance of Object. Received ${typeof options.env}`),
					{ code: "ERR_INVALID_ARG_TYPE" });
			}
			if (options.resourceLimits !== undefined && options.resourceLimits !== null && typeof options.resourceLimits !== "object") {
				throw Object.assign(new TypeError(`The "options.resourceLimits" property must be an instance of Object. Received ${typeof options.resourceLimits}`),
					{ code: "ERR_INVALID_ARG_TYPE" });
			}
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
		postMessage(value, transferList) {
			// The op clones `value` synchronously (structured clone across the agent
			// boundary). After the clone has read the bytes, detach each transferred
			// ArrayBuffer on the sender side via the engine's real transfer — so the
			// sender's buffer.byteLength becomes 0 and any view over it throws, while
			// the receiver got a copy of the bytes. (True zero-copy hand-off across a
			// real agent boundary isn't available; this matches the observable Node
			// contract: the sender loses the buffer, the receiver gains the bytes.)
			ops.worker_post(this._id, value);
			detachTransferList(transferList);
			return this;
		}
		terminate() { ops.worker_terminate(this._id); return Promise.resolve(0); }
		ref() { return this; }
		unref() { return this; }
		// The host pump calls this to deliver an event.
		_emit(type, value) {
			if (type === "message") this.emit("message", value);
			else if (type === "error") this.emit("error", reviveWorkerError(value));
			else this.emit(type, value);
		}
		addEventListener(type, fn) { return this.on(type, (v) => fn({ data: v })); }
		removeEventListener() { return this; }
	}

	// A working same-thread MessageChannel/MessagePort pair. port1.postMessage(x)
	// structured-clones x and queues a 'message' event on port2 (and vice versa).
	// A port is usable both EventEmitter-style (port.on('message', fn) — fn gets
	// the data) and DOM-style (port.onmessage = fn / addEventListener — fn gets a
	// { data } MessageEvent). Node buffers messages until the port is started; a
	// port starts on start(), on setting onmessage, or on adding a 'message'
	// listener. close() closes both ends and emits 'close' on each.
	//
	// LIMITATION: transferring a MessagePort itself across a real worker (agent)
	// boundary is NOT supported — the pair is same-realm. In-realm MessageChannel
	// usage (the common case) works fully; posting a port through Worker.postMessage
	// will not reconstruct a live cross-thread port.
	class MessagePort extends core.events {
		constructor() {
			super();
			this._peer = null;
			this._started = false;
			this._closed = false;
			this._queue = [];
			this._onmessage = null;
			this._elisteners = { message: [], messageerror: [], close: [] };
		}
		_recv(data) {
			if (this._closed) return;
			if (this._started) this._dispatch(data);
			else this._queue.push(data);
		}
		_dispatch(data) {
			const ev = { data, type: "message", target: this };
			if (this._onmessage) { try { this._onmessage(ev); } catch (e) { this.emit("error", e); } }
			for (const fn of this._elisteners.message.slice()) { try { fn(ev); } catch (e) { this.emit("error", e); } }
			this.emit("message", data); // EventEmitter listeners get the raw data
		}
		start() {
			if (this._started || this._closed) return;
			this._started = true;
			const q = this._queue; this._queue = [];
			for (const d of q) this._dispatch(d);
		}
		postMessage(value, transferList) {
			if (this._closed) return;
			const peer = this._peer;
			if (!peer || peer._closed) return;
			// Structured-clone the payload (transfer list detaches the sources).
			const cloned = structuredClone(value, transferList ? { transfer: transferList } : undefined);
			queueMicrotask(() => peer._recv(cloned));
		}
		close() {
			if (this._closed) return;
			this._closed = true;
			queueMicrotask(() => this.emit("close"));
			const peer = this._peer;
			if (peer && !peer._closed) peer.close();
		}
		on(type, fn) { const r = super.on(type, fn); if (type === "message") this.start(); return r; }
		addListener(type, fn) { return this.on(type, fn); }
		once(type, fn) { const r = super.once(type, fn); if (type === "message") this.start(); return r; }
		addEventListener(type, fn) {
			(this._elisteners[type] ||= []).push(fn);
			if (type === "message") this.start();
			return this;
		}
		removeEventListener(type, fn) {
			const l = this._elisteners[type];
			if (l) { const i = l.indexOf(fn); if (i >= 0) l.splice(i, 1); }
			return this;
		}
		get onmessage() { return this._onmessage; }
		set onmessage(fn) { this._onmessage = fn || null; if (fn) this.start(); }
		ref() { return this; }
		unref() { return this; }
	}

	class MessageChannel {
		constructor() {
			this.port1 = new MessagePort();
			this.port2 = new MessagePort();
			this.port1._peer = this.port2;
			this.port2._peer = this.port1;
		}
	}

	core.worker_threads = {
		isMainThread: !inWorker,
		threadId: inWorker ? globalThis.__wt_threadId : 0,
		workerData: inWorker ? globalThis.__wt_workerData : null,
		parentPort: inWorker ? globalThis.__wt_parentPort : null,
		resourceLimits: {},
		SHARE_ENV,
		Worker,
		MessageChannel,
		MessagePort,
		BroadcastChannel: class BroadcastChannel {
			constructor() { notSupported("worker_threads.BroadcastChannel")(); }
		},
		markAsUntransferable: () => {},
		getEnvironmentData: () => undefined,
		setEnvironmentData: () => {},
	};

	// node:dns over the host resolver. Every lookup goes through the same
	// Config.Resolve gate the connect paths use, and a refusal or a failure
	// arrives with Node's error shape (code/syscall/hostname).
	function dnsError(info) {
		const e = new Error(info.message || "dns query failed");
		e.code = info.code;
		e.errno = info.code;
		e.syscall = info.syscall;
		e.hostname = info.hostname;
		return e;
	}
	// query runs one lookup and hands the callback (err, result).
	function dnsQuery(kind, host, cb) {
		if (typeof cb !== "function") throw new TypeError("callback is not a function");
		ops.dns_resolve(String(kind), String(host), (err, result) => {
			if (err) cb(dnsError(err), undefined);
			else cb(null, result);
		});
	}
	const RRTYPES = ["A", "AAAA", "CNAME", "MX", "NS", "PTR", "SRV", "TXT"];

	// lookup answers with a single address (Node's shape), unless `all` was
	// asked for.
	function dnsLookup(host, options, cb) {
		if (typeof options === "function") { cb = options; options = {}; }
		options = options || {};
		if (typeof options === "number") options = { family: options };
		const kind = options.family === 4 ? "A" : options.family === 6 ? "AAAA" : "lookup";
		dnsQuery(kind, host, (err, list) => {
			if (err) return cb(err);
			if (!list || list.length === 0) {
				return cb(dnsError({ code: "ENOTFOUND", syscall: "getaddrinfo", hostname: String(host) }));
			}
			if (options.all) return cb(null, list);
			cb(null, list[0].address, list[0].family);
		});
	}

	function makeResolver() {
		const self = {
			// The server list is accepted and reported back, but the host uses the
			// system resolver: Go's net.Resolver has no per-instance server list,
			// so honouring it would be a lie. Reporting what was set is what lets
			// a caller round-trip its own configuration.
			_servers: [],
			setServers(list) { self._servers = Array.from(list || []).map(String); },
			getServers() { return [...self._servers]; },
			cancel() {},
			setLocalAddress() {},
			lookup: dnsLookup,
			resolve(host, type, cb) {
				if (typeof type === "function") { cb = type; type = "A"; }
				const t = String(type).toUpperCase();
				if (!RRTYPES.includes(t)) {
					throw Object.assign(new TypeError(`Unknown type: ${type}`), { code: "ERR_INVALID_ARG_VALUE" });
				}
				if (t === "A" || t === "AAAA") {
					return dnsQuery(t, host, (err, list) => {
						if (err) return cb(err);
						cb(null, list.map((e) => e.address));
					});
				}
				return dnsQuery(t, host, cb);
			},
			reverse(addr, cb) { return dnsQuery("PTR", addr, cb); },
		};
		for (const t of RRTYPES) {
			const name = "resolve" + (t === "A" ? "4" : t === "AAAA" ? "6" : t.charAt(0) + t.slice(1).toLowerCase());
			self[name] = (host, cb) => self.resolve(host, t, cb);
		}
		// Node spells these two with their record type in caps.
		self.resolveMx = (host, cb) => self.resolve(host, "MX", cb);
		self.resolveNs = (host, cb) => self.resolve(host, "NS", cb);
		self.resolveTxt = (host, cb) => self.resolve(host, "TXT", cb);
		self.resolveSrv = (host, cb) => self.resolve(host, "SRV", cb);
		self.resolveCname = (host, cb) => self.resolve(host, "CNAME", cb);
		self.resolvePtr = (host, cb) => self.resolve(host, "PTR", cb);
		return self;
	}

	function Resolver(options) {
		if (!(this instanceof Resolver)) return new Resolver(options);
		Object.assign(this, makeResolver());
	}

	// The promise form of every callback method, which is all dns/promises is.
	function promisify(obj) {
		const out = { Resolver: class extends Resolver { constructor(o) { super(o); Object.assign(this, promisify(makeResolver())); } } };
		for (const [k, v] of Object.entries(obj)) {
			if (typeof v !== "function" || k.startsWith("_")) { out[k] = v; continue; }
			if (k === "setServers" || k === "getServers" || k === "cancel" || k === "setLocalAddress") { out[k] = v; continue; }
			out[k] = (...args) => new Promise((resolve, reject) => {
				v(...args, (err, ...rest) => {
					if (err) return reject(err);
					// dns.promises.lookup resolves with { address, family }; every
					// other method's callback carries a single value.
					if (k === "lookup" && rest.length > 1) return resolve({ address: rest[0], family: rest[1] });
					resolve(rest[0]);
				});
			});
		}
		return out;
	}

	const dnsModule = makeResolver();
	dnsModule.Resolver = Resolver;
	dnsModule.promises = promisify(makeResolver());
	// The record-type constants Node exports.
	Object.assign(dnsModule, {
		ADDRCONFIG: 1024, ALL: 256, V4MAPPED: 8,
		NODATA: "ENODATA", FORMERR: "EFORMERR", SERVFAIL: "ESERVFAIL",
		NOTFOUND: "ENOTFOUND", NOTIMP: "ENOTIMP", REFUSED: "REFUSED",
		BADQUERY: "EBADQUERY", BADNAME: "EBADNAME", BADFAMILY: "EBADFAMILY",
		TIMEOUT: "ETIMEOUT", CANCELLED: "ECANCELLED",
	});
	core.dns = dnsModule;

	// node:tls over the tls_* host ops. TLSSocket is a Socket-shaped Duplex
	// whose reads come from an encrypted connection; the server side accepts
	// TLS connections and hands each to a TLSSocket.
	function TLSSocket(socket) {
		// new tls.TLSSocket(existingSocket) is a STARTTLS upgrade: it wraps an
		// already-connected plain socket in TLS. That needs the raw fd of the
		// underlying connection, which the host net bridge does not expose, so a
		// real upgrade is not implementable here. Rather than return a socket that
		// silently swallows writes (data loss), throw a clear error. Internal
		// callers (tls.connect, server accept) construct with no socket argument.
		if (socket !== undefined && socket !== null) {
			throw Object.assign(
				new Error("tls.TLSSocket upgrade of an existing socket is not supported"),
				{ code: "ERR_METHOD_NOT_IMPLEMENTED" },
			);
		}
		core.stream.Duplex.call(this, {});
		this._id = null;
		this._peerCert = null;
		this.encrypted = true;
		this._timeoutMs = 0;
		this._timeoutTimer = null;
		// Only true once a verified handshake is established. Set by tlsConnect
		// per rejectUnauthorized; a server-accepted socket stays unauthorized
		// (no client-cert verification here).
		this.authorized = false;
		this.authorizationError = null;
		this.autoDestroy = true; // owns a host connection, as net.Socket does
	}
	Object.setPrototypeOf(TLSSocket.prototype, core.stream.Duplex.prototype);
	Object.setPrototypeOf(TLSSocket, core.stream.Duplex);
	TLSSocket.prototype._write = function (chunk, enc, cb) { if (this._id !== null) { socketTimeoutTouch(this); ops.net_write(this._id, chunk, cb); } else cb(); };
	TLSSocket.prototype._read = function () { if (this._id !== null) ops.net_read_resume(this._id); };
	TLSSocket.prototype._final = function (cb) { if (this._id !== null) ops.net_end(this._id); cb(); };
	TLSSocket.prototype._destroy = function (err, callback) {
		socketTimeoutClear(this);
		if (this._id !== null) { ops.net_close(this._id); this._id = null; }
		if (callback) callback(err);
	};
	TLSSocket.prototype.destroy = function (err) { core.stream.Duplex.prototype.destroy.call(this, err); return this; };
	TLSSocket.prototype.setEncoding = core.stream.Readable.prototype.setEncoding;
	TLSSocket.prototype.end = core.stream.Writable.prototype.end;
	TLSSocket.prototype.setTimeout = socketSetTimeout;
	TLSSocket.prototype.setNoDelay = function () { return this; };
	// Peer certificate fields, snapshotted from the Go tls.Conn after the
	// handshake and stored on the socket by tlsConnect. Returns {} before the
	// handshake (or on a server-accepted socket with no client cert).
	TLSSocket.prototype.getPeerCertificate = function () { return this._peerCert || {}; };
	TLSSocket.prototype.getCipher = function () { return { name: "ECDHE-RSA-AES128-GCM-SHA256", version: "TLSv1.2" }; };
	TLSSocket.prototype.getProtocol = function () { return "TLSv1.2"; };

	// caToPEM normalizes the `ca` option (string | Buffer | array of either) to a
	// single concatenated PEM string for the host to parse into a CertPool.
	function caToPEM(ca) {
		if (ca === undefined || ca === null) return "";
		const list = Array.isArray(ca) ? ca : [ca];
		return list.map((c) => (Buffer.isBuffer(c) || c instanceof Uint8Array) ? Buffer.from(c).toString("utf8") : String(c)).join("\n");
	}

	function tlsConnect(options, cb) {
		if (typeof options === "number") { options = { port: validatePort(options), host: arguments[1] }; cb = arguments[2]; }
		if (options === undefined || options === null) options = {};
		validateTLSOptions(options);
		if (options.port !== undefined) validatePort(options.port);
		const sock = new TLSSocket();
		if (cb) sock.once("secureConnect", cb);
		const onData = (chunk) => { socketTimeoutTouch(sock); sock.push(Buffer.from(chunk)); };
		const onEnd = () => endOnPeerFin(sock);
		const onError = (info) => {
			const obj = info && typeof info === "object"; // {code,message} from async connect
			const e = new Error(obj ? info.message : info);
			e.code = (obj && info.code) || "ECONNRESET";
			// A certificate-verification failure is also surfaced on the socket
			// per Node; the host marks it structurally (info.verify).
			if (obj && info.verify) sock.authorizationError = e.code;
			sock.emit("error", e);
		};
		const onConnect = (cert) => {
			// The host hands over the peer's leaf cert as a plain object with `raw`
			// base64-encoded; rehydrate raw into a Buffer so getPeerCertificate()
			// matches Node's shape.
			if (cert && typeof cert === "object") {
				if (typeof cert.raw === "string") { try { cert.raw = Buffer.from(cert.raw, "base64"); } catch (_) {} }
				sock._peerCert = cert;
			}
			socketTimeoutTouch(sock);
			sock.emit("secureConnect");
			sock.emit("connect");
		};
		const verify = options.rejectUnauthorized !== false;
		// The Go tls.Dial verifies the chain when verify is true; a successful
		// connect then means authorized. When verification is skipped, the peer
		// is explicitly NOT authorized (report it rather than claiming trust).
		sock.authorized = verify;
		if (!verify) sock.authorizationError = "CERT_VERIFICATION_SKIPPED";
		const caPEM = caToPEM(options.ca);
		const servername = String(options.servername || "");
		const r = ops.tls_connect(String(options.host || "127.0.0.1"), Number(options.port), verify, caPEM, servername, onData, onEnd, onError, onConnect);
		if (isErr(r)) { const e = new Error(r.message); e.code = r.code; process.nextTick(() => sock.emit("error", e)); return sock; }
		sock._id = r;
		return sock;
	}

	function TLSServer(options, listener) {
		if (typeof options === "function") { listener = options; options = {}; }
		core.events.call(this);
		this._opts = options || {};
		this._id = null;
		this._connections = new Set();
		this._closePending = false;
		if (listener) this.on("secureConnection", listener);
	}
	Object.setPrototypeOf(TLSServer.prototype, core.events.prototype);
	Object.setPrototypeOf(TLSServer, core.events);
	TLSServer.prototype.listen = function (port, host, cb) {
		// Node: server.listen([port[, host]][, cb]) or listen(options[, cb]) — the
		// object form must be honored or the requested port is silently dropped
		// and an ephemeral one bound (the service becomes unreachable).
		if (typeof port === "object" && port !== null) { const o = port; cb = host; host = o.host; port = o.port; }
		if (typeof host === "function") { cb = host; host = undefined; }
		host = host || "127.0.0.1";
		const onConnection = (id, remote) => {
			const sock = new TLSSocket();
			sock._id = id;
			ops.net_attach(id, (chunk) => { socketTimeoutTouch(sock); sock.push(Buffer.from(chunk)); }, () => endOnPeerFin(sock), (msg) => sock.emit("error", new Error(msg)));
			trackServerConn(this, sock);
			this.emit("secureConnection", sock);
		};
		const r = ops.tls_listen(String(host), Number(port) || 0, this._opts.cert, this._opts.key, onConnection);
		if (isErr(r)) { const e = new Error(r.message); e.code = r.code; process.nextTick(() => this.emit("error", e)); return this; }
		this._id = r.id; this._port = r.port;
		if (cb) this.once("listening", cb);
		process.nextTick(() => this.emit("listening"));
		return this;
	};
	TLSServer.prototype.address = function () { return this._id !== null ? { address: "127.0.0.1", port: this._port, family: "IPv4" } : null; };
	TLSServer.prototype.ref = function () {
		if (this._unreffed) { ops.loop_ref(true); this._unreffed = false; }
		return this;
	};
	TLSServer.prototype.unref = function () {
		if (!this._unreffed && this._id !== null) { ops.loop_ref(false); this._unreffed = true; }
		return this;
	};
	TLSServer.prototype.close = function (cb) {
		if (this._id !== null) {
			// Rebalance the loop ref accounting before dropping the listen pending.
			if (this._unreffed) { ops.loop_ref(true); this._unreffed = false; }
			ops.net_close_srv(this._id); this._id = null; // stop accepting NOW
			if (cb) this.once("close", cb);
			this._closePending = true;
			maybeEmitServerClose(this); // 'close' only after all connections ended
		} else if (cb) {
			process.nextTick(() => cb(Object.assign(new Error("Server is not running."), { code: "ERR_SERVER_NOT_RUNNING" })));
		}
		return this;
	};

	// TLS options decide whether a connection is SECURE, so a malformed one is
	// worth naming rather than ignoring: a `ciphers: 1` that is silently dropped
	// leaves the caller believing they restricted the suite when they did not.
	// The TLS 1.2 cipher-suite names Go's crypto/tls can actually negotiate
	// (their OpenSSL spellings), plus the TLS 1.3 suites and list directives.
	// A ciphers option whose legacy portion matches none of them is the
	// handshake failure Node reports at CONTEXT time: no cipher match.
	const KNOWN_TLS_CIPHERS = new Set([
		"ECDHE-ECDSA-AES128-GCM-SHA256", "ECDHE-RSA-AES128-GCM-SHA256",
		"ECDHE-ECDSA-AES256-GCM-SHA384", "ECDHE-RSA-AES256-GCM-SHA384",
		"ECDHE-ECDSA-CHACHA20-POLY1305", "ECDHE-RSA-CHACHA20-POLY1305",
		"ECDHE-ECDSA-AES128-SHA", "ECDHE-RSA-AES128-SHA",
		"ECDHE-ECDSA-AES256-SHA", "ECDHE-RSA-AES256-SHA",
		"AES128-GCM-SHA256", "AES256-GCM-SHA384", "AES128-SHA", "AES256-SHA",
		"TLS_AES_128_GCM_SHA256", "TLS_AES_256_GCM_SHA384", "TLS_CHACHA20_POLY1305_SHA256",
	]);
	function validateCipherList(ciphers) {
		// TLS 1.3 names (TLS_*) configure their own list; directives and
		// keywords pass through. What must NOT pass silently is a legacy list
		// that names no cipher this runtime has.
		let sawLegacy = false;
		let matchedLegacy = false;
		for (const tok of String(ciphers).split(":")) {
			if (tok === "" || tok.startsWith("!") || tok.startsWith("-") || tok.startsWith("+")) continue;
			if (/^TLS_/.test(tok)) {
				if (KNOWN_TLS_CIPHERS.has(tok)) matchedLegacy = true;
				else sawLegacy = true;
				continue;
			}
			if (["ALL", "DEFAULT", "HIGH", "MEDIUM", "COMPLEMENTOFDEFAULT", "@STRENGTH"].includes(tok) || tok.startsWith("@SECLEVEL")) {
				matchedLegacy = true;
				continue;
			}
			sawLegacy = true;
			// OpenSSL cipher names are case-sensitive: "aes256-sha" matches
			// nothing even though "AES256-SHA" exists.
			if (KNOWN_TLS_CIPHERS.has(tok)) matchedLegacy = true;
		}
		if (sawLegacy && !matchedLegacy) {
			throw Object.assign(new Error("SSL_CTX_set_cipher_list error: no cipher match"),
				{ code: "ERR_SSL_NO_CIPHER_MATCH", library: "SSL routines", reason: "no cipher match" });
		}
	}
	function validateTLSOptions(options, name = "options") {
		if (options === undefined || options === null) return {};
		if (typeof options !== "object") {
			throw Object.assign(new TypeError(`The "${name}" argument must be of type object. Received ${typeof options}`),
				{ code: "ERR_INVALID_ARG_TYPE" });
		}
		// Custom OpenSSL engines were dropped upstream; naming one is the
		// error modern Node reports (its suite skips on exactly this code).
		if (options.clientCertEngine !== undefined && options.clientCertEngine !== null) {
			if (typeof options.clientCertEngine !== "string") {
				throw Object.assign(new TypeError(`The "options.clientCertEngine" property must be of type string. ${cryptoReceived(options.clientCertEngine)}`),
					{ code: "ERR_INVALID_ARG_TYPE" });
			}
			throw Object.assign(new Error("Custom engines not supported by this OpenSSL"),
				{ code: "ERR_CRYPTO_CUSTOM_ENGINE_NOT_SUPPORTED" });
		}
		if (options.ciphers !== undefined && options.ciphers !== null && typeof options.ciphers === "string") {
			validateCipherList(options.ciphers);
		}
		if (options.secureProtocol !== undefined && options.secureProtocol !== null) {
			const sp = String(options.secureProtocol);
			// SSLv23_method is the historical "negotiate anything" ALIAS and
			// is legal; only the SSLv2_/SSLv3_ families are disabled.
			if (/^SSLv2_/.test(sp)) {
				throw Object.assign(new Error("SSLv2 methods disabled"), { code: "ERR_TLS_INVALID_PROTOCOL_METHOD" });
			}
			if (/^SSLv3_/.test(sp)) {
				throw Object.assign(new Error("SSLv3 methods disabled"), { code: "ERR_TLS_INVALID_PROTOCOL_METHOD" });
			}
			const known = new Set(["TLS_method", "TLS_client_method", "TLS_server_method",
				"TLSv1_method", "TLSv1_client_method", "TLSv1_server_method",
				"TLSv1_1_method", "TLSv1_1_client_method", "TLSv1_1_server_method",
				"TLSv1_2_method", "TLSv1_2_client_method", "TLSv1_2_server_method",
				"SSLv23_method", "SSLv23_client_method", "SSLv23_server_method"]);
			if (!known.has(sp)) {
				throw Object.assign(new Error(`Unknown method: ${sp}`), { code: "ERR_TLS_INVALID_PROTOCOL_METHOD" });
			}
		}
		for (const key of ["ciphers", "servername", "clientCertEngine", "privateKeyEngine", "privateKeyIdentifier", "sigalgs"]) {
			if (options[key] !== undefined && options[key] !== null && typeof options[key] !== "string") {
				throw Object.assign(new TypeError(`The "options.${key}" property must be of type string.${cryptoReceived === undefined ? "" : " " + cryptoReceived(options[key])}`),
					{ code: "ERR_INVALID_ARG_TYPE" });
			}
		}
		// key/cert/ca: string, view, or an ARRAY of those; false/undefined/null
		// mean absent. Anything else is the exact TypeError the suite spells.
		// Node's configSecureContext order: ca, then cert, then key — the
		// suite pairs a bad key with a bad cert to check exactly this.
		for (const key of ["ca", "cert", "key"]) {
			const v = options[key];
			// Node's configSecureContext guards with `if (ca)` etc.: EVERY
			// falsy value means absent, 0 and "" included.
			if (!v) continue;
			const one = (x) => typeof x === "string" || ArrayBuffer.isView(x);
			const bad = (x) => {
				throw Object.assign(new TypeError(
					`The "options.${key}" property must be of type string or an instance of Buffer, TypedArray, or DataView. ${cryptoReceived(x)}`),
					{ code: "ERR_INVALID_ARG_TYPE" });
			};
			if (Array.isArray(v)) {
				// Element-wise, and the error names the ELEMENT — Node walks
				// the array and reports the first offender, not the array.
				for (const x of v) {
					if (one(x)) continue;
					if (key === "key" && x !== null && typeof x === "object" && one(x.pem)) continue;
					bad(x);
				}
				continue;
			}
			if (!one(v)) bad(v);
		}
		for (const key of ["sessionTimeout", "handshakeTimeout", "minVersion", "maxVersion"]) {
			const v = options[key];
			if (v === undefined || v === null) continue;
			const numeric = key === "sessionTimeout" || key === "handshakeTimeout";
			if (numeric ? typeof v !== "number" : typeof v !== "string") {
				throw Object.assign(new TypeError(`The "options.${key}" property must be of type ${numeric ? "number" : "string"}. Received ${typeof v}`),
					{ code: "ERR_INVALID_ARG_TYPE" });
			}
		}
		if (options.ticketKeys !== undefined && options.ticketKeys !== null && !ArrayBuffer.isView(options.ticketKeys)) {
			throw Object.assign(new TypeError(`The "options.ticketKeys" property must be an instance of Buffer, TypedArray, or DataView. Received ${typeof options.ticketKeys}`),
				{ code: "ERR_INVALID_ARG_TYPE" });
		}
		// An IP address cannot be an SNI servername — SNI carries a host NAME,
		// and sending an address is both useless and a protocol violation.
		if (typeof options.servername === "string" && options.servername !== ""
			&& (isIPv4(options.servername) || isIPv6(options.servername))) {
			throw Object.assign(new TypeError(`The "options.servername" property must not be an IP address. Received ${JSON.stringify(options.servername)}`),
				{ code: "ERR_INVALID_ARG_VALUE" });
		}
		return options;
	}

	core.tls = {
		connect: tlsConnect,
		createServer: (options, listener) => new TLSServer(validateTLSOptions(options), listener),
		createSecureContext: (opts) => validateTLSOptions(opts, "options"),
		TLSSocket: callableClass(TLSSocket),
		Server: callableClass(TLSServer),
		rootCertificates: [],
		// getCACertificates reports the trust store this runtime would verify
		// against. The host's own pool is what tls.Dial uses, and it is not
		// enumerable from Go, so "default" and "system" answer with what we can
		// state truthfully: an empty list, plus whatever the caller added.
		getCACertificates(type = "default") {
			if (!["default", "system", "bundled", "extra"].includes(String(type))) {
				throw Object.assign(new TypeError(`Invalid CA certificate type: ${type}`), { code: "ERR_INVALID_ARG_VALUE" });
			}
			return core.tls.rootCertificates;
		},
		setDefaultCACertificates(certs) {
			core.tls.rootCertificates = Array.from(certs || []).map(String);
		},
		// A convenience (not in Node): a self-signed cert for quick servers.
		generateSelfSigned: (host) => ops.tls_selfsigned(host),
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

	// readline: a line splitter over an input Readable, emitting 'line' and
	// answering question() prompts. Enough for interactive CLIs / config
	// readers driven by process.stdin.
	class Interface extends core.events {
		constructor(options) {
			super();
			this.input = options.input;
			this.output = options.output;
			this.terminal = !!options.terminal;
			if (options.prompt !== undefined) this._prompt = String(options.prompt);
			this._buf = "";
			this._questionCb = null;
			this._closed = false;
			if (this.input) {
				this.input.setEncoding && this.input.setEncoding("utf8");
				this.input.on("data", (chunk) => this._onData(String(chunk)));
				// Node's partial-line flush happens only on input EOF: a file
				// without a trailing newline still yields its last line. An
				// explicit close() must NOT flush (and must not answer a pending
				// question()) — closing is how callers CANCEL a question.
				this.input.on("end", () => {
					if (this._closed) return;
					if (this._buf) {
						const line = this._buf.replace(/\r$/, "");
						this._buf = "";
						if (this._questionCb) { const cb = this._questionCb; this._questionCb = null; cb(line); }
						else this.emit("line", line);
					}
					this.close();
				});
			}
		}
		_onData(str) {
			this._buf += str;
			let idx;
			while ((idx = this._buf.indexOf("\n")) >= 0) {
				const line = this._buf.slice(0, idx).replace(/\r$/, "");
				this._buf = this._buf.slice(idx + 1);
				if (this._questionCb) { const cb = this._questionCb; this._questionCb = null; cb(line); }
				else this.emit("line", line);
			}
		}
		question(query, cb) {
			if (this.output) this.output.write(query);
			this._questionCb = cb;
		}
		prompt() { if (this.output) this.output.write(this._prompt ?? "> "); }
		setPrompt(p) { this._prompt = String(p); }
		getPrompt() { return this._prompt ?? "> "; }
		// write() feeds the INPUT, as if the user had typed it — it does not
		// write to the output. Having it backwards meant every caller that
		// drives an Interface programmatically (the REPL above all) echoed its
		// script instead of running it.
		write(data) { if (data !== null && data !== undefined) this._onData(String(data)); }
		close() {
			if (this._closed) return;
			this._closed = true;
			this._buf = "";
			this._questionCb = null; // a pending question is cancelled, not answered
			this.emit("close");
		}
		[Symbol.asyncIterator]() {
			const lines = [];
			let done = false, wake = null;
			this.on("line", (l) => { lines.push(l); if (wake) { wake(); wake = null; } });
			this.on("close", () => { done = true; if (wake) { wake(); wake = null; } });
			return {
				async next() {
					for (;;) {
						if (lines.length) return { value: lines.shift(), done: false };
						if (done) return { value: undefined, done: true };
						await new Promise((r) => { wake = r; });
					}
				},
				[Symbol.asyncIterator]() { return this; },
			};
		}
	}
	core.readline = {
		Interface,
		createInterface: (options) => new Interface(options),
		clearLine: () => {},
		cursorTo: () => {},
		moveCursor: () => {},
	};

	core.cluster = {
		isMaster: true,
		isPrimary: true,
		isWorker: false,
		workers: {},
		fork: notSupported("cluster.fork"),
	};

	// diagnostics_channel: a real pub/sub implementation. channel(name) returns a
	// runtime-wide singleton (cached by name) so a publisher and a subscriber that
	// each call channel("x") share one Channel; publish() invokes every subscriber
	// synchronously and hasSubscribers reflects the live count.
	const dcChannels = new Map();
	class Channel {
		constructor(name) {
			this.name = name;
			this._subs = new Set();
			this._stores = new Map();
		}
		get hasSubscribers() { return this._subs.size > 0; }
		subscribe(onMessage) {
			if (typeof onMessage !== "function") throw new TypeError("onMessage must be a function");
			this._subs.add(onMessage);
		}
		unsubscribe(onMessage) { return this._subs.delete(onMessage); }
		publish(message) {
			// Snapshot so a subscriber that (un)subscribes during delivery does not
			// disturb this publish. A throw is routed to uncaughtException (Node's
			// behavior) rather than aborting the remaining subscribers.
			for (const cb of [...this._subs]) {
				try { cb(message, this.name); }
				catch (e) {
					if (globalThis.__node_emit_uncaught) globalThis.__node_emit_uncaught(e);
					else throw e;
				}
			}
		}
		// withStoreScope(context) is runStores turned inside out: instead of
		// wrapping a function, it ENTERS the stores and hands back a disposable
		// that leaves them again — which is what `using scope = ...` needs.
		withStoreScope(context) {
			const leave = enterBoundStores(this, context);
			this.publish(context);
			return { [Symbol.dispose]: leave };
		}
		bindStore(store, transform) { this._stores.set(store, transform || ((v) => v)); }
		unbindStore(store) { return this._stores.delete(store); }
		runStores(context, fn, thisArg, ...args) {
			// publish is INNERMOST, so subscribers see the stores already entered.
			let run = () => { this.publish(context); return Reflect.apply(fn, thisArg, args); };
			for (const [store, transform] of this._stores) {
				const next = run;
				run = () => store.run(transform(context), next);
			}
			return run();
		}
	}
	// Enter every store bound to a channel and return the function that leaves
	// them again. Entering BEFORE publishing is deliberate: a subscriber runs
	// inside the scope the channel is establishing, and asking it to observe a
	// store that is not set yet defeats the point of binding one.
	function enterBoundStores(channel, context) {
		const restore = [];
		for (const [store, transform] of channel._stores) {
			restore.push([store, store.getStore()]);
			store.enterWith(transform(context));
		}
		return () => { for (const [store, prev] of restore) store.enterWith(prev); };
	}

	function dcChannel(name) {
		name = String(name);
		let ch = dcChannels.get(name);
		if (!ch) { ch = new Channel(name); dcChannels.set(name, ch); }
		return ch;
	}
	// tracingChannel(name) fans one logical operation across start/end/asyncStart/
	// asyncEnd/error sub-channels; the traceSync/tracePromise/traceCallback helpers
	// publish the context object through them around the wrapped call.
	function makeTracingChannel(nameOrChannels) {
		const sub = (suffix) => typeof nameOrChannels === "string"
			? dcChannel(`tracing:${nameOrChannels}:${suffix}`)
			: nameOrChannels[suffix];
		const channels = {
			start: sub("start"), end: sub("end"),
			asyncStart: sub("asyncStart"), asyncEnd: sub("asyncEnd"),
			error: sub("error"),
		};
		return {
			...channels,
			get hasSubscribers() {
				return Object.values(channels).some((c) => c && c.hasSubscribers);
			},
			subscribe(handlers) {
				for (const k of Object.keys(handlers)) if (channels[k]) channels[k].subscribe(handlers[k]);
			},
			unsubscribe(handlers) {
				let ok = true;
				for (const k of Object.keys(handlers)) if (channels[k]) ok = channels[k].unsubscribe(handlers[k]) && ok;
				return ok;
			},
			traceSync(fn, context = {}, thisArg, ...args) {
				channels.start.publish(context);
				try {
					const result = Reflect.apply(fn, thisArg, args);
					context.result = result;
					return result;
				} catch (error) {
					context.error = error;
					channels.error.publish(context);
					throw error;
				} finally {
					channels.end.publish(context);
				}
			},
			// traceCallback wraps a callback-style call: start publishes before,
			// asyncStart/asyncEnd around the CALLBACK, and the stores stay bound
			// for its duration — which is the whole reason it is not traceSync.
			traceCallback(fn, position = -1, context = {}, thisArg, ...args) {
				channels.start.publish(context);
				const cb = position >= 0 ? args[position] : args[args.length - 1];
				const wrapped = function (err, res) {
					if (err) { context.error = err; channels.error.publish(context); }
					else { context.result = res; }
					channels.asyncStart.publish(context);
					try {
						return typeof cb === "function" ? Reflect.apply(cb, this, arguments) : undefined;
					} finally {
						channels.asyncEnd.publish(context);
					}
				};
				if (position >= 0) args[position] = wrapped;
				else args[args.length - 1] = wrapped;
				try {
					return Reflect.apply(fn, thisArg, args);
				} catch (error) {
					context.error = error;
					channels.error.publish(context);
					throw error;
				} finally {
					channels.end.publish(context);
				}
			},
			// The stores bound to `start`, held for the duration of fn. Node names
			// it withStoreScope because the SCOPE is what it gives you, not a trace.
			withStoreScope(context, fn, thisArg, ...args) {
				return channels.start.runStores(context, fn, thisArg, ...args);
			},
			tracePromise(fn, context = {}, thisArg, ...args) {
				channels.start.publish(context);
				let promise;
				try {
					promise = Reflect.apply(fn, thisArg, args);
				} catch (error) {
					context.error = error;
					channels.error.publish(context);
					channels.end.publish(context);
					throw error;
				}
				channels.end.publish(context);
				return Promise.resolve(promise).then(
					(result) => { context.result = result; channels.asyncStart.publish(context); channels.asyncEnd.publish(context); return result; },
					(error) => { context.error = error; channels.error.publish(context); channels.asyncStart.publish(context); channels.asyncEnd.publish(context); throw error; },
				);
			},
		};
	}
	// A BoundedChannel is a tracing channel narrowed to the two edges of an
	// operation — start and end — for callers that only want to bound it, not
	// to trace what happened inside.
	function BoundedChannel(nameOrChannels) {
		if (!new.target) return new BoundedChannel(nameOrChannels);
		const sub = (suffix) => typeof nameOrChannels === "string"
			? dcChannel(`tracing:${nameOrChannels}:${suffix}`)
			: nameOrChannels[suffix];
		this.start = sub("start");
		this.end = sub("end");
	}
	Object.defineProperty(BoundedChannel.prototype, "hasSubscribers", {
		get() { return this.start.hasSubscribers || this.end.hasSubscribers; },
		configurable: true,
	});
	Object.assign(BoundedChannel.prototype, {
		subscribe(handlers) {
			for (const k of ["start", "end"]) if (handlers[k]) this[k].subscribe(handlers[k]);
		},
		unsubscribe(handlers) {
			// True only if something was actually removed, so a caller can tell a
			// real unsubscribe from a repeat.
			let removed = false;
			for (const k of ["start", "end"]) if (handlers[k] && this[k].unsubscribe(handlers[k])) removed = true;
			return removed;
		},
		// The context comes FIRST here: a bounded channel is about the operation
		// being bounded, and the function is what happens inside it.
		run(context, fn, thisArg, ...args) {
			this.start.publish(context);
			try {
				const result = Reflect.apply(fn, thisArg, args);
				context.result = result;
				return result;
			} finally {
				this.end.publish(context);
			}
		},
		// The disposable form: start now, end when the scope is left. The context
		// is published by reference, so what the block adds to it is visible to
		// the end subscriber.
		withScope(context) {
			// The stores bound to `start` stay entered for the whole scope, so the
			// `end` subscriber sees them too — the scope is the operation, not
			// just its first edge.
			const leave = enterBoundStores(this.start, context);
			this.start.publish(context);
			const end = this.end;
			return {
				[Symbol.dispose]() { end.publish(context); leave(); },
			};
		},
	});

	core.diagnostics_channel = {
		Channel,
		BoundedChannel,
		boundedChannel: (nameOrChannels) => new BoundedChannel(nameOrChannels),
		channel: dcChannel,
		hasSubscribers: (name) => {
			const ch = dcChannels.get(String(name));
			return !!ch && ch.hasSubscribers;
		},
		subscribe: (name, onMessage) => dcChannel(name).subscribe(onMessage),
		unsubscribe: (name, onMessage) => dcChannel(name).unsubscribe(onMessage),
		tracingChannel: makeTracingChannel,
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

	// core.console is registered in extended.js (which runs last); nothing
	// between here and there reads it, so no stub is needed.
	core.constants = { os: {}, fs: {} };

	Object.assign(core.os, {
		// A real loopback view (was {}): callers read os.networkInterfaces().lo.
		networkInterfaces: () => ({
			lo: [
				{ address: "127.0.0.1", netmask: "255.0.0.0", family: "IPv4", mac: "00:00:00:00:00:00", internal: true, cidr: "127.0.0.1/8" },
				{ address: "::1", netmask: "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", family: "IPv6", mac: "00:00:00:00:00:00", internal: true, cidr: "::1/128", scopeid: 0 },
			],
		}),
		userInfo: () => ({ username: "user", uid: 1000, gid: 1000, shell: null, homedir: "/" }),
		loadavg: () => [0, 0, 0],
		uptime: () => 0,
		version: () => "0.0.0",
		machine: () => "x86_64",
	});

	// ------------------------------------------------- legacy url.parse

	const qs = core.querystring;
	core.url.parse = function parse(str, parseQueryString, slashesDenoteHost) {
		str = String(str);
		const hasProtocol = /^[a-zA-Z][a-zA-Z0-9+.-]*:/.test(str);
		// Node: leading "//" denotes a host ONLY when slashesDenoteHost is true
		// (or a scheme precedes it). Otherwise "//evil.com/x" is a plain path.
		const networkPath = !hasProtocol && str.startsWith("//");
		const denotesHost = hasProtocol || (networkPath && !!slashesDenoteHost);
		let u;
		if (hasProtocol) u = new URL(str);
		else if (networkPath && slashesDenoteHost) u = new URL("http:" + str);
		else if (networkPath) u = new URL("http://placeholder.invalid" + str);
		else u = new URL(str, "http://placeholder.invalid");
		const qIndex = str.indexOf("?");
		const rawQuery = u.search ? u.search.slice(1) : qIndex >= 0 ? "" : null;
		// Legacy url.parse exposes auth DECODED (the WHATWG URL stores it
		// percent-encoded).
		const dec = (s) => { try { return decodeURIComponent(s); } catch { return s; } };
		const relHref = u.pathname + (u.search || "") + (u.hash || "");
		return {
			protocol: hasProtocol ? u.protocol : null,
			slashes: denotesHost ? true : null,
			auth: u.username ? (u.password ? `${dec(u.username)}:${dec(u.password)}` : dec(u.username)) : null,
			host: denotesHost ? u.host : null,
			hostname: denotesHost ? u.hostname : null,
			port: denotesHost && u.port ? u.port : null,
			pathname: u.pathname,
			search: u.search || (qIndex >= 0 ? "?" : null),
			query: parseQueryString ? qs.parse(rawQuery ?? "") : rawQuery,
			hash: u.hash || null,
			path: u.pathname + (u.search || ""),
			href: hasProtocol ? u.href
				: denotesHost ? "//" + u.host + relHref
				: relHref,
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
	core.url.resolve = (from, to) => {
		const fromHasProtocol = /^[a-zA-Z][a-zA-Z0-9+.-]*:/.test(String(from));
		const resolved = new URL(to, new URL(from, "http://placeholder.invalid"));
		if (fromHasProtocol) return resolved.href;
		// A scheme-less base resolves to a path-only result in Node; strip the
		// placeholder origin so we don't leak "http://placeholder.invalid" into the
		// returned string.
		return resolved.pathname + resolved.search + resolved.hash;
	};
})();
