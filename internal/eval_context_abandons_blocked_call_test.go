package internal

import (
	"context"
	"testing"
	"time"
)

// A context deadline must bound Eval even when the guest is blocked in a HOST
// CALL. The engine's interrupt is only checked at bytecode loop heads, so it
// cannot reach a guest waiting on a host function — and Eval used to wait for
// that unwind unconditionally, which meant ctx bounded nothing and one blocked
// call stalled the caller forever. (That is what stopped a full Node.js suite
// run dead: a handful of tests block in a host op, and the run never finished.)
//
// The call is abandoned instead: the caller gets ctx.Err() promptly, the
// instance is marked spent, and every later call is refused rather than
// deadlocking on the instance lock the abandoned call still holds.
func TestEvalContextAbandonsHostBlockedCall(t *testing.T) {
	js, env := newJS(t)

	release := make(chan struct{})
	defer close(release)
	global, err := js.Global()
	if err != nil {
		t.Fatalf("Global: %v", err)
	}
	defer js.FreeObject(global)
	defineFunc(t, js, env, global, "blockForever", "blockForever", 0, func(args []Value) (Value, error) {
		<-release
		return Undefined(), nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := js.EvalContext(ctx, `blockForever()`); err == nil {
		t.Fatal("expected the deadline to be reported")
	}
	if elapsed := time.Since(start); elapsed > unwindGrace+5*time.Second {
		t.Fatalf("EvalContext took %v; the deadline did not bound it", elapsed)
	}
	if _, err := js.EvalContext(context.Background(), `1 + 1`); err == nil {
		t.Error("a spent instance still accepted an eval")
	}
	// Close must return rather than deadlock on the abandoned call's lock.
	done := make(chan struct{})
	go func() { _ = js.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close deadlocked on the abandoned call")
	}
}
