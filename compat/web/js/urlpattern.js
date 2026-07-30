// compat/web: URLPattern (https://urlpattern.spec.whatwg.org/).
//
// A URL pattern is eight independent component patterns — protocol, username,
// password, hostname, port, pathname, search, hash — each compiled to a regular
// expression with named groups. The syntax is path-to-regexp's: `:name` for a
// named segment, `(regex)` for an inline pattern, `*` for a wildcard, `{…}` for
// a group, and `?`/`+`/`*` as modifiers on any of them.
//
// The pattern syntax itself — tokenizer, parser, canonical serialization, and
// the regular-expression source — is host-side in Go (see urlpattern.go). This
// file is the interface over it: the class, the component tables, and the
// matching, which has to run in the JavaScript engine because a pattern can
// embed JavaScript regular-expression syntax.
(() => {
	"use strict";

	const ops = globalThis.__web_ops;

	const COMPONENTS = [
		"protocol", "username", "password", "hostname",
		"port", "pathname", "search", "hash",
	];

	// Pattern compilation is host-side: the tokenizer, the parser, the canonical
	// pattern string and the regular-expression source are in Go, checked directly
	// against the standard's own test data. What has to stay here is the regular
	// expression itself — the standard defines matching in terms of a JavaScript
	// one, and a pattern can embed JavaScript regexp syntax in a "(...)" group, so
	// the engine that runs it must be the one whose syntax it is.
	// SPECIAL_SCHEMES decides how a pathname, a query and a fragment are escaped.
	// A protocol PATTERN counts as special when it matches any of them — which is
	// a question for the compiled regular expression, so it is answered here and
	// passed down rather than guessed at from the pattern text.
	const SPECIAL_SCHEMES = ["http", "https", "ws", "wss", "ftp", "file"];

	function compileComponent(pattern, component, protocol, ignoreCase, special) {
		const enc = new TextEncoder().encode(String(pattern));
		const r = ops.pattern_compile(component, enc, String(protocol ?? ""), !!special);
		if (r && r.__patternError) throw new TypeError(`Invalid pattern for ${component}: ${r.message}`);
		let regexp;
		try {
			regexp = new RegExp(r.regexp, ignoreCase ? "ui" : "u");
		} catch (e) {
			// An inline regular expression the engine rejects makes the whole
			// pattern invalid, which the standard reports from the constructor.
			throw new TypeError(`Invalid pattern for ${component}: ${e.message}`);
		}
		return { pattern: r.pattern, regexp, names: [...r.names], hasRegexpGroups: r.hasRegexp };
	}


	// Splitting a full-URL pattern into components is host-side: a pattern may
	// contain the very characters that delimit components, so this is a state
	// machine that skips over "{...}" groups and "(...)" regular expressions
	// rather than a search for ":" and "/".
	function componentsFromString(input, baseURL) {
		const enc = new TextEncoder().encode(String(input));
		const r = ops.pattern_from_string(enc);
		if (r && r.__patternError) throw new TypeError(`Invalid pattern: ${r.message}`);
		const out = {};
		for (const c of COMPONENTS) if (r[c] !== undefined) out[c] = r[c];
		if (baseURL !== undefined) {
			// A relative pattern takes only the components that LOCATE it from the
			// base URL — protocol, hostname, port — and never the credentials: a
			// base URL's username is not part of where the pattern points, and
			// inheriting it turns "any user" into "no user".
			const base = new URL(String(baseURL));
			const inherited = {
				protocol: base.protocol.slice(0, -1),
				hostname: base.hostname,
				port: base.port,
			};
			const firstNamed = COMPONENTS.findIndex((c) => out[c] !== undefined);
			COMPONENTS.forEach((c, i) => {
				if (inherited[c] !== undefined && out[c] === undefined && firstNamed >= 0 && i < firstNamed) {
					out[c] = inherited[c];
				}
			});
		}
		return out;
	}

	function inputComponents(input, baseURL) {
		if (typeof input === "string" || input instanceof URL) {
			const u = new URL(String(input), baseURL);
			return {
				protocol: u.protocol.replace(/:$/, ""),
				username: u.username,
				password: u.password,
				hostname: u.hostname,
				port: u.port,
				pathname: u.pathname,
				search: u.search.replace(/^\?/, ""),
				hash: u.hash.replace(/^#/, ""),
			};
		}
		if (input === null || input === undefined) return null;
		if (typeof input !== "object") throw new TypeError("URLPattern input must be a string or an object");
		const out = {};
		let base;
		if (input.baseURL !== undefined || baseURL !== undefined) {
			base = new URL(input.baseURL ?? baseURL);
		}
		for (const c of COMPONENTS) {
			if (input[c] !== undefined) {
				out[c] = String(input[c]);
			} else if (base) {
				out[c] = {
					protocol: base.protocol.replace(/:$/, ""),
					username: base.username,
					password: base.password,
					hostname: base.hostname,
					port: base.port,
					pathname: base.pathname,
					search: base.search.replace(/^\?/, ""),
					hash: base.hash.replace(/^#/, ""),
				}[c];
			} else {
				out[c] = "";
			}
		}
		return out;
	}

	class URLPattern {
		constructor(input, baseURL, options) {
			if (typeof baseURL === "object" && baseURL !== null && options === undefined) {
				options = baseURL;
				baseURL = undefined;
			}
			let init;
			if (typeof input === "string" || input instanceof URL) {
				// A string pattern is parsed as a URL, so it needs an origin: either
				// its own protocol or a base URL to take one from. "/foo" alone is a
				// TypeError, not a pathname-only pattern.
				init = componentsFromString(String(input), baseURL);
				if (init.protocol === undefined && baseURL === undefined) {
					throw new TypeError("URLPattern: a relative pattern needs a base URL: " + String(input));
				}
			} else if (input === undefined) {
				init = {};
			} else if (input !== null && typeof input === "object") {
				init = { ...input };
				if (init.baseURL !== undefined) {
					const inherited = componentsFromString("", init.baseURL);
					for (const c of COMPONENTS) {
						if (init[c] === undefined && inherited[c] !== undefined) init[c] = inherited[c];
					}
					delete init.baseURL;
				} else if (baseURL !== undefined) {
					throw new TypeError("URLPattern: a base URL cannot be given with an object pattern");
				}
			} else {
				throw new TypeError("URLPattern: invalid input");
			}
			// A literal port has to be a port. "100000" is not, and the spec makes
			// that a TypeError rather than a pattern that can never match.
			if (init.port !== undefined) {
				const literal = String(init.port);
				if (literal !== "" && /^[0-9]+$/.test(literal) && Number(literal) > 65535) {
					throw new TypeError("URLPattern: port out of range: " + literal);
				}
			}
			this._ignoreCase = !!(options && options.ignoreCase);
			this._parts = {};
			// The protocol is compiled first: every other component's escaping
			// depends on whether it can name a special scheme.
			const protoPattern = init.protocol === undefined ? "*" : String(init.protocol);
			const proto = compileComponent(protoPattern, "protocol", "", this._ignoreCase, true);
			const special = SPECIAL_SCHEMES.some((s) => proto.regexp.test(s));
			this._parts.protocol = proto;
			// Kept for generate(), which canonicalizes its result the same way the
			// pattern's own literal text was canonicalized.
			this._protoPattern = protoPattern;
			this._special = special;
			for (const c of COMPONENTS) {
				if (c === "protocol") continue;
				// An unspecified component matches anything.
				const pattern = init[c] === undefined ? "*" : String(init[c]);
				this._parts[c] = compileComponent(pattern, c, protoPattern, this._ignoreCase, special);
			}
		}

		get protocol() { return this._parts.protocol.pattern; }
		get username() { return this._parts.username.pattern; }
		get password() { return this._parts.password.pattern; }
		get hostname() { return this._parts.hostname.pattern; }
		get port() { return this._parts.port.pattern; }
		get pathname() { return this._parts.pathname.pattern; }
		get search() { return this._parts.search.pattern; }
		get hash() { return this._parts.hash.pattern; }
		get hasRegExpGroups() {
			return COMPONENTS.some((c) => this._parts[c].hasRegexpGroups);
		}

		test(input, baseURL) {
			return this.exec(input, baseURL) !== null;
		}

		// generate fills a component in from group values. Only a pattern that
		// describes exactly one component can be generated: a wildcard, an inline
		// regular expression or any modifier describes a SET of them, and there is
		// no single answer to give.
		generate(component, groups) {
			const c = String(component);
			if (!COMPONENTS.includes(c)) {
				throw new TypeError(`generate: ${c} is not a URL component`);
			}
			const plain = {};
			if (groups !== undefined && groups !== null) {
				for (const [k, v] of Object.entries(groups)) plain[k] = String(v);
			}
			const enc = (x) => new TextEncoder().encode(String(x));
			const r = ops.pattern_generate(c, enc(this._parts[c].pattern),
				String(this._protoPattern), this._special, enc(JSON.stringify(plain)));
			if (r && r.__patternError) throw new TypeError(`generate: ${r.message}`);
			return r;
		}

		exec(input, baseURL) {
			let values;
			try {
				values = inputComponents(input === undefined ? {} : input, baseURL);
			} catch (e) {
				return null; // an unparseable input is a non-match, not a throw
			}
			if (values === null) return null;
			const result = { inputs: baseURL === undefined ? [input] : [input, baseURL] };
			for (const c of COMPONENTS) {
				const { regexp, names } = this._parts[c];
				const m = regexp.exec(values[c] ?? "");
				if (!m) return null;
				const groups = {};
				names.forEach((n, i) => { groups[n] = m[i + 1]; });
				result[c] = { input: values[c] ?? "", groups };
			}
			return result;
		}
	}

	// compareComponent is a tentative API with no standard behind it; the ordering
	// is Chromium's, which is what the tests encode. It exists so a router can sort
	// patterns from least to most restrictive.
	URLPattern.compareComponent = function compareComponent(component, left, right) {
		const c = String(component);
		if (!COMPONENTS.includes(c)) {
			throw new TypeError(`compareComponent: ${c} is not a URL component`);
		}
		if (!(left instanceof URLPattern) || !(right instanceof URLPattern)) {
			throw new TypeError("compareComponent: expected two URLPattern objects");
		}
		const enc = (x) => new TextEncoder().encode(String(x));
		const r = ops.pattern_compare(c, enc(left[c]), enc(right[c]));
		if (r && r.__patternError) throw new TypeError(`compareComponent: ${r.message}`);
		return r;
	};

	Object.defineProperty(URLPattern.prototype, Symbol.toStringTag, {
		value: "URLPattern", configurable: true,
	});
	globalThis.URLPattern ??= URLPattern;
})();
