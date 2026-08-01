package nodejs_test

import (
	"context"
	spidermonkey "github.com/goccy/go-spidermonkey"
	gofs "github.com/goccy/go-spidermonkey/fs"
	"testing"
	"time"
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

// TestChildProcessSignalExit verifies a child killed by a signal reports
// code=null and the correct signal name (not code=-1 / a lost signal).
func TestChildProcessSignalExit(t *testing.T) {
	js, rt := newRuntime(t, allowExec())
	runScript(t, rt, `
		globalThis.r = {};
		const { spawn } = require("child_process");
		const c = spawn("sleep", ["5"]);
		c.on("exit", (code, sig) => { r.codeIsNull = code === null; r.sig = sig; });
		setTimeout(() => c.kill("SIGHUP"), 100);
	`)
	if got := evalStr(t, js, `String(r.codeIsNull)`); got != "true" {
		t.Errorf("exit code on signal death = not-null (want null); r.codeIsNull=%q", got)
	}
	if got := evalStr(t, js, `String(r.sig)`); got != "SIGHUP" {
		t.Errorf("signal name = %q, want SIGHUP", got)
	}
}

// child_process.fork() threw outright — there was no node binary to re-spawn.
// A child here is a nested interpreter, so the missing piece was never the
// binary, it was the message channel: fork IS spawn plus IPC, and 164 tests of
// Node's suite are written against it.
func TestForkIPC(t *testing.T) {
	fsys := gofs.NewMemFS()
	js, rt := newRuntime(t, spidermonkey.Config{
		FS:   fsys,
		Exec: func(path string, argv []string) bool { return true },
	})

	// The child echoes what it is sent, then reports what it saw on exit.
	if _, err := rt.RunScript(context.Background(), `
		require("fs").writeFileSync("/child.js", `+"`"+`
			process.on("message", (m) => {
				if (m === "stop") { process.disconnect(); return; }
				process.send({ echo: m, connected: process.connected });
			});
		`+"`"+`);
	`); err != nil {
		t.Fatalf("write child: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := rt.RunScript(ctx, `
		const { fork } = require("child_process");
		globalThis.r = { messages: [], hasSend: typeof process.send };
		const child = fork("/child.js");
		r.connected = child.connected;
		child.on("message", (m) => {
			r.messages.push(JSON.stringify(m));
			if (r.messages.length === 2) child.send("stop");
		});
		child.on("disconnect", () => { r.disconnected = true; });
		child.send("one");
		child.send({ two: 2 });
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	for _, c := range []struct{ expr, want string }{
		// The parent is not a forked child, so it has no process.send.
		{`r.hasSend`, "undefined"},
		{`String(r.connected)`, "true"},
		{`r.messages.join(" | ")`, `{"echo":"one","connected":true} | {"echo":{"two":2},"connected":true}`},
		{`String(r.disconnected)`, "true"},
	} {
		if got := evalStr(t, js, c.expr); got != c.want {
			t.Errorf("%s = %q, want %q", c.expr, got, c.want)
		}
	}
}
