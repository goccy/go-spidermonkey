package nodejs_test

// The compat/nodejs Phase 3 flagships (docs/nodejs-compat-plan.md): real,
// unmodified npm packages on the Node runtime — lodash (CJS), chalk (pure
// ESM with #-imports), commander (dual CJS/ESM via an interop wrapper).
// Opt-in like the test262 suite: skipped unless examples/nodejs/node_modules
// exists (run `npm ci` in examples/nodejs).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
)

func newFlagshipRuntime(t *testing.T) (*spidermonkey.JS, *nodejs.Runtime) {
	t.Helper()
	dir := filepath.Join("..", "..", "examples", "nodejs")
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "lodash", "package.json")); err != nil {
		t.Skip("examples/nodejs/node_modules not installed; run `npm ci` in examples/nodejs")
	}
	return newRuntime(t, spidermonkey.Config{FS: os.DirFS(dir)})
}

func TestLodashFlagshipRequire(t *testing.T) {
	js, rt := newFlagshipRuntime(t)

	runScript(t, rt, `
		const _ = require("lodash");
		globalThis.r = {};
		r.version = typeof _.VERSION;
		r.chunk = JSON.stringify(_.chunk([1, 2, 3, 4, 5], 2));
		r.get = _.get({ a: { b: [{ c: 42 }] } }, "a.b[0].c");
		r.sorted = _.sortBy([{ n: 3 }, { n: 1 }, { n: 2 }], "n").map(o => o.n).join(",");
		r.debounced = typeof _.debounce(() => {}, 10);
		r.template = _.template("hi <%= name %>")({ name: "goccy" });
		r.cloned = (() => {
			const orig = { deep: { arr: [1, { x: 2 }] } };
			const c = _.cloneDeep(orig);
			c.deep.arr[1].x = 99;
			return orig.deep.arr[1].x;
		})();
	`)
	for expr, want := range map[string]string{
		"r.version":   "string",
		"r.chunk":     "[[1,2],[3,4],[5]]",
		"r.get":       "42",
		"r.sorted":    "1,2,3",
		"r.debounced": "function",
		"r.template":  "hi goccy",
		"r.cloned":    "2",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestLodashFlagshipESMImport(t *testing.T) {
	js, rt := newFlagshipRuntime(t)

	r, err := rt.RunModule(context.Background(), "main.mjs", `
		import _ from "lodash";
		globalThis.result = _.camelCase("hello world example") + "|" + _.uniq([1, 1, 2, 3, 3]).join("");
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("module threw: %v", r.Error)
	}
	if got := evalStr(t, js, `result`); got != "helloWorldExample|123" {
		t.Errorf("result = %s", got)
	}
}

func TestChalkFlagship(t *testing.T) {
	js, rt := newFlagshipRuntime(t)

	r, err := rt.RunModule(context.Background(), "main.mjs", `
		import chalk, { Chalk } from "chalk";
		globalThis.r = {};
		// No TTY: color level 0, styling is an identity transform.
		r.plain = chalk.red("danger");
		r.level = chalk.level;
		// Forcing a level makes real ANSI codes appear.
		const forced = new Chalk({ level: 2 });
		r.colored = forced.red("danger");
		r.hasAnsi = r.colored.includes("[31m") && r.colored.includes("[39m");
		r.compose = forced.bold.underline("x").length > 1;
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("module threw: %v", r.Error)
	}
	if got := evalStr(t, js, `r.plain`); got != "danger" {
		t.Errorf("uncolored output = %q", got)
	}
	if got := evalStr(t, js, `String(r.level)`); got != "0" {
		t.Errorf("chalk.level = %s, want 0 (no TTY)", got)
	}
	if got := evalStr(t, js, `String(r.hasAnsi)`); got != "true" {
		t.Errorf("forced-level output lacks ANSI codes: %q", evalStr(t, js, `JSON.stringify(r.colored)`))
	}
	if got := evalStr(t, js, `String(r.compose)`); got != "true" {
		t.Errorf("style composition failed")
	}
}

func TestCommanderFlagshipESM(t *testing.T) {
	js, rt := newFlagshipRuntime(t)

	r, err := rt.RunModule(context.Background(), "main.mjs", `
		import { Command } from "commander";
		globalThis.r = {};
		const program = new Command();
		program
			.name("mycli")
			.version("1.2.3")
			.option("-p, --port <number>", "port to listen on", "8080")
			.option("--verbose", "verbose output")
			.argument("<file>", "input file");
		program.parse(["node", "mycli", "--port", "3000", "--verbose", "input.txt"]);
		r.port = program.opts().port;
		r.verbose = program.opts().verbose;
		r.file = program.args[0];
		r.name = program.name();
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("module threw: %v", r.Error)
	}
	for expr, want := range map[string]string{
		"r.port":            "3000",
		"String(r.verbose)": "true",
		"r.file":            "input.txt",
		"r.name":            "mycli",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestCommanderFlagshipRequire(t *testing.T) {
	js, rt := newFlagshipRuntime(t)

	runScript(t, rt, `
		const { Command } = require("commander");
		const program = new Command();
		program.option("-n, --count <n>", "count", "1");
		program.parse(["node", "app", "--count", "5"]);
		globalThis.count = program.opts().count;
	`)
	if got := evalStr(t, js, `count`); got != "5" {
		t.Errorf("count = %q, want \"5\"", got)
	}
	_ = strings.TrimSpace("")
}
