// Package eventloop provides the macro-task loop the compat packages share:
// host-side timers (setTimeout/setInterval), completions posted from other
// goroutines (async ops like fetch), and the microtask drain between tasks.
//
// One Loop owns one *spidermonkey.JS. All calls into the guest happen on the
// goroutine running Run; other goroutines interact with the loop only through
// Post and the pending counter. Host functions invoked DURING guest execution
// (setTimeout registering a timer) may call SetTimer/ClearTimer directly —
// they only mutate Go-side state.
package eventloop

import (
	"context"
	"sync"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// Loop is the shared macro-task loop. Create with New; drive with Run.
type Loop struct {
	js *spidermonkey.JS

	mu         sync.Mutex
	nextID     int64
	timers     map[int64]*timer
	dueBatch   []*timer // timers taken for the current turn, still cancellable
	immediates []*immediate
	batch      []*immediate // check-phase snapshot currently executing
	posts      []func() error
	pending    int
	// unrefPending is the number of AddPending handles that have been unref'd
	// (server.unref() etc.). Unref'd work still runs, but it does not by itself
	// keep the loop alive: the idle check uses pending-unrefPending. Kept
	// balanced by Ref/Unref and rebalanced by the handle's owner on close.
	unrefPending int

	// microDrain replaces the engine-only microtask drain when set
	// (compat/nodejs interleaves the process.nextTick queue here).
	microDrain func(context.Context) error

	wake chan struct{}
}

type immediate struct {
	id      int64
	fn      *spidermonkey.Object
	cleared bool
}

// freeFn releases the callback handle exactly once; a second call is a no-op.
// Every free site goes through this so a handle taken into dueBatch/the check
// batch can't be double-freed (which, if the engine recycles handle ids, would
// unpin an unrelated live object). All frees happen on the loop goroutine.
func (im *immediate) freeFn() {
	if im.fn != nil {
		im.fn.Free()
		im.fn = nil
	}
}

type timer struct {
	id       int64
	due      time.Time
	interval time.Duration // 0 for one-shot
	fn       *spidermonkey.Object
	running  bool // callback currently executing (the loop owns the free)
	cleared  bool
	unref    bool // does not keep the loop alive, but still fires if it stays alive
}

func (t *timer) freeFn() {
	if t.fn != nil {
		t.fn.Free()
		t.fn = nil
	}
}

// New creates a loop bound to js.
func New(js *spidermonkey.JS) *Loop {
	return &Loop{js: js, timers: map[int64]*timer{}, wake: make(chan struct{}, 1)}
}

// SetTimer schedules fn after delay; repeat re-arms it every delay. The
// returned id cancels it via ClearTimer. The loop owns fn's handle and frees
// it when the timer is cleared or (for one-shots) has fired.
func (l *Loop) SetTimer(fn *spidermonkey.Object, delay time.Duration, repeat bool) int64 {
	if delay < 0 {
		delay = 0
	}
	// A repeating timer needs a positive interval, else takeDue treats it as a
	// one-shot (fires once, then uncancellable). Node clamps intervals to 1ms.
	if repeat && delay < time.Millisecond {
		delay = time.Millisecond
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextID++
	t := &timer{id: l.nextID, due: time.Now().Add(delay), fn: fn}
	if repeat {
		t.interval = delay
	}
	l.timers[t.id] = t
	l.wakeup()
	return t.id
}

// ClearTimer cancels the timer; unknown ids are ignored (a cleared or fired
// timer's id clears as a no-op, like clearTimeout). A timer clearing itself
// from its own callback is safe: the loop frees the callback handle exactly
// once, after the call returns.
func (l *Loop) ClearTimer(id int64) {
	l.mu.Lock()
	t, ok := l.timers[id]
	if ok {
		t.cleared = true
		delete(l.timers, id)
	} else {
		// The timer may be in the batch already taken for this turn (a sibling
		// due in the same tick, deleted from l.timers by takeDue). Node lets an
		// earlier same-tick callback cancel it, so mark it and the run loop
		// skips it before calling.
		for _, dt := range l.dueBatch {
			if dt.id == id {
				dt.cleared = true
				t, ok = dt, true
				break
			}
		}
	}
	// A timer taken for this turn has running=true, so the run loop (not us)
	// owns its Free; only free a not-yet-running timer here. freeFn is
	// idempotent, so even if the run loop already fired-and-freed this id
	// (it lingers in dueBatch), this is a safe no-op rather than a double-free.
	free := ok && !t.running
	l.mu.Unlock()
	if free {
		t.freeFn()
	}
}

// SetMicroDrain replaces the between-callback microtask drain. The default
// pumps only the engine job queue; compat/nodejs installs a hook that
// interleaves the process.nextTick queue with it. fn should call
// DrainEngineJobs itself. Not safe to call concurrently with Run.
func (l *Loop) SetMicroDrain(fn func(context.Context) error) { l.microDrain = fn }

// DrainEngineJobs pumps the engine's own job queue to exhaustion — the
// building block a SetMicroDrain hook composes with its host-side queues.
func (l *Loop) DrainEngineJobs(ctx context.Context) error { return l.drainJobs(ctx) }

// PostImmediate schedules fn for the check phase of the current loop turn —
// after due timers, before sleeping (setImmediate). The loop owns fn's
// handle. The returned id cancels it via ClearImmediate.
func (l *Loop) PostImmediate(fn *spidermonkey.Object) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextID++
	l.immediates = append(l.immediates, &immediate{id: l.nextID, fn: fn})
	l.wakeup()
	return l.nextID
}

// ClearImmediate cancels a PostImmediate; unknown ids are ignored. It only
// marks the entry — the loop's check phase frees the handle exactly once,
// which keeps clearing an immediate from another immediate's callback safe.
func (l *Loop) ClearImmediate(id int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, list := range [][]*immediate{l.immediates, l.batch} {
		for _, im := range list {
			if im.id == id {
				im.cleared = true
				return
			}
		}
	}
}

// Reset clears every armed timer, immediate, queued post and the pending
// counter, freeing all held callback handles. A pooled instance (cfworkers)
// calls this between requests so one request's un-awaited work — a leftover
// setTimeout, an unresolved pending op — can never fire during the next
// request on the same instance (a cross-request state leak). Call only when the
// loop goroutine is idle (Run has returned), so nothing is mid-callback.
func (l *Loop) Reset() {
	// Run any leftover engine jobs (queued promise reactions) now, on THIS
	// instance's teardown, so a reaction one request left behind can't execute
	// during the next request on the same instance — the cross-request leak the
	// Go-side clears below don't cover on their own. Bounded by the engine
	// draining to quiescence; errors are irrelevant during teardown.
	_ = l.drainJobs(context.Background())
	l.mu.Lock()
	timers := l.timers
	l.timers = map[int64]*timer{}
	imms := l.immediates
	l.immediates = nil
	batch := l.batch
	due := l.dueBatch
	l.dueBatch = nil
	l.batch = nil
	l.posts = nil
	l.pending = 0
	l.unrefPending = 0
	l.mu.Unlock()
	// The loop is idle here (Run has returned), so nothing is mid-callback and
	// every handle is safe to free. freeFn is idempotent, so timers that also
	// sit in dueBatch, or immediates in both immediates and batch, free once.
	for _, t := range timers {
		t.freeFn()
	}
	for _, t := range due {
		t.freeFn()
	}
	for _, im := range imms {
		im.freeFn()
	}
	for _, im := range batch {
		im.freeFn()
	}
}

// Post queues f to run on the loop goroutine — the only safe way for another
// goroutine (an async op's completion) to touch the guest. Safe to call
// concurrently.
func (l *Loop) Post(f func() error) {
	l.mu.Lock()
	l.posts = append(l.posts, f)
	l.mu.Unlock()
	l.wakeup()
}

// AddPending marks an in-flight async op: the loop stays alive until a
// matching DonePending, even with no timers due. Safe to call concurrently.
func (l *Loop) AddPending() {
	l.mu.Lock()
	l.pending++
	l.mu.Unlock()
}

// DonePending releases an AddPending.
func (l *Loop) DonePending() {
	l.mu.Lock()
	l.pending--
	l.mu.Unlock()
	l.wakeup()
}

// Unref marks one AddPending handle as not keeping the loop alive (Node's
// handle.unref()). It does not release the pending — the op still completes and
// its callbacks still fire — it only stops that handle from, on its own, keeping
// the process from exiting. Balanced by Ref. The handle's owner must Ref again
// on close if it was unref'd, so unrefPending never outlives its pending.
func (l *Loop) Unref() {
	l.mu.Lock()
	l.unrefPending++
	l.mu.Unlock()
	l.wakeup()
}

// Ref undoes an Unref.
func (l *Loop) Ref() {
	l.mu.Lock()
	if l.unrefPending > 0 {
		l.unrefPending--
	}
	l.mu.Unlock()
}

// SetTimerRef sets whether a timer keeps the loop alive. An unref'd timer still
// fires while the loop runs for other reasons, but a loop with ONLY unref'd
// timers left is idle and exits (Node's timer.unref()). Unknown ids are ignored.
func (l *Loop) SetTimerRef(id int64, ref bool) {
	l.mu.Lock()
	if t, ok := l.timers[id]; ok {
		t.unref = !ref
	} else {
		for _, dt := range l.dueBatch {
			if dt.id == id {
				dt.unref = !ref
				break
			}
		}
	}
	l.mu.Unlock()
	l.wakeup()
}

// Run processes timers, posts and microtasks until the loop is idle — no
// timers armed, no pending ops, nothing posted — or ctx is done. A JS
// exception thrown by a timer callback (or an error from a posted completion)
// stops the loop and is returned, mirroring Node's uncaught-exception
// behavior.
func (l *Loop) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Drain microtasks left over from work that ran before (or between)
		// loop turns — an Eval or host Call may have queued promise jobs the
		// engine has not pumped yet. Without this, a loop with no timers or
		// posts would declare idle over an unpumped job queue.
		if err := l.drainMicro(ctx); err != nil {
			return err
		}

		// Due timers first (Node's timers phase), in due order; microtasks
		// drain after each callback. Timers scheduled DURING this turn's later
		// phases are not yet in l.timers, so they wait for the next turn — which
		// is what makes setImmediate (check phase, below) beat setTimeout(0)
		// scheduled from the same I/O (post) callback, matching Node.
		dueTimers := l.takeDue(time.Now())
		for _, t := range dueTimers {
			l.mu.Lock()
			skip := t.cleared // cancelled by an earlier sibling this same tick
			l.mu.Unlock()
			if skip {
				t.freeFn()
				continue
			}
			_, err := t.fn.Call()
			l.mu.Lock()
			t.running = false
			free := t.interval == 0 || t.cleared
			l.mu.Unlock()
			if free {
				t.freeFn()
			}
			if err != nil {
				return err
			}
			if err := l.drainMicro(ctx); err != nil {
				return err
			}
		}
		l.mu.Lock()
		l.dueBatch = nil
		l.mu.Unlock()

		// Posted completions (the poll phase): finished async work whose
		// callbacks unblock the guest.
		for {
			l.mu.Lock()
			if len(l.posts) == 0 {
				l.mu.Unlock()
				break
			}
			f := l.posts[0]
			l.posts = l.posts[1:]
			l.mu.Unlock()
			if err := f(); err != nil {
				return err
			}
			if err := l.drainMicro(ctx); err != nil {
				return err
			}
		}

		// Check phase: run the immediates queued when this turn started;
		// ones their callbacks queue run next turn (setImmediate semantics).
		l.mu.Lock()
		batch := l.immediates
		l.immediates = nil
		l.batch = batch
		l.mu.Unlock()
		for _, im := range batch {
			l.mu.Lock()
			skip := im.cleared
			l.mu.Unlock()
			if skip {
				im.freeFn()
				continue
			}
			_, err := im.fn.Call()
			im.freeFn()
			if err != nil {
				return err
			}
			if err := l.drainMicro(ctx); err != nil {
				return err
			}
		}
		l.mu.Lock()
		l.batch = nil
		l.mu.Unlock()

		l.mu.Lock()
		// next is the earliest of ALL armed timers (so unref'd timers still fire
		// on time while the loop is alive); hasRefTimer tracks whether any armed
		// timer keeps the loop alive. A loop with only unref'd timers (and no
		// other ref'd work) is idle and exits without firing them, like Node.
		var next time.Time
		hasRefTimer := false
		for _, t := range l.timers {
			if next.IsZero() || t.due.Before(next) {
				next = t.due
			}
			if !t.unref {
				hasRefTimer = true
			}
		}
		activePending := l.pending - l.unrefPending
		idle := !hasRefTimer && activePending <= 0 && len(l.posts) == 0 && len(l.immediates) == 0
		l.mu.Unlock()
		if idle {
			return nil
		}

		var due <-chan time.Time
		var tm *time.Timer
		if !next.IsZero() {
			tm = time.NewTimer(time.Until(next))
			due = tm.C
		}
		select {
		case <-ctx.Done():
			if tm != nil {
				tm.Stop()
			}
			return ctx.Err()
		case <-l.wake:
		case <-due:
		}
		if tm != nil {
			tm.Stop()
		}
	}
}

// takeDue removes and returns the timers due at now, earliest first;
// repeating timers are re-armed in place.
func (l *Loop) takeDue(now time.Time) []*timer {
	l.mu.Lock()
	defer l.mu.Unlock()
	var due []*timer
	for _, t := range l.timers {
		if !t.due.After(now) {
			due = append(due, t)
		}
	}
	// Insertion sort by due time, tie-breaking on id so timers armed in the
	// same tick fire in registration order (Node's guarantee) rather than the
	// random map-iteration order.
	less := func(a, b *timer) bool {
		if a.due.Equal(b.due) {
			return a.id < b.id
		}
		return a.due.Before(b.due)
	}
	for i := 1; i < len(due); i++ {
		for j := i; j > 0 && less(due[j], due[j-1]); j-- {
			due[j], due[j-1] = due[j-1], due[j]
		}
	}
	for _, t := range due {
		t.running = true
		if t.interval > 0 {
			t.due = now.Add(t.interval)
		} else {
			delete(l.timers, t.id)
		}
	}
	// Keep the batch visible to ClearTimer so a same-tick sibling can cancel a
	// timer already removed from l.timers.
	l.dueBatch = due
	return due
}

// drainMicro is the between-callback microtask checkpoint: the installed
// hook when set, the engine job queue otherwise.
func (l *Loop) drainMicro(ctx context.Context) error {
	if l.microDrain != nil {
		return l.microDrain(ctx)
	}
	return l.drainJobs(ctx)
}

func (l *Loop) drainJobs(ctx context.Context) error {
	for _, err := range l.js.RunJobs(ctx) {
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (l *Loop) wakeup() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}
