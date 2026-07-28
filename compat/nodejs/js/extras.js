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

	// randomFillInto fills buf[offset, offset+size) with random bytes, honoring the
	// (buf[, offset[, size]]) overload and chunking the getRandomValues call so a
	// buffer larger than 64 KiB doesn't hit its per-call QuotaExceededError.
	function randomFillInto(buf, offset, size) {
		if (!ArrayBuffer.isView(buf)) throw new TypeError('The "buf" argument must be a TypedArray or DataView');
		const total = buf.byteLength;
		// Node counts offset/size in ELEMENTS for a typed array (bytes for a
		// DataView, which has no BYTES_PER_ELEMENT). Scale to bytes before bounds
		// checks so randomFillSync(new Uint32Array(4), 1, 1) fills element 1
		// (bytes 4-7), not byte 1.
		const elem = buf.BYTES_PER_ELEMENT || 1;
		offset = offset === undefined ? 0 : (offset >>> 0) * elem;
		size = size === undefined ? total - offset : (size >>> 0) * elem;
		if (offset > total) throw new RangeError('"offset" is out of range');
		if (offset + size > total) throw new RangeError('"size" is out of range');
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
				this._chunks.push(toBuf(data, inputEnc));
				return outputEnc ? "" : Buffer.alloc(0);
			}
			final(outputEnc) {
				if (this._done) throw new Error("Unsupported state or unable to authenticate data");
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
		createHash: (algorithm) => new Hash(algorithm),
		createHmac: (algorithm, key) => new Hash(algorithm, toBuf(keyMaterial(key))),
		Hash, Hmac: Hash,
		KeyObject,
		createSecretKey, createPublicKey, createPrivateKey,
		sign: signOneShot,
		verify: verifyOneShot,
		createCipheriv: (algo, key, iv) =>
			String(algo).toLowerCase() === "chacha20-poly1305" ? new ChaChaCipher(true, key, iv) : new Cipheriv(algo, key, iv),
		createDecipheriv: (algo, key, iv) =>
			String(algo).toLowerCase() === "chacha20-poly1305" ? new ChaChaCipher(false, key, iv) : new Decipheriv(algo, key, iv),
		Cipheriv, Decipheriv,
		publicEncrypt, privateDecrypt,
		publicDecrypt, privateEncrypt,
		createDiffieHellman: (prime, gen) => new DiffieHellman(prime, gen),
		DiffieHellman,
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
			// Node requires safe integers with max > min; a degenerate range must
			// throw ERR_OUT_OF_RANGE, not silently return NaN or a wrong number.
			if (!Number.isSafeInteger(min) || !Number.isSafeInteger(max) || max <= min) {
				throw new RangeError(`The value of "max" is out of range. It must be greater than the value of "min" (${min}). Received ${max}`);
			}
			const range = max - min;
			const buf = randomBytes(6);
			let n = 0;
			for (const b of buf) n = n * 256 + b;
			return min + (n % range);
		},
		pseudoRandomBytes: randomBytes,
		randomUUID: () => globalThis.crypto.randomUUID(),
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
			this.connecting = false;
			this.remoteAddress = host;
			this.remotePort = port;
			socketTimeoutTouch(this); // connect counts as activity for the idle timer
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
		socketTimeoutTouch(this); // a write is activity for the idle timer
		// The op fires our callback once the chunk is flushed off-loop, so a
		// slow peer backpressures the socket instead of an unbounded queue.
		ops.net_write(this._id, chunk, callback);
	};
	// _read is the read-backpressure signal: the Readable calls it when its buffer
	// drains, releasing one more host read (net_read_resume). Without this a fast
	// peer would stream unbounded data into the host regardless of guest demand.
	Socket.prototype._read = function _read() { if (this._id !== null) ops.net_read_resume(this._id); };
	// _final runs on socket.end(): half-close the write side (send FIN) so an
	// EOF-delimited peer sees end-of-request; the read side stays open for its
	// reply. Without this, end() never sent a FIN and such peers hung.
	Socket.prototype._final = function _final(callback) {
		if (this._id !== null) ops.net_end(this._id);
		callback();
	};
	Socket.prototype.destroy = function destroy(err) {
		socketTimeoutClear(this);
		if (this._id !== null) { ops.net_close(this._id); this._id = null; }
		core.stream.Duplex.prototype.destroy.call(this, err);
		return this;
	};
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

	core.net = {
		isIPv4,
		isIPv6,
		isIP: (s) => (isIPv4(s) ? 4 : isIPv6(s) ? 6 : 0),
		Socket,
		Stream: Socket,
		Server: NetServer,
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

	function Dgram(type) {
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
		const onMessage = (data, rinfo) => this.emit("message", Buffer.from(data), rinfo);
		const r = ops.udp_bind(String(address || ""), Number(port) || 0, onMessage);
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
		let port, address, offset, length;
		if (rest.length >= 4) { offset = rest[0]; length = rest[1]; port = rest[2]; address = rest[3]; }
		else if (rest.length === 3) { offset = rest[0]; length = rest[1]; port = rest[2]; }
		else { port = rest[0]; address = rest[1]; }
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
		let buf = Buffer.from(msg);
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
		ops.udp_send(this._id, buf, Number(port), String(address || "127.0.0.1"), (err) => {
			const e = err ? Object.assign(new Error(err.message), { code: err.code }) : null;
			if (cb) cb(e);
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
	const zlibSync = (method) => (data) => zlibRun(method, data);
	const zlibAsync = (method) => (data, opts, cb) => {
		if (typeof opts === "function") { cb = opts; }
		queueMicrotask(() => { try { cb(null, zlibRun(method, data)); } catch (e) { cb(e); } });
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
		createGzip: () => zlibStream("gzip"),
		createGunzip: () => zlibStream("gunzip"),
		createDeflate: () => zlibStream("deflate"),
		createInflate: () => zlibStream("inflate"),
		createDeflateRaw: () => zlibStream("deflateRaw"),
		createInflateRaw: () => zlibStream("inflateRaw"),
		createUnzip: () => zlibStream("unzip"),
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
	core.vm = {
		createContext: (o = {}) => o,
		isContext: () => false,
		runInThisContext: (code) => (0, eval)(String(code)),
		runInNewContext: (code, sandbox) => runInSandbox(code, sandbox),
		runInContext: (code, contextifiedObject) => runInSandbox(code, contextifiedObject),
		compileFunction: (code, params = []) => new Function(...params, String(code)),
		Script: class Script {
			constructor(code) { this._code = String(code); }
			runInThisContext() { return (0, eval)(this._code); }
			runInNewContext(sandbox) { return runInSandbox(this._code, sandbox); }
			runInContext(contextifiedObject) { return runInSandbox(this._code, contextifiedObject); }
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
		SHARE_ENV: Symbol.for("nodejs.worker_threads.SHARE_ENV"),
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

	const dnsErr = (cb) => queueMicrotask(() => cb(Object.assign(new Error("dns is not supported yet in this runtime"), { code: "ENOTSUP" })));
	core.dns = {
		lookup: (host, opts, cb) => dnsErr(typeof opts === "function" ? opts : cb),
		resolve: (host, type, cb) => dnsErr(typeof type === "function" ? type : cb),
		promises: {
			lookup: () => Promise.reject(new Error("dns is not supported yet in this runtime")),
			resolve: () => Promise.reject(new Error("dns is not supported yet in this runtime")),
		},
	};

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
	}
	Object.setPrototypeOf(TLSSocket.prototype, core.stream.Duplex.prototype);
	Object.setPrototypeOf(TLSSocket, core.stream.Duplex);
	TLSSocket.prototype._write = function (chunk, enc, cb) { if (this._id !== null) { socketTimeoutTouch(this); ops.net_write(this._id, chunk, cb); } else cb(); };
	TLSSocket.prototype._read = function () { if (this._id !== null) ops.net_read_resume(this._id); };
	TLSSocket.prototype._final = function (cb) { if (this._id !== null) ops.net_end(this._id); cb(); };
	TLSSocket.prototype.destroy = function (err) { socketTimeoutClear(this); if (this._id !== null) { ops.net_close(this._id); this._id = null; } core.stream.Duplex.prototype.destroy.call(this, err); return this; };
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
		if (typeof options === "number") { options = { port: options, host: arguments[1] }; cb = arguments[2]; }
		if (typeof options !== "object" || options === null) options = {};
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

	core.tls = {
		connect: tlsConnect,
		createServer: (options, listener) => new TLSServer(options, listener),
		createSecureContext: (opts) => opts || {},
		TLSSocket,
		Server: TLSServer,
		rootCertificates: [],
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
		prompt() { if (this.output) this.output.write("> "); }
		write(data) { if (this.output) this.output.write(data); }
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
		bindStore(store, transform) { this._stores.set(store, transform || ((v) => v)); }
		unbindStore(store) { return this._stores.delete(store); }
		runStores(context, fn, thisArg, ...args) {
			this.publish(context);
			let run = () => Reflect.apply(fn, thisArg, args);
			for (const [store, transform] of this._stores) {
				const next = run;
				run = () => store.run(transform(context), next);
			}
			return run();
		}
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
	core.diagnostics_channel = {
		Channel,
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
