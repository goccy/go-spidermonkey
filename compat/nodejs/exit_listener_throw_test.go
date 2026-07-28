package nodejs_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
)

// Node's test suite judges most tests through assertions registered on 'exit'
// (common.mustCall). If a throw from an 'exit' listener were swallowed, every
// such test would report a false PASS — so the whole Node conformance baseline
// rests on this.
func TestExitListenerThrowIsReported(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{FS: fstest.MapFS{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	rt, err := nodejs.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer rt.Close()

	r, err := rt.RunScript(context.Background(), `
		let called = false;
		process.on("exit", () => {
			if (!called) throw new Error("mustCall: function was not called");
		});
	`)
	got := ""
	if err != nil {
		got = err.Error()
	} else if r.Error != nil {
		got = r.Error.Error()
	}
	if !strings.Contains(got, "mustCall") {
		t.Fatalf("a throwing 'exit' listener was not reported (got %q); every "+
			"mustCall-based Node test would falsely pass", got)
	}
}
