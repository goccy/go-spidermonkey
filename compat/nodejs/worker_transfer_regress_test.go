package nodejs_test

import (
	"context"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// A postMessage transfer list must detach the transferred ArrayBuffer on the
// sender side (its byteLength becomes 0) while the receiver still gets the
// bytes. Covers Worker.postMessage(value, [ab]).
func TestWorkerPostMessageTransferDetachesSender(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const w = new Worker(`+"`"+`
			const { parentPort } = require("worker_threads");
			parentPort.on("message", (msg) => {
				// msg.buf is an ArrayBuffer the main thread transferred; read it back.
				const view = new Uint8Array(msg.buf);
				parentPort.postMessage("recv:" + view.length + ":" + view[0] + "," + view[1] + "," + view[2]);
				process.exit(0);
			});
		`+"`"+`, { eval: true });
		const ab = new Uint8Array([10, 20, 30]).buffer;
		w.on("online", () => {
			w.postMessage({ buf: ab }, [ab]);
			// After transfer the sender's ArrayBuffer is detached: byteLength 0.
			r.senderByteLength = ab.byteLength;
		});
		w.on("message", (m) => { r.workerSaw = m; });
		w.on("exit", (c) => { r.exit = c; });
	`)
	if err != nil || r.Error != nil {
		t.Fatalf("eval: %v %v", err, r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `String(r.senderByteLength)`); got != "0" {
		t.Errorf("sender ArrayBuffer.byteLength after transfer = %q, want 0 (not detached)", got)
	}
	if got := evalStr(t, js, `String(r.workerSaw)`); got != "recv:3:10,20,30" {
		t.Errorf("worker did not receive the transferred bytes: %q", got)
	}
}

// The same, in the worker->main direction: parentPort.postMessage(value, [ab])
// detaches on the worker side and the main thread receives the bytes.
func TestParentPortPostMessageTransferDetachesSender(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const w = new Worker(`+"`"+`
			const { parentPort } = require("worker_threads");
			const ab = new Uint8Array([1, 2, 3, 4]).buffer;
			parentPort.postMessage({ buf: ab, before: ab.byteLength }, [ab]);
			// Report the sender-side byteLength AFTER transfer via a second message.
			parentPort.postMessage({ after: ab.byteLength });
			process.exit(0);
		`+"`"+`, { eval: true });
		r.msgs = [];
		w.on("message", (m) => { r.msgs.push(m); });
		w.on("exit", (c) => { r.exit = c; });
	`)
	if err != nil || r.Error != nil {
		t.Fatalf("eval: %v %v", err, r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `String(new Uint8Array(r.msgs[0].buf).join(","))`); got != "1,2,3,4" {
		t.Errorf("main did not receive transferred bytes: %q", got)
	}
	if got := evalStr(t, js, `String(r.msgs[0].before)`); got != "4" {
		t.Errorf("worker byteLength before transfer = %q, want 4", got)
	}
	if got := evalStr(t, js, `String(r.msgs[1].after)`); got != "0" {
		t.Errorf("worker byteLength after transfer = %q, want 0 (not detached)", got)
	}
}

// A worker that throws must surface the ORIGINAL error message/name/stack on the
// main thread's 'error' event — not a flattened "Name: message" string.
func TestWorkerErrorPreservesMessageAndStack(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	r, err := js.Eval(context.Background(), `
		const { Worker } = require("worker_threads");
		globalThis.r = {};
		const w = new Worker("throw new TypeError('boom')", { eval: true });
		w.on("error", (e) => {
			r.isError = e instanceof Error;
			r.message = e.message;
			r.name = e.name;
			r.hasStack = typeof e.stack === "string" && e.stack.length > 0;
		});
		w.on("exit", (c) => { r.exit = c; });
	`)
	if err != nil || r.Error != nil {
		t.Fatalf("eval: %v %v", err, r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalStr(t, js, `String(r.isError)`); got != "true" {
		t.Errorf("worker error is not an Error instance")
	}
	if got := evalStr(t, js, `r.message`); got != "boom" {
		t.Errorf("err.message = %q, want exactly \"boom\"", got)
	}
	if got := evalStr(t, js, `r.name`); got != "TypeError" {
		t.Errorf("err.name = %q, want TypeError", got)
	}
	if got := evalStr(t, js, `String(r.hasStack)`); got != "true" {
		t.Errorf("err.stack was not preserved")
	}
}
