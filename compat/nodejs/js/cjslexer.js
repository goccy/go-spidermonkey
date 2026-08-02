// compat/nodejs: the CommonJS export lexer, over the engine's own AST.
//
// When an ES module imports a CommonJS file, the named exports it can bind
// are whichever string keys the CJS module would put on `module.exports` —
// a question about code that has not run yet. Node answers it with
// cjs-module-lexer; this answers it with Reflect.parse, the engine's parser,
// which is the same question asked of STRUCTURE rather than of text. The
// difference is not stylistic: a pattern-matching answer sees `exports.foo`
// inside a string literal or a comment, and misses one written any way its
// patterns did not anticipate.
//
// What it recognises (the shapes real transpiled code emits):
//
//	exports.NAME = …                     module.exports.NAME = …
//	exports["NAME"] = …                  module.exports['NAME'] = …
//	Object.defineProperty(exports, "NAME", …)
//	module.exports = { NAME, NAME2: …, "NAME3": …, NAME4() {} }
//	module.exports = require("…")        → the target's names (re-export)
//	__exportStar(require("…"), exports)  → the target's names
//	Object.keys(_m).forEach(k => exports[k] = _m[k])  where _m = require("…")
//
// The last three name another module rather than a binding; they come back as
// `reexports` for the host to resolve and merge, since only the host can walk
// the filesystem.
(() => {
	"use strict";

	const isIdent = (s) => typeof s === "string" && /^[A-Za-z_$][\w$]*$/.test(s);

	// exports / module.exports, as an expression shape.
	function isExportsRef(node) {
		if (!node) return false;
		if (node.type === "Identifier" && node.name === "exports") return true;
		return node.type === "MemberExpression" && !node.computed &&
			node.object && node.object.type === "Identifier" && node.object.name === "module" &&
			node.property && node.property.name === "exports";
	}
	// The static name a member expression addresses: .foo or ["foo"].
	function memberName(node) {
		if (!node || node.type !== "MemberExpression") return null;
		if (!node.computed) return node.property && node.property.name || null;
		const p = node.property;
		if (p && p.type === "Literal" && typeof p.value === "string") return p.value;
		return null;
	}
	function stringArg(node) {
		return node && node.type === "Literal" && typeof node.value === "string" ? node.value : null;
	}
	// require("…") as an expression.
	function requireTarget(node) {
		if (!node || node.type !== "CallExpression") return null;
		if (!node.callee || node.callee.type !== "Identifier" || node.callee.name !== "require") return null;
		return node.arguments && node.arguments.length ? stringArg(node.arguments[0]) : null;
	}

	function analyze(source) {
		let ast;
		try {
			ast = Reflect.parse(source, { source: "cjs", loc: false });
		} catch {
			// A file this parser rejects has no analysable structure; the
			// caller falls back to a default-only view rather than guessing.
			return { names: [], reexports: [], parsed: false };
		}
		const names = [];
		const reexports = [];
		const seen = new Set();
		const requireVars = new Map(); // local binding -> specifier
		const add = (n) => {
			if (!isIdent(n) || n === "default" || seen.has(n)) return;
			seen.add(n);
			names.push(n);
		};
		const addReexport = (spec) => {
			if (typeof spec === "string" && spec !== "" && !reexports.includes(spec)) reexports.push(spec);
		};

		const walk = (node) => {
			if (!node || typeof node !== "object") return;
			if (Array.isArray(node)) { for (const n of node) walk(n); return; }
			if (typeof node.type !== "string") { for (const k in node) walk(node[k]); return; }

			switch (node.type) {
				case "AssignmentExpression": {
					const left = node.left;
					// exports.NAME = … / exports["NAME"] = …
					if (left && left.type === "MemberExpression" && isExportsRef(left.object)) {
						add(memberName(left));
					}
					// module.exports = …
					if (isExportsRef(left)) {
						const right = node.right;
						if (right && right.type === "ObjectExpression") {
							for (const prop of right.properties || []) {
								const key = prop.key;
								if (!key) continue;
								if (key.type === "Identifier") add(key.name);
								else if (key.type === "Literal" && typeof key.value === "string") add(key.value);
							}
						}
						const target = requireTarget(right);
						if (target) addReexport(target);
					}
					break;
				}
				case "VariableDeclarator": {
					// var _m = require("…") — the binding a star-re-export loop reads.
					const target = requireTarget(node.init);
					if (target && node.id && node.id.type === "Identifier") {
						requireVars.set(node.id.name, target);
					}
					break;
				}
				case "CallExpression": {
					const callee = node.callee;
					const args = node.arguments || [];
					// Object.defineProperty(exports, "NAME", …)
					if (callee && callee.type === "MemberExpression" && !callee.computed &&
						callee.object && callee.object.type === "Identifier" && callee.object.name === "Object" &&
						callee.property && callee.property.name === "defineProperty" &&
						args.length >= 2 && isExportsRef(args[0])) {
						add(stringArg(args[1]));
					}
					// __exportStar(require("…"), exports) — the TypeScript helper,
					// bare or through tslib.
					if (callee && (
						(callee.type === "Identifier" && callee.name === "__exportStar") ||
						(callee.type === "MemberExpression" && callee.property && callee.property.name === "__exportStar")
					) && args.length >= 1) {
						const target = requireTarget(args[0]);
						if (target) addReexport(target);
					}
					// Object.keys(_m).forEach(…) with an exports[k] copy inside:
					// Babel's star re-export.
					if (callee && callee.type === "MemberExpression" && callee.property &&
						callee.property.name === "forEach" && callee.object &&
						callee.object.type === "CallExpression") {
						const inner = callee.object;
						const innerCallee = inner.callee;
						if (innerCallee && innerCallee.type === "MemberExpression" &&
							innerCallee.object && innerCallee.object.type === "Identifier" &&
							innerCallee.object.name === "Object" &&
							innerCallee.property && innerCallee.property.name === "keys" &&
							inner.arguments && inner.arguments.length &&
							inner.arguments[0].type === "Identifier") {
							const spec = requireVars.get(inner.arguments[0].name);
							if (spec && copiesOntoExports(args[0])) addReexport(spec);
						}
					}
					break;
				}
			}
			for (const k in node) {
				if (k === "type" || k === "loc") continue;
				walk(node[k]);
			}
		};

		// Does this callback body write to exports under a computed key? That
		// is what makes an Object.keys(...).forEach a re-export rather than
		// some other iteration.
		function copiesOntoExports(fn) {
			if (!fn || (fn.type !== "FunctionExpression" && fn.type !== "ArrowFunctionExpression")) return false;
			let found = false;
			const scan = (n) => {
				if (found || !n || typeof n !== "object") return;
				if (Array.isArray(n)) { for (const x of n) scan(x); return; }
				if (n.type === "AssignmentExpression" && n.left && n.left.type === "MemberExpression" &&
					n.left.computed && isExportsRef(n.left.object)) {
					found = true;
					return;
				}
				if (n.type === "CallExpression" && n.callee && n.callee.type === "MemberExpression" &&
					n.callee.property && n.callee.property.name === "defineProperty" &&
					n.arguments && n.arguments.length && isExportsRef(n.arguments[0])) {
					found = true;
					return;
				}
				for (const k in n) {
					if (k === "type" || k === "loc") continue;
					scan(n[k]);
				}
			};
			scan(fn.body);
			return found;
		}

		walk(ast);
		return { names, reexports, parsed: true };
	}

	Object.defineProperty(globalThis, "__node_cjs_lex", {
		value: (source) => JSON.stringify(analyze(String(source))),
		writable: true, enumerable: false, configurable: true,
	});
})();
