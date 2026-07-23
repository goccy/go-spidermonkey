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
			this._h = handle;
		}
	}
	globalThis.CryptoKey = CryptoKey;

	const need = (key, usage) => {
		if (!(key instanceof CryptoKey)) throw new TypeError("expected a CryptoKey");
		if (!key.usages.includes(usage)) {
			throw new DOMException(`key does not permit ${usage}`, "InvalidAccessError");
		}
	};
	const unsupported = (what) => { throw new DOMException(`unsupported ${what}`, "NotSupportedError"); };

	const RSA_NAMES = ["RSASSA-PKCS1-V1_5", "RSA-PSS"];
	const rsaScheme = (name) => (name === "RSA-PSS" ? "pss" : "pkcs1");

	const subtle = {
		async digest(alg, data) {
			return toBuf(ops.subtle_digest(hashName(alg), toU8(data)));
		},

		async generateKey(alg, extractable, usages) {
			const name = algName(alg).toUpperCase();
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
			if (RSA_NAMES.includes(name)) {
				const hash = hashName(alg.hash);
				const bits = Number(alg.modulusLength);
				const r = ops.subtle_rsa_generate(bits);
				const algo = { name: algName(alg), hash: { name: hash }, modulusLength: bits, publicExponent: new Uint8Array([1, 0, 1]) };
				return {
					privateKey: new CryptoKey("private", extractable, algo, usages.filter((u) => u === "sign"), r.priv),
					publicKey: new CryptoKey("public", true, algo, usages.filter((u) => u === "verify"), r.pub),
				};
			}
			unsupported(`algorithm ${algName(alg)}`);
		},

		async importKey(format, keyData, alg, extractable, usages) {
			const name = algName(alg).toUpperCase();
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
			if (RSA_NAMES.includes(name)) {
				let r;
				if (format === "jwk") r = ops.subtle_rsa_import_jwk(JSON.stringify(keyData));
				else if (format === "pkcs8" || format === "spki") r = ops.subtle_rsa_import_der(format, toU8(keyData));
				else unsupported(`RSA key format ${format}`);
				const algo = { name: algName(alg), hash: { name: hashName(alg.hash) }, modulusLength: r.bits, publicExponent: new Uint8Array([1, 0, 1]) };
				return new CryptoKey(r.type, extractable, algo, usages, r.id);
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
			if (RSA_NAMES.includes(name)) {
				if (format === "jwk") return JSON.parse(ops.subtle_rsa_export_jwk(key._h));
				if (format === "pkcs8" || format === "spki") return toBuf(ops.subtle_rsa_export_der(format, key._h));
				unsupported(`RSA export format ${format}`);
			}
			unsupported(`algorithm ${key.algorithm.name}`);
		},

		async sign(alg, key, data) {
			need(key, "sign");
			const name = algName(alg).toUpperCase();
			if (name === "HMAC") return toBuf(ops.subtle_hmac_sign(key.algorithm.hash.name, key._h, toU8(data)));
			if (name === "ECDSA") return toBuf(ops.subtle_ec_sign(hashName(alg.hash), key._h, toU8(data)));
			if (RSA_NAMES.includes(name)) {
				return toBuf(ops.subtle_rsa_sign(rsaScheme(name), key.algorithm.hash.name, Number(alg.saltLength ?? 0), key._h, toU8(data)));
			}
			unsupported(`algorithm ${algName(alg)}`);
		},

		async verify(alg, key, signature, data) {
			need(key, "verify");
			const name = algName(alg).toUpperCase();
			if (name === "HMAC") return ops.subtle_hmac_verify(key.algorithm.hash.name, key._h, toU8(signature), toU8(data));
			if (name === "ECDSA") return ops.subtle_ec_verify(hashName(alg.hash), key._h, toU8(signature), toU8(data));
			if (RSA_NAMES.includes(name)) {
				return ops.subtle_rsa_verify(rsaScheme(name), key.algorithm.hash.name, Number(alg.saltLength ?? 0), key._h, toU8(signature), toU8(data));
			}
			unsupported(`algorithm ${algName(alg)}`);
		},
	};

	globalThis.crypto.subtle = subtle;
})();
