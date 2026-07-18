package spidermonkey_test

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func TestObjectNavigation(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	g := js.Global()

	// A built-in global object is reachable and reports as an object.
	jsonObj, err := g.Get("JSON")
	if err != nil {
		t.Fatalf("Get(JSON): %v", err)
	}
	if !jsonObj.IsObject() {
		t.Errorf("JSON should be an object")
	}

	// Create an object, hang it off the global, read it back, and confirm from
	// the guest side.
	obj, err := js.NewObject()
	if err != nil {
		t.Fatalf("NewObject: %v", err)
	}
	if err := g.Set("host", obj); err != nil {
		t.Fatalf("Set(host): %v", err)
	}
	got, err := g.Get("host")
	if err != nil {
		t.Fatalf("Get(host): %v", err)
	}
	if !got.IsObject() {
		t.Errorf("host should be an object")
	}
	if r, _ := js.Eval(context.Background(), `typeof globalThis.host`); r.Value.String() != "object" {
		t.Errorf("typeof host = %q, want \"object\"", r.Value.String())
	}
}
