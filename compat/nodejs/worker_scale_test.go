package nodejs_test

// How many live workers an instance can hold, and that teardown stays prompt
// with all of them alive.
//
// Every guest thread's stack is allocated out of the instance's linear memory,
// so the worker ceiling is a memory budget: Config.MaxMemoryBytes divided by
// what a worker actually uses. Two things used to make that budget far smaller
// than it looks — each thread took the LINKED main-stack size (8 MiB) rather
// than a size chosen for it, which also starved the engine's own helper
// threads at teardown-GC time and hung the shutdown outright.

import (
	"context"
	"fmt"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
)

// spawnLiveWorkers starts n workers that come online and then idle forever
// holding a 'message' listener, and reports how many reported in.
func spawnLiveWorkers(t *testing.T, js *spidermonkey.JS, rt interface {
	Wait(context.Context) error
}, n int) {
	t.Helper()
	r, err := js.Eval(context.Background(), fmt.Sprintf(`
		const { Worker } = require("worker_threads");
		globalThis.r = { online: 0 };
		for (let i = 0; i < %d; i++) {
			const w = new Worker(`+"`"+`
				const { parentPort } = require("worker_threads");
				parentPort.postMessage("up");
				parentPort.on("message", (m) => parentPort.postMessage(m));
			`+"`"+`, { eval: true });
			w.unref();
			w.on("message", () => { r.online++; });
		}
	`, n))
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("spawning %d workers threw: %v", n, r.Error)
	}
	// The workers never exit, so the loop only ends on the deadline; a few
	// seconds is ample for them all to report in.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = rt.Wait(ctx)
	if got := evalStr(t, js, `String(r.online)`); got != fmt.Sprint(n) {
		t.Fatalf("%s of %d workers came online", got, n)
	}
}

// The regression: teardown with many live workers used to hang forever once
// the count reached the point where thread stacks exhausted linear memory —
// deterministically, from 16 benign idle workers, wedging the instance for the
// process lifetime.
func TestTeardownWithManyLiveWorkers(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rt, err := nodejs.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	spawnLiveWorkers(t, js, rt, 20)

	done := make(chan error, 1)
	go func() { rt.Close(); done <- js.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("teardown hung with 20 live workers")
	}
}

// The ceiling is a memory budget and nothing else, so the documented knob
// moves it: a count that does not fit the default cap fits a larger one.
func TestWorkerCeilingScalesWithMemoryBudget(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{MaxMemoryBytes: 1 << 30})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rt, err := nodejs.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer func() { rt.Close(); js.Close() }()
	spawnLiveWorkers(t, js, rt, 96)
}
