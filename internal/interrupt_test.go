package internal

// Proves EvalContext aborts a runaway script when its context times out — the
// interrupt path the public API drives. The interrupter itself is unexported;
// this exercises it only through EvalContext.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEvalContextInterruptsRunawayScript(t *testing.T) {
	js, _ := newJS(t)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	raw, err := js.EvalContext(ctx, `while (true) {}`)
	elapsed := time.Since(start)

	// EvalContext returns the context error once the interrupt unwinds the loop.
	if err != context.DeadlineExceeded {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("EvalContext took %v; the interrupt did not stop the loop", elapsed)
	}
	// The unwound eval reports the interrupt in its envelope.
	var r evalResult
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("undecodable envelope %q: %v", raw, err)
	}
	if r.Ok {
		t.Errorf("expected the interrupted script to fail, got Ok=true")
	}
	if !strings.Contains(strings.ToLower(r.Error), "interrupt") {
		t.Errorf("error = %q, want it to mention the interrupt", r.Error)
	}
}
