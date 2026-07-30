package web

// White-box on purpose: the tests assemble real wasm binaries with the same
// section/LEB helpers wasmbin.go emits its synthetic modules with, which keeps
// the encodings honest from both directions — a bug in the helpers breaks the
// fixtures too, visibly.

import (
	"context"
	"fmt"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// buildModule assembles a module from raw section bodies.
func buildModule(sections ...[]byte) []byte {
	out := append([]byte(nil), wasmMagic...)
	for _, s := range sections {
		out = append(out, s...)
	}
	return out
}

// A (func (export "add") (param i32 i32) (result i32) i32.add) module.
func addModule() []byte {
	return buildModule(
		section(secType, []byte{0x01, 0x60, 0x02, 0x7f, 0x7f, 0x01, 0x7f}),
		section(3, []byte{0x01, 0x00}), // function section
		section(secExport, append(append([]byte{0x01}, wasmName("add")...), 0x00, 0x00)),
		section(10, []byte{0x01, 0x07, 0x00, 0x20, 0x00, 0x20, 0x01, 0x6a, 0x0b}),
	)
}

// A module that imports env.inc and exposes run(x) = inc(x).
func importModule() []byte {
	imp := []byte{0x01}
	imp = append(imp, wasmName("env")...)
	imp = append(imp, wasmName("inc")...)
	imp = append(imp, 0x00, 0x00)
	return buildModule(
		section(secType, []byte{0x01, 0x60, 0x01, 0x7f, 0x01, 0x7f}),
		section(secImport, imp),
		section(3, []byte{0x01, 0x00}),
		section(secExport, append(append([]byte{0x01}, wasmName("run")...), 0x00, 0x01)),
		section(10, []byte{0x01, 0x06, 0x00, 0x20, 0x00, 0x10, 0x00, 0x0b}),
	)
}

// A module exporting a memory, poke(addr, val), and growit() = memory.grow(1).
func memoryModule() []byte {
	exports := []byte{0x03}
	exports = append(exports, wasmName("mem")...)
	exports = append(exports, byte(kindMemory), 0x00)
	exports = append(exports, wasmName("poke")...)
	exports = append(exports, byte(kindFunc), 0x00)
	exports = append(exports, wasmName("growit")...)
	exports = append(exports, byte(kindFunc), 0x01)
	return buildModule(
		section(secType, []byte{0x02,
			0x60, 0x02, 0x7f, 0x7f, 0x00, // (i32, i32) -> ()
			0x60, 0x00, 0x01, 0x7f, // () -> i32
		}),
		section(3, []byte{0x02, 0x00, 0x01}),
		section(secMemory, []byte{0x01, 0x00, 0x01}),
		section(secExport, exports),
		section(10, append(
			[]byte{0x02},
			append(
				[]byte{0x09, 0x00, 0x20, 0x00, 0x20, 0x01, 0x36, 0x02, 0x00, 0x0b},
				0x06, 0x00, 0x41, 0x01, 0x40, 0x00, 0x0b,
			)...,
		)),
	)
}

// A module importing a memory and exposing peek(addr) = i32.load.
func memImportModule() []byte {
	imp := []byte{0x01}
	imp = append(imp, wasmName("js")...)
	imp = append(imp, wasmName("mem")...)
	imp = append(imp, byte(kindMemory), 0x00, 0x01)
	return buildModule(
		section(secType, []byte{0x01, 0x60, 0x01, 0x7f, 0x01, 0x7f}),
		section(secImport, imp),
		section(3, []byte{0x01, 0x00}),
		section(secExport, append(append([]byte{0x01}, wasmName("peek")...), 0x00, 0x00)),
		section(10, []byte{0x01, 0x07, 0x00, 0x20, 0x00, 0x28, 0x02, 0x00, 0x0b}),
	)
}

// id64 and a trapping function, plus a custom section.
func miscModule() []byte {
	custom := wasmName("meta")
	custom = append(custom, 0xAA, 0xBB)
	exports := []byte{0x02}
	exports = append(exports, wasmName("id64")...)
	exports = append(exports, byte(kindFunc), 0x00)
	exports = append(exports, wasmName("boom")...)
	exports = append(exports, byte(kindFunc), 0x01)
	return buildModule(
		section(secType, []byte{0x02,
			0x60, 0x01, 0x7e, 0x01, 0x7e, // (i64) -> i64
			0x60, 0x00, 0x00, // () -> ()
		}),
		section(3, []byte{0x02, 0x00, 0x01}),
		section(secExport, exports),
		section(10, []byte{0x02,
			0x04, 0x00, 0x20, 0x00, 0x0b,
			0x03, 0x00, 0x00, 0x0b, // unreachable
		}),
		section(secCustom, custom),
	)
}

func runWasm(t *testing.T, module []byte, script string) string {
	t.Helper()
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	w, err := Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer w.Close()

	hex := ""
	for _, b := range module {
		hex += fmt.Sprintf("%02x", b)
	}
	full := fmt.Sprintf(`globalThis.__r = "?";
		const BYTES = new Uint8Array(%q.match(/../g).map((h) => parseInt(h, 16)));
		(async () => { %s })().then(
			(v) => { if (globalThis.__r === "?") globalThis.__r = String(v); },
			(e) => { globalThis.__r = "REJECTED " + (e && e.name) + ": " + (e && e.message); });`,
		hex, script)
	if r, err := js.Eval(context.Background(), full); err != nil {
		t.Fatalf("eval: %v", err)
	} else if r.Error != nil {
		t.Fatalf("threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	r, err := js.Eval(context.Background(), `String(globalThis.__r)`)
	if err != nil {
		t.Fatal(err)
	}
	return r.Value.String()
}

func TestWasmBasics(t *testing.T) {
	for _, tc := range []struct {
		name   string
		module func() []byte
		script string
		want   string
	}{
		{
			"validate and compile", addModule,
			`const ok = WebAssembly.validate(BYTES);
			 const bad = WebAssembly.validate(new Uint8Array([1, 2, 3]));
			 const mod = new WebAssembly.Module(BYTES);
			 return ok + "," + bad + "," + (mod instanceof WebAssembly.Module);`,
			"true,false,true",
		},
		{
			"exported function runs", addModule,
			`const { instance } = await WebAssembly.instantiate(BYTES);
			 return instance.exports.add(20, 22) + "," + instance.exports.add.length;`,
			"42,2",
		},
		{
			"module metadata", importModule,
			`const mod = new WebAssembly.Module(BYTES);
			 const imp = WebAssembly.Module.imports(mod)[0];
			 const exp = WebAssembly.Module.exports(mod)[0];
			 return [imp.module, imp.name, imp.kind, exp.name, exp.kind].join(",");`,
			"env,inc,function,run,function",
		},
		{
			"a JS import is called from wasm", importModule,
			`let seen = null;
			 const { instance } = await WebAssembly.instantiate(BYTES, {
				env: { inc: (x) => { seen = x; return x + 1; } },
			 });
			 return instance.exports.run(41) + ",saw:" + seen;`,
			"42,saw:41",
		},
		{
			// An exception thrown by an import propagates through the wasm frames
			// AS ITSELF. Turning it into a RuntimeError — which is what a genuine
			// trap looks like — would report the wrong failure to the caller and
			// lose the value they threw.
			"a throwing import propagates its own exception", importModule,
			`const marker = new RangeError("nope");
			 const { instance } = await WebAssembly.instantiate(BYTES, {
				env: { inc: () => { throw marker; } },
			 });
			 try { instance.exports.run(1); return "no-throw"; }
			 catch (e) { return (e === marker) + ":" + e.name; }`,
			"true:RangeError",
		},
		{
			"exported memory is visible after a store", memoryModule,
			`const { instance } = await WebAssembly.instantiate(BYTES);
			 const mem = instance.exports.mem;
			 instance.exports.poke(8, 0x11223344);
			 const view = new DataView(mem.buffer);
			 return "0x" + view.getUint32(8, true).toString(16);`,
			"0x11223344",
		},
		{
			"growing from inside wasm detaches the buffer", memoryModule,
			`const { instance } = await WebAssembly.instantiate(BYTES);
			 const mem = instance.exports.mem;
			 const before = mem.buffer;
			 const prev = instance.exports.growit();
			 return prev + "," + before.detached + "," + (mem.buffer.byteLength / 65536);`,
			"1,true,2",
		},
		{
			"an imported Memory is shared with the instance", memImportModule,
			`const mem = new WebAssembly.Memory({ initial: 1 });
			 const { instance } = await WebAssembly.instantiate(BYTES, { js: { mem } });
			 new DataView(mem.buffer).setUint32(4, 7777, true);
			 return instance.exports.peek(4);`,
			"7777",
		},
		{
			"i64 speaks BigInt, and a Number is refused", miscModule,
			`const { instance } = await WebAssembly.instantiate(BYTES);
			 const big = instance.exports.id64(-(2n ** 62n));
			 let refused = "no";
			 try { instance.exports.id64(1); } catch (e) { refused = e.name; }
			 return typeof big + "," + big + "," + refused;`,
			"bigint,-4611686018427387904,TypeError",
		},
		{
			"a trap is a RuntimeError", miscModule,
			`const { instance } = await WebAssembly.instantiate(BYTES);
			 try { instance.exports.boom(); return "no-throw"; }
			 catch (e) { return (e instanceof WebAssembly.RuntimeError) + ":" + e.name; }`,
			"true:RuntimeError",
		},
		{
			"custom sections come back by name", miscModule,
			`const mod = new WebAssembly.Module(BYTES);
			 const secs = WebAssembly.Module.customSections(mod, "meta");
			 const none = WebAssembly.Module.customSections(mod, "absent");
			 const v = new Uint8Array(secs[0]);
			 return secs.length + "," + none.length + ",0x" + v[0].toString(16) + v[1].toString(16);`,
			"1,0,0xaabb",
		},
		{
			"Global round-trips values and mutability", addModule,
			`const g = new WebAssembly.Global({ value: "i32", mutable: true }, 7);
			 const c = new WebAssembly.Global({ value: "i64" }, 9n);
			 g.value = 8;
			 let frozen = "no";
			 try { c.value = 1n; } catch (e) { frozen = e.name; }
			 return g.value + "," + c.value + "," + frozen + "," + (g.valueOf() === 8);`,
			"8,9,TypeError,true",
		},
		{
			"compile errors have their own class", addModule,
			`try { new WebAssembly.Module(new Uint8Array([0, 0, 0, 0])); return "no-throw"; }
			 catch (e) { return (e instanceof WebAssembly.CompileError) + ":" + e.name; }`,
			"true:CompileError",
		},
		{
			"a missing import is a LinkError-family failure", importModule,
			`try { await WebAssembly.instantiate(BYTES, {}); return "no-throw"; }
			 catch (e) { return e.name; }`,
			"TypeError",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runWasm(t, tc.module(), tc.script); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}
