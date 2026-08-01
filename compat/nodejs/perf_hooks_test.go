package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// perf_hooks used to record nothing: mark/measure returned throwaway objects,
// getEntries* returned [], and PerformanceObserver never fired. Entries must
// now be stored and retrievable, and observers must be delivered new entries.
func TestPerfHooksMarkMeasureAndEntries(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const { performance } = require("perf_hooks");
		globalThis.r = {};
		performance.clearMarks();
		performance.clearMeasures();
		performance.mark("A");
		performance.mark("B");
		performance.measure("A-to-B", "A", "B");
		performance.measure("explicit", { start: 5, end: 25, detail: { k: 1 } });

		r.byName = performance.getEntriesByName("A").length;
		r.marks = performance.getEntriesByType("mark").length;
		r.measures = performance.getEntriesByType("measure").length;
		const m = performance.getEntriesByName("explicit")[0];
		r.explicitDur = m.duration;
		r.explicitType = m.entryType;
		r.explicitDetail = JSON.stringify(m.detail);
		r.allCount = performance.getEntries().length;

		performance.clearMarks("A");
		r.afterClearA = performance.getEntriesByName("A").length;
		r.marksLeft = performance.getEntriesByType("mark").length;

		r.supported = (performance.supportedEntryTypes || []).join(",");
		r.nowWorks = typeof performance.now() === "number" && performance.now() >= 0;
	`)
	for expr, want := range map[string]string{
		"r.byName":         "1",
		"r.marks":          "2",
		"r.measures":       "2",
		"r.explicitDur":    "20",
		"r.explicitType":   "measure",
		"r.explicitDetail": `{"k":1}`,
		"r.allCount":       "4",
		"r.afterClearA":    "0",
		"r.marksLeft":      "1",
		"r.supported":      "mark,measure",
		"r.nowWorks":       "true",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// PerformanceObserver.observe({entryTypes}) must deliver newly created entries
// to the callback via an async task (a PerformanceObserverEntryList).
func TestPerfHooksObserverFires(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const { performance, PerformanceObserver } = require("perf_hooks");
		globalThis.r = { delivered: [], listType: "" };
		const obs = new PerformanceObserver((list) => {
			r.listType = typeof list.getEntries === "function" ? "list" : "plain";
			for (const e of list.getEntries()) r.delivered.push(e.entryType + ":" + e.name);
		});
		obs.observe({ entryTypes: ["mark", "measure"] });
		performance.mark("m1");
		performance.measure("meas", { start: 0, duration: 10 });
	`)
	// The observer callback runs on a microtask after the script body; runScript
	// drains the loop, so by now it must have been delivered.
	if got := evalStr(t, js, `r.listType`); got != "list" {
		t.Errorf("observer received %q, want a PerformanceObserverEntryList", got)
	}
	if got := evalStr(t, js, `r.delivered.join(",")`); got != "mark:m1,measure:meas" {
		t.Errorf("delivered = %q, want mark:m1,measure:meas", got)
	}
}
