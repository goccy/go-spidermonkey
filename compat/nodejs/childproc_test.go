package nodejs_test

import (
	spidermonkey "github.com/goccy/go-spidermonkey"
	"testing"
)

func allowExec() spidermonkey.Config {
	return spidermonkey.Config{Exec: func(path string, argv []string) bool { return true }}
}

func TestChildProcessSpawnSync(t *testing.T) {
	js, rt := newRuntime(t, allowExec())
	runScript(t, rt, `
		const cp = require("child_process");
		globalThis.r = {};
		const res = cp.spawnSync("echo", ["hello", "world"], { encoding: "utf8" });
		r.status = res.status;
		r.stdout = res.stdout.trim();
		// execSync through a shell.
		r.exec = cp.execSync("echo shell-works", { encoding: "utf8" }).trim();
	`)
	if got := evalStr(t, js, "String(r.status)"); got != "0" {
		t.Errorf("status = %q", got)
	}
	if got := evalStr(t, js, "r.stdout"); got != "hello world" {
		t.Errorf("stdout = %q", got)
	}
	if got := evalStr(t, js, "r.exec"); got != "shell-works" {
		t.Errorf("execSync = %q", got)
	}
}

func TestChildProcessSpawnAsync(t *testing.T) {
	js, rt := newRuntime(t, allowExec())
	runScript(t, rt, `
		const cp = require("child_process");
		globalThis.r = { out: "" };
		const child = cp.spawn("printf", ["a\nb\nc"]);
		child.stdout.setEncoding("utf8");
		child.stdout.on("data", (d) => { r.out += d; });
		child.on("exit", (code) => { r.exit = code; });
		child.on("close", () => { r.closed = true; });
	`)
	if got := evalStr(t, js, "r.out"); got != "a\nb\nc" {
		t.Errorf("stdout = %q", got)
	}
	if got := evalStr(t, js, "String(r.exit)"); got != "0" {
		t.Errorf("exit = %q", got)
	}
	if got := evalStr(t, js, "String(r.closed)"); got != "true" {
		t.Errorf("close not fired")
	}
}

func TestChildProcessStdin(t *testing.T) {
	js, rt := newRuntime(t, allowExec())
	runScript(t, rt, `
		const cp = require("child_process");
		globalThis.r = { out: "" };
		const child = cp.spawn("cat", []);
		child.stdout.setEncoding("utf8");
		child.stdout.on("data", (d) => { r.out += d; });
		child.on("close", () => { r.done = true; });
		child.stdin.write("piped input");
		child.stdin.end();
	`)
	if got := evalStr(t, js, "r.out"); got != "piped input" {
		t.Errorf("cat stdin->stdout = %q", got)
	}
}

func TestChildProcessDeniedByDefault(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{}) // no Exec hook
	runScript(t, rt, `
		const cp = require("child_process");
		globalThis.r = {};
		try { cp.spawnSync("echo", ["x"]); r.code = "no-throw"; } catch(e) { r.code = e.code; }
		r.syncErr = cp.spawnSync("echo", ["x"]).error ? cp.spawnSync("echo", ["x"]).error.code : "none";
	`)
	if got := evalStr(t, js, "r.syncErr"); got != "EPERM" {
		t.Errorf("spawnSync without Exec hook: error code = %q, want EPERM", got)
	}
}
