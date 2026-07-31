// compat/web: the Cache API (https://w3c.github.io/ServiceWorker/#cache-interface).
//
// `caches` is a store of request/response pairs. It is JavaScript rather than
// Go because what it stores are live Request and Response objects and what it
// does with them is entirely their own vocabulary — cloning a response, reading
// its body, comparing its Vary headers against a request's. There is no
// algorithm underneath for a Go package to own.
//
// The store is per-instance and in memory. That is the whole of it: nothing
// here writes to disk, and a cache does not outlive the interpreter that made
// it. Said plainly rather than implied, because "cache" invites the assumption
// that it persists, and code that relies on that would be wrong here.
//
// A stored response is a SNAPSHOT: put() reads the body to completion and keeps
// the bytes, so the response handed back later is a fresh one over the same
// bytes. A cache that kept the caller's Response would hand out a body that
// could only be read once, and the second match() would return an empty one.
(() => {
	"use strict";

	// One stored entry: the request that was cached under, and everything needed
	// to rebuild the response it was cached with.
	//
	// Vary is kept as the header NAMES it listed, resolved at put() time against
	// the request, because that is when the pairing is decided. Re-deriving it at
	// match() time from the stored response would work too, but only as long as
	// nobody mutated the headers in between.
	class CacheEntry {
		constructor(request, response, bytes, varyNames) {
			this.request = request;
			this.status = response.status;
			this.statusText = response.statusText;
			this.headers = new Headers(response.headers);
			this.type = response.type;
			this.url = response.url;
			this.bytes = bytes;
			this.varyNames = varyNames;
		}
		// A response built fresh each time, so two matches never share a body.
		toResponse() {
			// A null-body status may not carry one, and constructing a Response with
			// a body for such a status is a TypeError.
			const nullBody = this.status === 101 || this.status === 204
				|| this.status === 205 || this.status === 304;
			// Built without a status in the init so the constructor's 200-599 range
			// guard is skipped: an opaque response's status is 0, and it must come
			// back out of the cache as it went in.
			const res = new Response(nullBody ? null : this.bytes.slice(), { statusText: this.statusText, headers: this.headers });
			res.status = this.status;
			res.ok = this.status >= 200 && this.status <= 299;
			res.type = this.type;
			res.url = this.url;
			return res;
		}
	}

	const VARY_SPLIT = /\s*,\s*/;

	// varyMatches decides whether a cached entry may answer a new request: every
	// header the response's Vary named must have the same value in both requests.
	function varyMatches(entry, request) {
		for (const name of entry.varyNames) {
			if (name === "*") return false;
			if (entry.request.headers.get(name) !== request.headers.get(name)) return false;
		}
		return true;
	}

	// requestMatches is the URL comparison, with the two options that loosen it.
	// The fragment never takes part: it is not sent, so it cannot distinguish two
	// requests that reach the same resource.
	function requestMatches(entryRequest, request, options) {
		const opts = options || {};
		if (!opts.ignoreMethod && entryRequest.method !== request.method) return false;
		const a = new URL(entryRequest.url);
		const b = new URL(request.url);
		a.hash = "";
		b.hash = "";
		if (opts.ignoreSearch) {
			a.search = "";
			b.search = "";
		}
		return a.href === b.href;
	}

	// toRequest accepts what every Cache operation accepts: a Request, or a URL
	// string resolved against the base.
	function toRequest(input, who) {
		if (input instanceof Request) return input;
		if (input === undefined) throw new TypeError(`${who}: a request is required`);
		return new Request(String(input));
	}

	class Cache {
		constructor(internal) {
			if (internal !== CACHE_INTERNAL) throw new TypeError("Illegal constructor");
			Object.defineProperty(this, "_entries", { value: [], writable: true });
		}

		async match(request, options) {
			const all = await this.matchAll(request, options);
			return all.length > 0 ? all[0] : undefined;
		}

		async matchAll(request, options) {
			if (request === undefined) return this._entries.map((e) => e.toResponse());
			const req = toRequest(request, "matchAll");
			// Only GET is ever cached, so a request by any other method matches
			// nothing at all unless the caller asked for the method to be ignored.
			const opts = options || {};
			if (req.method !== "GET" && !opts.ignoreMethod) return [];
			const out = [];
			for (const entry of this._entries) {
				if (!requestMatches(entry.request, req, opts)) continue;
				if (!opts.ignoreVary && !varyMatches(entry, req)) continue;
				out.push(entry.toResponse());
			}
			return out;
		}

		async put(request, response) {
			if (arguments.length < 2) throw new TypeError("put requires 2 arguments");
			const req = toRequest(request, "put");
			const url = new URL(req.url);
			if (url.protocol !== "http:" && url.protocol !== "https:") {
				throw new TypeError("put: only http and https requests can be cached");
			}
			if (req.method !== "GET") throw new TypeError("put: only GET requests can be cached");
			if (!(response instanceof Response)) throw new TypeError("put: a Response is required");
			// 206 is a PIECE of a resource; storing it under the whole resource's URL
			// would hand a later match a body that is not the resource.
			if (response.status === 206) throw new TypeError("put: a partial response cannot be cached");
			const vary = response.headers.get("vary");
			const varyNames = vary === null ? [] : vary.trim().toLowerCase().split(VARY_SPLIT).filter(Boolean);
			if (varyNames.includes("*")) throw new TypeError("put: a response that varies on * cannot be cached");
			if (response.bodyUsed) throw new TypeError("put: the response body has already been used");
			// Read the body HERE: the entry keeps bytes, not a stream, so a later
			// match hands out a response that can be read like any other.
			const bytes = response.body === null ? new Uint8Array(0) : new Uint8Array(await response.arrayBuffer());
			const entry = new CacheEntry(req, response, bytes, varyNames);
			// A put REPLACES anything it would itself have matched: a cache holds one
			// answer per request, not a history of them.
			this._entries = this._entries.filter((e) => !(requestMatches(e.request, req, {}) && varyMatches(e, req)));
			this._entries.push(entry);
		}

		async add(request) {
			if (arguments.length < 1) throw new TypeError("add requires 1 argument");
			return this.addAll([request]);
		}

		async addAll(requests) {
			if (arguments.length < 1) throw new TypeError("addAll requires 1 argument");
			if (requests === null || typeof requests[Symbol.iterator] !== "function") {
				throw new TypeError("addAll: an iterable of requests is required");
			}
			const list = [...requests].map((r) => toRequest(r, "addAll"));
			for (const req of list) {
				const url = new URL(req.url);
				if (url.protocol !== "http:" && url.protocol !== "https:") {
					throw new TypeError("addAll: only http and https requests can be cached");
				}
				if (req.method !== "GET") throw new TypeError("addAll: only GET requests can be cached");
			}
			// Every fetch runs before anything is stored, so a single failure leaves
			// the cache exactly as it was.
			const responses = await Promise.all(list.map((req) => {
				const c = req.clone();
				if (typeof c.url !== "string") throw new TypeError("DIAG clone.url=" + typeof c.url + " isReq=" + (c instanceof Request) + " proto=" + (Object.getPrototypeOf(c) === Request.prototype));
				return fetch(c);
			}));
			for (const res of responses) {
				if (res.type === "error" || res.status < 200 || res.status > 299) {
					throw new TypeError("addAll: a request did not answer with a successful response");
				}
			}
			await Promise.all(list.map((req, i) => this.put(req, responses[i])));
		}

		async delete(request, options) {
			if (arguments.length < 1) throw new TypeError("delete requires 1 argument");
			const req = toRequest(request, "delete");
			const opts = options || {};
			if (req.method !== "GET" && !opts.ignoreMethod) return false;
			const before = this._entries.length;
			this._entries = this._entries.filter((e) => {
				if (!requestMatches(e.request, req, opts)) return true;
				if (!opts.ignoreVary && !varyMatches(e, req)) return true;
				return false;
			});
			return this._entries.length !== before;
		}

		async keys(request, options) {
			if (request === undefined) return this._entries.map((e) => e.request.clone());
			const req = toRequest(request, "keys");
			const opts = options || {};
			if (req.method !== "GET" && !opts.ignoreMethod) return [];
			return this._entries
				.filter((e) => requestMatches(e.request, req, opts) && (opts.ignoreVary || varyMatches(e, req)))
				.map((e) => e.request.clone());
		}
	}
	Object.defineProperty(Cache.prototype, Symbol.toStringTag, { value: "Cache", configurable: true });

	const CACHE_INTERNAL = Symbol("Cache.internal");

	class CacheStorage {
		constructor(internal) {
			if (internal !== CACHE_INTERNAL) throw new TypeError("Illegal constructor");
			// Insertion order is the order keys() reports, which is what the standard
			// specifies and what a test that opens three caches checks.
			Object.defineProperty(this, "_caches", { value: new Map() });
		}

		async open(cacheName) {
			if (arguments.length < 1) throw new TypeError("open requires 1 argument");
			const name = String(cacheName);
			let cache = this._caches.get(name);
			if (!cache) this._caches.set(name, cache = new Cache(CACHE_INTERNAL));
			return cache;
		}

		async has(cacheName) {
			if (arguments.length < 1) throw new TypeError("has requires 1 argument");
			return this._caches.has(String(cacheName));
		}

		async delete(cacheName) {
			if (arguments.length < 1) throw new TypeError("delete requires 1 argument");
			return this._caches.delete(String(cacheName));
		}

		async keys() {
			return [...this._caches.keys()];
		}

		// match without a cacheName asks every cache, in the order they were
		// created, and answers with the first that has something.
		async match(request, options) {
			if (arguments.length < 1) throw new TypeError("match requires 1 argument");
			const opts = options || {};
			if (opts.cacheName !== undefined) {
				// A name that no cache has is simply a miss: there is nothing there,
				// which is the same answer as a cache that does not hold the request.
				const cache = this._caches.get(String(opts.cacheName));
				return cache ? cache.match(request, opts) : undefined;
			}
			for (const cache of this._caches.values()) {
				const found = await cache.match(request, opts);
				if (found !== undefined) return found;
			}
			return undefined;
		}
	}
	Object.defineProperty(CacheStorage.prototype, Symbol.toStringTag, { value: "CacheStorage", configurable: true });

	globalThis.Cache = Cache;
	globalThis.CacheStorage = CacheStorage;
	globalThis.caches = new CacheStorage(CACHE_INTERNAL);
})();
