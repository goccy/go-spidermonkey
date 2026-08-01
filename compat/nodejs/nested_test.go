package nodejs_test

import (
	"context"
	"testing"
	"testing/fstest"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
)

// Spawning process.execPath runs a NESTED INTERPRETER rather than an OS
// process: there is no node binary here, and there does not need to be one.
//
// This is what a large part of Node's own suite is written against — it
// re-executes node to check flag handling, exit codes and stdio — and it is a
// real capability besides: tooling that shells out to `node` needs it.
func TestSpawnExecPathRunsNestedInterpreter(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{
		FS:   fstest.MapFS{"app/hello.js": {Data: []byte(`console.log("from file:" + process.argv[2]);`)}},
		Exec: func(cmd string, args []string) bool { return true },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	rt, err := nodejs.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer rt.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := rt.RunScript(ctx, `
		const { spawnSync, spawn } = require("child_process");
		const out = [];
		const sync = (args, opts) => spawnSync(process.execPath, args, opts);

		let r = sync(["-e", "console.log('hi from child')"]);
		out.push("eval:" + r.status + ":" + String(r.stdout).trim());
		out.push("print:" + sync(["-p", "40 + 2"]).status + ":" + String(sync(["-p", "40 + 2"]).stdout).trim());
		out.push("exit:" + sync(["-e", "process.exit(3)"]).status);
		r = sync(["-e", "throw new Error('boom')"]);
		out.push("throw:" + r.status + ":" + (String(r.stderr).includes("boom") ? "on-stderr" : "MISSING"));
		out.push("file:" + String(sync(["/app/hello.js", "ARG"]).stdout).trim());
		out.push("env:" + String(sync(["-e", "console.log(process.env.FOO)"], { env: { FOO: "bar" } }).stdout).trim());
		// The child is a real child: its own globals, its own exit.
		out.push("isolated:" + String(sync(["-e", "console.log(typeof globalThis.__parentOnly)"]).stdout).trim());
		globalThis.__parentOnly = 1;

		globalThis.result = out.join(" | ");
		const child = spawn(process.execPath, ["-e", "console.log('async child'); process.exit(7)"]);
		let acc = "";
		child.stdout.on("data", (d) => { acc += d; });
		child.on("exit", (code) => { globalThis.result += " | async:" + code + ":" + acc.trim(); });
	`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("threw: %v", r.Error)
	}
	v, err := js.Eval(context.Background(), `String(globalThis.result)`)
	if err != nil {
		t.Fatal(err)
	}
	want := "eval:0:hi from child | print:0:42 | exit:3 | throw:1:on-stderr | " +
		"file:from file:ARG | env:bar | isolated:undefined | async:7:async child"
	if got := v.Value.String(); got != want {
		t.Errorf("child node behaviour =\n %s\nwant\n %s", got, want)
	}
}
