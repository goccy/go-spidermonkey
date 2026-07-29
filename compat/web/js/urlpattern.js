// compat/web: URLPattern (https://urlpattern.spec.whatwg.org/).
//
// A URL pattern is eight independent component patterns — protocol, username,
// password, hostname, port, pathname, search, hash — each compiled to a regular
// expression with named groups. The syntax is path-to-regexp's: `:name` for a
// named segment, `(regex)` for an inline pattern, `*` for a wildcard, `{…}` for
// a group, and `?`/`+`/`*` as modifiers on any of them.
//
// It is implemented here rather than left absent because it is a WinterTC API,
// it has no host dependencies at all, and its absence was worth 809 Web
// Platform Test subtests.
(() => {
	"use strict";

	const COMPONENTS = [
		"protocol", "username", "password", "hostname",
		"port", "pathname", "search", "hash",
	];

	// Each component decides what a bare `*` or `:name` may span. A pathname
	// segment stops at "/", a hostname label at "."; everything else is opaque.
	const OPTIONS = {
		protocol: { prefix: "", delimiter: "" },
		username: { prefix: "", delimiter: "" },
		password: { prefix: "", delimiter: "" },
		hostname: { prefix: "", delimiter: "." },
		port: { prefix: "", delimiter: "" },
		pathname: { prefix: "/", delimiter: "/" },
		search: { prefix: "", delimiter: "" },
		hash: { prefix: "", delimiter: "" },
	};

	const escapeRegExp = (s) => s.replace(/[.+*?^${}()[\]|/\\]/g, "\\$&");
	const isNameChar = (c) =>
		(c >= "a" && c <= "z") || (c >= "A" && c <= "Z") ||
		(c >= "0" && c <= "9") || c === "_" || c === "$";

	// parsePattern turns one component's pattern into a list of parts:
	//   {type:"fixed", value}
	//   {type:"segment", name, regex, prefix, suffix, modifier}
	// A modifier is "", "?", "+" or "*".
	// checkRegexGroup rejects a custom matcher the spec forbids: it must be
	// plain ASCII and it must actually compile. Accepting either meant a
	// pattern the platform requires to be a TypeError was quietly built and
	// then matched nothing.
	function checkRegexGroup(body) {
		for (let i = 0; i < body.length; i++) {
			if (body.charCodeAt(i) > 0x7f) {
				throw new TypeError("URLPattern: a custom matcher must be ASCII: (" + body + ")");
			}
		}
		try {
			new RegExp(body, "u");
		} catch (e) {
			throw new TypeError("URLPattern: invalid custom matcher (" + body + "): " + e.message);
		}
		return body;
	}

	// A group name is ASCII too, for the same reason.
	function checkName(name) {
		for (let i = 0; i < name.length; i++) {
			if (name.charCodeAt(i) > 0x7f) {
				throw new TypeError("URLPattern: a group name must be ASCII: :" + name);
			}
		}
		return name;
	}

	function parsePattern(pattern, opts) {
		const parts = [];
		let fixed = "";
		let anonymous = 0;
		const pushFixed = () => {
			if (fixed) { parts.push({ type: "fixed", value: fixed }); fixed = ""; }
		};
		let i = 0;
		while (i < pattern.length) {
			const c = pattern[i];
			if (c === "\\") {
				fixed += pattern[i + 1] ?? "";
				i += 2;
				continue;
			}
			if (c === "{") {
				// A group: everything to the matching "}", which may itself contain a
				// :name or (regex), plus an optional modifier after the brace.
				let depth = 1, j = i + 1, inner = "";
				while (j < pattern.length && depth > 0) {
					if (pattern[j] === "\\") { inner += pattern[j] + (pattern[j + 1] ?? ""); j += 2; continue; }
					if (pattern[j] === "{") depth++;
					if (pattern[j] === "}") { depth--; if (depth === 0) break; }
					inner += pattern[j];
					j++;
				}
				i = j + 1;
				const modifier = "?+*".includes(pattern[i]) ? pattern[i++] : "";
				const innerParts = parsePattern(inner, opts);
				pushFixed();
				parts.push({ type: "group", parts: innerParts, modifier });
				continue;
			}
			if (c === ":") {
				let j = i + 1, name = "";
				while (j < pattern.length && isNameChar(pattern[j])) name += pattern[j++];
				if (!name) { fixed += c; i++; continue; }
				let regex = null;
				if (pattern[j] === "(") {
					let depth = 1, k = j + 1, body = "";
					while (k < pattern.length && depth > 0) {
						if (pattern[k] === "\\") { body += pattern[k] + (pattern[k + 1] ?? ""); k += 2; continue; }
						if (pattern[k] === "(") depth++;
						if (pattern[k] === ")") { depth--; if (depth === 0) break; }
						body += pattern[k];
						k++;
					}
					regex = checkRegexGroup(body);
					j = k + 1;
				}
				const modifier = "?+*".includes(pattern[j]) ? pattern[j++] : "";
				pushFixed();
				parts.push({ type: "segment", name: checkName(name), regex, modifier });
				i = j;
				continue;
			}
			if (c === "(") {
				let depth = 1, k = i + 1, body = "";
				while (k < pattern.length && depth > 0) {
					if (pattern[k] === "\\") { body += pattern[k] + (pattern[k + 1] ?? ""); k += 2; continue; }
					if (pattern[k] === "(") depth++;
					if (pattern[k] === ")") { depth--; if (depth === 0) break; }
					body += pattern[k];
					k++;
				}
				let j = k + 1;
				const modifier = "?+*".includes(pattern[j]) ? pattern[j++] : "";
				pushFixed();
				parts.push({ type: "segment", name: String(anonymous++), regex: checkRegexGroup(body), modifier });
				i = j;
				continue;
			}
			if (c === "*") {
				let j = i + 1;
				const modifier = "?+*".includes(pattern[j]) ? pattern[j++] : "";
				pushFixed();
				parts.push({ type: "segment", name: String(anonymous++), regex: ".*", modifier, wildcard: true });
				i = j;
				continue;
			}
			fixed += c;
			i++;
		}
		pushFixed();
		// path-to-regexp's PREFIX rule: a delimiter immediately before a modified
		// segment belongs to that segment, so "/foo/:bar?" matches "/foo" (the
		// slash is optional too) and not "/foo/". Without this every ?/+/* pattern
		// matched the wrong set of inputs.
		if (opts.prefix) {
			for (let k = 1; k < parts.length; k++) {
				const seg = parts[k];
				const before = parts[k - 1];
				if (seg.type !== "segment" || before.type !== "fixed") continue;
				if (!before.value.endsWith(opts.prefix)) continue;
				seg.prefix = opts.prefix;
				before.value = before.value.slice(0, -opts.prefix.length);
				if (before.value === "") { parts.splice(k - 1, 1); k--; }
			}
		}
		return parts;
	}

	// compile turns parts into a RegExp source plus the group names, in order.
	function compileParts(parts, opts, names) {
		let src = "";
		for (const p of parts) {
			if (p.type === "fixed") { src += escapeRegExp(p.value); continue; }
			if (p.type === "group") {
				const inner = compileParts(p.parts, opts, names);
				src += p.modifier ? `(?:${inner})${p.modifier}` : `(?:${inner})`;
				continue;
			}
			names.push(p.name);
			// A bare :name stops at the component's delimiter; an explicit regex
			// says exactly what it accepts.
			const body = p.regex !== null && p.regex !== undefined
				? p.regex
				: opts.delimiter
					? `[^${escapeRegExp(opts.delimiter)}]+?`
					: "[^]+?";
			const pre = p.prefix ? escapeRegExp(p.prefix) : "";
			// A repeated segment spans the delimiter (":bar+" matches "bar/baz", not
			// one segment) and the FIRST prefix stays outside the capture: the group
			// is "bar/baz", not "/bar/baz". An absent optional group is undefined,
			// which is why the whole thing — prefix included — is what repeats.
			const repeat = `(?:${body})(?:${pre}(?:${body}))*`;
			switch (p.modifier) {
				case "?": src += pre ? `(?:${pre}(${body}))?` : `(${body})?`; break;
				case "+": src += pre ? `${pre}(${repeat})` : `((?:${body})+)`; break;
				case "*": src += pre ? `(?:${pre}(${repeat}))?` : `((?:${body})*)`; break;
				default: src += `${pre}(${body})`;
			}
		}
		return src;
	}

	// serializeParts renders the CANONICAL form of a pattern: "(.*)"" is "*",
	// escapes are normalized, and a prefix moved into a segment comes back out.
	// The component getters must return this, not the input text — the tests
	// compare `pattern.pathname` against the canonical spelling.
	function serializeParts(parts) {
		let out = "";
		for (const p of parts) {
			if (p.type === "fixed") { out += p.value.replace(/([:*?+{}()\\])/g, "\\$1"); continue; }
			if (p.type === "group") {
				const inner = serializeParts(p.parts);
				// Braces that carry no modifier and wrap only literal text mean
				// nothing: "{/bar}" IS "/bar".
				if (!p.modifier && p.parts.every((q) => q.type === "fixed")) { out += inner; continue; }
				out += `{${inner}}${p.modifier}`;
				continue;
			}
			out += p.prefix || "";
			if (p.wildcard || (p.regex === ".*" && /^\d+$/.test(p.name))) {
				out += "*";
			} else if (/^\d+$/.test(p.name)) {
				out += `(${p.regex})`;
			} else {
				out += `:${p.name}`;
				if (p.regex !== null && p.regex !== undefined) out += `(${p.regex})`;
			}
			out += p.modifier;
		}
		return out;
	}

	function compileComponent(pattern, component) {
		const opts = OPTIONS[component];
		const names = [];
		const parts = parsePattern(pattern, opts);
		const src = compileParts(parts, opts, names);
		// The spec restricts an inline regex to ASCII; a non-ASCII one is a
		// TypeError from the constructor rather than a pattern that never matches.
		for (const p of parts) {
			if (p.type === "segment" && p.regex && /[^\x00-\x7f]/.test(p.regex)) {
				throw new TypeError(`Invalid pattern for ${component}: regexp must be ASCII`);
			}
		}
		let regexp;
		try {
			regexp = new RegExp(`^${src}$`, "u");
		} catch (e) {
			// A pattern whose inline regex is invalid is a TypeError from the
			// constructor, which is what the spec asks for.
			throw new TypeError(`Invalid pattern for ${component}: ${pattern}`);
		}
		return {
			pattern: serializeParts(parts),
			regexp,
			names,
			hasRegexpGroups: parts.some(hasExplicitRegex),
		};
	}

	function hasExplicitRegex(p) {
		if (p.type === "group") return p.parts.some(hasExplicitRegex);
		return p.type === "segment" && !p.wildcard && p.regex !== null && p.regex !== undefined;
	}

	// componentsFromString parses a full-URL pattern into its components by
	// treating it as a URL whose parts are patterns. Anything it cannot split is
	// left to the pathname, which is what a bare "/foo/:id" means.
	function componentsFromString(input, baseURL) {
		const out = {};
		let rest = String(input);
		const protoEnd = rest.indexOf("://");
		if (protoEnd > 0 && !rest.slice(0, protoEnd).includes("/")) {
			out.protocol = rest.slice(0, protoEnd);
			rest = rest.slice(protoEnd + 3);
			// userinfo@host
			const slash = rest.search(/[/?#]/);
			let authority = slash < 0 ? rest : rest.slice(0, slash);
			rest = slash < 0 ? "" : rest.slice(slash);
			const at = authority.lastIndexOf("@");
			if (at >= 0) {
				const userinfo = authority.slice(0, at);
				authority = authority.slice(at + 1);
				const colon = userinfo.indexOf(":");
				if (colon >= 0) {
					out.username = userinfo.slice(0, colon);
					out.password = userinfo.slice(colon + 1);
				} else {
					out.username = userinfo;
				}
			}
			// A ":" that is a port separator, not a :name pattern, has digits or a
			// pattern after it and no "{" before it.
			const portMatch = /^(.*?):([^:]*)$/.exec(authority);
			if (portMatch && !portMatch[2].startsWith(":") && /^[0-9*{(\\]/.test(portMatch[2] || "0")) {
				out.hostname = portMatch[1];
				out.port = portMatch[2];
			} else {
				out.hostname = authority;
			}
		}
		const hashIdx = rest.indexOf("#");
		if (hashIdx >= 0) { out.hash = rest.slice(hashIdx + 1); rest = rest.slice(0, hashIdx); }
		const qIdx = rest.indexOf("?");
		if (qIdx >= 0) { out.search = rest.slice(qIdx + 1); rest = rest.slice(0, qIdx); }
		if (rest !== "") out.pathname = rest;
		// A string pattern is a URL, so the parts a URL always has are present
		// even when the text does not spell them out: an authority with no port
		// means the empty port, not "any port", and an absolute URL has a path.
		// Left as "*" these matched anything, which is how a pattern for
		// "https://example.com/" also matched "https://example.com:8080/".
		if (out.protocol !== undefined && out.port === undefined) out.port = "";
		if (baseURL !== undefined) {
			const base = new URL(baseURL);
			if (out.protocol === undefined) out.protocol = base.protocol.replace(/:$/, "");
			if (out.hostname === undefined) out.hostname = base.hostname;
			if (out.port === undefined) out.port = base.port;
			if (out.pathname === undefined) out.pathname = base.pathname;
		}
		return out;
	}

	// inputComponents turns exec/test input into the eight strings to match.
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
			for (const c of COMPONENTS) {
				// An unspecified component matches anything.
				const pattern = init[c] === undefined ? "*" : String(init[c]);
				const compiled = compileComponent(pattern, c);
				if (this._ignoreCase) {
					compiled.regexp = new RegExp(compiled.regexp.source, "ui");
				}
				this._parts[c] = compiled;
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

	Object.defineProperty(URLPattern.prototype, Symbol.toStringTag, {
		value: "URLPattern", configurable: true,
	});
	globalThis.URLPattern ??= URLPattern;
})();
