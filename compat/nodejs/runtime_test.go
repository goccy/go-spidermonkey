package nodejs_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"testing/fstest"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
	"github.com/goccy/go-spidermonkey/memfs"
)

func newRuntime(t *testing.T, cfg spidermonkey.Config, opts ...nodejs.Options) (*spidermonkey.JS, *nodejs.Runtime) {
	t.Helper()
	// The network/listen hooks are fail-closed (nil denies). Functionality
	// tests exercise real sockets, so default the unset hooks to allow-all;
	// a test that wants to verify denial sets its own returning-false hook,
	// which (being non-nil) overrides these defaults.
	if cfg.Dial == nil {
		cfg.Dial = func(network, host, ip string, port int) bool { return true }
	}
	if cfg.Resolve == nil {
		cfg.Resolve = func(host string) bool { return true }
	}
	if cfg.Listen == nil {
		cfg.Listen = func(network, addr string) bool { return true }
	}
	js, err := spidermonkey.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { js.Close() })
	rt, err := nodejs.Install(js, opts...)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Cleanup(func() { rt.Close() })
	return js, rt
}

func evalStr(t *testing.T, js *spidermonkey.JS, src string) string {
	t.Helper()
	return evalVal(t, js, src).String()
}

func evalVal(t *testing.T, js *spidermonkey.JS, src string) spidermonkey.Value {
	t.Helper()
	r, err := js.Eval(context.Background(), src)
	if err != nil {
		t.Fatalf("Eval(%s): %v", src, err)
	}
	if r.Error != nil {
		t.Fatalf("Eval(%s) threw: %v", src, r.Error)
	}
	return r.Value
}

func runScript(t *testing.T, rt *nodejs.Runtime, src string) {
	t.Helper()
	r, err := rt.RunScript(context.Background(), src)
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
}

func TestMicrotaskOrdering(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		globalThis.order = [];
		setTimeout(() => order.push("timeout"), 0);
		setImmediate(() => order.push("immediate"));
		process.nextTick(() => order.push("tick"));
		Promise.resolve().then(() => order.push("promise"));
		order.push("sync");
	`)
	// Node: sync first; nextTick before promise microtasks; the 0ms timer
	// (timers phase) before setImmediate (check phase).
	if got := evalStr(t, js, `order.join(",")`); got != "sync,tick,promise,timeout,immediate" {
		t.Errorf("order = %s, want sync,tick,promise,timeout,immediate", got)
	}
}

func TestNextTickInterleaving(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		globalThis.order = [];
		process.nextTick(() => order.push("t1"));
		Promise.resolve().then(() => { order.push("p1"); process.nextTick(() => order.push("t2")); });
		Promise.resolve().then(() => order.push("p2"));
	`)
	// Documented deviation from Node's per-job interleave: a tick queued BY
	// a promise runs after the current promise batch, still before any
	// macrotask. Node itself would give t1,p1,t2,p2.
	if got := evalStr(t, js, `order.join(",")`); got != "t1,p1,p2,t2" {
		t.Errorf("order = %s, want t1,p1,p2,t2", got)
	}
}

func TestSetImmediateVsTimeoutAcrossTurns(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		globalThis.order = [];
		setImmediate(() => {
			order.push("i1");
			setImmediate(() => order.push("i3")); // next check-phase turn
		});
		setImmediate(() => order.push("i2"));
		setTimeout(() => order.push("t1"), 30);
	`)
	// i3 lands the turn after i1/i2 (setImmediate semantics); loop turns do
	// not wait for the pending timer, so i3 still precedes t1 (as in Node).
	if got := evalStr(t, js, `order.join(",")`); got != "i1,i2,i3,t1" {
		t.Errorf("order = %s, want i1,i2,i3,t1", got)
	}
}

func TestCoreModulesViaRequire(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const path = require("path");
		const { EventEmitter } = require("events");
		const util = require("node:util");
		const qs = require("querystring");
		const assert = require("assert");

		globalThis.r = {};
		r.join = path.join("a", "b", "..", "c/");
		r.resolve = path.resolve("/x/y", "../z");
		r.ext = path.extname("/a/b/file.tar.gz");
		r.base = path.basename("/a/b/file.txt", ".txt");
		r.rel = path.relative("/a/b/c", "/a/d");

		const em = new EventEmitter();
		const seen = [];
		em.on("ping", (v) => seen.push("on:" + v));
		em.once("ping", (v) => seen.push("once:" + v));
		em.emit("ping", 1);
		em.emit("ping", 2);
		r.events = seen.join(",");
		r.count = em.listenerCount("ping");

		r.fmt = util.format("%s=%d %j", "n", 42, { a: 1 });
		r.qs = qs.stringify(qs.parse("a=1&b=x%20y&a=2"));
		assert.strictEqual(1 + 1, 2);
		assert.deepStrictEqual({ a: [1, 2] }, { a: [1, 2] });
		r.assertOk = true;
	`)
	for expr, want := range map[string]string{
		"r.join":     "a/c/",
		"r.resolve":  "/x/z",
		"r.ext":      ".gz",
		"r.base":     "file",
		"r.rel":      "../../d",
		"r.events":   "on:1,once:1,on:2",
		"r.count":    "1",
		"r.fmt":      `n=42 {"a":1}`,
		"r.qs":       "a=1&a=2&b=x%20y",
		"r.assertOk": "true",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestCoreModulesViaESMImport(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := rt.RunModule(context.Background(), "main.mjs", `
		import pathDefault, { join } from "node:path";
		import { EventEmitter } from "events";
		globalThis.result = [
			join("a", "b"),
			pathDefault.dirname("/x/y/z"),
			typeof EventEmitter,
		].join("|");
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("module threw: %v", r.Error)
	}
	if got := evalStr(t, js, `result`); got != "a/b|/x/y|function" {
		t.Errorf("result = %s", got)
	}
}

func TestBuffer(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		globalThis.r = {};
		const b = Buffer.from("héllo", "utf8");
		r.len = b.length;
		r.utf8 = b.toString();
		r.hex = b.toString("hex");
		r.b64 = b.toString("base64");
		r.b64url = Buffer.from("\xfb\xff").toString("base64url");
		r.roundtrip = Buffer.from(b.toString("base64"), "base64").toString();
		const c = Buffer.concat([Buffer.from("ab"), Buffer.from("cd")]);
		r.concat = c.toString();
		r.isBuf = Buffer.isBuffer(c) && c instanceof Uint8Array;
		const s = c.slice(1, 3);
		s[0] = 0x58; // shares memory with c
		r.shared = c.toString();
		r.eq = Buffer.from("aa").equals(Buffer.from("aa"));
		r.cmp = Buffer.compare(Buffer.from("a"), Buffer.from("b"));
		r.idx = Buffer.from("hello world").indexOf("world");
		r.json = JSON.stringify(Buffer.from([1, 2]));
		// copy() clamps to the target's remaining room and returns the count
		// copied, rather than throwing when the source is larger (Node semantics).
		const dst = Buffer.alloc(4);
		r.copyN = String(Buffer.from("hello world").copy(dst));
		r.copyDst = dst.toString();
	`)
	for expr, want := range map[string]string{
		"r.len":       "6",
		"r.utf8":      "héllo",
		"r.hex":       "68c3a96c6c6f",
		"r.b64":       "aMOpbGxv",
		"r.b64url":    "w7vDvw",
		"r.roundtrip": "héllo",
		"r.concat":    "abcd",
		"r.isBuf":     "true",
		"r.shared":    "aXcd",
		"r.eq":        "true",
		"r.cmp":       "-1",
		"r.idx":       "6",
		"r.json":      `{"type":"Buffer","data":[1,2]}`,
		"r.copyN":     "4",
		"r.copyDst":   "hell",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestFS(t *testing.T) {
	fsys := memfs.New()
	if err := fsys.MkdirAll("data", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("data/hello.txt", []byte("こんにちは"), 0o644); err != nil {
		t.Fatal(err)
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})

	runScript(t, rt, `
		const fs = require("fs");
		globalThis.r = {};
		r.read = fs.readFileSync("/data/hello.txt", "utf8");
		r.buf = fs.readFileSync("data/hello.txt").toString("hex").slice(0, 6);
		fs.writeFileSync("/data/out.txt", "one");
		fs.appendFileSync("/data/out.txt", "+two");
		r.out = fs.readFileSync("/data/out.txt", "utf8");
		fs.mkdirSync("/data/sub/deep", { recursive: true });
		fs.writeFileSync("/data/sub/deep/x.txt", Buffer.from("bin"));
		r.dir = fs.readdirSync("/data").sort().join(",");
		const st = fs.statSync("/data/out.txt");
		r.stat = [st.isFile(), st.isDirectory(), st.size].join("|");
		fs.renameSync("/data/out.txt", "/data/moved.txt");
		r.moved = fs.existsSync("/data/moved.txt") && !fs.existsSync("/data/out.txt");
		fs.unlinkSync("/data/moved.txt");
		r.gone = !fs.existsSync("/data/moved.txt");
		try { fs.readFileSync("/nope.txt"); r.enoent = "no-throw"; }
		catch (e) { r.enoent = e.code; }
	`)
	for expr, want := range map[string]string{
		"r.read":   "こんにちは",
		"r.buf":    "e38193",
		"r.out":    "one+two",
		"r.dir":    "hello.txt,out.txt,sub",
		"r.stat":   "true|false|7",
		"r.moved":  "true",
		"r.gone":   "true",
		"r.enoent": "ENOENT",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestFSPromisesAndCallbacks(t *testing.T) {
	fsys := memfs.New()
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})

	runScript(t, rt, `
		const fsp = require("fs/promises");
		const fs = require("fs");
		globalThis.r = {};
		(async () => {
			await fsp.writeFile("/a.txt", "async");
			r.p = await fsp.readFile("/a.txt", "utf8");
			r.list = (await fsp.readdir("/")).join(",");
		})().catch(e => { r.err = String(e); });
		fs.readFile("/a.txt", "utf8", (err, data) => { r.cbErr = err; r.cb = data; });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("promises chain rejected: %s", got)
	}
	if got := evalStr(t, js, `r.p`); got != "async" {
		t.Errorf("promises read = %q", got)
	}
	if got := evalStr(t, js, `r.cb`); got != "async" {
		t.Errorf("callback read = %q", got)
	}
}

func TestFSReadOnlyAndPermissions(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{
		FS: fstest.MapFS{"ro.txt": {Data: []byte("read only")}},
	})
	runScript(t, rt, `
		const fs = require("fs");
		globalThis.r = {};
		r.read = fs.readFileSync("/ro.txt", "utf8");
		try { fs.writeFileSync("/new.txt", "x"); r.write = "no-throw"; }
		catch (e) { r.write = e.code; }
	`)
	if got := evalStr(t, js, `r.read`); got != "read only" {
		t.Errorf("read = %q", got)
	}
	if got := evalStr(t, js, `r.write`); got != "EROFS" {
		t.Errorf("write to read-only FS: code = %q, want EROFS", got)
	}

	// A policy FS that denies a read surfaces EACCES to the guest — the FS is
	// the single access-control point (a sheena Volume works the same way).
	js2, rt2 := newRuntime(t, spidermonkey.Config{
		FS: denyAllFS{fstest.MapFS{"secret.txt": {Data: []byte("s")}}},
	})
	runScript(t, rt2, `
		const fs = require("fs");
		globalThis.denied = (() => {
			try { fs.readFileSync("/secret.txt"); return "no-throw"; }
			catch (e) { return e.code; }
		})();
	`)
	if got := evalStr(t, js2, `denied`); got != "EACCES" {
		t.Errorf("FS-denied read: code = %q, want EACCES", got)
	}
}

func cjsFixtureFS() fstest.MapFS {
	return fstest.MapFS{
		"lib/greet.js": {Data: []byte(`
			const { upper } = require("./util");
			module.exports = function greet(name) { return upper("hello " + name); };
		`)},
		"lib/util.js": {Data: []byte(`
			exports.upper = (s) => s.toUpperCase();
		`)},
		"config.json":                       {Data: []byte(`{"version": 7, "tags": ["a", "b"]}`)},
		"node_modules/mathpkg/package.json": {Data: []byte(`{"name": "mathpkg", "main": "./src/index.js"}`)},
		"node_modules/mathpkg/src/index.js": {Data: []byte(`
			const dep = require("depkg");
			module.exports = { double: (n) => n * 2, viaDep: dep.tag };
		`)},
		"node_modules/depkg/package.json": {Data: []byte(`{"name": "depkg", "main": "index.js"}`)},
		"node_modules/depkg/index.js":     {Data: []byte(`exports.tag = "dep-ok";`)},
	}
}

func TestRequireCJS(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{FS: cjsFixtureFS()})

	runScript(t, rt, `
		globalThis.r = {};
		const greet = require("./lib/greet");
		r.greet = greet("node");
		const cfg = require("./config.json");
		r.json = cfg.version + ":" + cfg.tags.join("");
		const math = require("mathpkg");
		r.pkg = math.double(21);
		r.dep = math.viaDep;
		r.cached = require("./lib/greet") === greet;
		r.resolved = require.resolve("mathpkg");
	`)
	for expr, want := range map[string]string{
		"r.greet":    "HELLO NODE",
		"r.json":     "7:ab",
		"r.pkg":      "42",
		"r.dep":      "dep-ok",
		"r.cached":   "true",
		"r.resolved": "/node_modules/mathpkg/src/index.js",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestESMImportsCJS(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{FS: cjsFixtureFS()})

	r, err := rt.RunModule(context.Background(), "main.mjs", `
		import math from "mathpkg";
		import greet from "./lib/greet.js";
		import cfg from "./config.json";
		globalThis.result = [math.double(4), greet("esm"), cfg.version].join("|");
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("module threw: %v", r.Error)
	}
	if got := evalStr(t, js, `result`); got != "8|HELLO ESM|7" {
		t.Errorf("result = %s", got)
	}
}

func TestProcess(t *testing.T) {
	var stdout bytes.Buffer
	js, rt := newRuntime(t,
		spidermonkey.Config{
			Env:    []string{"FOO=bar", "EMPTY="},
			Stdout: &stdout,
		},
		nodejs.Options{Argv: []string{"node", "app.js", "--flag"}},
	)

	runScript(t, rt, `
		globalThis.r = {};
		r.foo = process.env.FOO;
		r.missing = String(process.env.NOPE);
		r.argv = process.argv.join(" ");
		r.platformOk = ["darwin", "linux", "win32"].includes(process.platform);
		r.cwd = process.cwd();
		process.stdout.write("no newline");
		process.on("custom", (v) => { r.event = v; });
		process.emit("custom", "fired");
	`)
	if got := evalStr(t, js, `r.foo`); got != "bar" {
		t.Errorf("env FOO = %q", got)
	}
	if got := evalStr(t, js, `r.missing`); got != "undefined" {
		t.Errorf("missing env = %q", got)
	}
	if got := evalStr(t, js, `r.argv`); got != "node app.js --flag" {
		t.Errorf("argv = %q", got)
	}
	if got := evalStr(t, js, `r.platformOk`); got != "true" {
		t.Errorf("platform not recognized")
	}
	if got := evalStr(t, js, `r.cwd`); got != "/" {
		t.Errorf("cwd = %q", got)
	}
	if got := stdout.String(); got != "no newline" {
		t.Errorf("stdout.write = %q", got)
	}
	if got := evalStr(t, js, `r.event`); got != "fired" {
		t.Errorf("process events = %q", got)
	}
}

func TestTimersPromisesModule(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		globalThis.r = {};
		(async () => {
			const { setTimeout: sleep } = require("timers/promises");
			const v = await sleep(5, "waited");
			r.value = v;
		})().catch(e => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("rejected: %s", got)
	}
	if got := evalStr(t, js, `r.value`); got != "waited" {
		t.Errorf("value = %q", got)
	}
}

func TestRequireErrorHasCode(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{FS: fstest.MapFS{}})

	runScript(t, rt, `
		globalThis.r = {};
		try { require("not-installed"); r.code = "no-throw"; }
		catch (e) { r.code = e.code; r.msg = String(e.message).slice(0, 60); }
	`)
	if got := evalStr(t, js, `r.code`); got != "MODULE_NOT_FOUND" {
		t.Errorf("code = %q", got)
	}
	if got := evalStr(t, js, `r.msg`); !strings.Contains(got, "not-installed") {
		t.Errorf("message = %q", got)
	}
}
