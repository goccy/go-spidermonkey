package internal

// What one agent costs in linear memory.
//
// This is the instance's single hard budget (Options.MaxMemory), and an agent
// spends it twice: on its JS runtime, which it genuinely uses, and on its
// thread stack, which is allocated up front whether or not the agent ever
// recurses. The stack was the larger of the two — a thread created without an
// explicit size takes the LINKED main-stack size, 8 MiB — so an idle agent
// that did nothing at all still cost more in stack than in engine. That is
// what capped an instance at ~17 agents and starved the GC helper threads
// (each one a thread too) at teardown.
//
// The ceiling is meant to be a function of what an agent USES, so this pins
// the per-agent cost. It measures a delta rather than an absolute, so it does
// not depend on the base image size.

import (
	"testing"
	"time"

	"github.com/goccy/spidermonkeywasm2go/base"
)

func TestAgentLinearMemoryFootprint(t *testing.T) {
	js, _ := newJS(t)
	memMiB := func() float64 {
		return float64(base.MemorySize(js.m.g)) * 65536 / (1 << 20)
	}
	if _, err := js.Eval(`1+1`); err != nil {
		t.Fatalf("Eval: %v", err)
	}

	const agents = 4
	before := memMiB()
	for i := 0; i < agents; i++ {
		// An agent with no source at all: it evaluates nothing and parks in
		// its pump. Whatever it costs is pure per-agent overhead.
		if _, err := js.AgentSpawn("", ""); err != nil {
			t.Fatalf("AgentSpawn %d: %v", i, err)
		}
	}
	// The cost is paid on the agent's own thread, so wait for it to land.
	deadline := time.Now().Add(10 * time.Second)
	var per float64
	for time.Now().Before(deadline) {
		per = (memMiB() - before) / agents
		if per > 0 {
			time.Sleep(300 * time.Millisecond)
			per = (memMiB() - before) / agents
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("linear memory: %.1f MiB before, %.1f MiB after %d agents (%.1f MiB each)",
		before, memMiB(), agents, per)

	// Generous: measured at ~7 MiB each, of which ~6 is the JS runtime. The
	// bound exists to catch a per-agent regression of several MiB — the shape
	// an inherited main-stack size has — not to pin the exact figure.
	const budgetMiB = 10
	if per > budgetMiB {
		t.Fatalf("each idle agent costs %.1f MiB of linear memory, over the %d MiB budget; "+
			"an agent thread is likely allocating a stack sized for the main thread",
			per, budgetMiB)
	}
}

// Agents that come and go must cost nothing CUMULATIVELY. Only one is ever
// alive here, so the footprint has to plateau at one agent's working set;
// anything proportional to the number of agents ever created is a leak.
//
// It used to grow by a whole agent every cycle. A finished thread holds its
// entire stack until it is joined, and nothing joined one until the instance
// closed — so an instance that retired workers over time grew without bound
// even while never running two at once.
func TestAgentChurnDoesNotAccumulate(t *testing.T) {
	js, _ := newJS(t)
	memMiB := func() float64 {
		return float64(base.MemorySize(js.m.g)) * 65536 / (1 << 20)
	}
	if _, err := js.Eval(`1+1`); err != nil {
		t.Fatalf("Eval: %v", err)
	}

	churn := func() {
		t.Helper()
		id, err := js.AgentSpawn("", "")
		if err != nil {
			t.Fatalf("AgentSpawn: %v", err)
		}
		if _, err := js.AgentInterrupt(id); err != nil {
			t.Fatalf("AgentInterrupt: %v", err)
		}
		time.Sleep(150 * time.Millisecond)
	}

	// The first cycles pay for one agent's working set; measure the plateau
	// from there, so the assertion is about accumulation, not startup.
	for i := 0; i < 4; i++ {
		churn()
	}
	settled := memMiB()
	const cycles = 16
	for i := 0; i < cycles; i++ {
		churn()
	}
	grew := memMiB() - settled
	t.Logf("linear memory: %.1f MiB settled, %.1f MiB after %d more spawn/exit cycles",
		settled, memMiB(), cycles)

	// A leak of a whole agent per cycle would be well over 100 MiB here; one
	// agent's working set is ~7 MiB. Allow one agent's worth of drift for
	// allocator fragmentation and still catch anything proportional.
	const driftMiB = 8
	if grew > driftMiB {
		t.Fatalf("%d spawn/exit cycles with never more than one agent alive grew linear "+
			"memory by %.1f MiB (%.2f MiB per cycle); agents are accumulating",
			cycles, grew, grew/cycles)
	}
}
