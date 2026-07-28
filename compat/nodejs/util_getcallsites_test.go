package nodejs_test

import (
	"testing"
	"testing/fstest"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// util.getCallSites (Node >= 22) reports the current call stack as structured
// frames. Node's own test harness destructures it from node:util, and tracing
// libraries use it; without it those tests fail with "getCallSites is not a
// function" before reaching what they meant to check.
func TestUtilGetCallSites(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{FS: fstest.MapFS{}})
	runScript(t, rt, `
		const { getCallSites } = require("util");
		function inner() { return getCallSites(); }
		function outer() { return inner(); }
		const sites = outer();
		globalThis.r = {
			n: sites.length,
			first: sites[0] && sites[0].functionName,
			second: sites[1] && sites[1].functionName,
			hasLine: !!(sites[0] && Number.isInteger(sites[0].lineNumber)),
			hasCol: !!(sites[0] && Number.isInteger(sites[0].columnNumber)),
			limited: getCallSites(1).length,
		};
	`)
	if got := evalStr(t, js, `String(r.first)`); got != "inner" {
		t.Errorf("first frame = %q, want \"inner\" (this function's own frame must be dropped)", got)
	}
	if got := evalStr(t, js, `String(r.second)`); got != "outer" {
		t.Errorf("second frame = %q, want \"outer\"", got)
	}
	if !evalVal(t, js, `r.hasLine && r.hasCol`).Bool() {
		t.Error("frames carry no line/column")
	}
	if got := evalStr(t, js, `String(r.limited)`); got != "1" {
		t.Errorf("getCallSites(1) returned %s frames, want 1", got)
	}
}
