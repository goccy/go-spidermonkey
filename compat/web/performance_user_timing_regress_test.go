package web_test

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// The compat/web performance object used to expose only {timeOrigin, now}. The
// User Timing API (mark/measure/getEntries*/clearMarks/clearMeasures) and
// PerformanceObserver must be present on globalThis.performance too, as
// browsers/workerd expose them.
func TestWebPerformanceUserTiming(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})
	eval(t, js, `
		globalThis.r = {};
		performance.clearMarks();
		performance.clearMeasures();
		performance.mark("start");
		performance.mark("stop");
		performance.measure("span", "start", "stop");
		r.marks = performance.getEntriesByType("mark").length;
		r.measures = performance.getEntriesByType("measure").length;
		r.byName = performance.getEntriesByName("start", "mark").length;
		r.total = performance.getEntries().length;
		r.hasNow = typeof performance.now() === "number";
		r.hasOrigin = typeof performance.timeOrigin === "number";
		r.observerCtor = typeof globalThis.PerformanceObserver === "function";
		performance.clearMarks("start");
		r.afterClear = performance.getEntriesByName("start", "mark").length;
	`)
	for expr, want := range map[string]string{
		"r.marks":        "2",
		"r.measures":     "1",
		"r.byName":       "1",
		"r.total":        "3",
		"r.hasNow":       "true",
		"r.hasOrigin":    "true",
		"r.observerCtor": "true",
		"r.afterClear":   "0",
	} {
		if got := evalString(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// PerformanceObserver on the web layer delivers newly recorded entries.
func TestWebPerformanceObserver(t *testing.T) {
	js, w := newWeb(t, spidermonkey.Config{})
	eval(t, js, `
		globalThis.r = { got: [] };
		const obs = new PerformanceObserver((list) => {
			for (const e of list.getEntries()) r.got.push(e.entryType + ":" + e.name);
		});
		obs.observe({ entryTypes: ["mark"] });
		performance.mark("m1");
		performance.mark("m2");
	`)
	// Delivery is asynchronous (a microtask); drain the loop.
	if err := w.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalString(t, js, `r.got.join(",")`); got != "mark:m1,mark:m2" {
		t.Errorf("observer delivered %q, want mark:m1,mark:m2", got)
	}
}
