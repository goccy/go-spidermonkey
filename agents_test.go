package spidermonkey_test

// The agent primitives end to end: Spawn runs source on its own thread (a
// goroutine), post/Receive carries structured clones back to the host, and
// Broadcast shares a SharedArrayBuffer's MEMORY (not a copy) with every agent.

import (
	"context"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// receiveOne polls the cluster until an agent posts, with a deadline.
func receiveOne(t *testing.T, a *spidermonkey.Agents) spidermonkey.Value {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, v, ok, err := a.Receive()
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		if ok {
			return v
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no agent post within the deadline")
	return nil
}

func waitAgentsGone(t *testing.T, a *spidermonkey.Agents) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if a.Alive() == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("agents still alive: %d", a.Alive())
}

func TestAgentSpawnPostReceive(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	agents := js.Agents()
	if _, err := agents.Spawn("", `__agent__.post(21 * 2); __agent__.leaving();`); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if v := receiveOne(t, agents); v.Int() != 42 {
		t.Errorf("posted value = %v, want 42", v.Export())
	}
	waitAgentsGone(t, agents)
}

func TestAgentBroadcastSharesSABMemory(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	agents := js.Agents()

	// The agent blocks in receive until the host broadcasts, then writes into
	// the shared memory and reports back.
	_, err := agents.Spawn("", `
		__agent__.receive(function (sab) {
			Atomics.store(new Int32Array(sab), 0, 42);
			__agent__.post("stored");
			__agent__.leaving();
		});
	`)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Create the SAB on the main runtime and broadcast it.
	r, err := js.Eval(context.Background(), `globalThis.sab = new SharedArrayBuffer(4); sab`)
	if err != nil || r.Error != nil {
		t.Fatalf("Eval(sab): %v / %v", err, r.Error)
	}
	sab := r.Value.Object()
	if sab == nil {
		t.Fatalf("completion value is not the SAB object")
	}
	defer sab.Free()
	if err := agents.Broadcast(sab); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}

	if v := receiveOne(t, agents); v.String() != "stored" {
		t.Fatalf("report = %q, want \"stored\"", v.String())
	}

	// The agent's write is visible through the MAIN runtime's SAB: the clone
	// shared the memory, it did not copy it.
	got, err := js.Eval(context.Background(), `Atomics.load(new Int32Array(sab), 0)`)
	if err != nil || got.Error != nil {
		t.Fatalf("Eval(load): %v / %v", err, got.Error)
	}
	if got.Value.Int() != 42 {
		t.Errorf("main runtime sees sab[0] = %v, want 42 (memory must be shared)", got.Value.Int())
	}
	waitAgentsGone(t, agents)
}

func TestAgentLateReceiverGetsLatchedBroadcast(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	agents := js.Agents()
	// Broadcast BEFORE the agent exists: the latch must still deliver.
	if err := agents.Broadcast(spidermonkey.ValueOf("early")); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	_, err := agents.Spawn("", `
		__agent__.receive(function (v) {
			__agent__.post("got:" + v);
			__agent__.leaving();
		});
	`)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if v := receiveOne(t, agents); v.String() != "got:early" {
		t.Errorf("late receiver got %q, want \"got:early\"", v.String())
	}
	waitAgentsGone(t, agents)
}

func TestCloseReleasesBlockedAgent(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})

	agents := js.Agents()
	// This agent blocks in receive forever; Close must release it (it unwinds
	// with an error) and join its thread rather than hang.
	if _, err := agents.Spawn("", `__agent__.receive(function () {});`); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- js.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close hung on an agent blocked in receive")
	}
}
