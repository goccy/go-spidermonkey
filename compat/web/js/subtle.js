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

	// "Get a copy of the buffer source" yields the bytes it HAS. A buffer
	// detached before the copy — which a caller can arrange from a getter on the
	// algorithm, since normalization runs first — has none, and the copy is
	// empty rather than an error: the operation then proceeds on no input, which
	// is exactly what the spec describes and what callers observe elsewhere.
	const toU8 = (data) => {
		if (data instanceof ArrayBuffer) {
			try { return new Uint8Array(data); } catch { return new Uint8Array(0); }
		}
		if (ArrayBuffer.isView(data)) {
			try { return new Uint8Array(data.buffer, data.byteOffset, data.byteLength); }
			catch { return new Uint8Array(0); }
		}
		throw new TypeError("expected a BufferSource");
	};
	const toBuf = (arr) => Uint8Array.from(arr).buffer;

	const HASHES = ["SHA-1", "SHA-256", "SHA-384", "SHA-512", "SHA3-256", "SHA3-384", "SHA3-512"];
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
	const rawOf = (k) => keyRaw.get(k);

	// CryptoKey is [[Serializable]]: structuredClone (and postMessage) must
	// produce a key that still works, so the clone needs the parts that are not
	// own properties of the object. The host handle is SHARED — handles are owned
	// by the host's key table, not by the JS object that names one — while raw
	// material is copied, because it lives in the WeakMap above and a clone that
	// shared the array would let a write through one key be seen through the
	// other.
	Object.defineProperty(CryptoKey.prototype, Symbol.for("go-spidermonkey.structuredClone"), {
		value(deep) {
			const out = new CryptoKey(this.type, this.extractable, deep(this.algorithm), this.usages, this._h);
			const raw = keyRaw.get(this);
			if (raw !== undefined) keyRaw.set(out, raw.slice());
			return out;
		},
		configurable: true,
	});

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
		if (name === "X25519") {
			if (!alg.public || !(alg.public instanceof CryptoKey)) {
				throw new TypeError("X25519 deriveBits needs a public key");
			}
			return toBuf(subtleFail(ops.subtle_x25519_derive(baseKey._h, alg.public._h, length ?? 0)));
		}
		if (name === "X448") {
			if (!alg.public || !(alg.public instanceof CryptoKey)) {
				throw new TypeError("X448 deriveBits needs a public key");
			}
			return toBuf(subtleFail(ops.subtle_x448_derive(baseKey._h, alg.public._h, length ?? 0)));
		}
		if (name === "ECDH") {
			if (!alg.public || !(alg.public instanceof CryptoKey)) {
				throw new TypeError("ECDH deriveBits needs a public key");
			}
			return toBuf(subtleFail(ops.subtle_ecdh(baseKey._h, alg.public._h, length ?? 0)));
		}
		// A KDF has no natural output size, so it cannot fall back to "all of it"
		// the way ECDH can: a null or zero length is an OperationError.
		if (name === "HKDF" || name === "PBKDF2") {
			// A NULL length is the error — the KDF has no natural output size to
			// fall back on. Zero is a legal request and yields an empty result;
			// treating it as an error broke every "with 0 length" case.
			if (length === null || length === undefined) {
				throw new DOMException(`Failed to execute 'deriveBits': ${name} needs an explicit length`, "OperationError");
			}
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
	// AES-OCB is an AES key in every respect that import/export/generate care
	// about — same lengths, same JWK `alg` shape — so it joins AES_ALL. It stays
	// out of AES_NAMES because those route to the AES cipher op and OCB has its
	// own.
	const AES_ALL = [...AES_NAMES, "AES-KW", "AES-OCB"];
	// ChaCha20-Poly1305 is a secret-key AEAD like the AES modes, but nothing
	// about it is negotiable: one key size, one nonce size, one tag size. The
	// canonical spelling is mixed-case, and a key reports the name it was asked
	// for, so it cannot be carried as the uppercased lookup key.
	const CHACHA = "CHACHA20-POLY1305";
	// A key reports the algorithm's REGISTERED spelling, not the one the caller
	// happened to type. Algorithm names are matched case-insensitively, so
	// generateKey({name: "rsa-oaep"}) is a valid RSA-OAEP request — and the key
	// it produces must say "RSA-OAEP". Echoing the caller's casing back made
	// every such call fail its own identity check.
	const CANONICAL_NAMES = {
		[CHACHA]: "ChaCha20-Poly1305",
		"RSASSA-PKCS1-V1_5": "RSASSA-PKCS1-v1_5",
		"ED25519": "Ed25519",
		"ED448": "Ed448",
	};
	const canonicalName = (upper) => CANONICAL_NAMES[String(upper).toUpperCase()] || String(upper).toUpperCase();
	// KMAC128/KMAC256 are keyed hashes: secret keys that sign and verify, with
	// the digest length chosen per call rather than fixed by the algorithm.
	const KMAC_NAMES = ["KMAC128", "KMAC256"];

	// A host failure carries the DOMException name the spec asks for in its own
	// field. Which name it is matters — a malformed key must be a DataError, not
	// the OperationError everything used to collapse into — so it is passed as
	// data rather than written into the message and read back out of it.
	const subtleFail = (r) => {
		if (r && r.__subtleError) throw new DOMException(r.message, r.name);
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
	// The post-quantum key pairs: ML-KEM (FIPS 203) encapsulates, ML-DSA
	// (FIPS 204) signs. Both use the "AKP" key type, whose private form is the
	// generation seed rather than an expanded key.
	const MLKEM_NAMES = ["ML-KEM-512", "ML-KEM-768", "ML-KEM-1024"];
	const MLKEM_USAGES = ["encapsulateKey", "encapsulateBits", "decapsulateKey", "decapsulateBits"];
	const MLDSA_NAMES = ["ML-DSA-44", "ML-DSA-65", "ML-DSA-87"];

	const GENERATE_USAGES = {
		"ED448": ["sign", "verify"],
		"X448": ["deriveKey", "deriveBits"],
		"SHA-1": [], "SHA-256": [], "SHA-384": [], "SHA-512": [],
		"TURBOSHAKE128": [], "TURBOSHAKE256": [], KT128: [], KT256: [],
		"SHA3-256": [], "SHA3-384": [], "SHA3-512": [], CSHAKE128: [], CSHAKE256: [],
		"AES-CBC": ["encrypt", "decrypt", "wrapKey", "unwrapKey"],
		"AES-CTR": ["encrypt", "decrypt", "wrapKey", "unwrapKey"],
		"AES-GCM": ["encrypt", "decrypt", "wrapKey", "unwrapKey"],
		"AES-KW": ["wrapKey", "unwrapKey"],
		"AES-OCB": ["encrypt", "decrypt", "wrapKey", "unwrapKey"],
		[CHACHA]: ["encrypt", "decrypt", "wrapKey", "unwrapKey"],
		KMAC128: ["sign", "verify"],
		KMAC256: ["sign", "verify"],
		"ML-DSA-44": ["sign", "verify"],
		"ML-DSA-65": ["sign", "verify"],
		"ML-DSA-87": ["sign", "verify"],
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
		"ML-KEM-512": MLKEM_USAGES,
		"ML-KEM-768": MLKEM_USAGES,
		"ML-KEM-1024": MLKEM_USAGES,
	};
	// Every algorithm the Web Crypto spec defines, whether or not this runtime
	// implements it. The distinction decides the ERROR ORDER: an algorithm the
	// spec does not know is NotSupportedError before usages are looked at, while
	// one it does know has its usages validated first — so a bad usage on
	// AES-KW is a SyntaxError even here, where AES-KW is not implemented.
	const KNOWN_ALGORITHMS = new Set([
		"RSASSA-PKCS1-V1_5", "RSA-PSS", "RSA-OAEP",
		"ECDSA", "ECDH", "ED25519", "ED448", "X25519", "X448",
		"AES-CTR", "AES-CBC", "AES-GCM", "AES-KW",
		"ML-KEM-512", "ML-KEM-768", "ML-KEM-1024",
		"HMAC", "SHA-1", "SHA-256", "SHA-384", "SHA-512",
		"HKDF", "PBKDF2",
		"CHACHA20-POLY1305", "KMAC128", "KMAC256", "AES-OCB",
		"TURBOSHAKE128", "TURBOSHAKE256", "KT128", "KT256",
		// The digest-only algorithms. They are names an operation can be asked for,
		// so they belong here even though no key is ever made for them.
		"SHA3-256", "SHA3-384", "SHA3-512", "CSHAKE128", "CSHAKE256",
		"ML-DSA-44", "ML-DSA-65", "ML-DSA-87",
	]);

	const ALL_USAGES = [
		"encrypt", "decrypt", "sign", "verify",
		"deriveKey", "deriveBits", "wrapKey", "unwrapKey",
		"encapsulateKey", "encapsulateBits", "decapsulateKey", "decapsulateBits",
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
		// An algorithm the spec does not define fails as unsupported FIRST: the
		// usages of a nonexistent algorithm are not a meaningful question.
		if (!KNOWN_ALGORITHMS.has(String(name).toUpperCase())) {
			throw new DOMException(`Failed to execute '${op}': unsupported algorithm ${name}`, "NotSupportedError");
		}
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

	// Algorithms whose parameters name ANOTHER algorithm — the hash — which has
	// to be a real one. `{name:"HMAC", hash:"MD5"}` is NotSupportedError, and it
	// is decided BEFORE the usages: the suite pairs a bad algorithm with bad
	// usages precisely to check that order.
	const KNOWN_HASHES = new Set(["SHA-1", "SHA-256", "SHA-384", "SHA-512"]);
	// HKDF and PBKDF2 are NOT here: their key carries no hash — it is chosen at
	// derive time — so requiring one at import rejects every correct call.
	const NEEDS_HASH = new Set([
		"HMAC", "RSASSA-PKCS1-V1_5", "RSA-PSS", "RSA-OAEP",
	]);

	// The curves an EC algorithm can name. Spelled exactly: unlike an algorithm
	// name, a namedCurve is compared case-sensitively.
	const EC_CURVES = new Set(["P-256", "P-384", "P-521"]);

	function checkInnerAlgorithms(alg, name, op) {
		const upper = String(name).toUpperCase();
		// The curve is part of the algorithm, so a curve this platform does not
		// have is NotSupportedError — the same answer an unknown algorithm gets,
		// and reached before the usages are looked at. Unchecked, a bad curve
		// surfaced as whatever the host happened to say about it.
		if (upper === "ECDSA" || upper === "ECDH") {
			const crv = alg === null || typeof alg !== "object" ? undefined : alg.namedCurve;
			if (crv !== undefined && !EC_CURVES.has(String(crv))) {
				throw new DOMException(
					`Failed to execute '${op}': unsupported curve ${String(crv)}`, "NotSupportedError");
			}
		}
		if (!NEEDS_HASH.has(upper)) return;
		if (alg === null || typeof alg !== "object") return;
		const h = alg.hash;
		// The hash is required for these, and must name a hash this platform has.
		const hName = h === null || h === undefined
			? undefined
			: String(typeof h === "object" ? h.name : h).toUpperCase();
		if (hName === undefined || !KNOWN_HASHES.has(hName)) {
			throw new DOMException(
				`Failed to execute '${op}': unsupported hash ${hName ?? "(missing)"}`, "NotSupportedError");
		}
	}

	// A derivation's base key must belong to the algorithm being asked for:
	// handing ECDH an RSA key (or an ECDH key to HKDF) is an InvalidAccessError,
	// not an unsupported-algorithm error. Unchecked, those calls proceeded and
	// the suite's "wrong key" cases all reported "should have thrown".
	function checkDeriveKeyMatches(alg, baseKey, op) {
		const want = String(normalizeAlgorithm(alg, op)).toUpperCase();
		const have = String((baseKey && baseKey.algorithm && baseKey.algorithm.name) || "").toUpperCase();
		if (have && want && have !== want) {
			throw new DOMException(
				`Failed to execute '${op}': the key is for ${have}, not ${want}`, "InvalidAccessError");
		}
	}

	// RSA generateKey parameters that the platform cannot honour are an
	// OperationError: a modulus that is not a whole number of bytes (or is far
	// too small), or a public exponent outside the two values every
	// implementation supports.
	function checkRSAParams(alg, op) {
		const bits = Number(alg.modulusLength);
		if (!Number.isFinite(bits) || bits < 256 || bits % 8 !== 0) {
			throw new DOMException(`Failed to execute '${op}': unsupported RSA modulus length`, "OperationError");
		}
		const e = alg.publicExponent;
		if (!(e instanceof Uint8Array) && !ArrayBuffer.isView(e) && !(e instanceof ArrayBuffer)) {
			throw new TypeError(`Failed to execute '${op}': publicExponent must be a BufferSource`);
		}
		const bytes = toU8(e);
		// Strip leading zeros. Only 65537 (0x010001) is accepted, because that
		// is the only exponent the host generator can produce — claiming to
		// honour another value and returning a 65537 key would be worse than
		// refusing it.
		let i = 0;
		while (i < bytes.length - 1 && bytes[i] === 0) i++;
		const v = bytes.slice(i);
		if (!(v.length === 3 && v[0] === 1 && v[1] === 0 && v[2] === 1)) {
			throw new DOMException(`Failed to execute '${op}': unsupported RSA public exponent`, "OperationError");
		}
	}

	// A derivation produces whole bytes, so a bit length that is not a multiple
	// of 8 is an OperationError rather than a silently rounded result.
	function checkDerivedLength(length, op) {
		if (length === null || length === undefined) return length;
		const n = Number(length);
		if (!Number.isFinite(n) || n < 0 || n % 8 !== 0) {
			throw new DOMException(`Failed to execute '${op}': length must be a multiple of 8`, "OperationError");
		}
		return n;
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

	// A JWK carries its own policy, and importKey must honour it: a key marked
	// non-extractable cannot be imported as extractable, key_ops must cover
	// every requested usage, and "use" must agree with them. Each mismatch is a
	// DataError. Unchecked, all of these imports SUCCEEDED, which is the
	// "operation succeeded, but should not have" family in the suite.
	const JWK_USE_USAGES = {
		enc: ["encrypt", "decrypt", "wrapKey", "unwrapKey", "deriveKey", "deriveBits"],
		sig: ["sign", "verify"],
	};

	function checkJWKConsistency(jwk, usages, extractable) {
		if (!jwk || typeof jwk !== "object") {
			throw new DOMException("importKey: JWK must be an object", "DataError");
		}
		if (jwk.ext === false && extractable) {
			throw new DOMException("importKey: JWK is not extractable", "DataError");
		}
		if (jwk.key_ops !== undefined) {
			if (!Array.isArray(jwk.key_ops)) {
				throw new DOMException("importKey: JWK key_ops must be a sequence", "DataError");
			}
			if (new Set(jwk.key_ops).size !== jwk.key_ops.length) {
				throw new DOMException("importKey: JWK key_ops has duplicates", "DataError");
			}
			for (const u of usages) {
				if (!jwk.key_ops.includes(u)) {
					throw new DOMException(`importKey: JWK key_ops does not permit '${u}'`, "DataError");
				}
			}
		}
		if (jwk.use !== undefined && usages.length > 0) {
			const allowed = JWK_USE_USAGES[jwk.use];
			if (!allowed) throw new DOMException(`importKey: unrecognized JWK use '${jwk.use}'`, "DataError");
			for (const u of usages) {
				if (!allowed.includes(u)) {
					throw new DOMException(`importKey: JWK use '${jwk.use}' does not permit '${u}'`, "DataError");
				}
			}
		}
	}

	// An operation names an algorithm, and the key names one too; WebCrypto
	// requires them to be the SAME and rejects the call with InvalidAccessError
	// when they are not. Unchecked, encrypting with (say) an AES-CBC key under
	// {name:"AES-GCM"} reached the cipher and failed with whatever the host
	// happened to say.
	// The name is passed IN, already read. An algorithm's `name` is normalized
	// exactly once per operation, and reading it again is observable: a caller
	// can make it a getter with side effects, and the WPT suite does precisely
	// that to detach a buffer mid-call. Reading it twice ran those side effects
	// twice.
	// What each half of an asymmetric key pair may be used for. Absent from the
	// table means the algorithm has no halves (a secret key) and the union check
	// already covered it.
	const HALF_USAGES = {
		ECDSA: { public: ["verify"], private: ["sign"] },
		ECDH: { public: [], private: ["deriveKey", "deriveBits"] },
		ED25519: { public: ["verify"], private: ["sign"] },
		ED448: { public: ["verify"], private: ["sign"] },
		X25519: { public: [], private: ["deriveKey", "deriveBits"] },
		X448: { public: [], private: ["deriveKey", "deriveBits"] },
		"RSASSA-PKCS1-V1_5": { public: ["verify"], private: ["sign"] },
		"RSA-PSS": { public: ["verify"], private: ["sign"] },
		"RSA-OAEP": { public: ["encrypt", "wrapKey"], private: ["decrypt", "unwrapKey"] },
		"ML-DSA-44": { public: ["verify"], private: ["sign"] },
		"ML-DSA-65": { public: ["verify"], private: ["sign"] },
		"ML-DSA-87": { public: ["verify"], private: ["sign"] },
		"ML-KEM-512": { public: ["encapsulateKey", "encapsulateBits"], private: ["decapsulateKey", "decapsulateBits"] },
		"ML-KEM-768": { public: ["encapsulateKey", "encapsulateBits"], private: ["decapsulateKey", "decapsulateBits"] },
		"ML-KEM-1024": { public: ["encapsulateKey", "encapsulateBits"], private: ["decapsulateKey", "decapsulateBits"] },
	};

	function checkHalfUsages(name, isPublic, usages, format) {
		const halves = HALF_USAGES[name];
		// A raw import of a secret key has no halves to distinguish.
		if (!halves || format === "raw-secret") return;
		// A seed IS the private key material, whatever the caller calls it.
		if (format === "raw-seed") isPublic = false;
		const allowed = isPublic ? halves.public : halves.private;
		for (const u of usages) {
			if (!allowed.includes(String(u))) {
				throw new DOMException(
					`Failed to execute 'importKey': ${isPublic ? "public" : "private"} ${name} keys cannot be used for ${u}`,
					"SyntaxError");
			}
		}
	}

	// ML-DSA signs over an optional context string, which binds a signature to
	// the protocol that asked for it. It is absent far more often than not.
	function mldsaContext(alg) {
		return alg && alg.context !== undefined ? toU8(alg.context) : new Uint8Array(0);
	}

	function checkKeyAlgMatches(name, key, op) {
		const want = String(name).toUpperCase();
		const have = String((key && key.algorithm && key.algorithm.name) || "").toUpperCase();
		if (have && want && have !== want) {
			throw new DOMException(
				`Failed to execute '${op}': the key is for ${have}, not ${want}`, "InvalidAccessError");
		}
	}

	// The body of exportKey, without its extractable check. getPublicKey needs
	// exactly this: a private key's public half is not a secret, so it can be
	// written out even when the private key it came from cannot.
	async function exportInner(format, key) {
		const name = key.algorithm.name.toUpperCase();
		if (MLDSA_NAMES.includes(name)) {
			const r = subtleFail(ops.subtle_mldsa_export(format, key._h));
			return format === "jwk" ? { ...r, ext: key.extractable, key_ops: [...key.usages] } : Uint8Array.from(r).buffer;
		}
		if (MLKEM_NAMES.includes(name)) {
			const r = subtleFail(ops.subtle_mlkem_export(format, key._h));
			return format === "jwk" ? { ...r, ext: key.extractable, key_ops: [...key.usages] } : Uint8Array.from(r).buffer;
		}
		if (name === "X25519") {
			const r = subtleFail(ops.subtle_x25519_export(format, key._h));
			return format === "jwk" ? { ...r, ext: true, key_ops: [...key.usages] } : Uint8Array.from(r).buffer;
		}
		if (name === "ED448" || name === "X448") {
			const r = subtleFail((name === "ED448" ? ops.subtle_ed448_export : ops.subtle_x448_export)(format, key._h));
			return format === "jwk" ? { ...r, ext: true, key_ops: [...key.usages] } : Uint8Array.from(r).buffer;
		}
		if (name === "HMAC") {
			const raw = Uint8Array.from(ops.subtle_hmac_export(key._h));
			if (format === "raw") return raw.buffer;
			if (format === "jwk") return { kty: "oct", k: b64uEncode(raw), ext: true, key_ops: [...key.usages] };
			unsupported(`HMAC export format ${format}`);
		}
		if (name === "ECDSA" || name === "ECDH") {
			if (format === "jwk") return JSON.parse(ops.subtle_ec_export_jwk(key._h));
			if (format === "raw") return toBuf(subtleFail(ops.subtle_ec_export_der("raw", key._h)));
			if (format === "pkcs8" || format === "spki") return toBuf(subtleFail(ops.subtle_ec_export_der(format, key._h)));
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
			if (format === "raw" || format === "raw-secret") return rawOf(key).slice().buffer;
			if (format === "jwk") {
				// The JWK alg encodes the AES variant AND the key size, e.g.
				// A128GCM/A192GCM/A256GCM, A256CBC, A256CTR — derive it from the
				// actual key length, not a hardcoded A256GCM.
				const bits = (rawOf(key).length * 8);
				const suffix = name === "AES-GCM" ? "GCM" : name === "AES-CBC" ? "CBC" : name === "AES-CTR" ? "CTR" : name === "AES-OCB" ? "OCB" : "KW";
				return { kty: "oct", k: b64uEncode(rawOf(key)), alg: `A${bits}${suffix}`, ext: true, key_ops: [...key.usages] };
			}
			unsupported(`AES export format ${format}`);
		}
		if (KMAC_NAMES.includes(name)) {
			if (format === "raw" || format === "raw-secret") return rawOf(key).slice().buffer;
			if (format === "jwk") {
				return { kty: "oct", k: b64uEncode(rawOf(key)), alg: `K${name.substring(4)}`, ext: true, key_ops: [...key.usages] };
			}
			unsupported(`${name} export format ${format}`);
		}
		if (name === CHACHA) {
			// No `alg` in the JWK: unlike AES there is no registered name for
			// it, because there is only one variant to name.
			if (format === "raw" || format === "raw-secret") return rawOf(key).slice().buffer;
			if (format === "jwk") return { kty: "oct", k: b64uEncode(rawOf(key)), ext: true, key_ops: [...key.usages] };
			unsupported(`ChaCha20-Poly1305 export format ${format}`);
		}
		unsupported(`algorithm ${key.algorithm.name}`);
	}

	const subtle = {
		async digest(alg, data) {
			// The name is read ONCE: the suite makes it a getter that detaches the
			// data buffer, so a second read runs that side effect a second time.
			const declared = algName(alg);
			// A missing name is a TypeError — the request is malformed — and not the
			// NotSupportedError that an algorithm this runtime lacks would get.
			if (declared === undefined || declared === null || declared === "") {
				throw new TypeError("Failed to execute 'digest': the algorithm has no name");
			}
			const name = String(declared).toUpperCase();
			if (name === "TURBOSHAKE128" || name === "TURBOSHAKE256") {
				// The domain separation byte is the caller's; 0x1F is the default the
				// RFC assigns when nothing else claims one.
				const d = alg.domainSeparation === undefined ? 0x1f : Number(alg.domainSeparation);
				return toBuf(subtleFail(ops.subtle_turboshake(
					name === "TURBOSHAKE128" ? 128 : 256, d, toU8(data), Number(alg.outputLength))));
			}
			if (name === "KT128" || name === "KT256") {
				const custom = alg.customization === undefined ? new Uint8Array(0) : toU8(alg.customization);
				return toBuf(subtleFail(ops.subtle_kangarootwelve(
					name === "KT128" ? 128 : 256, toU8(data), custom, Number(alg.outputLength))));
			}
			if (name === "CSHAKE128" || name === "CSHAKE256") {
				const bits = alg.outputLength === undefined ? (name === "CSHAKE128" ? 256 : 512) : Number(alg.outputLength);
				const custom = alg.customization === undefined ? new Uint8Array(0) : toU8(alg.customization);
				return toBuf(subtleFail(ops.subtle_cshake(name === "CSHAKE128" ? 128 : 256, toU8(data), custom, bits)));
			}
			// `name` is already in hand: reading alg.name again would run a
			// caller's getter a second time, and the suite uses one to mutate the
			// input mid-call.
			return toBuf(ops.subtle_digest(hashName(name), toU8(data)));
		},

		async generateKey(alg, extractable, usages) {
			const declared = normalizeAlgorithm(alg, "generateKey");
			const name = String(declared).toUpperCase();
			// A key pair may leave one half with no usages, so the empty check
			// applies to the pair as a whole rather than to each side.
			const isPair = ["ECDSA", "ECDH", "ED25519", "ED448", "X25519", "X448", "RSASSA-PKCS1-V1_5", "RSA-PSS", "RSA-OAEP",
				"ML-DSA-44", "ML-DSA-65", "ML-DSA-87", ...MLKEM_NAMES].includes(name);
			// The ALGORITHM is validated in full before the usages are looked at:
			// WebCrypto normalizes (and so rejects) the algorithm first, and the
			// suite pairs a bad parameter with bad usages precisely to check which
			// error wins.
			checkInnerAlgorithms(alg, name, "generateKey");
			if (RSA_ALL.includes(name)) checkRSAParams(alg, "generateKey");
			if (AES_ALL.includes(name)) checkAESLength(alg.length === undefined ? 256 : alg.length, "generateKey");
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
				const r = subtleFail(ops.subtle_rsa_generate(bits));
				const algo = { name: canonicalName(algName(alg)), hash: { name: hash }, modulusLength: bits, publicExponent: new Uint8Array([1, 0, 1]) };
				const isOAEP = name === "RSA-OAEP";
				return {
					privateKey: new CryptoKey("private", extractable, algo, usages.filter((u) => isOAEP ? u === "decrypt" || u === "unwrapKey" : u === "sign"), r.priv),
					publicKey: new CryptoKey("public", true, algo, usages.filter((u) => isOAEP ? u === "encrypt" || u === "wrapKey" : u === "verify"), r.pub),
				};
			}
			if (AES_ALL.includes(name)) {
				const length = checkAESLength(alg.length === undefined ? 256 : alg.length, "generateKey");
				const raw = crypto.getRandomValues(new Uint8Array(length / 8));
				const key = new CryptoKey("secret", extractable, { name: canonicalName(name), length }, usages, null);
				keyRaw.set(key, raw);
				return key;
			}
			if (KMAC_NAMES.includes(name)) {
				// The default key length is the variant's security strength, which
				// is what "KMAC128" and "KMAC256" name.
				const bits = alg.length === undefined ? (name === "KMAC128" ? 128 : 256) : Number(alg.length);
				if (!Number.isInteger(bits) || bits <= 0 || bits % 8 !== 0) {
					throw new DOMException(`Failed to execute 'generateKey': ${name} length must be a positive multiple of 8`, "OperationError");
				}
				const raw = crypto.getRandomValues(new Uint8Array(bits / 8));
				const key = new CryptoKey("secret", extractable, { name: canonicalName(name), length: bits }, usages, null);
				keyRaw.set(key, raw);
				return key;
			}
			if (name === CHACHA) {
				const raw = crypto.getRandomValues(new Uint8Array(32));
				const key = new CryptoKey("secret", extractable, { name: canonicalName(name) }, usages, null);
				keyRaw.set(key, raw);
				return key;
			}
			if (name === "ED448") {
				const r = subtleFail(ops.subtle_ed448_generate());
				const algo = { name: "Ed448" };
				return {
					privateKey: new CryptoKey("private", extractable, algo, usages.filter((u) => u === "sign"), r.priv),
					publicKey: new CryptoKey("public", true, algo, usages.filter((u) => u === "verify"), r.pub),
				};
			}
			if (name === "X448") {
				const r = subtleFail(ops.subtle_x448_generate());
				const algo = { name: "X448" };
				return {
					privateKey: new CryptoKey("private", extractable, algo,
						usages.filter((u) => u === "deriveKey" || u === "deriveBits"), r.priv),
					publicKey: new CryptoKey("public", true, algo, [], r.pub),
				};
			}
			if (name === "ED25519") {
				const r = ops.subtle_ed_generate();
				const algo = { name: "Ed25519" };
				return {
					privateKey: new CryptoKey("private", extractable, algo, usages.filter((u) => u === "sign"), r.priv),
					publicKey: new CryptoKey("public", true, algo, usages.filter((u) => u === "verify"), r.pub),
				};
			}
			if (MLDSA_NAMES.includes(name)) {
				const r = subtleFail(ops.subtle_mldsa_generate(name));
				const algo = { name: r.name };
				return {
					privateKey: new CryptoKey("private", extractable, algo, usages.filter((u) => u === "sign"), r.priv),
					publicKey: new CryptoKey("public", true, algo, usages.filter((u) => u === "verify"), r.pub),
				};
			}
			if (MLKEM_NAMES.includes(name)) {
				// `name` and not alg.name: the caller's spelling is only required to
				// match case-insensitively, and the host looks the parameter set up
				// by its registered name.
				const r = subtleFail(ops.subtle_mlkem_generate(name));
				const algo = { name: r.name };
				return {
					privateKey: new CryptoKey("private", extractable, algo,
						usages.filter((u) => u === "decapsulateKey" || u === "decapsulateBits"), r.priv),
					publicKey: new CryptoKey("public", true, algo,
						usages.filter((u) => u === "encapsulateKey" || u === "encapsulateBits"), r.pub),
				};
			}
			if (name === "X25519") {
				const r = subtleFail(ops.subtle_x25519_generate());
				const algo = { name: "X25519" };
				return {
					privateKey: new CryptoKey("private", extractable, algo, usages, r.priv),
					publicKey: new CryptoKey("public", true, algo, [], r.pub),
				};
			}
			if (name === "ECDH") {
				const crv = String(alg.namedCurve);
				const r = subtleFail(ops.subtle_ec_generate(crv)); // reuse EC keygen (same curves)
				const algo = { name: canonicalName(algName(alg)), namedCurve: crv };
				return {
					privateKey: new CryptoKey("private", extractable, algo, usages, r.priv),
					publicKey: new CryptoKey("public", true, algo, [], r.pub),
				};
			}
			unsupported(`algorithm ${algName(alg)}`);
		},

		async importKey(format, keyData, alg, extractable, usages) {
			const declared = normalizeAlgorithm(alg, "importKey");
			const name = String(declared).toUpperCase();
			checkInnerAlgorithms(alg, name, "importKey");
			// Empty usages are legal ONLY for a public key — a secret or private
			// key that can do nothing is a SyntaxError. Which one is being
			// imported follows from the format, except for JWK where the payload
			// says so ("oct" is secret, a private JWK carries "d").
			// "raw-public" and "raw-seed" name the half outright; the rest is
			// inferred: spki is public, pkcs8 private, a bare "raw" is public for
			// the algorithms whose raw form is a public point, and a JWK says so
			// itself by carrying "d" or not.
			const isPublicImport = format === "spki" || format === "raw-public" ||
				// A private JWK carries "d" (EC/RSA/OKP) or "priv" (AKP, the
				// post-quantum key type); either one means this is the private half.
				(format === "jwk" && keyData && keyData.kty !== "oct" && keyData.d === undefined && keyData.priv === undefined) ||
				(format === "raw" && ["ECDH", "ECDSA", "X25519", "ED25519", "X448", "ED448"].includes(name));
			usages = checkUsages(declared, usages, "importKey", { allowEmpty: isPublicImport });
			// A key's usages are bounded by what its HALF can do, not by what the
			// algorithm can do overall: a public key cannot sign and a private one
			// cannot verify, so importing an spki with ["sign"] is a mistake even
			// though the algorithm has that usage.
			checkHalfUsages(name, isPublicImport, usages, format);
			if (format === "jwk") checkJWKConsistency(keyData, usages, extractable);
			if (MLDSA_NAMES.includes(name)) {
				const payload = format === "jwk" ? JSON.stringify(keyData) : toU8(keyData);
				const r = subtleFail(ops.subtle_mldsa_import(name, format, payload));
				const isPub = r.type === "public";
				// extractable is what the CALLER asked for, for either half. Only
				// generateKey forces a public key to be extractable.
				return new CryptoKey(r.type, extractable, { name: r.name },
					usages.filter((u) => (isPub ? u === "verify" : u === "sign")), r.id);
			}
			if (MLKEM_NAMES.includes(name)) {
				const payload = format === "jwk" ? JSON.stringify(keyData) : toU8(keyData);
				const r = subtleFail(ops.subtle_mlkem_import(name, format, payload));
				const isPub = r.type === "public";
				return new CryptoKey(r.type, extractable, { name: r.name },
					usages.filter((u) => isPub
						? u === "encapsulateKey" || u === "encapsulateBits"
						: u === "decapsulateKey" || u === "decapsulateBits"), r.id);
			}
			if (name === "X25519") {
				const payload = format === "jwk" ? JSON.stringify(keyData) : toU8(keyData);
				const r = subtleFail(ops.subtle_x25519_import(format, payload));
				const algo = { name: "X25519" };
				return r.priv !== undefined
					? new CryptoKey("private", extractable, algo, usages, r.priv)
					: new CryptoKey("public", extractable, algo, [], r.pub);
			}
			if (name === "HMAC") {
				let raw;
				if (format === "raw") raw = toU8(keyData);
				else if (format === "jwk") {
					if (!keyData || keyData.kty !== "oct" || typeof keyData.k !== "string") {
						throw new DOMException("importKey: not an oct JWK", "DataError");
					}
					raw = b64uDecode(keyData.k);
				} else unsupported(`HMAC key format ${format}`);
				if (raw.length === 0) {
					throw new DOMException("importKey: HMAC key data is empty", "DataError");
				}
				const hash = hashName(alg.hash);
				const h = ops.subtle_hmac_import(raw);
				return new CryptoKey("secret", extractable, { name: "HMAC", hash: { name: hash }, length: raw.length * 8 }, usages, h);
			}
			if (RSA_ALL.includes(name)) {
				let r;
				if (format === "jwk") r = ops.subtle_rsa_import_jwk(JSON.stringify(keyData));
				else if (format === "pkcs8" || format === "spki") r = ops.subtle_rsa_import_der(format, toU8(keyData));
				else unsupported(`RSA key format ${format}`);
				const algo = { name: canonicalName(algName(alg)), hash: { name: hashName(alg.hash) }, modulusLength: r.bits, publicExponent: new Uint8Array([1, 0, 1]) };
				return new CryptoKey(r.type, extractable, algo, usages, r.id);
			}
			if (AES_ALL.includes(name)) {
				let raw;
				// "raw-secret" is the newer spelling of "raw" for a symmetric key;
				// the draft algorithms are specified only with it, so both name the
				// same thing here.
				if (format === "raw" || format === "raw-secret") raw = toU8(keyData);
				else if (format === "jwk") {
					if (!keyData || keyData.kty !== "oct" || typeof keyData.k !== "string") {
						throw new DOMException("importKey: not an oct JWK", "DataError");
					}
					raw = b64uDecode(keyData.k);
				} else unsupported(`AES key format ${format}`);
				if (raw.length !== 16 && raw.length !== 24 && raw.length !== 32) {
					throw new DOMException("importKey: invalid AES key length", "DataError");
				}
				const key = new CryptoKey("secret", extractable, { name: canonicalName(name), length: raw.length * 8 }, usages, null);
				keyRaw.set(key, raw);
				return key;
			}
			if (KMAC_NAMES.includes(name)) {
				let raw;
				if (format === "raw" || format === "raw-secret") raw = toU8(keyData);
				else if (format === "jwk") {
					if (!keyData || keyData.kty !== "oct" || typeof keyData.k !== "string") {
						throw new DOMException("importKey: not an oct JWK", "DataError");
					}
					raw = b64uDecode(keyData.k);
				} else unsupported(`${name} key format ${format}`);
				if (raw.length === 0) {
					throw new DOMException("importKey: KMAC key must not be empty", "DataError");
				}
				const key = new CryptoKey("secret", extractable, { name: canonicalName(name), length: raw.length * 8 }, usages, null);
				keyRaw.set(key, raw);
				return key;
			}
			if (name === CHACHA) {
				let raw;
				if (format === "raw" || format === "raw-secret") raw = toU8(keyData);
				else if (format === "jwk") {
					if (!keyData || keyData.kty !== "oct" || typeof keyData.k !== "string") {
						throw new DOMException("importKey: not an oct JWK", "DataError");
					}
					raw = b64uDecode(keyData.k);
				} else unsupported(`ChaCha20-Poly1305 key format ${format}`);
				// The one legal key size. A short key is a DataError, not
				// something to pad or stretch.
				if (raw.length !== 32) {
					throw new DOMException("importKey: ChaCha20-Poly1305 key must be 256 bits", "DataError");
				}
				const key = new CryptoKey("secret", extractable, { name: canonicalName(name) }, usages, null);
				keyRaw.set(key, raw);
				return key;
			}
			if (name === "ED448" || name === "X448") {
				const op = name === "ED448" ? ops.subtle_ed448_import : ops.subtle_x448_import;
				const r = subtleFail(format === "jwk"
					? op("jwk", JSON.stringify(keyData))
					: op(format, toU8(keyData)));
				const type = r.priv !== undefined ? "private" : "public";
				return new CryptoKey(type, extractable,
					{ name: canonicalName(name) }, usages, r.priv ?? r.pub);
			}
			if (name === "ED25519") {
				let r;
				if (format === "jwk") r = ops.subtle_ed_import("jwk", JSON.stringify(keyData));
				else if (["raw", "pkcs8", "spki"].includes(format)) r = ops.subtle_ed_import(format, toU8(keyData));
				else unsupported(`Ed25519 key format ${format}`);
				return new CryptoKey(r.type, extractable, { name: "Ed25519" }, usages, r.id);
			}
			if (name === "ECDH" || name === "ECDSA") {
				let r;
				if (format === "jwk") r = subtleFail(ops.subtle_ec_import_jwk(JSON.stringify(keyData)));
				else if (format === "raw") r = subtleFail(ops.subtle_ec_import_der("raw", toU8(keyData), String(alg.namedCurve)));
				else if (format === "pkcs8" || format === "spki") r = subtleFail(ops.subtle_ec_import_der(format, toU8(keyData)));
				else unsupported(`EC key format ${format}`);
				// A public key carries no usages in WebCrypto, whatever was asked for.
				const kUsages = r.type === "public" ? usages.filter((u) => name === "ECDSA" && u === "verify") : usages;
				return new CryptoKey(r.type, extractable, { name: canonicalName(algName(alg)), namedCurve: r.crv }, kUsages, r.id);
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
			return exportInner(format, key);
		},

		// getPublicKey derives the public half of a private key. It goes through
		// spki rather than a per-algorithm host op: every algorithm here can
		// already write the public half of a private handle, and coming back in
		// through importKey is what gets the usages judged as a public key's.
		async getPublicKey(key, usages) {
			if (!(key instanceof CryptoKey)) throw new TypeError("expected a CryptoKey");
			if (key.type === "secret") {
				throw new DOMException(
					`Failed to execute 'getPublicKey': ${key.algorithm.name} has no public half`, "NotSupportedError");
			}
			if (key.type !== "private") {
				throw new DOMException(
					"Failed to execute 'getPublicKey': the key is not a private key", "InvalidAccessError");
			}
			return subtle.importKey("spki", await exportInner("spki", key), key.algorithm, true, usages ?? []);
		},

		async sign(alg, key, data) {
			need(key, "sign");
			const name = algName(alg).toUpperCase();
			checkKeyAlgMatches(name, key, "sign");
			if (KMAC_NAMES.includes(name)) return toBuf(kmacRun(name, alg, key, data));
			if (name === "HMAC") return toBuf(ops.subtle_hmac_sign(key.algorithm.hash.name, key._h, toU8(data)));
			if (name === "ECDSA") return toBuf(ops.subtle_ec_sign(hashName(alg.hash), key._h, toU8(data)));
			if (MLDSA_NAMES.includes(name)) {
				return toBuf(subtleFail(ops.subtle_mldsa_sign(key._h, toU8(data), mldsaContext(alg))));
			}
			if (name === "ED448") return toBuf(subtleFail(ops.subtle_ed448_sign(key._h, toU8(data))));
			if (name === "ED25519") return toBuf(ops.subtle_ed_sign(key._h, toU8(data)));
			if (RSA_NAMES.includes(name)) {
				return toBuf(ops.subtle_rsa_sign(rsaScheme(name), key.algorithm.hash.name, alg.saltLength == null ? -1 : Number(alg.saltLength), key._h, toU8(data)));
			}
			unsupported(`algorithm ${name}`);
		},

		async verify(alg, key, signature, data) {
			need(key, "verify");
			const name = algName(alg).toUpperCase();
			checkKeyAlgMatches(name, key, "verify");
			if (KMAC_NAMES.includes(name)) {
				// Compare the whole tag in constant time, not just its prefix: a
				// verify that accepts a truncated match accepts a forgery.
				const want = Uint8Array.from(kmacRun(name, alg, key, data));
				const got = toU8(signature);
				if (got.length !== want.length) return false;
				let diff = 0;
				for (let i = 0; i < want.length; i++) diff |= want[i] ^ got[i];
				return diff === 0;
			}
			if (name === "HMAC") return ops.subtle_hmac_verify(key.algorithm.hash.name, key._h, toU8(signature), toU8(data));
			if (name === "ECDSA") return ops.subtle_ec_verify(hashName(alg.hash), key._h, toU8(signature), toU8(data));
			if (MLDSA_NAMES.includes(name)) {
				return subtleFail(ops.subtle_mldsa_verify(key._h, toU8(signature), toU8(data), mldsaContext(alg)));
			}
			if (name === "ED448") return ops.subtle_ed448_verify(key._h, toU8(signature), toU8(data));
			if (name === "ED25519") return ops.subtle_ed_verify(key._h, toU8(signature), toU8(data));
			if (RSA_NAMES.includes(name)) {
				return ops.subtle_rsa_verify(rsaScheme(name), key.algorithm.hash.name, alg.saltLength == null ? -1 : Number(alg.saltLength), key._h, toU8(signature), toU8(data));
			}
			unsupported(`algorithm ${name}`);
		},

		async encrypt(alg, key, data) {
			need(key, "encrypt");
			const name = algName(alg).toUpperCase();
			checkKeyAlgMatches(name, key, "encrypt");
			if (AES_NAMES.includes(name)) {
				const iv = toU8(name === "AES-CTR" ? alg.counter : alg.iv);
				const aad = alg.additionalData ? toU8(alg.additionalData) : new Uint8Array(0);
				const tagLen = alg.tagLength ?? 128;
				return toBuf(subtleFail(ops.subtle_aes_encrypt(name, rawOf(key), iv, toU8(data), aad, tagLen, Number(name === "AES-CTR" ? (alg.length ?? 128) : 128))));
			}
			if (name === "AES-OCB") {
				const aad = alg.additionalData ? toU8(alg.additionalData) : new Uint8Array(0);
				return toBuf(subtleFail(ops.subtle_aes_ocb(true, rawOf(key), toU8(alg.iv), toU8(data), aad, alg.tagLength ?? 128)));
			}
			if (name === CHACHA) {
				const aad = alg.additionalData ? toU8(alg.additionalData) : new Uint8Array(0);
				return toBuf(subtleFail(ops.subtle_chacha(true, rawOf(key), toU8(alg.nonce ?? alg.iv), toU8(data), aad)));
			}
			if (name === "RSA-OAEP") {
				const label = alg.label ? toU8(alg.label) : new Uint8Array(0);
				return toBuf(subtleFail(ops.subtle_rsa_oaep(true, key._h, key.algorithm.hash.name, toU8(data), label)));
			}
			unsupported(`encrypt algorithm ${name}`);
		},

		async decrypt(alg, key, data) {
			need(key, "decrypt");
			const name = algName(alg).toUpperCase();
			checkKeyAlgMatches(name, key, "decrypt");
			if (AES_NAMES.includes(name)) {
				const iv = toU8(name === "AES-CTR" ? alg.counter : alg.iv);
				const aad = alg.additionalData ? toU8(alg.additionalData) : new Uint8Array(0);
				const tagLen = alg.tagLength ?? 128;
				return toBuf(subtleFail(ops.subtle_aes_decrypt(name, rawOf(key), iv, toU8(data), aad, tagLen, Number(name === "AES-CTR" ? (alg.length ?? 128) : 128))));
			}
			if (name === "AES-OCB") {
				const aad = alg.additionalData ? toU8(alg.additionalData) : new Uint8Array(0);
				return toBuf(subtleFail(ops.subtle_aes_ocb(false, rawOf(key), toU8(alg.iv), toU8(data), aad, alg.tagLength ?? 128)));
			}
			if (name === CHACHA) {
				const aad = alg.additionalData ? toU8(alg.additionalData) : new Uint8Array(0);
				return toBuf(subtleFail(ops.subtle_chacha(false, rawOf(key), toU8(alg.nonce ?? alg.iv), toU8(data), aad)));
			}
			if (name === "RSA-OAEP") {
				const label = alg.label ? toU8(alg.label) : new Uint8Array(0);
				return toBuf(subtleFail(ops.subtle_rsa_oaep(false, key._h, key.algorithm.hash.name, toU8(data), label)));
			}
			unsupported(`decrypt algorithm ${name}`);
		},

		// wrapKey exports the key material, then encrypts it with the wrapping
		// key (spec: export to `format`, encrypt with `wrapAlg`). The internal
		// encrypt/decrypt bypass the encrypt/decrypt usage gate — the wrapping
		// key only carries wrapKey/unwrapKey usages.
		async wrapKey(format, key, wrappingKey, wrapAlg) {
			need(wrappingKey, "wrapKey");
			checkKeyAlgMatches(algName(wrapAlg), wrappingKey, "wrapKey");
			const exported = await subtle.exportKey(format, key);
			const bytes = format === "jwk" ? new TextEncoder().encode(JSON.stringify(exported)) : new Uint8Array(exported);
			return toBuf(rawCrypt(true, wrapAlg, wrappingKey, bytes));
		},

		async unwrapKey(format, wrappedKey, unwrappingKey, unwrapAlg, keyAlg, extractable, usages) {
			need(unwrappingKey, "unwrapKey");
			checkKeyAlgMatches(algName(unwrapAlg), unwrappingKey, "unwrapKey");
			const decrypted = new Uint8Array(rawCrypt(false, unwrapAlg, unwrappingKey, toU8(wrappedKey)));
			const material = format === "jwk"
				? JSON.parse(new TextDecoder().decode(decrypted))
				: decrypted;
			return subtle.importKey(format, material, keyAlg, extractable, usages);
		},

		// ------------------------------------------------- ML-KEM encapsulation
		// encapsulate produces a fresh shared secret plus the ciphertext that
		// lets the holder of the private key recover it; decapsulate is the
		// inverse. The *Bits forms hand back raw bytes, the *Key forms import
		// them as a CryptoKey of the caller's chosen algorithm.
		async encapsulateBits(alg, encapsulationKey) {
			need(encapsulationKey, "encapsulateBits");
			checkKeyAlgMatches(algName(alg), encapsulationKey, "encapsulateBits");
			const r = encapsulate(encapsulationKey);
			return { sharedKey: r.shared.buffer, ciphertext: r.ct.buffer };
		},

		async encapsulateKey(alg, encapsulationKey, sharedKeyAlg, extractable, usages) {
			need(encapsulationKey, "encapsulateKey");
			checkKeyAlgMatches(algName(alg), encapsulationKey, "encapsulateKey");
			const r = encapsulate(encapsulationKey);
			return {
				sharedKey: await sharedKeyFrom(r.shared, sharedKeyAlg, extractable, usages),
				ciphertext: r.ct.buffer,
			};
		},

		async decapsulateBits(alg, decapsulationKey, ciphertext) {
			need(decapsulationKey, "decapsulateBits");
			checkKeyAlgMatches(algName(alg), decapsulationKey, "decapsulateBits");
			return toBuf(subtleFail(ops.subtle_mlkem_decapsulate(decapsulationKey._h, toU8(ciphertext))));
		},

		async decapsulateKey(alg, decapsulationKey, ciphertext, sharedKeyAlg, extractable, usages) {
			need(decapsulationKey, "decapsulateKey");
			checkKeyAlgMatches(algName(alg), decapsulationKey, "decapsulateKey");
			const bits = subtleFail(ops.subtle_mlkem_decapsulate(decapsulationKey._h, toU8(ciphertext)));
			return sharedKeyFrom(bits, sharedKeyAlg, extractable, usages);
		},

		async deriveBits(alg, baseKey, length) {
			need(baseKey, "deriveBits");
			checkDeriveKeyMatches(alg, baseKey, "deriveBits");
			return deriveBitsRaw(alg, baseKey, checkDerivedLength(length, "deriveBits"));
		},

		async deriveKey(alg, baseKey, derivedKeyAlg, extractable, usages) {
			// Gate on deriveKey only — WebCrypto does NOT require the base key to
			// also carry deriveBits. Use the internal ungated deriveBitsRaw so a
			// key imported with just ["deriveKey"] (the canonical PBKDF2 pattern)
			// works, mirroring how wrapKey uses rawCrypt.
			need(baseKey, "deriveKey");
			checkDeriveKeyMatches(alg, baseKey, "deriveKey");
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
			if (AES_ALL.includes(derivedName)) {
				return subtle.importKey("raw", bits, { name: derivedName }, extractable, usages);
			}
			if (derivedName === "HMAC") {
				return subtle.importKey("raw", bits, { name: "HMAC", hash: derivedKeyAlg.hash }, extractable, usages);
			}
			unsupported(`deriveKey derived algorithm ${derivedName}`);
		},
	};

	// kmacRun applies the per-call parameters: an output length in bits (the
	// variant's own strength when unstated) and an optional customization string
	// that separates one application's MACs from another's.
	function kmacRun(name, alg, key, data) {
		const bits = alg.outputLength === undefined
			? (name === "KMAC128" ? 256 : 512)
			: Number(alg.outputLength);
		if (!Number.isInteger(bits) || bits <= 0 || bits % 8 !== 0) {
			throw new DOMException(`${name} outputLength must be a positive multiple of 8`, "OperationError");
		}
		const custom = alg.customization === undefined ? new Uint8Array(0) : toU8(alg.customization);
		return subtleFail(ops.subtle_kmac(name === "KMAC128" ? 128 : 256, rawOf(key), toU8(data), custom, bits));
	}

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
		if (name === "AES-OCB") {
			const aad = alg.additionalData ? toU8(alg.additionalData) : new Uint8Array(0);
			return subtleFail(ops.subtle_aes_ocb(encrypt, rawOf(key), toU8(alg.iv), toU8(data), aad, alg.tagLength ?? 128));
		}
		if (name === CHACHA) {
			const aad = alg.additionalData ? toU8(alg.additionalData) : new Uint8Array(0);
			return subtleFail(ops.subtle_chacha(encrypt, rawOf(key), toU8(alg.nonce ?? alg.iv), toU8(data), aad));
		}
		if (name === "RSA-OAEP") {
			const label = alg.label ? toU8(alg.label) : new Uint8Array(0);
			return subtleFail(ops.subtle_rsa_oaep(encrypt, key._h, key.algorithm.hash.name, toU8(data), label));
		}
		throw new DOMException(`unsupported wrap algorithm ${name}`, "NotSupportedError");
	}

	// encapsulate splits the host's one flat buffer: the 32-byte shared key
	// first, then the ciphertext.
	function encapsulate(key) {
		const all = Uint8Array.from(subtleFail(ops.subtle_mlkem_encapsulate(key._h)));
		return { shared: all.slice(0, 32), ct: all.slice(32) };
	}

	// sharedKeyFrom turns 32 encapsulated bytes into the key the caller asked
	// for. HKDF/PBKDF2 take the secret whole; a fixed-size algorithm takes the
	// prefix its length names, as WebCrypto's "get key length" prescribes.
	function sharedKeyFrom(bits, alg, extractable, usages) {
		const raw = Uint8Array.from(bits);
		const name = String(algName(alg)).toUpperCase();
		if (name === CHACHA) {
			// One key size, so the whole 32-byte secret is the key.
			if (raw.length < 32) {
				throw new DOMException("encapsulateKey: shared secret is shorter than the requested key", "OperationError");
			}
			return subtle.importKey("raw", raw.slice(0, 32), alg, extractable, usages);
		}
		if (AES_ALL.includes(name)) {
			const len = checkAESLength(alg.length === undefined ? 256 : alg.length, "encapsulateKey") / 8;
			if (len > raw.length) {
				throw new DOMException("encapsulateKey: shared secret is shorter than the requested key", "OperationError");
			}
			return subtle.importKey("raw", raw.slice(0, len), alg, extractable, usages);
		}
		return subtle.importKey("raw", raw, alg, extractable, usages);
	}

	globalThis.crypto.subtle = subtle;
})();
