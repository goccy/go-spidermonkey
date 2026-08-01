package web

import (
	"context"
	"encoding/binary"
	"fmt"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/internal/eventloop"
	"github.com/tetratelabs/wazero"
	wazapi "github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"math"
	"strconv"
	"strings"
	"sync"
)

// wasm.go: the Go half of the WebAssembly JS API
// (https://webassembly.github.io/spec/js-api/).
//
// The engine cannot run WebAssembly: SpiderMonkey only executes wasm by
// compiling it to native code, which a wasm-hosted engine cannot emit (see
// docs/engine-followups.md item 9). So the JS API is implemented the way fetch
// and WebSocket are — the API shape in the guest (js/wasm.js), the machinery
// host-side — with wazero's pure-Go interpreter executing the code. ECMA-429
// makes this namespace REQUIRED, so "the engine can't" was never an answer.
//
// The one impedance mismatch worth naming: wazero has no module-less entities
// and links imports strictly by module NAME, while the JS API hands loose
// Memory/Global objects around in import objects. The bridge between the two
// is in the binary-format section below — every standalone entity is born
// inside a tiny emitted
// module with a unique name, and an importer's import section is rewritten to
// point at whichever modules back its JS-provided imports.
//
// i64 values cross the JS/Go boundary as decimal strings. The boundary cannot
// carry a BigInt structurally (the bridge's value set is float64 | string |
// bool | object), and a float64 would corrupt beyond 2^53; decimal text is a
// total, unambiguous encoding both sides parse exactly.

type wasmAPI struct {
	js         *spidermonkey.JS
	loop       *eventloop.Loop
	importCall *spidermonkey.Object // __wasm_import(token, ...args) -> value

	// Everything below is loop-goroutine-only: every op runs there, and wasm
	// execution is synchronous inside an op. The mutex guards nothing hotter
	// than teardown.
	mu        sync.Mutex
	rt        wazero.Runtime
	modules   map[int64]*wasmModule
	instances map[int64]wazapi.Module
	memories  map[int64]wazapi.Memory
	globals   map[int64]wazapi.Global
	next      int64
	// lastResults holds the values of the most recent exported-function call,
	// fetched one op at a time: a single op result cannot carry a NaN-bearing
	// list without corrupting it (composites cross as JSON).
	lastResults []uint64
	lastTypes   []wazapi.ValueType
}

type wasmModule struct {
	bytes    []byte
	compiled wazero.CompiledModule
	meta     *wasmMeta
}

func installWasm(js *spidermonkey.JS, loop *eventloop.Loop) (*wasmAPI, error) {
	a := &wasmAPI{
		js: js, loop: loop,
		// Threads are enabled so a shared memory can exist: WebAssembly.Memory
		// accepts `shared: true`, and the buffer it then reports is a
		// SharedArrayBuffer — a type the suite derives from exactly that
		// expression (common/sab.js). Refusing the memory would refuse the type.
		rt: wazero.NewRuntimeWithConfig(context.Background(),
			wazero.NewRuntimeConfigInterpreter().WithCoreFeatures(
				wazapi.CoreFeaturesV2|experimental.CoreFeaturesThreads)),
		modules:   map[int64]*wasmModule{},
		instances: map[int64]wazapi.Module{},
		memories:  map[int64]wazapi.Memory{},
		globals:   map[int64]wazapi.Global{},
	}
	v, err := js.Global().Get("__wasm_import")
	if err != nil {
		return nil, err
	}
	o := v.Object()
	if o == nil || !o.IsFunction() {
		return nil, fmt.Errorf("web: __wasm_import is not a function")
	}
	a.importCall = o
	for name, fn := range map[string]spidermonkey.Func{
		"__wasm_validate":    a.opValidate,
		"__wasm_compile":     a.opCompile,
		"__wasm_meta":        a.opMeta,
		"__wasm_customs":     a.opCustoms,
		"__wasm_custom":      a.opCustom,
		"__wasm_instantiate": a.opInstantiate,
		"__wasm_call":        a.opCall,
		"__wasm_ret":         a.opRet,
		"__wasm_mem_new":     a.opMemNew,
		"__wasm_mem_size":    a.opMemSize,
		"__wasm_mem_grow":    a.opMemGrow,
		"__wasm_mem_read":    a.opMemRead,
		"__wasm_mem_write":   a.opMemWrite,
		"__wasm_global_new":  a.opGlobalNew,
		"__wasm_global_get":  a.opGlobalGet,
		"__wasm_global_set":  a.opGlobalSet,
	} {
		if err := js.Global().DefineFunc(name, fn); err != nil {
			return nil, err
		}
	}
	return a, nil
}

func (a *wasmAPI) close() {
	if a.rt != nil {
		_ = a.rt.Close(context.Background())
	}
}

func (a *wasmAPI) id() int64 {
	a.next++
	return a.next
}

// ------------------------------------------------------------ types

func valTypeName(t wazapi.ValueType) string {
	switch t {
	case wazapi.ValueTypeI32:
		return "i32"
	case wazapi.ValueTypeI64:
		return "i64"
	case wazapi.ValueTypeF32:
		return "f32"
	case wazapi.ValueTypeF64:
		return "f64"
	case wazapi.ValueTypeExternref:
		return "externref"
	}
	return "v128"
}

func valTypeByte(t byte) string {
	switch t {
	case 0x7f:
		return "i32"
	case 0x7e:
		return "i64"
	case 0x7d:
		return "f32"
	case 0x7c:
		return "f64"
	case 0x7b:
		return "v128"
	case 0x70:
		return "funcref"
	case 0x6f:
		return "externref"
	}
	return fmt.Sprintf("type%02x", t)
}

func valTypeFromName(s string) (byte, bool) {
	switch s {
	case "i32":
		return 0x7f, true
	case "i64":
		return 0x7e, true
	case "f32":
		return 0x7d, true
	case "f64":
		return 0x7c, true
	case "funcref":
		return 0x70, true
	case "externref":
		return 0x6f, true
	}
	return 0, false
}

// jsToBits converts a bridge value into the raw bits of a wasm value of the
// given type; bitsToJS is its inverse. i64 travels as decimal text (see the
// file comment); everything numeric else as float64, which holds every i32,
// f32 and f64 exactly.
func jsToBits(typ string, v spidermonkey.Value) (uint64, error) {
	switch typ {
	case "i32":
		return uint64(uint32(int32(int64(v.Float())))), nil
	case "i64":
		n, err := strconv.ParseInt(v.String(), 10, 64)
		if err != nil {
			// The guest wraps BigInt values to 64 bits before sending; a parse
			// failure here means the protocol was violated, not the user.
			return 0, fmt.Errorf("wasm: bad i64 %q", v.String())
		}
		return uint64(n), nil
	case "f32":
		return uint64(math.Float32bits(float32(v.Float()))), nil
	case "f64":
		return f64bits(v.Float()), nil
	}
	return 0, fmt.Errorf("wasm: cannot pass a %s from JavaScript", typ)
}

func bitsToJS(typ string, bits uint64) spidermonkey.Value {
	switch typ {
	case "i32":
		return spidermonkey.ValueOf(float64(int32(uint32(bits))))
	case "i64":
		return spidermonkey.ValueOf(strconv.FormatInt(int64(bits), 10))
	case "f32":
		return spidermonkey.ValueOf(float64(math.Float32frombits(uint32(bits))))
	case "f64":
		return spidermonkey.ValueOf(f64FromBits(bits))
	}
	return spidermonkey.Undefined()
}

// ------------------------------------------------------------ compile

func (a *wasmAPI) opValidate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	b, err := argBytes(args[0])
	if err != nil {
		return nil, err
	}
	c, cerr := a.rt.CompileModule(context.Background(), b)
	if cerr != nil {
		return spidermonkey.ValueOf(false), nil
	}
	_ = c.Close(context.Background())
	return spidermonkey.ValueOf(true), nil
}

func (a *wasmAPI) opCompile(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	b, err := argBytes(args[0])
	if err != nil {
		return nil, err
	}
	compiled, err := a.rt.CompileModule(context.Background(), b)
	if err != nil {
		return nil, fmt.Errorf("CompileError: %v", err)
	}
	meta, err := parseWasm(b)
	if err != nil {
		return nil, fmt.Errorf("CompileError: %v", err)
	}
	id := a.id()
	a.modules[id] = &wasmModule{bytes: append([]byte(nil), b...), compiled: compiled, meta: meta}
	return spidermonkey.ValueOf(float64(id)), nil
}

// opMeta reports a module's imports and exports with the type detail the JS
// API exposes: Module.imports()/exports(), and the arity every exported
// function wrapper needs.
func (a *wasmAPI) opMeta(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	m := a.modules[int64(args[0].Float())]
	if m == nil {
		return nil, fmt.Errorf("wasm: unknown module")
	}
	type funcType struct {
		Params  []string `json:"params"`
		Results []string `json:"results"`
	}
	sig := func(def wazapi.FunctionDefinition) funcType {
		var ft funcType
		for _, p := range def.ParamTypes() {
			ft.Params = append(ft.Params, valTypeName(p))
		}
		for _, r := range def.ResultTypes() {
			ft.Results = append(ft.Results, valTypeName(r))
		}
		return ft
	}
	importSigs := map[string]funcType{}
	for _, def := range m.compiled.ImportedFunctions() {
		mod, name, _ := def.Import()
		importSigs[mod+"\x00"+name] = sig(def)
	}
	exportSigs := map[string]funcType{}
	for name, def := range m.compiled.ExportedFunctions() {
		exportSigs[name] = sig(def)
	}
	kindName := map[int]string{kindFunc: "function", kindTable: "table", kindMemory: "memory", kindGlobal: "global"}
	type impOut struct {
		Module  string   `json:"module"`
		Name    string   `json:"name"`
		Kind    string   `json:"kind"`
		Type    string   `json:"type,omitempty"`
		Mut     bool     `json:"mutable,omitempty"`
		Params  []string `json:"params"`
		Results []string `json:"results"`
	}
	type expOut struct {
		Name    string   `json:"name"`
		Kind    string   `json:"kind"`
		Index   uint32   `json:"index"`
		Params  []string `json:"params"`
		Results []string `json:"results"`
	}
	out := struct {
		Imports []impOut `json:"imports"`
		Exports []expOut `json:"exports"`
	}{Imports: []impOut{}, Exports: []expOut{}}
	for _, imp := range m.meta.Imports {
		io := impOut{Module: imp.Module, Name: imp.Name, Kind: kindName[imp.Kind]}
		if imp.Kind == kindGlobal {
			io.Type, io.Mut = valTypeByte(imp.ValType), imp.Mutable
		}
		if imp.Kind == kindFunc {
			ft := importSigs[imp.Module+"\x00"+imp.Name]
			io.Params, io.Results = ft.Params, ft.Results
		}
		out.Imports = append(out.Imports, io)
	}
	for _, exp := range m.meta.Exports {
		eo := expOut{Name: exp.Name, Kind: kindName[exp.Kind], Index: exp.Index}
		if exp.Kind == kindFunc {
			ft := exportSigs[exp.Name]
			eo.Params, eo.Results = ft.Params, ft.Results
		}
		out.Exports = append(out.Exports, eo)
	}
	return spidermonkey.ValueOf(out), nil
}

func (a *wasmAPI) opCustoms(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	m := a.modules[int64(args[0].Float())]
	if m == nil {
		return nil, fmt.Errorf("wasm: unknown module")
	}
	return spidermonkey.ValueOf(float64(len(m.meta.Customs[args[1].String()]))), nil
}

func (a *wasmAPI) opCustom(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	m := a.modules[int64(args[0].Float())]
	if m == nil {
		return nil, fmt.Errorf("wasm: unknown module")
	}
	list := m.meta.Customs[args[1].String()]
	i := int(args[2].Float())
	if i < 0 || i >= len(list) {
		return nil, fmt.Errorf("wasm: no such custom section")
	}
	u8, err := a.js.NewBytes(list[i])
	if err != nil {
		return nil, err
	}
	return u8, nil
}

// ------------------------------------------------------------ entities

func (a *wasmAPI) instantiateNamed(b []byte, name string) (wazapi.Module, error) {
	compiled, err := a.rt.CompileModule(context.Background(), b)
	if err != nil {
		return nil, err
	}
	return a.rt.InstantiateModule(context.Background(), compiled, wazero.NewModuleConfig().WithName(name))
}

func (a *wasmAPI) opMemNew(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	min := uint32(args[0].Float())
	max := int64(args[1].Float())
	shared := args[2].Bool()
	id := a.id()
	mod, err := a.instantiateNamed(emitMemoryModule(min, max, shared), fmt.Sprintf("__mem%d", id))
	if err != nil {
		return nil, fmt.Errorf("RangeError: %v", err)
	}
	a.memories[id] = mod.ExportedMemory("m")
	return spidermonkey.ValueOf(float64(id)), nil
}

func (a *wasmAPI) opMemSize(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	mem := a.memories[int64(args[0].Float())]
	if mem == nil {
		return nil, fmt.Errorf("wasm: unknown memory")
	}
	return spidermonkey.ValueOf(float64(mem.Size())), nil
}

func (a *wasmAPI) opMemGrow(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	mem := a.memories[int64(args[0].Float())]
	if mem == nil {
		return nil, fmt.Errorf("wasm: unknown memory")
	}
	prev, ok := mem.Grow(uint32(args[1].Float()))
	if !ok {
		return spidermonkey.ValueOf(float64(-1)), nil
	}
	return spidermonkey.ValueOf(float64(prev)), nil
}

func (a *wasmAPI) opMemRead(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	mem := a.memories[int64(args[0].Float())]
	if mem == nil {
		return nil, fmt.Errorf("wasm: unknown memory")
	}
	b, ok := mem.Read(0, mem.Size())
	if !ok {
		return nil, fmt.Errorf("wasm: memory read out of range")
	}
	u8, err := a.js.NewBytes(b)
	if err != nil {
		return nil, err
	}
	return u8, nil
}

func (a *wasmAPI) opMemWrite(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	mem := a.memories[int64(args[0].Float())]
	if mem == nil {
		return nil, fmt.Errorf("wasm: unknown memory")
	}
	b, err := argBytes(args[1])
	if err != nil {
		return nil, err
	}
	if uint32(len(b)) > mem.Size() {
		b = b[:mem.Size()]
	}
	if !mem.Write(0, b) {
		return nil, fmt.Errorf("wasm: memory write out of range")
	}
	return spidermonkey.Undefined(), nil
}

func (a *wasmAPI) opGlobalNew(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	typ := args[0].String()
	mutable := args[1].Bool()
	vt, ok := valTypeFromName(typ)
	if !ok {
		return nil, fmt.Errorf("TypeError: %s is not a global type", typ)
	}
	var bits uint64
	if len(args) > 2 && !args[2].IsUndefined() {
		var err error
		if bits, err = jsToBits(typ, args[2]); err != nil {
			return nil, err
		}
	}
	id := a.id()
	mod, err := a.instantiateNamed(emitGlobalModule(vt, mutable, bits), fmt.Sprintf("__glb%d", id))
	if err != nil {
		return nil, fmt.Errorf("TypeError: %v", err)
	}
	a.globals[id] = mod.ExportedGlobal("g")
	return spidermonkey.ValueOf(float64(id)), nil
}

func (a *wasmAPI) opGlobalGet(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	g := a.globals[int64(args[0].Float())]
	if g == nil {
		return nil, fmt.Errorf("wasm: unknown global")
	}
	return bitsToJS(valTypeName(g.Type()), g.Get()), nil
}

func (a *wasmAPI) opGlobalSet(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	g := a.globals[int64(args[0].Float())]
	if g == nil {
		return nil, fmt.Errorf("wasm: unknown global")
	}
	mg, ok := g.(wazapi.MutableGlobal)
	if !ok {
		return nil, fmt.Errorf("TypeError: the global is immutable")
	}
	bits, err := jsToBits(valTypeName(g.Type()), args[1])
	if err != nil {
		return nil, err
	}
	mg.Set(bits)
	return spidermonkey.Undefined(), nil
}

// ------------------------------------------------------------ instantiate

// opInstantiate(moduleID, importsSpec) links and runs a module. importsSpec is
// a guest array, one entry per import in module order, each an object naming
// what the guest resolved that import to: a function token, a Memory or
// Global handle, or a plain global value.
func (a *wasmAPI) opInstantiate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	m := a.modules[int64(args[0].Float())]
	if m == nil {
		return nil, fmt.Errorf("wasm: unknown module")
	}
	specObj := args[1].Object()
	if specObj != nil {
		defer specObj.Free()
	}
	instID := a.id()

	// Function imports become one host module per imported module name,
	// trampolining into the guest. Every other import kind is redirected to the
	// uniquely-named module that backs the JS object (or, for a plain value, a
	// fresh one-global module emitted for it).
	redirects := map[int][2]string{}
	importDefs := map[string]wazapi.FunctionDefinition{}
	for _, def := range m.compiled.ImportedFunctions() {
		mod, name, _ := def.Import()
		importDefs[mod+"\x00"+name] = def
	}

	readSpec := func(i int) (*spidermonkey.Object, error) {
		if specObj == nil {
			return nil, fmt.Errorf("LinkError: import %d is unresolved", i)
		}
		v, err := specObj.Get(strconv.Itoa(i))
		if err != nil {
			return nil, err
		}
		o := v.Object()
		if o == nil {
			return nil, fmt.Errorf("LinkError: import %d is unresolved", i)
		}
		return o, nil
	}

	for i, imp := range m.meta.Imports {
		spec, err := readSpec(i)
		if err != nil {
			return nil, err
		}
		kindV, _ := spec.Get("kind")
		kind := kindV.String()
		switch kind {
		case "func":
			tokenV, _ := spec.Get("token")
			token := tokenV.Float()
			hostName := fmt.Sprintf("__imp%d_%d", instID, i)
			def := importDefs[imp.Module+"\x00"+imp.Name]
			if def == nil {
				spec.Free()
				return nil, fmt.Errorf("LinkError: import %d has no function type", i)
			}
			redirects[i] = [2]string{hostName, "f"}
			a.buildTrampoline(hostName, def, token)
		case "memory":
			idV, _ := spec.Get("id")
			redirects[i] = [2]string{fmt.Sprintf("__mem%d", int64(idV.Float())), "m"}
		case "global":
			idV, _ := spec.Get("id")
			redirects[i] = [2]string{fmt.Sprintf("__glb%d", int64(idV.Float())), "g"}
		case "global-value":
			valV, _ := spec.Get("value")
			bits, err := jsToBits(valTypeByte(imp.ValType), valV)
			if err != nil {
				spec.Free()
				return nil, err
			}
			gid := a.id()
			gname := fmt.Sprintf("__glb%d", gid)
			gmod, gerr := a.instantiateNamed(emitGlobalModule(imp.ValType, false, bits), gname)
			if gerr != nil {
				spec.Free()
				return nil, fmt.Errorf("LinkError: %v", gerr)
			}
			a.globals[gid] = gmod.ExportedGlobal("g")
			redirects[i] = [2]string{gname, "g"}
		default:
			spec.Free()
			return nil, fmt.Errorf("LinkError: import %d (%s.%s) is not linkable", i, imp.Module, imp.Name)
		}
		spec.Free()
	}

	rewritten, err := rewriteImports(m.bytes, func(module, name string, index int) (string, string) {
		if r, ok := redirects[index]; ok {
			return r[0], r[1]
		}
		return module, name
	})
	if err != nil {
		return nil, fmt.Errorf("LinkError: %v", err)
	}
	compiled, err := a.rt.CompileModule(context.Background(), rewritten)
	if err != nil {
		return nil, fmt.Errorf("CompileError: %v", err)
	}
	inst, err := a.rt.InstantiateModule(context.Background(), compiled,
		wazero.NewModuleConfig().WithName(fmt.Sprintf("__inst%d", instID)))
	if err != nil {
		// Either linking failed or the start function trapped; the guest maps the
		// prefix onto the right error class.
		if strings.Contains(err.Error(), "wasm error") {
			return nil, fmt.Errorf("RuntimeError: %v", err)
		}
		return nil, fmt.Errorf("LinkError: %v", err)
	}
	a.instances[instID] = inst

	// Register exported entities so the guest wrappers can address them.
	type expOut struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
		ID   int64  `json:"id,omitempty"`
		Type string `json:"type,omitempty"`
	}
	out := struct {
		Instance int64    `json:"instance"`
		Exports  []expOut `json:"exports"`
	}{Instance: instID, Exports: []expOut{}}
	for _, exp := range m.meta.Exports {
		eo := expOut{Name: exp.Name}
		switch exp.Kind {
		case kindFunc:
			eo.Kind = "function"
		case kindMemory:
			eo.Kind = "memory"
			id := a.id()
			a.memories[id] = inst.ExportedMemory(exp.Name)
			eo.ID = id
		case kindGlobal:
			eo.Kind = "global"
			id := a.id()
			g := inst.ExportedGlobal(exp.Name)
			a.globals[id] = g
			eo.ID = id
			if g != nil {
				eo.Type = valTypeName(g.Type())
			}
		case kindTable:
			eo.Kind = "table"
		}
		out.Exports = append(out.Exports, eo)
	}
	return spidermonkey.ValueOf(out), nil
}

// buildTrampoline registers a single-function host module: wasm calls it, it
// calls the guest with the import's token, and the guest runs the JS function
// the import object supplied. Synchronous reentry into the engine from inside
// a host op is exactly what the import contract asks for, and the bridge
// supports it (see the reentry test).
func (a *wasmAPI) buildTrampoline(hostName string, def wazapi.FunctionDefinition, token float64) {
	params := def.ParamTypes()
	results := def.ResultTypes()
	fn := func(ctx context.Context, stack []uint64) {
		callArgs := make([]spidermonkey.Value, 0, len(params)+1)
		callArgs = append(callArgs, spidermonkey.ValueOf(token))
		for i, p := range params {
			callArgs = append(callArgs, bitsToJS(valTypeName(p), stack[i]))
		}
		ret, err := a.importCall.Call(callArgs...)
		if err != nil {
			// A throwing import must trap the wasm call; wazero turns a panic in a
			// host function into exactly that.
			panic(err)
		}
		if len(results) == 1 {
			bits, err := jsToBits(valTypeName(results[0]), ret)
			if err != nil {
				panic(err)
			}
			stack[0] = bits
		}
	}
	_, _ = a.rt.NewHostModuleBuilder(hostName).
		NewFunctionBuilder().
		WithGoFunction(wazapi.GoFunc(fn), params, results).
		Export("f").
		Instantiate(context.Background())
}

// ------------------------------------------------------------ calls

func (a *wasmAPI) opCall(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	inst := a.instances[int64(args[0].Float())]
	if inst == nil {
		return nil, fmt.Errorf("wasm: unknown instance")
	}
	fn := inst.ExportedFunction(args[1].String())
	if fn == nil {
		return nil, fmt.Errorf("wasm: no export %q", args[1].String())
	}
	def := fn.Definition()
	params := def.ParamTypes()
	callArgs := make([]uint64, len(params))
	for i := range params {
		if 2+i >= len(args) {
			break // missing args were already defaulted by the guest wrapper
		}
		bits, err := jsToBits(valTypeName(params[i]), args[2+i])
		if err != nil {
			return nil, err
		}
		callArgs[i] = bits
	}
	results, err := fn.Call(context.Background(), callArgs...)
	if err != nil {
		return nil, fmt.Errorf("RuntimeError: %v", firstLineOf(err.Error()))
	}
	a.lastResults, a.lastTypes = results, def.ResultTypes()
	return spidermonkey.ValueOf(float64(len(results))), nil
}

func (a *wasmAPI) opRet(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	i := int(args[0].Float())
	if i < 0 || i >= len(a.lastResults) {
		return nil, fmt.Errorf("wasm: no result %d", i)
	}
	return bitsToJS(valTypeName(a.lastTypes[i]), a.lastResults[i]), nil
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ------------------------- the minimum of the WebAssembly binary format this
// embedding has
// to read and write itself, alongside wazero.
//
// Reading: the JS API reports things wazero's public API does not — import
// kinds and their type details (a global's mutability, a table's element
// type), custom sections by name — so the relevant sections are parsed here.
// The format is a foreign binary format with a public specification; parsing
// it is the legitimate kind of byte work.
//
// Writing: two small jobs. WebAssembly.Memory and WebAssembly.Global stand
// alone in the JS API, but in wazero every entity lives in a module — so a
// constructor emits a one-entity module and instantiates it. And instantiating
// a module with JS-provided imports needs each import redirected to the module
// that actually backs it (a host module of trampolines, a Memory's backing
// module), which is a rewrite of the import section's name strings.

// wasm section ids and import kinds, as the specification numbers them.
const (
	secCustom = 0
	secType   = 1
	secImport = 2
	secMemory = 5
	secGlobal = 6
	secExport = 7

	kindFunc   = 0
	kindTable  = 1
	kindMemory = 2
	kindGlobal = 3
)

var wasmMagic = []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

// wasmImport is one entry of the import section, with the type details the JS
// API's Module.imports() and "read the imports" need.
type wasmImport struct {
	Module, Name string
	Kind         int
	// Global imports: value type byte and mutability.
	ValType byte
	Mutable bool
	// Function imports: type index (resolved through the type section by the
	// caller when needed).
	TypeIndex uint32
}

// wasmExport is one entry of the export section.
type wasmExport struct {
	Name string
	Kind int
	// Index is the entity's index in its own index space. For a function that
	// index IS the exported function's `name` in the JS API — an exported wasm
	// function is named by its position, not by its export string.
	Index uint32
}

type wasmMeta struct {
	Imports []wasmImport
	Exports []wasmExport
	// Customs maps a custom section name to its payloads, in order; the same
	// name may appear more than once.
	Customs map[string][][]byte
}

type wasmReader struct {
	b   []byte
	off int
}

func (r *wasmReader) u8() (byte, error) {
	if r.off >= len(r.b) {
		return 0, fmt.Errorf("truncated")
	}
	v := r.b[r.off]
	r.off++
	return v, nil
}

func (r *wasmReader) uleb() (uint64, error) {
	var v uint64
	var shift uint
	for {
		b, err := r.u8()
		if err != nil {
			return 0, err
		}
		v |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return v, nil
		}
		shift += 7
		if shift > 63 {
			return 0, fmt.Errorf("leb128 too long")
		}
	}
}

func (r *wasmReader) bytes(n uint64) ([]byte, error) {
	if uint64(r.off)+n > uint64(len(r.b)) {
		return nil, fmt.Errorf("truncated")
	}
	v := r.b[r.off : r.off+int(n)]
	r.off += int(n)
	return v, nil
}

func (r *wasmReader) name() (string, error) {
	n, err := r.uleb()
	if err != nil {
		return "", err
	}
	b, err := r.bytes(n)
	return string(b), err
}

// skipLimits reads a limits structure (shared flag included).
func (r *wasmReader) skipLimits() error {
	flags, err := r.u8()
	if err != nil {
		return err
	}
	if _, err := r.uleb(); err != nil {
		return err
	}
	if flags&0x01 != 0 {
		if _, err := r.uleb(); err != nil {
			return err
		}
	}
	return nil
}

// parseWasm reads the sections the JS API needs. It assumes the module has
// already been VALIDATED by wazero — this is metadata extraction, not a
// validator, and malformed input past the checks below just yields an error.
func parseWasm(b []byte) (*wasmMeta, error) {
	if len(b) < 8 || string(b[:4]) != "\x00asm" {
		return nil, fmt.Errorf("not a wasm module")
	}
	m := &wasmMeta{Customs: map[string][][]byte{}}
	r := &wasmReader{b: b, off: 8}
	for r.off < len(b) {
		id, err := r.u8()
		if err != nil {
			return nil, err
		}
		size, err := r.uleb()
		if err != nil {
			return nil, err
		}
		body, err := r.bytes(size)
		if err != nil {
			return nil, err
		}
		s := &wasmReader{b: body}
		switch id {
		case secCustom:
			name, err := s.name()
			if err != nil {
				return nil, err
			}
			m.Customs[name] = append(m.Customs[name], body[s.off:])
		case secImport:
			n, err := s.uleb()
			if err != nil {
				return nil, err
			}
			for i := uint64(0); i < n; i++ {
				var imp wasmImport
				if imp.Module, err = s.name(); err != nil {
					return nil, err
				}
				if imp.Name, err = s.name(); err != nil {
					return nil, err
				}
				kind, err := s.u8()
				if err != nil {
					return nil, err
				}
				imp.Kind = int(kind)
				switch kind {
				case kindFunc:
					t, err := s.uleb()
					if err != nil {
						return nil, err
					}
					imp.TypeIndex = uint32(t)
				case kindTable:
					if _, err := s.u8(); err != nil { // elemtype
						return nil, err
					}
					if err := s.skipLimits(); err != nil {
						return nil, err
					}
				case kindMemory:
					if err := s.skipLimits(); err != nil {
						return nil, err
					}
				case kindGlobal:
					vt, err := s.u8()
					if err != nil {
						return nil, err
					}
					mut, err := s.u8()
					if err != nil {
						return nil, err
					}
					imp.ValType, imp.Mutable = vt, mut == 1
				default:
					return nil, fmt.Errorf("unknown import kind %d", kind)
				}
				m.Imports = append(m.Imports, imp)
			}
		case secExport:
			n, err := s.uleb()
			if err != nil {
				return nil, err
			}
			for i := uint64(0); i < n; i++ {
				var exp wasmExport
				if exp.Name, err = s.name(); err != nil {
					return nil, err
				}
				kind, err := s.u8()
				if err != nil {
					return nil, err
				}
				exp.Kind = int(kind)
				idx, err := s.uleb()
				if err != nil {
					return nil, err
				}
				exp.Index = uint32(idx)
				m.Exports = append(m.Exports, exp)
			}
		}
	}
	return m, nil
}

// ------------------------------------------------------------ emitting

func uleb(v uint64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if v == 0 {
			return out
		}
	}
}

// sleb encodes a signed LEB128, which const-expression immediates use.
func sleb(v int64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		done := (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0)
		if !done {
			b |= 0x80
		}
		out = append(out, b)
		if done {
			return out
		}
	}
}

func wasmName(s string) []byte {
	return append(uleb(uint64(len(s))), s...)
}

func section(id byte, body []byte) []byte {
	out := []byte{id}
	out = append(out, uleb(uint64(len(body)))...)
	return append(out, body...)
}

// emitMemoryModule builds a module that defines one memory and exports it as
// "m" — the backing a standalone WebAssembly.Memory needs, since wazero has no
// module-less memories. max < 0 means no maximum.
func emitMemoryModule(min uint32, max int64, shared bool) []byte {
	var limits []byte
	switch {
	case shared:
		// A shared memory requires a maximum, which the JS constructor enforced.
		limits = append([]byte{0x03}, uleb(uint64(min))...)
		limits = append(limits, uleb(uint64(max))...)
	case max >= 0:
		limits = append([]byte{0x01}, uleb(uint64(min))...)
		limits = append(limits, uleb(uint64(max))...)
	default:
		limits = append([]byte{0x00}, uleb(uint64(min))...)
	}
	memSec := append([]byte{0x01}, limits...)
	expBody := append([]byte{0x01}, wasmName("m")...)
	expBody = append(expBody, byte(kindMemory), 0x00)
	out := append([]byte(nil), wasmMagic...)
	out = append(out, section(secMemory, memSec)...)
	out = append(out, section(secExport, expBody)...)
	return out
}

// emitGlobalModule builds a module that defines one global with the given
// value and exports it as "g". The value rides in the init expression, which
// is how a wasm global is born with a value.
func emitGlobalModule(valType byte, mutable bool, bits uint64) []byte {
	var init []byte
	switch valType {
	case 0x7f: // i32
		init = append([]byte{0x41}, sleb(int64(int32(uint32(bits))))...)
	case 0x7e: // i64
		init = append([]byte{0x42}, sleb(int64(bits))...)
	case 0x7d: // f32
		init = []byte{0x43}
		init = binary.LittleEndian.AppendUint32(init, uint32(bits))
	case 0x7c: // f64
		init = []byte{0x44}
		init = binary.LittleEndian.AppendUint64(init, bits)
	case 0x6f, 0x70: // externref / funcref: born null
		init = []byte{0xd0, valType}
	}
	init = append(init, 0x0b) // end
	mut := byte(0)
	if mutable {
		mut = 1
	}
	glbBody := append([]byte{0x01, valType, mut}, init...)
	expBody := append([]byte{0x01}, wasmName("g")...)
	expBody = append(expBody, byte(kindGlobal), 0x00)
	out := append([]byte(nil), wasmMagic...)
	out = append(out, section(secGlobal, glbBody)...)
	out = append(out, section(secExport, expBody)...)
	return out
}

// rewriteImports returns the module bytes with each import's (module, name)
// replaced according to redirect, leaving everything else — including the
// import's type — byte-for-byte intact. This is how a JS import object meets
// wazero's name-based linking: each JS-provided entity lives in a module of
// its own with a unique name, and the importer is rewritten to point there.
func rewriteImports(b []byte, redirect func(module, name string, index int) (string, string)) ([]byte, error) {
	if len(b) < 8 {
		return nil, fmt.Errorf("not a wasm module")
	}
	r := &wasmReader{b: b, off: 8}
	for r.off < len(b) {
		secStart := r.off
		id, err := r.u8()
		if err != nil {
			return nil, err
		}
		size, err := r.uleb()
		if err != nil {
			return nil, err
		}
		bodyStart := r.off
		body, err := r.bytes(size)
		if err != nil {
			return nil, err
		}
		if id != secImport {
			continue
		}
		s := &wasmReader{b: body}
		n, err := s.uleb()
		if err != nil {
			return nil, err
		}
		newBody := append([]byte(nil), uleb(n)...)
		for i := uint64(0); i < n; i++ {
			module, err := s.name()
			if err != nil {
				return nil, err
			}
			name, err := s.name()
			if err != nil {
				return nil, err
			}
			descStart := s.off
			kind, err := s.u8()
			if err != nil {
				return nil, err
			}
			switch kind {
			case kindFunc:
				if _, err := s.uleb(); err != nil {
					return nil, err
				}
			case kindTable:
				if _, err := s.u8(); err != nil {
					return nil, err
				}
				if err := s.skipLimits(); err != nil {
					return nil, err
				}
			case kindMemory:
				if err := s.skipLimits(); err != nil {
					return nil, err
				}
			case kindGlobal:
				if _, err := s.u8(); err != nil {
					return nil, err
				}
				if _, err := s.u8(); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("unknown import kind %d", kind)
			}
			newModule, newName := redirect(module, name, int(i))
			newBody = append(newBody, wasmName(newModule)...)
			newBody = append(newBody, wasmName(newName)...)
			newBody = append(newBody, body[descStart:s.off]...)
		}
		out := append([]byte(nil), b[:secStart]...)
		out = append(out, section(secImport, newBody)...)
		out = append(out, b[bodyStart+int(size):]...)
		return out, nil
	}
	// No import section: nothing to rewrite.
	return b, nil
}

// f32bits/f64bits are spelled out so the call sites read as conversions, not
// as bit tricks.
func f64bits(f float64) uint64     { return math.Float64bits(f) }
func f64FromBits(u uint64) float64 { return math.Float64frombits(u) }
