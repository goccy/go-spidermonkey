package nodejs_test

import (
	"context"
	"testing"
	"testing/fstest"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// A JSON module imported with the type attribute — `import data from "./x.json"
// with { type: "json" }`, the only form Node accepts — must yield the parsed
// data. The engine implements the attribute itself and JSON-parses whatever the
// module loader returns, so the loader has to answer with the raw bytes; it used
// to answer with `export default JSON.parse("…")`, which the engine then tried
// to parse AS JSON, and every such import failed. Modern packages hit this
// immediately: @babel/core's graph imports .json data modules, so nothing that
// depends on it could load.
func TestJSONModuleWithTypeAttribute(t *testing.T) {
	fsys := fstest.MapFS{
		"data.json":  {Data: []byte(`{"name": "babel", "list": [1, 2, 3]}`)},
		"array.json": {Data: []byte(`["a", "b"]`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})

	r, err := rt.RunModule(context.Background(), "main.mjs", `
		import data from "./data.json" with { type: "json" };
		const arr = (await import("./array.json", { with: { type: "json" } })).default;
		globalThis.result = [data.name, data.list.length, arr.join("")].join("|");
	`)
	if err != nil {
		t.Fatalf("RunModule: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("module threw: %v", r.Error)
	}
	if got := evalStr(t, js, "result"); got != "babel|3|ab" {
		t.Errorf("result = %q, want \"babel|3|ab\"", got)
	}
}

// The inert-data guarantee must also hold for the attributed form: a .json file
// whose bytes are an executable JavaScript expression must not run, whichever
// way the import spells it.
func TestJSONModuleAttributedImportStaysInert(t *testing.T) {
	fsys := fstest.MapFS{
		"evil.json": {Data: []byte(`(globalThis.__PWNED = 1, {"x": 2})`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})

	if _, err := rt.RunModule(context.Background(), "main.mjs", `
		globalThis.r = {};
		try {
			await import("./evil.json", { with: { type: "json" } });
			r.imported = true;
		} catch (e) {
			r.threw = true;
		}
		r.pwned = globalThis.__PWNED ?? "unset";
	`); err != nil {
		t.Fatalf("RunModule: %v", err)
	}
	if got := evalStr(t, js, "String(r.pwned)"); got != "unset" {
		t.Fatalf("attributed .json import executed its payload: __PWNED = %q", got)
	}
	if evalVal(t, js, "!!r.imported").Bool() {
		t.Error("importing non-JSON .json succeeded; expected a parse error")
	}
}
