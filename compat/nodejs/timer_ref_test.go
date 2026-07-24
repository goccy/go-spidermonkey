package nodejs_test

import (
	"context"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestUnrefTimerLetsLoopExit verifies an unref'd timer does not, on its own, keep
// the loop alive: a loop whose only remaining work is an unref'd interval must
// reach idle and return, rather than blocking until the context deadline.
func TestUnrefTimerLetsLoopExit(t *testing.T) {
	_, rt := newRuntime(t, spidermonkey.Config{})
	done := make(chan error, 1)
	go func() {
		_, err := rt.RunScript(context.Background(), `
			const t = setInterval(() => {}, 30000);
			t.unref();
		`)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunScript: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not exit with only an unref'd interval armed (hang)")
	}
}

// TestUnrefServerLetsLoopExit verifies an unref'd listening server does not keep
// the loop alive on its own: with nothing else pending, the loop reaches idle.
func TestUnrefServerLetsLoopExit(t *testing.T) {
	_, rt := newRuntime(t, spidermonkey.Config{})
	done := make(chan error, 1)
	go func() {
		_, err := rt.RunScript(context.Background(), `
			const http = require("http");
			const server = http.createServer(() => {});
			server.listen(0, "127.0.0.1");
			server.unref();
		`)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunScript: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not exit with only an unref'd server listening (hang)")
	}
}

// TestTimerThrowRoutedToUncaught verifies a throw in a timer callback is routed
// to the uncaughtException handler and does NOT tear down the whole loop — the
// interval keeps firing afterwards, matching Node.
func TestTimerThrowRoutedToUncaught(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = { ticks: 0, caught: 0 };
		process.on("uncaughtException", () => { r.caught++; });
		let n = 0;
		const t = setInterval(() => {
			n++;
			if (n === 1) throw new Error("boom");
			r.ticks++;
			if (n >= 3) clearInterval(t);
		}, 5);
	`)
	if got := evalVal(t, js, "r.caught").Int(); got < 1 {
		t.Errorf("uncaughtException not invoked: caught=%d", got)
	}
	if got := evalVal(t, js, "r.ticks").Int(); got < 1 {
		t.Errorf("loop stopped after timer throw: ticks=%d", got)
	}
}
