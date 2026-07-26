package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// process.memoryUsage()/cpuUsage()/resourceUsage() used to be all-zero stubs.
// They must now report real numbers from the Go host: heapUsed/heapTotal must
// be real and non-zero so leak-guards and ratios work.
func TestProcessMemoryUsageReal(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const m = process.memoryUsage();
		r.shape = ["rss","heapTotal","heapUsed","external","arrayBuffers"].every(k => typeof m[k] === "number");
		r.heapUsed = m.heapUsed > 0;
		r.heapTotal = m.heapTotal > 0;
		r.rss = m.rss > 0;
		r.ratio = Number.isFinite(m.heapUsed / m.heapTotal);
		r.rssFn = typeof process.memoryUsage.rss === "function" && process.memoryUsage.rss() > 0;
	`)
	for _, expr := range []string{"r.shape", "r.heapUsed", "r.heapTotal", "r.rss", "r.ratio", "r.rssFn"} {
		if got := evalStr(t, js, expr); got != "true" {
			t.Errorf("%s = %s, want true", expr, got)
		}
	}
}

// cpuUsage() returns cumulative microseconds; cpuUsage(prev) returns the delta.
func TestProcessCPUUsageDelta(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const a = process.cpuUsage();
		r.shape = typeof a.user === "number" && typeof a.system === "number";
		r.nonzero = a.user > 0 || a.system > 0;
		// A delta against a prior reading must be >= 0 (monotonic).
		const d = process.cpuUsage(a);
		r.deltaNonNeg = d.user >= 0 && d.system >= 0;
	`)
	for _, expr := range []string{"r.shape", "r.nonzero", "r.deltaNonNeg"} {
		if got := evalStr(t, js, expr); got != "true" {
			t.Errorf("%s = %s, want true", expr, got)
		}
	}
}

// resourceUsage() returns the ~16-field shape with real CPU/maxRSS fields.
func TestProcessResourceUsageShape(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const u = process.resourceUsage();
		const fields = ["userCPUTime","systemCPUTime","maxRSS","sharedMemorySize",
			"unsharedDataSize","unsharedStackSize","minorPageFault","majorPageFault",
			"swappedOut","fsRead","fsWrite","ipcSent","ipcReceived","signalsCount",
			"voluntaryContextSwitches","involuntaryContextSwitches"];
		r.allPresent = fields.every(k => typeof u[k] === "number");
		r.fieldCount = Object.keys(u).length;
		r.maxRSS = u.maxRSS > 0;
	`)
	for _, expr := range []string{"r.allPresent", "r.maxRSS"} {
		if got := evalStr(t, js, expr); got != "true" {
			t.Errorf("%s = %s, want true", expr, got)
		}
	}
	if got := evalStr(t, js, `r.fieldCount`); got != "16" {
		t.Errorf("resourceUsage field count = %s, want 16", got)
	}
}
