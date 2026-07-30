// compat/web: the WebAssembly JS API (https://webassembly.github.io/spec/js-api/).
//
// Execution is host-side (wasm.go, over wazero's interpreter); this file is
// the API surface, and in this API the surface IS most of the specification:
// argument coercion order, brand checks, the exact error class for each way a
// call can go wrong. The wasm/jsapi tests check those before they check that
// any code runs.
//
// i64 crosses the host boundary as decimal text (see wasm.go). BigInt is the
// JS face of it; the string is only ever the wire form.
//
// Linear memory: the host owns the bytes; mem.buffer is a guest ArrayBuffer
// kept in sync around every boundary crossing (calls in, imports out,
// instantiation, grow). A grow — from either side — detaches the old buffer,
// exactly as the specification says it must.
(() => {
	"use strict";

	// ------------------------------------------------------------ errors
	function defineErrorClass(name) {
		const cls = class extends Error {
			constructor(message) {
				super(message);
			}
		};
		Object.defineProperty(cls, "name", { value: name, configurable: true });
		Object.defineProperty(cls.prototype, "name", { value: name, writable: true, configurable: true });
		return cls;
	}
	const CompileError = defineErrorClass("CompileError");
	const LinkError = defineErrorClass("LinkError");
	const RuntimeError = defineErrorClass("RuntimeError");

	// rethrow maps a host-op failure onto the class its prefix names. The host
	// cannot construct guest error objects, so the class rides in front of the
	// message; the mapping is total — an unprefixed message becomes a plain
	// Error rather than vanishing.
	function rethrow(e) {
		const msg = String((e && e.message) || e);
		const m = /^(?:Error: )?(CompileError|LinkError|RuntimeError|TypeError|RangeError): ([\s\S]*)$/.exec(msg);
		if (!m) throw e;
		const body = m[2].split("\n")[0];
		switch (m[1]) {
			case "CompileError": throw new CompileError(body);
			case "LinkError": throw new LinkError(body);
			case "RuntimeError": throw new RuntimeError(body);
			case "TypeError": throw new TypeError(body);
			case "RangeError": throw new RangeError(body);
		}
	}

	// ------------------------------------------------------------ values
	function toBytes(source, ctor) {
		if (source instanceof ArrayBuffer) return new Uint8Array(source.slice(0));
		if (ArrayBuffer.isView(source)) {
			return new Uint8Array(source.buffer.slice(source.byteOffset, source.byteOffset + source.byteLength));
		}
		throw new TypeError(`${ctor}: argument must be a BufferSource`);
	}

	// toWasm applies the specification's ToWebAssemblyValue: i64 wants a BigInt
	// (a Number is a TypeError), every other numeric type wants a Number (a
	// BigInt is a TypeError). The returned value is what the host op accepts.
	function toWasm(type, v) {
		if (type === "i64") {
			if (typeof v === "number") throw new TypeError("cannot pass a Number as an i64");
			return String(BigInt.asIntN(64, BigInt(v)));
		}
		if (type === "v128" || type === "funcref" || type === "externref") {
			throw new TypeError(`cannot pass a JavaScript value as ${type}`);
		}
		if (typeof v === "bigint") throw new TypeError(`cannot pass a BigInt as ${type}`);
		return Number(v);
	}

	function fromWasm(type, v) {
		return type === "i64" ? BigInt.asIntN(64, BigInt(v)) : v;
	}

	// ------------------------------------------------------------ memory sync
	// The host owns a memory's bytes; the guest sees a copy in mem.buffer. Around
	// a crossing into wasm the copy is pushed in and pulled back out, so both
	// sides observe each other's stores.
	//
	// The set synced is the crossing's OWN memories — the ones the instance
	// imports or exports — never every memory that ever existed. That is not an
	// optimization but a correctness-of-scale property: a Memory nothing links
	// to cannot be touched by this call, and copying it anyway costs its full
	// size twice per crossing. wasm/jsapi's bad-imports constructs a 16 MiB
	// Memory as a wrong-type argument and then instantiates 200 more modules;
	// syncing it each time meant gigabytes of garbage through the engine heap,
	// and the engine faults rather than reporting exhaustion.
	function pushMemories(mems) {
		for (const mem of mems) {
			if (!mem._ab.detached) __wasm_mem_write(mem._id, new Uint8Array(mem._ab));
		}
	}
	function pullMemories(mems) {
		for (const mem of mems) mem._pull();
	}

	// withMemories wraps a crossing into wasm for one instance's memories.
	function withMemories(mems, fn) {
		pushMemories(mems);
		try {
			return fn();
		} finally {
			pullMemories(mems);
		}
	}

	// ------------------------------------------------------------ Memory
	class Memory {
		constructor(descriptor) {
			if (!new.target) throw new TypeError("WebAssembly.Memory must be called with new");
			if (descriptor === null || typeof descriptor !== "object") {
				throw new TypeError("Memory: descriptor must be an object");
			}
			// Coercion order is observable and tested: initial before maximum.
			const rawInitial = descriptor.initial ?? descriptor.minimum;
			if (rawInitial === undefined) throw new TypeError("Memory: initial is required");
			const initial = enforceU32(rawInitial, "initial");
			const hasMax = descriptor.maximum !== undefined;
			const maximum = hasMax ? enforceU32(descriptor.maximum, "maximum") : -1;
			if (hasMax && maximum < initial) {
				throw new RangeError("Memory: maximum is smaller than initial");
			}
			const shared = !!descriptor.shared;
			if (shared && !hasMax) {
				throw new TypeError("Memory: a shared memory must have a maximum");
			}
			try {
				this._id = __wasm_mem_new(initial, maximum, shared);
			} catch (e) {
				rethrow(e);
			}
			this._shared = shared;
			this._ab = new ArrayBuffer(initial * 0x10000);
		}
		get buffer() {
			brand(this, Memory, "buffer");
			return this._ab;
		}
		grow(delta) {
			brand(this, Memory, "grow");
			const d = enforceU32(delta, "delta");
			let prev;
			try {
				prev = __wasm_mem_grow(this._id, d);
			} catch (e) {
				rethrow(e);
			}
			if (prev < 0) throw new RangeError("Memory.grow: failed to grow");
			this._growTo();
			return prev;
		}
		// _pull refreshes the guest bytes from the host. A size change means the
		// memory grew (either side), and a grown memory's old buffer is DETACHED,
		// which is the one observable the tests lean on hardest.
		_pull() {
			const bytes = __wasm_mem_read(this._id);
			if (bytes.byteLength !== this._ab.byteLength) {
				this._adopt(bytes);
				return;
			}
			new Uint8Array(this._ab).set(bytes);
		}
		// _adopt takes the host's array as the guest buffer outright, rather than
		// allocating a second one and copying into it — for a large memory that
		// halves the traffic, and the host's array is freshly minted for us.
		_adopt(bytes) {
			if (typeof this._ab.transfer === "function" && !this._ab.detached) {
				this._ab.transfer(); // detach the old buffer, as grow must
			}
			this._ab = bytes.buffer;
		}
		_growTo() { this._adopt(__wasm_mem_read(this._id)); }
	}

	// ------------------------------------------------------------ Global
	const globalTypes = ["i32", "i64", "f32", "f64", "v128", "externref", "funcref"];
	class Global {
		constructor(descriptor, value) {
			if (!new.target) throw new TypeError("WebAssembly.Global must be called with new");
			if (descriptor === null || typeof descriptor !== "object") {
				throw new TypeError("Global: descriptor must be an object");
			}
			const mutable = !!descriptor.mutable;
			const type = String(descriptor.value);
			if (!globalTypes.includes(type)) {
				throw new TypeError(`Global: ${type} is not a value type`);
			}
			if (type === "v128") throw new TypeError("Global: v128 has no JavaScript representation");
			let wire;
			if (arguments.length > 1 && value !== undefined) {
				wire = toWasm(type, value);
			} else if (type === "i64") {
				wire = "0";
			} else {
				wire = 0;
			}
			try {
				this._id = __wasm_global_new(type, mutable, wire);
			} catch (e) {
				rethrow(e);
			}
			this._type = type;
			this._mutable = mutable;
		}
		get value() {
			brand(this, Global, "value");
			if (this._type === "externref" || this._type === "funcref") return null;
			try {
				return fromWasm(this._type, __wasm_global_get(this._id));
			} catch (e) {
				rethrow(e);
			}
		}
		set value(v) {
			brand(this, Global, "value");
			if (!this._mutable) throw new TypeError("Global: the global is immutable");
			try {
				__wasm_global_set(this._id, toWasm(this._type, v));
			} catch (e) {
				rethrow(e);
			}
		}
		valueOf() {
			brand(this, Global, "valueOf");
			return this.value;
		}
	}

	// ------------------------------------------------------------ Table
	// Table is API-complete but engine-detached: wazero exposes no table
	// manipulation, so a table constructed here tracks its own entries and an
	// exported table reports only its existence. Recorded as partial in
	// docs/conformance-plan.md.
	class Table {
		constructor(descriptor, value) {
			if (!new.target) throw new TypeError("WebAssembly.Table must be called with new");
			if (descriptor === null || typeof descriptor !== "object") {
				throw new TypeError("Table: descriptor must be an object");
			}
			const element = String(descriptor.element);
			if (element !== "anyfunc" && element !== "funcref" && element !== "externref") {
				throw new TypeError(`Table: ${element} is not an element type`);
			}
			if (descriptor.initial === undefined) throw new TypeError("Table: initial is required");
			const initial = enforceU32(descriptor.initial, "initial");
			const hasMax = descriptor.maximum !== undefined;
			const maximum = hasMax ? enforceU32(descriptor.maximum, "maximum") : Infinity;
			if (maximum < initial) throw new RangeError("Table: maximum is smaller than initial");
			const fill = arguments.length > 1 ? value : null;
			this._element = element;
			this._max = maximum;
			this._entries = new Array(initial).fill(fill ?? null);
		}
		get length() {
			brand(this, Table, "length");
			return this._entries.length;
		}
		get(i) {
			brand(this, Table, "get");
			const idx = enforceU32(i, "index");
			if (idx >= this._entries.length) throw new RangeError("Table.get: out of range");
			return this._entries[idx];
		}
		set(i, v) {
			brand(this, Table, "set");
			const idx = enforceU32(i, "index");
			if (idx >= this._entries.length) throw new RangeError("Table.set: out of range");
			this._entries[idx] = arguments.length > 1 ? v : null;
		}
		grow(delta, value) {
			brand(this, Table, "grow");
			const d = enforceU32(delta, "delta");
			const prev = this._entries.length;
			if (prev + d > this._max) throw new RangeError("Table.grow: beyond maximum");
			this._entries.length = prev + d;
			this._entries.fill(arguments.length > 1 ? value : null, prev);
			return prev;
		}
	}

	// ------------------------------------------------------------ Tag / Exception
	class Tag {
		constructor(type) {
			if (!new.target) throw new TypeError("WebAssembly.Tag must be called with new");
			if (type === null || typeof type !== "object") {
				throw new TypeError("Tag: type must be an object");
			}
			const params = type.parameters;
			if (!Array.isArray(params) && !(params && typeof params[Symbol.iterator] === "function")) {
				throw new TypeError("Tag: parameters must be iterable");
			}
			this._params = [...params].map(String);
			for (const p of this._params) {
				if (!globalTypes.includes(p)) throw new TypeError(`Tag: ${p} is not a value type`);
			}
		}
		type() {
			brand(this, Tag, "type");
			return { parameters: [...this._params] };
		}
	}

	class WasmException {
		constructor(tag, payload, options = {}) {
			if (!new.target) throw new TypeError("WebAssembly.Exception must be called with new");
			if (!(tag instanceof Tag)) throw new TypeError("Exception: first argument must be a Tag");
			const values = [...payload];
			if (values.length !== tag._params.length) {
				throw new TypeError("Exception: payload length does not match the tag");
			}
			this._tag = tag;
			this._values = values.map((v, i) => {
				const t = tag._params[i];
				return t === "i64" ? BigInt.asIntN(64, BigInt(v))
					: t === "externref" || t === "funcref" ? v : Number(v);
			});
			this._stack = options && options.traceStack ? new Error().stack : undefined;
		}
		is(tag) {
			brand(this, WasmException, "is");
			if (!(tag instanceof Tag)) throw new TypeError("Exception.is: argument must be a Tag");
			return tag === this._tag;
		}
		getArg(tag, index) {
			brand(this, WasmException, "getArg");
			if (!(tag instanceof Tag)) throw new TypeError("Exception.getArg: argument must be a Tag");
			if (tag !== this._tag) throw new TypeError("Exception.getArg: tag does not match");
			const i = enforceU32(index, "index");
			if (i >= this._values.length) throw new RangeError("Exception.getArg: out of range");
			return this._values[i];
		}
		get stack() { return this._stack; }
	}

	// ------------------------------------------------------------ Module
	const moduleState = new WeakMap(); // Module -> {id, meta}
	class Module {
		constructor(bytes) {
			if (!new.target) throw new TypeError("WebAssembly.Module must be called with new");
			const b = toBytes(bytes, "Module");
			let id;
			try {
				id = __wasm_compile(b);
			} catch (e) {
				rethrow(e);
			}
			moduleState.set(this, { id, meta: __wasm_meta(id) });
		}
		static exports(module) {
			const st = moduleState.get(module);
			if (!st) throw new TypeError("Module.exports: argument must be a Module");
			return st.meta.exports.map((e) => ({ name: e.name, kind: e.kind }));
		}
		static imports(module) {
			const st = moduleState.get(module);
			if (!st) throw new TypeError("Module.imports: argument must be a Module");
			return st.meta.imports.map((i) => ({ module: i.module, name: i.name, kind: i.kind }));
		}
		static customSections(module, name) {
			const st = moduleState.get(module);
			if (!st) throw new TypeError("Module.customSections: argument must be a Module");
			if (arguments.length < 2) throw new TypeError("Module.customSections: a section name is required");
			const sectionName = String(name);
			const out = [];
			const n = __wasm_customs(st.id, sectionName);
			for (let i = 0; i < n; i++) {
				const bytes = __wasm_custom(st.id, sectionName, i);
				out.push(new Uint8Array(bytes).slice().buffer);
			}
			return out;
		}
	}

	// ------------------------------------------------------------ imports
	// Import tokens: the host trampoline reports a token; this table maps it
	// back to the JS function the import object supplied.
	const importFns = new Map();
	let nextToken = 1;

	// pendingImportError carries a JS exception thrown by an import back out to
	// the JS caller UNCHANGED. The specification propagates such an exception
	// through the wasm frames as itself, so wrapping it in a RuntimeError — which
	// is what a host-side trap looks like — would be the wrong error entirely.
	// The host cannot hold a JS value across the trap, so it is parked here and
	// rethrown by the wrapper that started the call.
	let pendingImportError = { has: false, value: undefined };

	globalThis.__wasm_import = (token, ...args) => {
		const entry = importFns.get(token);
		// The bytes wasm wrote so far must be visible to the import, and what the
		// import writes must be visible to wasm when it resumes — the same push/
		// pull as a call in, with the directions swapped.
		pullMemories(entry.mems);
		try {
			const jsArgs = entry.params.map((t, i) => fromWasm(t, args[i]));
			const ret = entry.fn(...jsArgs);
			return entry.results.length === 1 ? toWasm(entry.results[0], ret) : undefined;
		} catch (e) {
			pendingImportError = { has: true, value: e };
			throw e;
		} finally {
			pushMemories(entry.mems);
		}
	};

	// callIntoWasm runs one crossing and, if an import threw on the way, rethrows
	// that exception in place of the trap the host reports.
	function callIntoWasm(mems, fn) {
		pendingImportError = { has: false, value: undefined };
		try {
			return withMemories(mems, fn);
		} catch (e) {
			if (pendingImportError.has) {
				const thrown = pendingImportError.value;
				pendingImportError = { has: false, value: undefined };
				throw thrown;
			}
			throw e;
		}
	}

	// readImports walks the import object in the specification's own order —
	// every Get is observable — and resolves each import to what the host can
	// link: a function token, an entity handle, or a global value.
	function readImports(meta, importObject, mems) {
		if (meta.imports.length > 0 && (importObject === undefined || importObject === null)) {
			throw new TypeError("Instance: the module has imports, so an import object is required");
		}
		const spec = [];
		for (const imp of meta.imports) {
			const moduleValue = importObject[imp.module];
			if (moduleValue === null || typeof moduleValue !== "object" && typeof moduleValue !== "function") {
				throw new TypeError(`Instance: import module ${imp.module} is not an object`);
			}
			const v = moduleValue[imp.name];
			switch (imp.kind) {
				case "function": {
					if (typeof v !== "function") {
						throw new LinkError(`Instance: import ${imp.module}.${imp.name} is not a function`);
					}
					const token = nextToken++;
					importFns.set(token, {
						fn: v,
						params: imp.params || [],
						results: imp.results || [],
						mems,
					});
					spec.push({ kind: "func", token });
					break;
				}
				case "memory": {
					if (!(v instanceof Memory)) {
						throw new LinkError(`Instance: import ${imp.module}.${imp.name} is not a Memory`);
					}
					spec.push({ kind: "memory", id: v._id });
					mems.push(v);
					break;
				}
				case "global": {
					if (v instanceof Global) {
						spec.push({ kind: "global", id: v._id });
					} else if (typeof v === "number" || typeof v === "bigint") {
						spec.push({ kind: "global-value", value: toWasm(imp.type, v) });
					} else {
						throw new LinkError(`Instance: import ${imp.module}.${imp.name} is not a global`);
					}
					break;
				}
				case "table": {
					// wazero exposes no table plumbing; a table import cannot link.
					throw new LinkError(`Instance: table import ${imp.module}.${imp.name} is not supported`);
				}
				default:
					throw new LinkError(`Instance: unsupported import kind ${imp.kind}`);
			}
		}
		return spec;
	}

	// ------------------------------------------------------------ Instance
	// mems is the instance's own memory list, filled in as the exports are built
	// and shared by reference with every exported function and import trampoline
	// of that instance — so a memory exported later is still synced by a call
	// made through a wrapper built earlier.
	function makeExportedFunction(instanceId, exp, meta, mems) {
		const sig = meta.exports.find((e) => e.name === exp.name) || {};
		const params = sig.params || [];
		const fn = function (...args) {
			return callIntoWasm(mems, () => {
				const wire = params.map((t, i) => toWasm(t, args[i] === undefined && t !== "i64" ? NaN : args[i]));
				let n;
				try {
					n = __wasm_call(instanceId, exp.name, ...wire);
				} catch (e) {
					rethrow(e);
				}
				if (n === 0) return undefined;
				if (n === 1) return fromWasmRet(0);
				const out = [];
				for (let i = 0; i < n; i++) out.push(fromWasmRet(i));
				return out;
			});
		};
		// An exported wasm function is named by its FUNCTION INDEX, as a string —
		// not by the string it was exported under. Two exports of the same
		// function share a name; an export named "" still has a numeric one.
		Object.defineProperty(fn, "length", { value: params.length, writable: false, enumerable: false, configurable: true });
		Object.defineProperty(fn, "name", { value: String(sig.index ?? 0), writable: false, enumerable: false, configurable: true });
		return fn;
	}

	function fromWasmRet(i) {
		const v = __wasm_ret(i);
		return typeof v === "string" ? BigInt.asIntN(64, BigInt(v)) : v;
	}

	const instanceExports = new WeakMap();
	class Instance {
		constructor(module, importObject) {
			if (!new.target) throw new TypeError("WebAssembly.Instance must be called with new");
			const st = moduleState.get(module);
			if (!st) throw new TypeError("Instance: first argument must be a Module");
			// The instance's memories: those it imports, plus those it exports,
			// discovered below. The list is shared by reference with the import
			// trampolines readImports registers, so an import called during the
			// start function already sees the right set.
			const mems = [];
			const spec = readImports(st.meta, importObject, mems);
			let result;
			try {
				result = callIntoWasm(mems, () => __wasm_instantiate(st.id, spec));
			} catch (e) {
				rethrow(e);
			}
			// The exports object's properties are non-writable, ENUMERABLE and
			// non-configurable, and the object itself is non-extensible with a null
			// prototype. Object.freeze would also make them non-enumerable-safe but
			// leaves configurable false and writable false — which is what is wanted
			// — so the values are defined explicitly and the object sealed after.
			const exports = Object.create(null);
			const define = (name, value) => {
				Object.defineProperty(exports, name, {
					value, writable: false, enumerable: true, configurable: false,
				});
			};
			for (const exp of result.exports) {
				switch (exp.kind) {
					case "function":
						define(exp.name, makeExportedFunction(result.instance, exp, st.meta, mems));
						break;
					case "memory": {
						const mem = Object.create(Memory.prototype);
						mem._id = exp.id;
						mem._shared = false;
						mem._ab = new ArrayBuffer(0);
						mem._pull();
						mems.push(mem);
						define(exp.name, mem);
						break;
					}
					case "global": {
						const g = Object.create(Global.prototype);
						g._id = exp.id;
						const probe = __wasm_global_get(exp.id);
						g._type = typeof probe === "string" ? "i64" : "f64";
						g._type = exp.type || g._type;
						g._mutable = true; // the host rejects an immutable set
						define(exp.name, g);
						break;
					}
					case "table": {
						const t = Object.create(Table.prototype);
						t._element = "funcref";
						t._max = Infinity;
						t._entries = [];
						define(exp.name, t);
						break;
					}
				}
			}
			Object.preventExtensions(exports);
			instanceExports.set(this, exports);
		}
		get exports() {
			const e = instanceExports.get(this);
			if (!e) throw new TypeError("Instance.exports: receiver is not an Instance");
			return e;
		}
	}

	// ------------------------------------------------------------ namespace
	function validate(bytes) {
		return __wasm_validate(toBytes(bytes, "validate"));
	}
	// The IDL arity is the number of REQUIRED arguments, so instantiate declares
	// one even though it reads two, and the namespace's operations are writable,
	// non-enumerable and configurable like any other.


	async function compile(bytes) {
		// The copy happens synchronously (detaching after the call must not
		// affect the compile), the work in a microtask, per the specification.
		const b = toBytes(bytes, "compile");
		return new Module(b);
	}

	async function instantiate(bytesOrModule, importObject) {
		if (moduleState.has(bytesOrModule)) {
			return new Instance(bytesOrModule, importObject);
		}
		const b = toBytes(bytesOrModule, "instantiate");
		const module = new Module(b);
		const instance = new Instance(module, importObject);
		return { module, instance };
	}

	async function streamSource(source, who) {
		const response = await source;
		if (typeof Response === "undefined" || !(response instanceof Response)) {
			throw new TypeError(`${who}: argument must be a Response`);
		}
		const ct = (response.headers.get("Content-Type") || "").split(";")[0].trim().toLowerCase();
		if (ct !== "application/wasm") {
			throw new TypeError(`${who}: the response has MIME type '${ct}', expected 'application/wasm'`);
		}
		if (!response.ok) {
			throw new TypeError(`${who}: the response has status ${response.status}`);
		}
		return new Uint8Array(await response.arrayBuffer());
	}

	async function compileStreaming(source) {
		return new Module(await streamSource(source, "compileStreaming"));
	}

	async function instantiateStreaming(source, importObject) {
		const module = new Module(await streamSource(source, "instantiateStreaming"));
		return { module, instance: new Instance(module, importObject) };
	}

	// ------------------------------------------------------------ helpers
	function enforceU32(v, what) {
		const n = Number(v);
		if (!Number.isFinite(n) && !Number.isNaN(n)) {
			throw new TypeError(`${what} must be a valid unsigned integer`);
		}
		const i = Math.trunc(n) || 0;
		if (i < 0 || i > 0xffffffff) throw new TypeError(`${what} is out of range`);
		return i >>> 0;
	}

	function brand(self, cls, what) {
		if (!(self instanceof cls)) {
			throw new TypeError(`${what} called on an incompatible receiver`);
		}
	}

	for (const [cls, tag, arity] of [[Module, "WebAssembly.Module", 1], [Instance, "WebAssembly.Instance", 1],
		[Memory, "WebAssembly.Memory", 1], [Table, "WebAssembly.Table", 1], [Global, "WebAssembly.Global", 1],
		[Tag, "WebAssembly.Tag", 1], [WasmException, "WebAssembly.Exception", 2]]) {
		Object.defineProperty(cls.prototype, Symbol.toStringTag, { value: tag, configurable: true });
		Object.defineProperty(cls, "length", { value: arity, writable: false, enumerable: false, configurable: true });
		// The interface NAME is the last component: idlharness reads it, and a
		// class expression would otherwise report the binding it was written as.
		Object.defineProperty(cls, "name", { value: tag.split(".").pop(), writable: false, enumerable: false, configurable: true });
	}

	// A NAMESPACE's members are enumerable — unlike an interface's, which are not.
	// idlharness checks the distinction on every member.
	const WebAssembly = {};
	for (const [name, value] of [["Module", Module], ["Instance", Instance],
		["Memory", Memory], ["Table", Table], ["Global", Global], ["Tag", Tag],
		["Exception", WasmException], ["CompileError", CompileError],
		["LinkError", LinkError], ["RuntimeError", RuntimeError]]) {
		Object.defineProperty(WebAssembly, name, { value, writable: true, enumerable: true, configurable: true });
	}
	for (const [name, fn, arity] of [["compile", compile, 1], ["compileStreaming", compileStreaming, 1],
		["instantiate", instantiate, 1], ["instantiateStreaming", instantiateStreaming, 1],
		["validate", validate, 1]]) {
		Object.defineProperty(fn, "length", { value: arity, writable: false, enumerable: false, configurable: true });
		Object.defineProperty(fn, "name", { value: name, writable: false, enumerable: false, configurable: true });
		Object.defineProperty(WebAssembly, name, { value: fn, writable: true, enumerable: true, configurable: true });
	}
	Object.defineProperty(WebAssembly, "JSTag", {
		value: new Tag({ parameters: ["externref"] }), writable: false, enumerable: true, configurable: false,
	});
	Object.defineProperty(WebAssembly, Symbol.toStringTag, { value: "WebAssembly", configurable: true });
	globalThis.WebAssembly = WebAssembly;
})();
