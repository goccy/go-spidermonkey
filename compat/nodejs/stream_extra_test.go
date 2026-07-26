package nodejs_test

import (
	spidermonkey "github.com/goccy/go-spidermonkey"
	"testing"
)

func TestObjectModeStreams(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const { Readable, Writable, Transform } = require("stream");
		globalThis.r = {};
		// objectMode Readable -> Transform -> Writable, passing objects (not Buffers).
		const src = Readable.from([{n:1},{n:2},{n:3}]);
		const doubler = new Transform({ objectMode: true, transform(o, e, cb){ cb(null, {n:o.n*2}); } });
		const collected = [];
		const sink = new Writable({ objectMode: true, write(o, e, cb){ collected.push(o.n); cb(); } });
		sink.on("finish", () => { r.collected = collected.join(","); });
		src.pipe(doubler).pipe(sink);
	`)
	if got := evalStr(t, js, "r.collected"); got != "2,4,6" {
		t.Errorf("objectMode pipeline = %q, want 2,4,6", got)
	}
}

func TestBackpressure(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const { Writable } = require("stream");
		globalThis.r = {};
		const pending = [];
		const w = new Writable({ highWaterMark: 4, write(chunk, e, cb){ pending.push(cb); } }); // never auto-drains
		// Write 4 bytes: fills hwm -> next write returns false.
		r.first = w.write(Buffer.from("ab"));   // buffered=2 < 4 -> true
		r.second = w.write(Buffer.from("cd"));  // buffered=4 >= 4 -> false
		r.drained = false;
		w.on("drain", () => { r.drained = true; });
		// Flush the callbacks -> buffered drops -> drain fires.
		pending.forEach((cb) => cb());
	`)
	if got := evalStr(t, js, "String(r.first)"); got != "true" {
		t.Errorf("first write = %q, want true", got)
	}
	if got := evalStr(t, js, "String(r.second)"); got != "false" {
		t.Errorf("second write (over hwm) = %q, want false", got)
	}
	if got := evalStr(t, js, "String(r.drained)"); got != "true" {
		t.Errorf("drain event = %q, want true", got)
	}
}
