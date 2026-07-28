package nodejs_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
)

// A 'beforeExit' listener that throws must surface as a JavaScript error from
// RunScript. It used to crash the HOST: Wait consulted the completion value of
// the eval that fires the event without checking whether that eval threw, and a
// thrown script has no completion value, so the read was a nil-interface
// dereference — a guest exception taking the process down with it.
func TestBeforeExitListenerThrows(t *testing.T) {
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
		process.on("beforeExit", () => { throw new Error("boom"); });
	`)
	if r.Error == nil && err == nil {
		t.Fatal("a throwing beforeExit listener reported success")
	}
	got := ""
	if err != nil {
		got = err.Error()
	} else {
		got = r.Error.Error()
	}
	if !strings.Contains(got, "boom") {
		t.Errorf("error %q does not mention the thrown message", got)
	}
}
