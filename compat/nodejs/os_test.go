package nodejs_test

import (
	"runtime"
	"strconv"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// os.cpus() used to return [] and os.totalmem()/freemem() 0, breaking apps that
// size worker pools from os.cpus().length or compute freemem()/totalmem()
// ratios (NaN). They must now report real, non-zero values from the host.
func TestOSCPUsAndMemoryAreReal(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const os = require("os");
		globalThis.r = {};
		const cpus = os.cpus();
		r.isArray = Array.isArray(cpus);
		r.len = cpus.length;
		const c0 = cpus[0] || {};
		r.hasModel = typeof c0.model === "string" && c0.model.length > 0;
		r.hasSpeed = typeof c0.speed === "number" && c0.speed > 0;
		r.hasTimes = c0.times && ["user","nice","sys","idle","irq"].every(k => typeof c0.times[k] === "number");
		r.total = os.totalmem();
		r.free = os.freemem();
		r.ratioFinite = Number.isFinite(r.free / r.total);
		r.parallel = os.availableParallelism();
	`)
	wantCPU := runtime.NumCPU()
	if wantCPU < 1 {
		wantCPU = 1
	}
	if got := evalStr(t, js, `r.isArray`); got != "true" {
		t.Errorf("os.cpus() is not an array")
	}
	if got := evalStr(t, js, `r.len`); got != strconv.Itoa(wantCPU) {
		t.Errorf("os.cpus().length = %s, want %d (runtime.NumCPU)", got, wantCPU)
	}
	for _, expr := range []string{"r.hasModel", "r.hasSpeed", "r.hasTimes", "r.ratioFinite"} {
		if got := evalStr(t, js, expr); got != "true" {
			t.Errorf("%s = %s, want true", expr, got)
		}
	}
	if got := evalStr(t, js, `r.parallel`); got != strconv.Itoa(wantCPU) {
		t.Errorf("availableParallelism = %s, want %d", got, wantCPU)
	}
	if got := evalStr(t, js, `r.total > 0`); got != "true" {
		t.Errorf("totalmem not > 0")
	}
	if got := evalStr(t, js, `r.free > 0`); got != "true" {
		t.Errorf("freemem not > 0")
	}
}

// The configured MaxMemoryBytes ceiling is what os.totalmem() reports.
func TestOSTotalmemFollowsMemoryCap(t *testing.T) {
	const cap = 128 << 20
	js, rt := newRuntime(t, spidermonkey.Config{MaxMemoryBytes: cap})
	runScript(t, rt, `globalThis.total = require("os").totalmem();`)
	// Compare in JS to avoid Go float formatting differences (scientific notation).
	if got := evalStr(t, js, `total === `+strconv.Itoa(cap)); got != "true" {
		if raw := evalStr(t, js, `String(total)`); raw != strconv.Itoa(cap) {
			t.Errorf("totalmem = %s, want %d (the configured cap)", raw, cap)
		}
	}
}

// networkInterfaces()/userInfo()/loadavg() must return correctly-shaped values,
// not crash or return broken shapes.
func TestOSAuxiliaryShapes(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const os = require("os");
		globalThis.r = {};
		const ni = os.networkInterfaces();
		r.niObject = ni && typeof ni === "object";
		r.loHasV4 = Array.isArray(ni.lo) && ni.lo.some(a => a.family === "IPv4" && a.address === "127.0.0.1");
		const u = os.userInfo();
		r.userOk = typeof u.username === "string" && typeof u.uid === "number" && typeof u.homedir === "string";
		const la = os.loadavg();
		r.loadOk = Array.isArray(la) && la.length === 3 && la.every(n => typeof n === "number");
	`)
	for _, expr := range []string{"r.niObject", "r.loHasV4", "r.userOk", "r.loadOk"} {
		if got := evalStr(t, js, expr); got != "true" {
			t.Errorf("%s = %s, want true", expr, got)
		}
	}
}
