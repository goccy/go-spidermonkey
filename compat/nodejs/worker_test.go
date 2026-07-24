package nodejs_test

import (
	"context"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func TestWorkerThreadsEchoAndWorkerData(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	// A worker that greets using workerData, then echoes messages back
	// upper-cased. Real parallel agent.
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = { messages: [] };
		const w = new Worker(`+"`"+`
			const { parentPort, workerData } = require("worker_threads");
			parentPort.postMessage("ready:" + workerData.name);
			parentPort.on("message", (msg) => {
				if (msg === "stop") { process.exit(0); return; }
				parentPort.postMessage(String(msg).toUpperCase());
			});
		`+"`"+`, { eval: true, workerData: { name: "goccy" } });
		w.on("online", () => { r.online = true; });
		w.on("message", (m) => {
			r.messages.push(m);
			if (m === "ready:goccy") { w.postMessage("hello"); w.postMessage("world"); }
			if (m === "WORLD") { w.postMessage("stop"); }
		});
		w.on("exit", (code) => { r.exitCode = code; });
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if !evalVal(t, js, `r.online`).Bool() {
		t.Error("online event did not fire")
	}
	if got := evalStr(t, js, `r.messages.join(",")`); got != "ready:goccy,HELLO,WORLD" {
		t.Errorf("messages = %q, want ready:goccy,HELLO,WORLD", got)
	}
	if got := evalStr(t, js, `String(r.exitCode)`); got != "0" {
		t.Errorf("exit code = %q", got)
	}
}

func TestWorkerThreadsParallelCompute(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	// Fan out to 4 workers each summing a range; verify the total. This runs
	// on 4 real goroutines/realms in parallel.
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = { partials: [], total: 0, done: 0 };
		const N = 4;
		const perWorker = 250000;
		for (let i = 0; i < N; i++) {
			const w = new Worker(`+"`"+`
				const { parentPort, workerData } = require("worker_threads");
				let sum = 0;
				for (let n = workerData.start; n < workerData.end; n++) sum += n;
				parentPort.postMessage(sum);
				process.exit(0);
			`+"`"+`, { eval: true, workerData: { start: i * perWorker, end: (i + 1) * perWorker } });
			w.on("message", (partial) => {
				r.partials.push(partial);
				r.total += partial;
				r.done++;
			});
		}
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if got := evalStr(t, js, `String(r.done)`); got != "4" {
		t.Errorf("completed workers = %q, want 4", got)
	}
	// Sum of 0..999999 = 999999*1000000/2 = 499999500000.
	if got := evalStr(t, js, `String(r.total)`); got != "499999500000" {
		t.Errorf("parallel sum = %q, want 499999500000", got)
	}
}

func TestWorkerThreadsSharedArrayBuffer(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	// The main thread and worker share an Int32Array over a SharedArrayBuffer;
	// the worker atomically increments a counter the main thread then reads.
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const sab = new SharedArrayBuffer(8);
		const view = new Int32Array(sab);
		const w = new Worker(`+"`"+`
			const { parentPort, workerData } = require("worker_threads");
			const view = new Int32Array(workerData.sab);
			for (let i = 0; i < 1000; i++) Atomics.add(view, 0, 1);
			Atomics.store(view, 1, 42);
			parentPort.postMessage("done");
			process.exit(0);
		`+"`"+`, { eval: true, workerData: { sab } });
		w.on("message", () => {
			r.counter = Atomics.load(view, 0);
			r.flag = Atomics.load(view, 1);
		});
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if got := evalStr(t, js, `String(r.counter)`); got != "1000" {
		t.Errorf("shared counter = %q, want 1000 (SharedArrayBuffer not shared?)", got)
	}
	if got := evalStr(t, js, `String(r.flag)`); got != "42" {
		t.Errorf("shared flag = %q, want 42", got)
	}
}

func TestWorkerThreadsTerminate(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const w = new Worker(`+"`"+`
			const { parentPort } = require("worker_threads");
			parentPort.postMessage("started");
			parentPort.on("message", () => {}); // keep alive
		`+"`"+`, { eval: true });
		w.on("message", (m) => { if (m === "started") { r.started = true; w.terminate(); } });
		w.on("exit", (code) => { r.exited = true; });
	`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !evalVal(t, js, `r.started`).Bool() {
		t.Error("worker did not start")
	}
	if !evalVal(t, js, `r.exited`).Bool() {
		t.Error("terminate did not fire exit")
	}
}
