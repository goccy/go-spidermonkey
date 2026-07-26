package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// diagnostics_channel used to be a total silent no-op: publish() reached no
// subscribers and hasSubscribers was always false. It must now be a real
// pub/sub with process-wide (runtime-wide) singleton channels keyed by name.
func TestDiagnosticsChannelPubSub(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const dc = require("diagnostics_channel");
		globalThis.r = {};
		// Two separate channel(name) calls must return the SAME channel object.
		const a = dc.channel("app:event");
		const b = dc.channel("app:event");
		r.singleton = a === b;
		r.emptyBefore = a.hasSubscribers;

		const received = [];
		const onMsg = (msg, name) => received.push(name + ":" + JSON.stringify(msg));
		a.subscribe(onMsg);
		r.hasSubs = a.hasSubscribers;
		// A publisher that only has channel "b" still reaches the subscriber on "a".
		b.publish({ n: 1 });
		b.publish({ n: 2 });
		r.received = received.join(",");

		// Top-level dc.subscribe/unsubscribe route to the named channel.
		let topCount = 0;
		const topCb = () => topCount++;
		dc.subscribe("app:event", topCb);
		a.publish({ n: 3 });
		r.topAfterSub = topCount;
		dc.unsubscribe("app:event", topCb);
		a.publish({ n: 4 });
		r.topAfterUnsub = topCount;
		r.dcHasSubs = dc.hasSubscribers("app:event");

		a.unsubscribe(onMsg);
		r.emptyAfter = a.hasSubscribers;
		r.dcHasSubsAfter = dc.hasSubscribers("app:event");
		r.unknownChannel = dc.hasSubscribers("never:created");
	`)
	for expr, want := range map[string]string{
		"r.singleton":      "true",
		"r.emptyBefore":    "false",
		"r.hasSubs":        "true",
		"r.received":       "app:event:{\"n\":1},app:event:{\"n\":2}",
		"r.topAfterSub":    "1",
		"r.topAfterUnsub":  "1",
		"r.dcHasSubs":      "true",
		"r.emptyAfter":     "false",
		"r.dcHasSubsAfter": "false",
		"r.unknownChannel": "false",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// tracingChannel wraps a synchronous operation with start/end/error channels.
func TestDiagnosticsTracingChannel(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const dc = require("diagnostics_channel");
		globalThis.r = {};
		const tc = dc.tracingChannel("req");
		const events = [];
		tc.subscribe({
			start: () => events.push("start"),
			end: () => events.push("end"),
			error: () => events.push("error"),
		});
		const out = tc.traceSync((x) => x * 2, { id: 1 }, null, 21);
		r.syncResult = out;
		r.syncEvents = events.join(",");

		events.length = 0;
		try { tc.traceSync(() => { throw new Error("boom"); }, {}, null); } catch (e) { r.threw = e.message; }
		r.errEvents = events.join(",");
	`)
	for expr, want := range map[string]string{
		"r.syncResult": "42",
		"r.syncEvents": "start,end",
		"r.threw":      "boom",
		"r.errEvents":  "start,error,end",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}
