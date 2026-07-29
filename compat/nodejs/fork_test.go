package nodejs_test

import (
	"context"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	gofs "github.com/goccy/go-spidermonkey/fs"
)

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
