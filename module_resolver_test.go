package spidermonkey_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/memfs"
)

// denyFS wraps an fs.FS and denies the named paths — the "policy lives in the
// FS" model (a stand-in for a sheena Volume's Refuse rule). Access control is
// enforced by the FS itself; there is no separate Config hook.
type denyFS struct {
	fs.FS
	deny map[string]bool
}

func (d denyFS) Open(name string) (fs.File, error) {
	if d.deny[name] {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	return d.FS.Open(name)
}

func TestModuleLoaderFSDenied(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{
		FS: denyFS{
			FS:   fstest.MapFS{"dep.js": {Data: []byte(`export const answer = 42;`)}},
			deny: map[string]bool{"dep.js": true},
		},
	})
	defer js.Close()

	r, err := js.EvalModule(context.Background(), "main",
		`import { answer } from "dep.js";`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error == nil {
		t.Fatal("import succeeded although the FS denied the read")
	}
	if !strings.Contains(r.Error.Error(), "permission") {
		t.Errorf("error = %q, want a permission error", r.Error)
	}
}

func TestModuleLoaderFSAllowed(t *testing.T) {
	// The same FS with nothing denied loads normally.
	js, _ := spidermonkey.New(spidermonkey.Config{
		FS: denyFS{
			FS:   fstest.MapFS{"dep.js": {Data: []byte(`export const answer = 42;`)}},
			deny: map[string]bool{},
		},
	})
	defer js.Close()

	r, err := js.EvalModule(context.Background(), "main",
		`import { answer } from "dep.js"; globalThis.result = answer;`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("EvalModule failed: %v", r.Error)
	}
	got, _ := js.Eval(context.Background(), `globalThis.result`)
	if got.Value.String() != "42" {
		t.Errorf("result = %q, want \"42\"", got.Value)
	}
}

func TestRegisterModuleResolverPrefix(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{
		FS: fstest.MapFS{
			"dep.js": {Data: []byte(`export const from = "fs";`)},
		},
	})
	defer js.Close()

	js.RegisterModuleResolver("node:", func(cfg spidermonkey.Config, specifier, referrer string) (string, error) {
		if specifier != "node:path" {
			t.Errorf("specifier = %q, want \"node:path\"", specifier)
		}
		return `export const from = "node";`, nil
	})

	r, err := js.EvalModule(context.Background(), "main", `
		import { from as a } from "node:path";
		import { from as b } from "dep.js";
		globalThis.result = a + "," + b;
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("EvalModule failed: %v", r.Error)
	}
	got, _ := js.Eval(context.Background(), `globalThis.result`)
	if got.Value.String() != "node,fs" {
		t.Errorf("result = %q, want \"node,fs\"", got.Value)
	}
}

func TestRegisterModuleResolverLongestPrefixWins(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	js.RegisterModuleResolver("x:", func(cfg spidermonkey.Config, specifier, referrer string) (string, error) {
		return `export const from = "short";`, nil
	})
	js.RegisterModuleResolver("x:special/", func(cfg spidermonkey.Config, specifier, referrer string) (string, error) {
		return `export const from = "long";`, nil
	})

	r, err := js.EvalModule(context.Background(), "main", `
		import { from as a } from "x:plain";
		import { from as b } from "x:special/thing";
		globalThis.result = a + "," + b;
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("EvalModule failed: %v", r.Error)
	}
	got, _ := js.Eval(context.Background(), `globalThis.result`)
	if got.Value.String() != "short,long" {
		t.Errorf("result = %q, want \"short,long\"", got.Value)
	}
}

func TestRegisterModuleResolverRemove(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{
		FS: fstest.MapFS{
			"pkg:dep": {Data: []byte(`export const from = "fallback";`)},
		},
	})
	defer js.Close()

	js.RegisterModuleResolver("pkg:", func(cfg spidermonkey.Config, specifier, referrer string) (string, error) {
		return `export const from = "resolver";`, nil
	})
	js.RegisterModuleResolver("pkg:", nil) // removed: the FS fallback should serve pkg:dep

	r, err := js.EvalModule(context.Background(), "main",
		`import { from } from "pkg:dep"; globalThis.result = from;`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("EvalModule failed: %v", r.Error)
	}
	got, _ := js.Eval(context.Background(), `globalThis.result`)
	if got.Value.String() != "fallback" {
		t.Errorf("result = %q, want \"fallback\"", got.Value)
	}
}

func TestEvalModuleFromMemFS(t *testing.T) {
	fsys := memfs.New()
	if err := fsys.WriteFile("dep.js", []byte(`export const answer = 21;`), 0o644); err != nil {
		t.Fatal(err)
	}
	js, _ := spidermonkey.New(spidermonkey.Config{FS: fsys})
	defer js.Close()

	r, err := js.EvalModule(context.Background(), "main",
		`import { answer } from "dep.js"; globalThis.result = answer * 2;`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("EvalModule failed: %v", r.Error)
	}
	got, _ := js.Eval(context.Background(), `globalThis.result`)
	if got.Value.String() != "42" {
		t.Errorf("result = %q, want \"42\"", got.Value)
	}
}
