package spidermonkey_test

// Forceful agent termination: Agents.Interrupt stops an agent that no message
// can reach.
//
// Everything else in the agent surface is a MESSAGE, and an agent only reads
// messages between job-queue drains. A guest that never drains — the runaway
// `while(true){}` a Worker terminate has to be able to stop — is therefore
// unreachable by any Send, Broadcast or Wake. Interrupt goes through the
// agent's own engine context instead.

import (
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func TestAgentInterruptStopsSynchronousInfiniteLoop(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()

	agents := js.Agents()
	// Posts once so the test knows the agent is really running, then spins
	// forever without ever returning to its job queue.
	id, err := agents.Spawn("", `__agent__.post("running"); while (true) {}`)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if got := receiveOne(t, agents); got.String() != "running" {
		t.Fatalf("agent post = %q, want %q", got.String(), "running")
	}

	signalled, err := agents.Interrupt(id)
	if err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if !signalled {
		t.Fatal("Interrupt reported no such agent, but it had just posted")
	}
	waitAgentsGone(t, agents)
}

// The termination is UNCATCHABLE: a guest cannot keep itself alive by wrapping
// its loop in try/catch, which is the whole point of routing through the
// engine's interrupt rather than throwing an ordinary exception.
func TestAgentInterruptIsUncatchable(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()

	agents := js.Agents()
	id, err := agents.Spawn("", `
		__agent__.post("running");
		for (;;) {
			try { while (true) {} } catch (e) { /* swallow and keep going */ }
		}
	`)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if got := receiveOne(t, agents); got.String() != "running" {
		t.Fatalf("agent post = %q, want %q", got.String(), "running")
	}

	if _, err := agents.Interrupt(id); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	waitAgentsGone(t, agents)
}

// An idle agent parks on the event futex rather than in the engine, so nothing
// is executing for an interrupt to land on. It still has to stop.
func TestAgentInterruptStopsIdleAgent(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()

	agents := js.Agents()
	id, err := agents.Spawn("", `__agent__.post("running");`)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if got := receiveOne(t, agents); got.String() != "running" {
		t.Fatalf("agent post = %q, want %q", got.String(), "running")
	}

	if _, err := agents.Interrupt(id); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	waitAgentsGone(t, agents)
}

// Interrupting the instant after Spawn returns, before the agent's thread has
// finished starting up. This is the realistic case — a caller aborting a
// worker it has just created — and the one with a race to lose: at that moment
// the agent has an id but no engine context yet, so there is nothing to
// request an interrupt on. Dropping it left the agent running forever.
func TestAgentInterruptImmediatelyAfterSpawn(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()

	agents := js.Agents()
	// Repeat: the window is small, so one attempt could pass by luck.
	for i := 0; i < 8; i++ {
		id, err := agents.Spawn("", `while (true) {}`)
		if err != nil {
			t.Fatalf("Spawn %d: %v", i, err)
		}
		signalled, err := agents.Interrupt(id)
		if err != nil {
			t.Fatalf("Interrupt %d: %v", i, err)
		}
		if !signalled {
			t.Fatalf("Interrupt %d reported no such agent for an id Spawn had just returned", i)
		}
	}
	waitAgentsGone(t, agents)
}

// Interrupting an id that never existed is not an error: the caller's goal —
// that agent is not running — already holds.
func TestAgentInterruptUnknownAgent(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()

	signalled, err := js.Agents().Interrupt(spidermonkey.AgentID(4242))
	if err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if signalled {
		t.Fatal("Interrupt reported signalling an agent that never existed")
	}
}

// Terminating one agent must leave its siblings running: the interrupt is
// per-agent, not a cluster-wide shutdown.
func TestAgentInterruptLeavesSiblingsRunning(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()

	agents := js.Agents()
	doomed, err := agents.Spawn("", `__agent__.post("doomed"); while (true) {}`)
	if err != nil {
		t.Fatalf("Spawn doomed: %v", err)
	}
	if got := receiveOne(t, agents); got.String() != "doomed" {
		t.Fatalf("first post = %q, want %q", got.String(), "doomed")
	}

	// The survivor waits on its inbox, so it is still alive after the interrupt
	// and can prove it by answering afterwards.
	survivor, err := agents.Spawn("", `
		var m = __agent__.recv();
		__agent__.post("survivor:" + m);
		__agent__.leaving();
	`)
	if err != nil {
		t.Fatalf("Spawn survivor: %v", err)
	}

	if _, err := agents.Interrupt(doomed); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for agents.IsAlive(doomed) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if agents.IsAlive(doomed) {
		t.Fatal("the interrupted agent is still alive")
	}
	if !agents.IsAlive(survivor) {
		t.Fatal("interrupting one agent also stopped its sibling")
	}

	if err := agents.Send(survivor, spidermonkey.ValueOf("ping")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := receiveOne(t, agents); got.String() != "survivor:ping" {
		t.Fatalf("survivor post = %q, want %q", got.String(), "survivor:ping")
	}
}
