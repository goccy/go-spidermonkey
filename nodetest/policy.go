package nodetest

import (
	"regexp"
	"strings"
)

// This file holds the one judgement call the runner makes before running
// anything: whether a Node test is addressed to a *public API this embedding
// implements* or to something only the real node binary can answer (its own
// private internals, a respawn of itself, a V8 flag). Those are SKIPPED with a
// reason and accounted per reason, never silently passed and never counted as
// failures — the same policy the test262 run uses.
//
// The skip set is deliberately mechanical (source markers and the test's own
// `// Flags:` line) rather than a hand-maintained list of file names, so it
// cannot quietly go stale as the suite moves.

var flagsRE = regexp.MustCompile(`(?m)^// Flags:(.*)$`)

// flagsHonored are the command-line flags a test may ask for that this runtime
// either implements or that make no difference here. Anything outside this set
// means the test is configuring a node binary we are not, so it is skipped by
// name — as flags become supported, they move into this map and their tests
// start running automatically.
var flagsHonored = map[string]bool{
	"--no-warnings":                 true,
	"--no-deprecation":              true,
	"--trace-warnings":              true,
	"--trace-deprecation":           true,
	"--no-force-async-hooks-checks": true,
	"--test-udp-no-try-send":        true,
	"--expose-internals":            false, // explicit: the big one
}

// SkipReason returns why this embedding cannot host rel, or "" when it can.
// src is the test's source text.
func SkipReason(rel, src string) string {
	// A test that reaches into Node's private module graph is testing Node's
	// implementation, not the API this layer provides.
	if strings.Contains(src, "require('internal/") || strings.Contains(src, `require("internal/`) ||
		strings.Contains(src, "internalBinding(") {
		return "tests node-private internals"
	}
	// Respawning the node binary: this runtime is a library, not an executable,
	// so there is no process.execPath to re-exec. (A test that only *mentions*
	// execPath in a message is rare enough to be caught by the marker.)
	if strings.Contains(src, "process.execPath") {
		return "respawns the node binary"
	}
	// child_process.fork re-executes the node binary with a new script; there is
	// no binary to execute (the same reason process.execPath tests are skipped).
	if strings.Contains(src, ".fork(") || strings.Contains(src, "fork(__filename") {
		return "respawns the node binary"
	}
	// Native addons cannot exist in a wasm sandbox.
	if strings.Contains(src, ".node')") || strings.Contains(src, `.node")`) ||
		strings.Contains(src, "process.dlopen") {
		return "loads a native addon"
	}
	for _, m := range moduleSkips {
		if requiresModule(src, m.module) {
			return m.reason
		}
	}
	if fl := flagsRE.FindStringSubmatch(src); fl != nil {
		for _, f := range strings.Fields(fl[1]) {
			name := f
			if i := strings.IndexByte(name, '='); i >= 0 {
				name = name[:i]
			}
			if !flagsHonored[name] {
				return "needs node flag " + name
			}
		}
	}
	return ""
}

// moduleSkips lists core modules this embedding does not provide, with the
// reason. Each entry is a promise about scope, so it doubles as the roadmap:
// implementing the module deletes the entry and turns its tests on.
var moduleSkips = []struct{ module, reason string }{
	{"http2", "node:http2 not implemented"},
	{"inspector", "node:inspector is the V8 debugger protocol"},
	{"v8", "node:v8 exposes V8 internals"},
	{"repl", "node:repl needs an interactive TTY"},
	{"cluster", "node:cluster needs process forking"},
	{"trace_events", "node:trace_events is V8 tracing"},
	{"sqlite", "node:sqlite would embed a SQL engine in the guest"},
	{"wasi", "node:wasi would nest a second wasm runtime"},
	{"sea", "node:sea has no host analogue"},
	{"test", "node:test is a JS test runner; we test in Go"},
	{"domain", "node:domain is deprecated and stubbed"},
}

// requiresModule reports whether src requires the named core module, in any of
// the spellings the suite uses.
func requiresModule(src, name string) bool {
	for _, form := range []string{
		"require('" + name + "')", `require("` + name + `")`,
		"require('node:" + name + "')", `require("node:` + name + `")`,
		"from '" + name + "'", `from "` + name + `"`,
		"from 'node:" + name + "'", `from "node:` + name + `"`,
	} {
		if strings.Contains(src, form) {
			return true
		}
	}
	return false
}
