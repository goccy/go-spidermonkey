package nodejs_test

// Closing a Runtime twice must be harmless.
//
// It is a reachable shape, not an abuse: an embedder that closes explicitly
// still has its deferred close run. It used to release the web layer's cached
// engine handles a second time, deleting the guest's GC roots for them twice
// — which corrupted the guest heap silently and surfaced later as a
// wild-pointer fault inside JS_DestroyContext, at whatever unrelated test
// happened to tear down next.

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func TestRuntimeCloseTwice(t *testing.T) {
	_, rt := newRuntime(t, spidermonkey.Config{})

	// Give the instance a heap worth walking at teardown.
	runScript(t, rt, `
		globalThis.keep = [];
		for (let i = 0; i < 500; i++) keep.push({ i, s: "x".repeat(64) });
		Promise.resolve().then(() => {});
	`)

	if err := rt.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// The test cleanup closes a third time, then closes the interpreter — the
	// point at which a corrupted root list would fault.
}
