package nodejs_test

import (
	"context"
	"io/fs"
	"testing"
	"testing/fstest"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
)

func newJS(t *testing.T, fsys fstest.MapFS) *spidermonkey.JS {
	t.Helper()
	js, err := spidermonkey.New(spidermonkey.Config{FS: fsys})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { js.Close() })
	js.SetModuleLoader(nodejs.ESMLoader)
	return js
}

func result(t *testing.T, js *spidermonkey.JS, src string) string {
	t.Helper()
	r, err := js.EvalModule(context.Background(), "main.js", src)
	if err != nil {
		t.Fatalf("EvalModule: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("module threw: %v", r.Error)
	}
	v, err := js.Eval(context.Background(), `globalThis.result`)
	if err != nil {
		t.Fatal(err)
	}
	return v.Value.String()
}

func TestBareSpecifierViaExports(t *testing.T) {
	js := newJS(t, fstest.MapFS{
		"node_modules/mylib/package.json": {Data: []byte(`{
			"name": "mylib",
			"exports": { ".": { "worker": "./dist/worker.js", "import": "./dist/node.js" } }
		}`)},
		// The worker condition must win over import.
		"node_modules/mylib/dist/worker.js": {Data: []byte(`
			import { helper } from "./helper.js";
			export const where = "worker:" + helper;
		`)},
		"node_modules/mylib/dist/helper.js": {Data: []byte(`export const helper = "rel";`)},
		"node_modules/mylib/dist/node.js":   {Data: []byte(`export const where = "node";`)},
	})
	if got := result(t, js, `
		import { where } from "mylib";
		globalThis.result = where;
	`); got != "worker:rel" {
		t.Errorf("resolved %q, want \"worker:rel\" (worker condition + relative import)", got)
	}
}

func TestBareSpecifierDefaultExport(t *testing.T) {
	js := newJS(t, fstest.MapFS{
		"node_modules/dft/package.json": {Data: []byte(`{"exports": "./index.js"}`)},
		"node_modules/dft/index.js":     {Data: []byte(`export default { tag: "the-default" };`)},
	})
	if got := result(t, js, `
		import d from "dft";
		globalThis.result = d.tag;
	`); got != "the-default" {
		t.Errorf("default export = %q", got)
	}
}

func TestBareSpecifierSubpath(t *testing.T) {
	js := newJS(t, fstest.MapFS{
		"node_modules/pkg/package.json": {Data: []byte(`{
			"exports": { ".": "./index.js", "./extra": "./lib/extra.js" }
		}`)},
		"node_modules/pkg/index.js":     {Data: []byte(`export const which = "root";`)},
		"node_modules/pkg/lib/extra.js": {Data: []byte(`export const which = "extra";`)},
	})
	if got := result(t, js, `
		import { which as a } from "pkg";
		import { which as b } from "pkg/extra";
		globalThis.result = a + "+" + b;
	`); got != "root+extra" {
		t.Errorf("subpath resolution = %q", got)
	}
}

func TestMainFallbackWithoutExports(t *testing.T) {
	js := newJS(t, fstest.MapFS{
		"node_modules/old/package.json": {Data: []byte(`{"main": "./lib/entry.js"}`)},
		"node_modules/old/lib/entry.js": {Data: []byte(`export const ok = "main-field";`)},
	})
	if got := result(t, js, `
		import { ok } from "old";
		globalThis.result = ok;
	`); got != "main-field" {
		t.Errorf("main fallback = %q", got)
	}
}

func TestRelativeAndExtensionFallback(t *testing.T) {
	js := newJS(t, fstest.MapFS{
		"src/util.js":      {Data: []byte(`export const u = 1;`)},
		"src/dir/index.js": {Data: []byte(`export const d = 2;`)},
	})
	if got := result(t, js, `
		import { u } from "./src/util";
		import { d } from "./src/dir";
		globalThis.result = u + d;
	`); got != "3" {
		t.Errorf("fallback resolution = %q", got)
	}
}

// denyAllFS denies every read — a policy FS stand-in (like a sheena Volume
// that refuses everything). Access control is in the FS, not a Config hook.
type denyAllFS struct{ fs.FS }

func (d denyAllFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
}

func TestFSDeniesModuleReads(t *testing.T) {
	fsys := fstest.MapFS{
		"node_modules/sec/package.json": {Data: []byte(`{"exports": "./index.js"}`)},
		"node_modules/sec/index.js":     {Data: []byte(`export const x = 1;`)},
	}
	js, err := spidermonkey.New(spidermonkey.Config{FS: denyAllFS{fsys}})
	if err != nil {
		t.Fatal(err)
	}
	defer js.Close()
	js.SetModuleLoader(nodejs.ESMLoader)
	r, err := js.EvalModule(context.Background(), "main.js", `import { x } from "sec";`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error == nil {
		t.Fatal("import succeeded although the FS denied the read")
	}
}
