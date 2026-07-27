package spidermonkey_test

// Releasing an object handle twice must be harmless.
//
// A handle IS a pointer to the guest's GC root for that object, so a second
// release used to delete it twice and corrupt the guest heap. Nothing failed
// at the point of the double free: the damage only surfaced much later, when
// teardown walked the root list, as a wild-pointer fault inside
// JS_DestroyContext that looked like an engine bug. Host close paths hand
// ownership of handles around enough that this has to be safe at the
// primitive.

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func TestObjectFreeIsIdempotent(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()

	// Enough objects that a corrupted root list has something to trip over.
	for i := 0; i < 64; i++ {
		o, err := js.NewObject()
		if err != nil {
			t.Fatalf("NewObject: %v", err)
		}
		o.Set("k", spidermonkey.ValueOf(i))
		if err := o.Free(); err != nil {
			t.Fatalf("first Free: %v", err)
		}
		if err := o.Free(); err != nil {
			t.Fatalf("second Free: %v", err)
		}
	}

	// The interpreter must still be usable, and a GC must be able to walk the
	// roots — that is what a double free would have broken.
	if _, err := js.Eval(context.Background(), `globalThis.x = []; for (let i = 0; i < 2000; i++) x.push({i}); x.length`); err != nil {
		t.Fatalf("Eval after repeated frees: %v", err)
	}
}
