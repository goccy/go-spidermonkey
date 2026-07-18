package spidermonkey_test

// The Web Worker class, driven from JS as `new Worker(src)` — the proof the
// whole adapter is host-composed (see worker_adapter_test.go: agent primitives
// + a host constructor), and that the worker's source is evaluated verbatim
// (its own strict directive, line numbers), unlike the old prepended prelude.

import (
	"context"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// newWorkerJS installs the Worker class plus a `report(v)` host func the guest
// uses to hand worker replies back to Go, and returns the report channel.
func newWorkerJS(t *testing.T) (*spidermonkey.JS, chan spidermonkey.Value) {
	t.Helper()
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatal(err)
	}
	wm, err := installWebWorker(js)
	if err != nil {
		t.Fatalf("installWebWorker: %v", err)
	}
	// Stop the message pump BEFORE the interpreter closes (LIFO: js.Close
	// registered first runs last).
	t.Cleanup(func() { _ = js.Close() })
	t.Cleanup(wm.close)
	ch := make(chan spidermonkey.Value, 16)
	err = js.Global().DefineFunc("report", func(cfg spidermonkey.Config, a []spidermonkey.Value) (spidermonkey.Value, error) {
		if len(a) > 0 {
			ch <- a[0]
		}
		return spidermonkey.Undefined(), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return js, ch
}

func eval(t *testing.T, js *spidermonkey.JS, src string) {
	t.Helper()
	r, err := js.Eval(context.Background(), src)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("Eval threw: %v", r.Error)
	}
}

func TestNewWorkerRoundTrip(t *testing.T) {
	js, got := newWorkerJS(t)

	// A real `new Worker(...)` from JS: the worker doubles each message and the
	// main thread's onmessage forwards it to Go via report().
	eval(t, js, "var w = new Worker(`self.onmessage = (e) => self.postMessage(e.data * 2);`);\n"+
		"w.onmessage = (e) => report(e.data);\n"+
		"w.postMessage(21); w.postMessage(100);")

	want := map[int]bool{42: true, 200: true}
	deadline := time.After(15 * time.Second)
	for len(want) > 0 {
		select {
		case v := <-got:
			if !want[v.Int()] {
				t.Errorf("unexpected reply %d", v.Int())
			}
			delete(want, v.Int())
		case <-deadline:
			t.Fatalf("timed out; missing %v", want)
		}
	}
}

func TestNewWorkerAsyncHandler(t *testing.T) {
	js, got := newWorkerJS(t)

	// An async onmessage in the worker (await a promise) — works because the
	// worker's event loop keeps its job queue draining.
	eval(t, js, "var w = new Worker(`self.onmessage = async (e) => {\n"+
		"  const x = await Promise.resolve(e.data + 1);\n"+
		"  self.postMessage(x);\n"+
		"};`);\n"+
		"w.onmessage = (e) => report(e.data);\n"+
		"w.postMessage(41);")

	select {
	case v := <-got:
		if v.Int() != 42 {
			t.Errorf("async worker reply = %d, want 42", v.Int())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("async worker did not reply")
	}
}

func TestNewWorkerSharesSAB(t *testing.T) {
	js, got := newWorkerJS(t)

	// postMessage a SharedArrayBuffer; the worker writes into it and replies.
	eval(t, js, "globalThis.sab = new SharedArrayBuffer(4);\n"+
		"var w = new Worker(`self.onmessage = (e) => { Atomics.store(new Int32Array(e.data), 0, 77); self.postMessage('ok'); };`);\n"+
		"w.onmessage = (e) => report(e.data);\n"+
		"w.postMessage(sab);")

	select {
	case v := <-got:
		if v.String() != "ok" {
			t.Fatalf("worker reply = %q, want \"ok\"", v.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no worker reply")
	}
	// The worker's write is visible on the main runtime's SAB — shared memory.
	r, err := js.Eval(context.Background(), `Atomics.load(new Int32Array(sab), 0)`)
	if err != nil || r.Error != nil {
		t.Fatalf("Eval(load): %v / %v", err, r.Error)
	}
	if r.Value.Int() != 77 {
		t.Errorf("main sees sab[0] = %d, want 77", r.Value.Int())
	}
}

func TestNewWorkerTerminate(t *testing.T) {
	js, _ := newWorkerJS(t)

	eval(t, js, "globalThis.w = new Worker(`self.onmessage = (e) => self.postMessage(e.data);`);")
	agents := js.Agents()
	// wait for the worker to be up
	deadline := time.Now().Add(10 * time.Second)
	for agents.Alive() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	eval(t, js, `w.terminate();`)
	for agents.Alive() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("worker did not terminate; alive=%d", agents.Alive())
		}
		time.Sleep(time.Millisecond)
	}
}

// TestWorkerSourceKeepsStrict proves the old prelude bug is gone: a worker
// source with a leading "use strict" is now genuinely strict, because the
// source is evaluated as its own script (not concatenated after glue).
func TestWorkerSourceKeepsStrict(t *testing.T) {
	js, got := newWorkerJS(t)

	eval(t, js, "var w = new Worker(`\"use strict\";\n"+
		"self.onmessage = (e) => {\n"+
		"  try { undeclaredX = 1; self.postMessage('NOT-STRICT'); }\n"+
		"  catch (err) { self.postMessage('STRICT:' + err.name); }\n"+
		"};`);\n"+
		"w.onmessage = (e) => report(e.data);\n"+
		"w.postMessage(1);")

	select {
	case v := <-got:
		if v.String() != "STRICT:ReferenceError" {
			t.Errorf("worker \"use strict\" not honored: got %q", v.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no worker reply")
	}
}
