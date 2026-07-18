package spidermonkey_test

// $262.agent rebuilt ENTIRELY host-side — the proof the agent primitives are
// sufficient: what js.cc used to hard-code (test262's INTERPRETING.md agent
// surface) is now an adapter in ordinary Go + a guest prelude, composed on
// the public API. A Web Worker or worker_threads adapter would follow the
// same pattern on the same primitives.

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// agent262ChildPrelude reshapes the generic __agent__ primitives into the
// child half of test262's $262.agent, then hides them. broadcast carries
// {sab, id}, matching receiveBroadcast(cb(sab, id)).
const agent262ChildPrelude = `
(() => {
	const A = globalThis.__agent__;
	delete globalThis.__agent__;
	globalThis.$262 = {
		agent: {
			receiveBroadcast: (cb) => A.receive((m) => cb(m.sab, m.id)),
			report: (s) => A.post(String(s)),
			sleep: (ms) => A.sleep(ms),
			leaving: () => A.leaving(),
			monotonicNow: () => A.monotonicNow(),
		},
		global: globalThis,
	};
})();
`

func TestAgent262AdapterRunsATest262StyleAgentTest(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	// The whole $262 (agent included) is composed host-side from the public
	// API + engine primitives — no engine-side test262 hook exists.
	if _, err := newHarness262(js); err != nil {
		t.Fatalf("newHarness262: %v", err)
	}

	// The exact shape of a test262 agent test: start an agent, broadcast a
	// SAB with a payload, spin on getReport (sleeping between polls), then
	// assert on the shared memory.
	r, err := js.Eval(context.Background(), `
		$262.agent.start(`+"`"+`
			$262.agent.receiveBroadcast(function (sab, id) {
				Atomics.store(new Int32Array(sab), 0, id + 35);
				$262.agent.report("stored");
				$262.agent.leaving();
			});
		`+"`"+`);
		const sab = new SharedArrayBuffer(4);
		$262.agent.broadcast(sab, 7);
		let report;
		while ((report = $262.agent.getReport()) === null) {
			$262.agent.sleep(10);
		}
		report + ":" + Atomics.load(new Int32Array(sab), 0);
	`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("test262-style script threw: %v", r.Error)
	}
	if got := r.Value.String(); got != "stored:42" {
		t.Errorf("script result = %q, want \"stored:42\"", got)
	}
}
