package spidermonkey_test

import (
	"context"
	"strings"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// A guest call that ABORTS — as opposed to raising a JavaScript "out of memory",
// which the engine does gracefully — leaves its heap inconsistent, and the
// instance must then be closable without taking the host process down.
//
// It used to SIGSEGV: Close called the guest's teardown (JS_DestroyContext),
// which walked a heap whose allocator had already aborted. Marshalling a large
// argument for a host call is the reachable route to that abort — the buffer is
// malloc'd outside the JS heap, so the cap is hit where there is no JS exception
// to raise. This is how it was found: WPT's console-log-large-array test formats
// a ten-million-element array, and the whole run died on the host side.
func TestCloseAfterGuestTrap(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{MaxMemoryBytes: 256 << 20})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := js.Global().DefineFunc("sink", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		return spidermonkey.ValueOf(len(args[0].String())), nil
	}); err != nil {
		t.Fatalf("DefineFunc: %v", err)
	}

	// 60 MB of argument against a 256 MB cap: the guest-side buffer for the host
	// call cannot be allocated and the guest aborts.
	_, err = js.Eval(context.Background(), `sink("x".repeat(60000000))`)
	if err == nil {
		t.Skip("the guest did not abort on this build; nothing to close-after-trap")
	}
	if !strings.Contains(err.Error(), "trap") {
		t.Fatalf("expected a wasm trap, got %v", err)
	}

	// Every further call must be refused rather than re-entering a broken guest.
	if _, err := js.Eval(context.Background(), `1 + 1`); err == nil {
		t.Error("a spent instance still accepted an eval")
	}
	// And Close must return, not fault.
	if err := js.Close(); err != nil {
		t.Logf("Close reported: %v", err)
	}
}
