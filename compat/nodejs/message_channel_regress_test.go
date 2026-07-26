package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// A same-thread MessageChannel: port1.postMessage(x) delivers a 'message' on
// port2 and vice versa, usable both EventEmitter-style (on('message') gets the
// data) and DOM-style (onmessage / addEventListener get a { data } event). The
// payload is structured-cloned. Covers the previously-throwing stub.
func TestMessageChannelSameThreadBothStyles(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	// runScript drains the full event loop (timers + microtasks), so the
	// queueMicrotask-based deliveries all complete before it returns.
	runScript(t, rt, `
		const { MessageChannel } = require("worker_threads");
		globalThis.r = { onData: [], onmessageData: null };
		const { port1, port2 } = new MessageChannel();

		// EventEmitter style on port2: receives the raw data.
		port2.on("message", (data) => { r.onData.push(data); });
		// DOM style onmessage on port1: receives a { data } MessageEvent.
		port1.onmessage = (ev) => { r.onmessageData = ev.data; };

		port1.postMessage("to-port2-a");
		port1.postMessage({ n: 42 });
		port2.postMessage("to-port1");
	`)

	if got := evalStr(t, js, `r.onData.length + ":" + typeof r.onData[1]`); got != "2:object" {
		t.Errorf("port2 on('message') deliveries = %q, want 2:object", got)
	}
	if got := evalStr(t, js, `String(r.onData[0])`); got != "to-port2-a" {
		t.Errorf("port2 first message = %q", got)
	}
	if got := evalStr(t, js, `String(r.onData[1].n)`); got != "42" {
		t.Errorf("structured-cloned object not received on port2: %q", got)
	}
	if got := evalStr(t, js, `String(r.onmessageData)`); got != "to-port1" {
		t.Errorf("port1.onmessage data = %q, want to-port1", got)
	}
}

// The payload must be a structured CLONE: mutating the original after sending
// must not change what the receiver got.
func TestMessageChannelStructuredClone(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const { MessageChannel } = require("worker_threads");
		globalThis.r = {};
		const { port1, port2 } = new MessageChannel();
		const payload = { arr: [1, 2, 3] };
		port2.on("message", (data) => { r.received = data; });
		port1.postMessage(payload);
		payload.arr.push(999); // mutate AFTER post; receiver must not see it
	`)

	if got := evalStr(t, js, `r.received.arr.join(",")`); got != "1,2,3" {
		t.Errorf("payload was not cloned (receiver saw a mutation): %q", got)
	}
}

// addEventListener('message', ...) starts the port and delivers { data } events;
// a message posted before any listener is attached is buffered until the port
// starts, not dropped.
func TestMessagePortBuffersUntilStarted(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const { MessageChannel } = require("worker_threads");
		globalThis.r = { events: [] };
		const { port1, port2 } = new MessageChannel();
		// Post before port2 has any listener: must be buffered.
		port1.postMessage("early");
		// Attach a listener a turn later, then post again.
		queueMicrotask(() => {
			port2.addEventListener("message", (ev) => { r.events.push(ev.data); });
			port1.postMessage("late");
		});
	`)

	if got := evalStr(t, js, `r.events.join(",")`); got != "early,late" {
		t.Errorf("buffered+live delivery = %q, want early,late", got)
	}
}

// close() closes both ends and emits 'close' on each; a post after close is a
// no-op (does not throw).
func TestMessageChannelClose(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const { MessageChannel } = require("worker_threads");
		globalThis.r = { closed: [], threw: false };
		const { port1, port2 } = new MessageChannel();
		port1.on("close", () => r.closed.push("port1"));
		port2.on("close", () => r.closed.push("port2"));
		port1.close();
		try { port2.postMessage("ignored"); } catch (e) { r.threw = true; }
	`)

	if got := evalStr(t, js, `r.closed.slice().sort().join(",")`); got != "port1,port2" {
		t.Errorf("close did not fire on both ports: %q", got)
	}
	if got := evalStr(t, js, `String(r.threw)`); got != "false" {
		t.Errorf("postMessage after close threw")
	}
}

// The MessageChannel/MessagePort are exported and constructible (the previous
// stub threw "not supported").
func TestMessageChannelExportsConstructible(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const wt = require("worker_threads");
		globalThis.r = {};
		r.hasChannel = typeof wt.MessageChannel === "function";
		r.hasPort = typeof wt.MessagePort === "function";
		const ch = new wt.MessageChannel();
		r.portsAreObjects = (ch.port1 instanceof wt.MessagePort) && (ch.port2 instanceof wt.MessagePort);
	`)
	for expr, want := range map[string]string{
		"String(r.hasChannel)":      "true",
		"String(r.hasPort)":         "true",
		"String(r.portsAreObjects)": "true",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}
