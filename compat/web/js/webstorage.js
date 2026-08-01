// compat/web: Web Storage
// (https://html.spec.whatwg.org/multipage/webstorage.html).
//
// The storage areas live entirely in the guest: a Storage object is an
// ordered string map with a quota, and everything interesting about it is
// JS-object behaviour — it is a legacy platform object whose named
// getter/setter/deleter make `storage.name = v` and `"name" in storage`
// operate on the map, while everything on the prototype chain (getItem,
// length, even Object.prototype.toString) shadows the named properties
// because Storage is not [LegacyOverrideBuiltIns]. That contract is a Proxy,
// and belongs where the Proxy lives.
//
// The areas are in-memory and per-interpreter. An embedding that wants
// persistence needs a document/origin model first; until then localStorage
// and sessionStorage are distinct areas with identical lifetime, which is
// exactly what they are in every test-runner runtime (a fresh profile).
(() => {
	"use strict";

	// The conventional browser quota: 5 MiB of UTF-16 code units, keys and
	// values together, per area. The number is not in the specification — the
	// specification only demands THAT a quota exists and maps exceeding it to
	// QuotaExceededError — and 5 MiB is what the major engines grant.
	const QUOTA = 5 * 1024 * 1024;

	// area, keyed by BOTH the proxy (methods see `this === proxy`) and the
	// proxy's target (traps see the target): {map: Map, used: number}.
	const areas = new WeakMap();
	const areaOf = (o) => {
		const a = areas.get(o);
		if (!a) throw new TypeError("called on an object that is not a Storage");
		return a;
	};

	class Storage {
		constructor() { throw new TypeError("Illegal constructor"); }
		key(index) {
			if (arguments.length < 1) throw new TypeError("Storage.key: an index is required");
			// unsigned long: ToNumber, then modulo 2^32 — key(-1) asks for the
			// 4294967295th key, which does not exist.
			const n = Number(index) >>> 0;
			const { map } = areaOf(this);
			if (n >= map.size) return null;
			let i = 0;
			for (const k of map.keys()) if (i++ === n) return k;
			return null;
		}
		getItem(key) {
			if (arguments.length < 1) throw new TypeError("Storage.getItem: a key is required");
			const { map } = areaOf(this);
			key = String(key);
			return map.has(key) ? map.get(key) : null;
		}
		setItem(key, value) {
			if (arguments.length < 2) throw new TypeError("Storage.setItem: a key and a value are required");
			const a = areaOf(this);
			key = String(key);
			value = String(value);
			const delta = key.length + value.length -
				(a.map.has(key) ? key.length + a.map.get(key).length : 0);
			if (a.used + delta > QUOTA) {
				throw new QuotaExceededError("the quota has been exceeded");
			}
			a.used += delta;
			a.map.set(key, value);
		}
		removeItem(key) {
			if (arguments.length < 1) throw new TypeError("Storage.removeItem: a key is required");
			const a = areaOf(this);
			key = String(key);
			if (a.map.has(key)) {
				a.used -= key.length + a.map.get(key).length;
				a.map.delete(key);
			}
		}
		clear() {
			const a = areaOf(this);
			a.map.clear();
			a.used = 0;
		}
	}
	Object.defineProperty(Storage.prototype, "length", {
		get() { return areaOf(this).map.size; },
		enumerable: true, configurable: true,
	});
	Object.defineProperty(Storage.prototype, Symbol.toStringTag, {
		value: "Storage", configurable: true,
	});

	function makeStorage() {
		const target = Object.create(Storage.prototype);
		const map = new Map();
		// A named property is VISIBLE only where nothing shadows it: an own
		// property of the target (a symbol Object.defineProperty put there) or
		// anything on the prototype chain (getItem, length, toString) wins on
		// read, while writes always go to the map. That asymmetry — set
		// storage.length and length is still a number — is the platform-object
		// contract the tests hold us to.
		const visible = (p) => typeof p === "string" && map.has(p) && !Reflect.has(target, p);
		const proxy = new Proxy(target, {
			get(t, p, receiver) {
				if (visible(p)) return map.get(p);
				return Reflect.get(t, p, receiver);
			},
			set(t, p, value, receiver) {
				if (typeof p === "symbol") return Reflect.set(t, p, value, receiver);
				proxy.setItem(p, value);
				return true;
			},
			has(t, p) {
				return (typeof p === "string" && map.has(p)) || Reflect.has(t, p);
			},
			deleteProperty(t, p) {
				if (typeof p === "symbol") return Reflect.deleteProperty(t, p);
				if (map.has(p)) { proxy.removeItem(p); return true; }
				return Reflect.deleteProperty(t, p);
			},
			defineProperty(t, p, desc) {
				if (typeof p === "symbol") return Reflect.defineProperty(t, p, desc);
				// Only a data descriptor can be stored as a string value.
				if ("get" in desc || "set" in desc) return false;
				proxy.setItem(p, desc.value);
				return true;
			},
			getOwnPropertyDescriptor(t, p) {
				if (visible(p)) {
					return { value: map.get(p), writable: true, enumerable: true, configurable: true };
				}
				return Reflect.getOwnPropertyDescriptor(t, p);
			},
			ownKeys(t) {
				// The supported property names — every key, in storage order —
				// plus whatever symbols were legitimately defined on the target.
				return [...map.keys(), ...Reflect.ownKeys(t)];
			},
			preventExtensions() {
				// A platform object with a named setter cannot be made
				// non-extensible: the next setItem would violate the freeze.
				return false;
			},
		});
		const area = { map, used: 0 };
		areas.set(target, area);
		areas.set(proxy, area);
		return proxy;
	}

	globalThis.Storage ??= Storage;
	for (const name of ["localStorage", "sessionStorage"]) {
		if (name in globalThis) continue;
		const storage = makeStorage();
		Object.defineProperty(globalThis, name, {
			get: () => storage, enumerable: true, configurable: true,
		});
	}

	class StorageEvent extends Event {
		constructor(type, init = {}) {
			// super() would pass two arguments either way; the required-argument
			// check has to look at what the CALLER passed.
			if (arguments.length < 1) throw new TypeError("StorageEvent: a type is required");
			super(type, init);
			const opts = init === null || init === undefined ? {} : init;
			const nullable = (v) => v === null || v === undefined ? null : String(v);
			Object.defineProperties(this, {
				_key: { value: nullable(opts.key), writable: true },
				_oldValue: { value: nullable(opts.oldValue), writable: true },
				_newValue: { value: nullable(opts.newValue), writable: true },
				// url is a plain USVString: absent means "", null means "null".
				_url: { value: opts.url === undefined ? "" : String(opts.url), writable: true },
				_storageArea: { value: opts.storageArea ?? null, writable: true },
			});
		}
		get key() { return this._key; }
		get oldValue() { return this._oldValue; }
		get newValue() { return this._newValue; }
		get url() { return this._url; }
		get storageArea() { return this._storageArea; }
		initStorageEvent(type, bubbles = false, cancelable = false, key = null,
			oldValue = null, newValue = null, url = "", storageArea = null) {
			if (arguments.length < 1) throw new TypeError("initStorageEvent: a type is required");
			this._type = String(type);
			this._bubbles = Boolean(bubbles);
			this._cancelable = Boolean(cancelable);
			const nullable = (v) => v === null || v === undefined ? null : String(v);
			this._key = nullable(key);
			this._oldValue = nullable(oldValue);
			this._newValue = nullable(newValue);
			this._url = url === undefined ? "" : String(url);
			this._storageArea = storageArea ?? null;
		}
	}
	Object.defineProperty(StorageEvent.prototype.initStorageEvent, "length", { value: 1 });
	Object.defineProperty(StorageEvent.prototype, Symbol.toStringTag, {
		value: "StorageEvent", configurable: true,
	});
	globalThis.StorageEvent ??= StorageEvent;
})();
