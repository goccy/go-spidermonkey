package spidermonkey_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/memfs"
)

func TestModuleLoaderFSAccessDenied(t *testing.T) {
	var asked []string
	js, _ := spidermonkey.New(spidermonkey.Config{
		FS: fstest.MapFS{
			"dep.js": {Data: []byte(`export const answer = 42;`)},
		},
		FSAccess: func(path string, write bool) bool {
			asked = append(asked, path)
			if write {
				t.Errorf("module load asked for write access to %q", path)
			}
			return false
		},
	})
	defer js.Close()

	r, err := js.EvalModule(context.Background(), "main",
		`import { answer } from "dep.js";`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error == nil {
		t.Fatal("import succeeded although FSAccess denied it")
	}
	if !strings.Contains(r.Error.Error(), "permission") {
		t.Errorf("error = %q, want a permission error", r.Error)
	}
	if len(asked) == 0 || asked[0] != "dep.js" {
		t.Errorf("FSAccess consulted with %v, want [dep.js ...]", asked)
	}
}

func TestModuleLoaderFSAccessAllowed(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{
		FS: fstest.MapFS{
			"dep.js": {Data: []byte(`export const answer = 42;`)},
		},
		FSAccess: func(path string, write bool) bool { return path == "dep.js" && !write },
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
