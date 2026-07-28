// crypto.subtle over the __web_ops host crypto primitives (Go crypto stdlib).
// Supported: digest (SHA-1/256/384/512); HMAC; ECDSA (P-256/384/521);
// RSASSA-PKCS1-v1_5 and RSA-PSS. Key formats: raw (HMAC), jwk, pkcs8, spki.
// This is the JWS surface the jose flagship needs; encryption algorithms
// (AES-GCM, RSA-OAEP, ECDH) come with the JWE milestone.
(() => {
	"use strict";
	const ops = globalThis.__web_ops;

	const b64uEncode = (u8) =>
		btoa(String.fromCharCode(...u8)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
	const b64uDecode = (s) => {
		const bin = atob(String(s).replace(/-/g, "+").replace(/_/g, "/"));
		const u = new Uint8Array(bin.length);
		for (let i = 0; i < bin.length; i++) u[i] = bin.charCodeAt(i);
		return u;
	};

	const toU8 = (data) => {
		if (data instanceof ArrayBuffer) return new Uint8Array(data);
		if (ArrayBuffer.isView(data)) return new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
		throw new TypeError("expected a BufferSource");
	};
	const toBuf = (arr) => Uint8Array.from(arr).buffer;

	const HASHES = ["SHA-1", "SHA-256", "SHA-384", "SHA-512"];
	const hashName = (h) => {
		const n = String(h !== null && typeof h === "object" ? h.name : h).toUpperCase();
		if (!HASHES.includes(n)) throw new DOMException(`unsupported hash ${n}`, "NotSupportedError");
		return n;
	};
	const algName = (a) => String(a !== null && typeof a === "object" ? a.name : a);

	class CryptoKey {
		constructor(type, extractable, algorithm, usages, handle) {
			this.type = type;
			this.extractable = extractable;
			this.algorithm = algorithm;
			this.usages = [...usages];
			// Not enumerable: `usages` and friends are the interface, the handle is
			// not, and WPT's structural checks walk own enumerable properties.
			Object.defineProperty(this, "_h", { value: handle, writable: true });
		}
	}
	// Every Web IDL interface has one; WPT checks it on every key it produces,
	// and its absence fails not only that assertion but everything downstream
	// that reuses the key.
	Object.defineProperty(CryptoKey.prototype, Symbol.toStringTag, {
		value: "CryptoKey", configurable: true,
	});
	globalThis.CryptoKey = CryptoKey;

	// Secret key material lives OUTSIDE the CryptoKey object, in WeakMaps, so a
	// non-extractable AES/HKDF/PBKDF2/ECDH key cannot be read back through a
	// plain property — exportKey (which enforces `extractable`) is the only path
	// out. HMAC/EC/RSA keep opaque host handles (_h) and were never leakable.
	const keyRaw = new WeakMap(); // CryptoKey -> raw Uint8Array
	const keyJwk = new WeakMap(); // CryptoKey -> JWK object
	const rawOf = (k) => keyRaw.get(k);
	const jwkObjOf = (k) => keyJwk.get(k);

	const need = (key, usage) => {
		if (!(key instanceof CryptoKey)) throw new TypeError("expected a CryptoKey");
		if (!key.usages.includes(usage)) {
			throw new DOMException(`key does not permit ${usage}`, "InvalidAccessError");
		}
	};
	const unsupported = (what) => { throw new DOMException(`unsupported ${what}`, "NotSupportedError"); };

	// deriveBitsRaw is the ungated core of deriveBits (no usage check), shared by
	// the public deriveBits (which gates "deriveBits") and deriveKey (which gates
	// only "deriveKey", per spec).
	function deriveBitsRaw(alg, baseKey, length) {
		const name = algName(alg).toUpperCase();
		if (name === "ECDH") {
			const privJWK = jwkOf(baseKey);
			const pubJWK = jwkOf(alg.public);
			return toBuf(subtleFail(ops.subtle_ecdh(privJWK, pubJWK, length ?? 0)));
		}
		if (name === "HKDF") {
			return toBuf(subtleFail(ops.subtle_hkdf(hashName(alg.hash), rawOf(baseKey), toU8(alg.salt), toU8(alg.info), length)));
		}
		if (name === "PBKDF2") {
			return toBuf(subtleFail(ops.subtle_pbkdf2(hashName(alg.hash), rawOf(baseKey), toU8(alg.salt), Number(alg.iterations), length)));
		}
		unsupported(`deriveBits algorithm ${algName(alg)}`);
	}

	const RSA_NAMES = ["RSASSA-PKCS1-V1_5", "RSA-PSS"];
	const RSA_ALL = ["RSASSA-PKCS1-V1_5", "RSA-PSS", "RSA-OAEP"];
	const rsaScheme = (name) => (name === "RSA-PSS" ? "pss" : "pkcs1");
	const AES_NAMES = ["AES-GCM", "AES-CBC", "AES-CTR"];
	// AES-KW is a secret AES key too (import/export/generate), but only wraps —
	// it never appears in encrypt/decrypt, which stay gated on AES_NAMES.
	const AES_ALL = [...AES_NAMES, "AES-KW"];

	const subtleFail = (r) => {
		if (r && r.__subtleError) throw new DOMException(r.message, "OperationError");
		return r;
	};

	// ------------------------------------------------- argument validation
	// Web Crypto rejects a bad call BEFORE it looks at whether the algorithm is
	// supported, and each error type is specified: a malformed algorithm is a
	// TypeError, a usage the algorithm cannot have is a SyntaxError, a bad key
	// length is an OperationError. None of this was checked, so calls that must
	// fail SUCCEEDED — the largest single group of WebCryptoAPI failures in the
	// Web Platform Tests, which run these cases for every algorithm.

	// The key usages each algorithm accepts, per operation. A key type that is
	// not here is validated only for "usages must be recognized".
	const GENERATE_USAGES = {
		"AES-CBC": ["encrypt", "decrypt", "wrapKey", "unwrapKey"],
		"AES-CTR": ["encrypt", "decrypt", "wrapKey", "unwrapKey"],
		"AES-GCM": ["encrypt", "decrypt", "wrapKey", "unwrapKey"],
		"AES-KW": ["wrapKey", "unwrapKey"],
		HMAC: ["sign", "verify"],
		ECDSA: ["sign", "verify"],
		ED25519: ["sign", "verify"],
		ECDH: ["deriveKey", "deriveBits"],
		X25519: ["deriveKey", "deriveBits"],
		"RSASSA-PKCS1-V1_5": ["sign", "verify"],
		"RSA-PSS": ["sign", "verify"],
		"RSA-OAEP": ["encrypt", "decrypt", "wrapKey", "unwrapKey"],
		HKDF: ["deriveKey", "deriveBits"],
		PBKDF2: ["deriveKey", "deriveBits"],
	};
	const ALL_USAGES = [
		"encrypt", "decrypt", "sign", "verify",
		"deriveKey", "deriveBits", "wrapKey", "unwrapKey",
	];

	// normalizeAlgorithm: a missing or malformed algorithm is a TypeError, which
	// is checked BEFORE support (an unsupported name is NotSupportedError).
	function normalizeAlgorithm(alg, op) {
		if (typeof alg === "string") return alg;
		if (alg === null || alg === undefined || typeof alg !== "object") {
			throw new TypeError(`Failed to execute '${op}': algorithm must be an object or a string`);
		}
		if (alg.name === undefined || alg.name === null) {
			throw new TypeError(`Failed to execute '${op}': algorithm has no name`);
		}
		return String(alg.name);
	}

	// checkUsages: every entry must be a recognized usage (SyntaxError), and for
	// an algorithm we know, one it can actually have. An EMPTY list is a
	// SyntaxError too for anything that produces a secret or private key.
	function checkUsages(name, usages, op, { allowEmpty = false } = {}) {
		const list = usages === undefined || usages === null ? [] : Array.from(usages);
		for (const u of list) {
			if (!ALL_USAGES.includes(String(u))) {
				throw new SyntaxError(`Failed to execute '${op}': invalid key usage '${u}'`);
			}
		}
		const allowed = GENERATE_USAGES[String(name).toUpperCase()];
		if (allowed) {
			for (const u of list) {
				if (!allowed.includes(String(u))) {
					throw new SyntaxError(`Failed to execute '${op}': ${name} keys cannot be used for '${u}'`);
				}
			}
		}
		if (!allowEmpty && list.length === 0) {
			throw new SyntaxError(`Failed to execute '${op}': usages cannot be empty`);
		}
		return list;
	}

	// AES accepts exactly these key lengths; anything else is an OperationError
	// (not a TypeError — the call was well formed, the value was not).
	function checkAESLength(length, op) {
		const n = Number(length);
		if (n !== 128 && n !== 192 && n !== 256) {
			throw new DOMException(`Failed to execute '${op}': invalid AES key length`, "OperationError");
		}
		return n;
	}

	const subtle = {
		async digest(alg, data) {
			return toBuf(ops.subtle_digest(hashName(alg), toU8(data)));
		},

		async generateKey(alg, extractable, usages) {
			const declared = normalizeAlgorithm(alg, "generateKey");
			const name = String(declared).toUpperCase();
			// A key pair may leave one half with no usages, so the empty check
			// applies to the pair as a whole rather than to each side.
			const isPair = ["ECDSA", "ECDH", "ED25519", "X25519", "RSASSA-PKCS1-V1_5", "RSA-PSS", "RSA-OAEP"].includes(name);
			usages = checkUsages(declared, usages, "generateKey", { allowEmpty: isPair && false });
			if (name === "HMAC") {
				const hash = hashName(alg.hash);
				const lenBits = alg.length || (hash === "SHA-384" || hash === "SHA-512" ? 1024 : 512);
				const raw = crypto.getRandomValues(new Uint8Array(Math.ceil(lenBits / 8)));
				const h = ops.subtle_hmac_import(raw);
				return new CryptoKey("secret", extractable, { name: "HMAC", hash: { name: hash }, length: lenBits }, usages, h);
			}
			if (name === "ECDSA") {
				const crv = String(alg.namedCurve);
				const r = ops.subtle_ec_generate(crv);
				const algo = { name: "ECDSA", namedCurve: crv };
				return {
					privateKey: new CryptoKey("private", extractable, algo, usages.filter((u) => u === "sign"), r.priv),
					publicKey: new CryptoKey("public", true, algo, usages.filter((u) => u === "verify"), r.pub),
				};
			}
			if (RSA_ALL.includes(name)) {
				const hash = hashName(alg.hash);
				const bits = Number(alg.modulusLength);
				const r = ops.subtle_rsa_generate(bits);
				const algo = { name: algName(alg), hash: { name: hash }, modulusLength: bits, publicExponent: new Uint8Array([1, 0, 1]) };
				const isOAEP = name === "RSA-OAEP";
				return {
					privateKey: new CryptoKey("private", extractable, algo, usages.filter((u) => isOAEP ? u === "decrypt" || u === "unwrapKey" : u === "sign"), r.priv),
					publicKey: new CryptoKey("public", true, algo, usages.filter((u) => isOAEP ? u === "encrypt" || u === "wrapKey" : u === "verify"), r.pub),
				};
			}
			if (AES_ALL.includes(name)) {
				const length = checkAESLength(alg.length === undefined ? 256 : alg.length, "generateKey");
				const raw = crypto.getRandomValues(new Uint8Array(length / 8));
				const key = new CryptoKey("secret", extractable, { name, length }, usages, null);
				keyRaw.set(key, raw);
				return key;
			}
			if (name === "ED25519") {
				const r = ops.subtle_ed_generate();
				const algo = { name: "Ed25519" };
				return {
					privateKey: new CryptoKey("private", extractable, algo, usages.filter((u) => u === "sign"), r.priv),
					publicKey: new CryptoKey("public", true, algo, usages.filter((u) => u === "verify"), r.pub),
				};
			}
			if (name === "ECDH") {
				const crv = String(alg.namedCurve);
				const r = ops.subtle_ec_generate(crv); // reuse EC keygen (same curves)
				const algo = { name: "ECDH", namedCurve: crv };
				const priv = new CryptoKey("private", extractable, algo, usages, r.priv);
				const pub = new CryptoKey("public", true, algo, [], r.pub);
				return { privateKey: priv, publicKey: pub };
			}
			unsupported(`algorithm ${algName(alg)}`);
		},

		async importKey(format, keyData, alg, extractable, usages) {
			const declared = normalizeAlgorithm(alg, "importKey");
			const name = String(declared).toUpperCase();
			// A public key may legitimately be imported with no usages.
			usages = checkUsages(declared, usages, "importKey", { allowEmpty: true });
			if (name === "HMAC") {
				let raw;
				if (format === "raw") raw = toU8(keyData);
				else if (format === "jwk") {
					if (!keyData || keyData.kty !== "oct" || typeof keyData.k !== "string") {
						throw new DOMException("importKey: not an oct JWK", "DataError");
					}
					raw = b64uDecode(keyData.k);
				} else unsupported(`HMAC key format ${format}`);
				const hash = hashName(alg.hash);
				const h = ops.subtle_hmac_import(raw);
				return new CryptoKey("secret", extractable, { name: "HMAC", hash: { name: hash }, length: raw.length * 8 }, usages, h);
			}
			if (name === "ECDSA") {
				let r;
				if (format === "jwk") r = ops.subtle_ec_import_jwk(JSON.stringify(keyData));
				else if (format === "pkcs8" || format === "spki") r = ops.subtle_ec_import_der(format, toU8(keyData));
				else unsupported(`EC key format ${format}`);
				return new CryptoKey(r.type, extractable, { name: "ECDSA", namedCurve: r.crv }, usages, r.id);
			}
			if (RSA_ALL.includes(name)) {
				let r;
				if (format === "jwk") r = ops.subtle_rsa_import_jwk(JSON.stringify(keyData));
				else if (format === "pkcs8" || format === "spki") r = ops.subtle_rsa_import_der(format, toU8(keyData));
				else unsupported(`RSA key format ${format}`);
				const algo = { name: algName(alg), hash: { name: hashName(alg.hash) }, modulusLength: r.bits, publicExponent: new Uint8Array([1, 0, 1]) };
				return new CryptoKey(r.type, extractable, algo, usages, r.id);
			}
			if (AES_ALL.includes(name)) {
				let raw;
				if (format === "raw") raw = toU8(keyData);
				else if (format === "jwk") raw = b64uDecode(keyData.k);
				else unsupported(`AES key format ${format}`);
				const key = new CryptoKey("secret", extractable, { name, length: raw.length * 8 }, usages, null);
				keyRaw.set(key, raw);
				return key;
			}
			if (name === "ED25519") {
				let r;
				if (format === "jwk") r = ops.subtle_ed_import("jwk", JSON.stringify(keyData));
				else if (["raw", "pkcs8", "spki"].includes(format)) r = ops.subtle_ed_import(format, toU8(keyData));
				else unsupported(`Ed25519 key format ${format}`);
				return new CryptoKey(r.type, extractable, { name: "Ed25519" }, usages, r.id);
			}
			if (name === "ECDH") {
				if (format !== "jwk") {
					if (format === "raw") {
						// raw public point: keep as JWK-less handle via EC import DER path unsupported; require jwk
					}
					unsupported(`ECDH key format ${format}`);
				}
				const key = new CryptoKey(keyData.d ? "private" : "public", extractable, { name: "ECDH", namedCurve: keyData.crv }, usages, null);
				keyJwk.set(key, { ...keyData, crv: keyData.crv });
				return key;
			}
			if (name === "HKDF" || name === "PBKDF2") {
				if (format !== "raw") unsupported(`${name} key format ${format}`);
				const key = new CryptoKey("secret", false, { name }, usages, null);
				keyRaw.set(key, toU8(keyData));
				return key;
			}
			unsupported(`algorithm ${algName(alg)}`);
		},

		async exportKey(format, key) {
			if (!(key instanceof CryptoKey)) throw new TypeError("expected a CryptoKey");
			if (!key.extractable) throw new DOMException("key is not extractable", "InvalidAccessError");
			const name = key.algorithm.name.toUpperCase();
			if (name === "HMAC") {
				const raw = Uint8Array.from(ops.subtle_hmac_export(key._h));
				if (format === "raw") return raw.buffer;
				if (format === "jwk") return { kty: "oct", k: b64uEncode(raw), ext: true, key_ops: [...key.usages] };
				unsupported(`HMAC export format ${format}`);
			}
			if (name === "ECDSA") {
				if (format === "jwk") return JSON.parse(ops.subtle_ec_export_jwk(key._h));
				if (format === "pkcs8" || format === "spki") return toBuf(ops.subtle_ec_export_der(format, key._h));
				unsupported(`EC export format ${format}`);
			}
			if (RSA_ALL.includes(name)) {
				if (format === "jwk") return JSON.parse(ops.subtle_rsa_export_jwk(key._h));
				if (format === "pkcs8" || format === "spki") return toBuf(ops.subtle_rsa_export_der(format, key._h));
				unsupported(`RSA export format ${format}`);
			}
			if (name === "ED25519") {
				if (format === "jwk") return JSON.parse(ops.subtle_ed_export("jwk", key._h));
				if (format === "raw") return Uint8Array.from(ops.subtle_ed_export("raw", key._h)).buffer;
				if (format === "pkcs8" || format === "spki") return toBuf(ops.subtle_ed_export(format, key._h));
				unsupported(`Ed25519 export format ${format}`);
			}
			if (AES_ALL.includes(name)) {
				if (format === "raw") return rawOf(key).slice().buffer;
				if (format === "jwk") {
					// The JWK alg encodes the AES variant AND the key size, e.g.
					// A128GCM/A192GCM/A256GCM, A256CBC, A256CTR — derive it from the
					// actual key length, not a hardcoded A256GCM.
					const bits = (rawOf(key).length * 8);
					const suffix = name === "AES-GCM" ? "GCM" : name === "AES-CBC" ? "CBC" : name === "AES-CTR" ? "CTR" : "KW";
					return { kty: "oct", k: b64uEncode(rawOf(key)), alg: `A${bits}${suffix}`, ext: true, key_ops: [...key.usages] };
				}
				unsupported(`AES export format ${format}`);
			}
			unsupported(`algorithm ${key.algorithm.name}`);
		},

		async sign(alg, key, data) {
			need(key, "sign");
			const name = algName(alg).toUpperCase();
			if (name === "HMAC") return toBuf(ops.subtle_hmac_sign(key.algorithm.hash.name, key._h, toU8(data)));
			if (name === "ECDSA") return toBuf(ops.subtle_ec_sign(hashName(alg.hash), key._h, toU8(data)));
			if (name === "ED25519") return toBuf(ops.subtle_ed_sign(key._h, toU8(data)));
			if (RSA_NAMES.includes(name)) {
				return toBuf(ops.subtle_rsa_sign(rsaScheme(name), key.algorithm.hash.name, alg.saltLength == null ? -1 : Number(alg.saltLength), key._h, toU8(data)));
			}
			unsupported(`algorithm ${algName(alg)}`);
		},

		async verify(alg, key, signature, data) {
			need(key, "verify");
			const name = algName(alg).toUpperCase();
			if (name === "HMAC") return ops.subtle_hmac_verify(key.algorithm.hash.name, key._h, toU8(signature), toU8(data));
			if (name === "ECDSA") return ops.subtle_ec_verify(hashName(alg.hash), key._h, toU8(signature), toU8(data));
			if (name === "ED25519") return ops.subtle_ed_verify(key._h, toU8(signature), toU8(data));
			if (RSA_NAMES.includes(name)) {
				return ops.subtle_rsa_verify(rsaScheme(name), key.algorithm.hash.name, alg.saltLength == null ? -1 : Number(alg.saltLength), key._h, toU8(signature), toU8(data));
			}
			unsupported(`algorithm ${algName(alg)}`);
		},

		async encrypt(alg, key, data) {
			need(key, "encrypt");
			const name = algName(alg).toUpperCase();
			if (AES_NAMES.includes(name)) {
				const iv = toU8(name === "AES-CTR" ? alg.counter : alg.iv);
				const aad = alg.additionalData ? toU8(alg.additionalData) : new Uint8Array(0);
				const tagLen = alg.tagLength ?? 128;
				return toBuf(subtleFail(ops.subtle_aes_encrypt(name, rawOf(key), iv, toU8(data), aad, tagLen, Number(name === "AES-CTR" ? (alg.length ?? 128) : 128))));
			}
			if (name === "RSA-OAEP") {
				const label = alg.label ? toU8(alg.label) : new Uint8Array(0);
				return toBuf(subtleFail(ops.subtle_rsa_oaep(true, key._h, key.algorithm.hash.name, toU8(data), label)));
			}
			unsupported(`encrypt algorithm ${algName(alg)}`);
		},

		async decrypt(alg, key, data) {
			need(key, "decrypt");
			const name = algName(alg).toUpperCase();
			if (AES_NAMES.includes(name)) {
				const iv = toU8(name === "AES-CTR" ? alg.counter : alg.iv);
				const aad = alg.additionalData ? toU8(alg.additionalData) : new Uint8Array(0);
				const tagLen = alg.tagLength ?? 128;
				return toBuf(subtleFail(ops.subtle_aes_decrypt(name, rawOf(key), iv, toU8(data), aad, tagLen, Number(name === "AES-CTR" ? (alg.length ?? 128) : 128))));
			}
			if (name === "RSA-OAEP") {
				const label = alg.label ? toU8(alg.label) : new Uint8Array(0);
				return toBuf(subtleFail(ops.subtle_rsa_oaep(false, key._h, key.algorithm.hash.name, toU8(data), label)));
			}
			unsupported(`decrypt algorithm ${algName(alg)}`);
		},

		// wrapKey exports the key material, then encrypts it with the wrapping
		// key (spec: export to `format`, encrypt with `wrapAlg`). The internal
		// encrypt/decrypt bypass the encrypt/decrypt usage gate — the wrapping
		// key only carries wrapKey/unwrapKey usages.
		async wrapKey(format, key, wrappingKey, wrapAlg) {
			need(wrappingKey, "wrapKey");
			const exported = await subtle.exportKey(format, key);
			const bytes = format === "jwk" ? new TextEncoder().encode(JSON.stringify(exported)) : new Uint8Array(exported);
			return toBuf(rawCrypt(true, wrapAlg, wrappingKey, bytes));
		},

		async unwrapKey(format, wrappedKey, unwrappingKey, unwrapAlg, keyAlg, extractable, usages) {
			need(unwrappingKey, "unwrapKey");
			const decrypted = new Uint8Array(rawCrypt(false, unwrapAlg, unwrappingKey, toU8(wrappedKey)));
			const material = format === "jwk"
				? JSON.parse(new TextDecoder().decode(decrypted))
				: decrypted;
			return subtle.importKey(format, material, keyAlg, extractable, usages);
		},

		async deriveBits(alg, baseKey, length) {
			need(baseKey, "deriveBits");
			return deriveBitsRaw(alg, baseKey, length);
		},

		async deriveKey(alg, baseKey, derivedKeyAlg, extractable, usages) {
			// Gate on deriveKey only — WebCrypto does NOT require the base key to
			// also carry deriveBits. Use the internal ungated deriveBitsRaw so a
			// key imported with just ["deriveKey"] (the canonical PBKDF2 pattern)
			// works, mirroring how wrapKey uses rawCrypt.
			need(baseKey, "deriveKey");
			const derivedName = algName(derivedKeyAlg).toUpperCase();
			// WebCrypto "get key length": for HMAC an omitted length defaults to the
			// hash's BLOCK size (512 bits for SHA-1/SHA-256, 1024 for SHA-384/SHA-512),
			// NOT a flat 256 — mirror the generateKey HMAC default above so a derived
			// HMAC key matches the material every compliant engine produces.
			let length = Number(derivedKeyAlg.length);
			if (!length) {
				if (derivedName === "HMAC") {
					const h = hashName(derivedKeyAlg.hash);
					length = (h === "SHA-384" || h === "SHA-512") ? 1024 : 512;
				} else {
					length = 256; // AES fallback
				}
			}
			const bits = await deriveBitsRaw(alg, baseKey, length);
			if (AES_NAMES.includes(derivedName)) {
				return subtle.importKey("raw", bits, { name: derivedName }, extractable, usages);
			}
			if (derivedName === "HMAC") {
				return subtle.importKey("raw", bits, { name: "HMAC", hash: derivedKeyAlg.hash }, extractable, usages);
			}
			unsupported(`deriveKey derived algorithm ${derivedName}`);
		},
	};

	// rawCrypt runs AES/RSA-OAEP encrypt or decrypt WITHOUT the usage gate,
	// for the internal wrapKey/unwrapKey path. Returns the raw op result
	// (bytes), throwing on OperationError.
	function rawCrypt(encrypt, alg, key, data) {
		const name = algName(alg).toUpperCase();
		if (name === "AES-KW") {
			return subtleFail(ops.subtle_aes_kw(encrypt, rawOf(key), toU8(data)));
		}
		if (AES_NAMES.includes(name)) {
			const iv = toU8(name === "AES-CTR" ? alg.counter : alg.iv);
			const aad = alg.additionalData ? toU8(alg.additionalData) : new Uint8Array(0);
			const tagLen = alg.tagLength ?? 128;
			const op = encrypt ? ops.subtle_aes_encrypt : ops.subtle_aes_decrypt;
			return subtleFail(op(name, rawOf(key), iv, toU8(data), aad, tagLen, Number(name === "AES-CTR" ? (alg.length ?? 128) : 128)));
		}
		if (name === "RSA-OAEP") {
			const label = alg.label ? toU8(alg.label) : new Uint8Array(0);
			return subtleFail(ops.subtle_rsa_oaep(encrypt, key._h, key.algorithm.hash.name, toU8(data), label));
		}
		throw new DOMException(`unsupported wrap algorithm ${name}`, "NotSupportedError");
	}

	// jwkOf returns a key's JWK JSON string, whether it was imported (has
	// _jwk) or generated (has an EC handle to export).
	function jwkOf(key) {
		const j = jwkObjOf(key); if (j) return JSON.stringify(j);
		if (key._h != null) return ops.subtle_ec_export_jwk(key._h);
		throw new DOMException("key has no derivable material", "InvalidAccessError");
	}

	globalThis.crypto.subtle = subtle;
})();
