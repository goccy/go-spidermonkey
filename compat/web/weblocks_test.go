package web_test

// Web Locks is pure guest-side promise orchestration, so its tests drive the
// same scenarios the WPT files do — grant order, shared/exclusive mixing,
// steal's task-queue position — and read back a transcript.

import (
	"context"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

func runLocks(t *testing.T, script string) string {
	t.Helper()
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	w, err := web.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer w.Close()

	if r, err := js.Eval(context.Background(), `globalThis.__r = "?";`+script); err != nil {
		t.Fatalf("eval: %v", err)
	} else if r.Error != nil {
		t.Fatalf("threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	r, err := js.Eval(context.Background(), `String(globalThis.__r)`)
	if err != nil {
		t.Fatal(err)
	}
	return r.Value.String()
}

func TestWebLocksGrantOrderAndModes(t *testing.T) {
	got := runLocks(t, `
		(async () => {
			let unblock;
			const blocked = new Promise((r) => { unblock = r; });
			const granted = [];
			navigator.locks.request("a", { mode: "shared" }, async () => { granted.push("s1"); await blocked; });
			navigator.locks.request("a", { mode: "shared" }, async () => { granted.push("s2"); await blocked; });
			const exclusive = navigator.locks.request("a", async () => { granted.push("xa"); });
			await navigator.locks.request("b", () => { granted.push("xb"); });
			const before = granted.join(",");
			unblock();
			await exclusive;
			globalThis.__r = before + " | " + granted.join(",");
		})().catch((e) => { globalThis.__r = "THREW " + e; });`)
	if want := "s1,s2,xb | s1,s2,xb,xa"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWebLocksQuerySeesGrantsAndWaiters(t *testing.T) {
	got := runLocks(t, `
		(async () => {
			let release;
			const hold = new Promise((r) => { release = r; });
			const never = new Promise(() => {});
			const excl = navigator.locks.request("r", () => hold);
			for (let i = 0; i < 3; i++) {
				navigator.locks.request("r", { mode: "shared" }, () => never).catch(() => {});
			}
			const q1 = await navigator.locks.query();
			release();
			await excl;
			const q2 = await navigator.locks.query();
			globalThis.__r = q1.held.length + "/" + q1.pending.length + " then " +
				q2.held.length + "/" + q2.pending.length + " " + q2.held.every((l) => l.mode === "shared");
		})().catch((e) => { globalThis.__r = "THREW " + e; });`)
	if want := "1/3 then 3/0 true"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWebLocksStealTakesTheTaskQueuePath(t *testing.T) {
	got := runLocks(t, `
		(async () => {
			const events = [];
			const never = new Promise(() => {});
			// The victim is only REQUESTED here; it is granted a task later. The
			// steal must still preempt it, because steal travels the same queue
			// and therefore runs after the grant.
			const victim = navigator.locks.request("s", () => never)
				.catch((e) => { events.push("victim:" + e.name); });
			await navigator.locks.request("s", { steal: true }, () => { events.push("stole"); });
			await victim;
			globalThis.__r = events.join(",");
		})().catch((e) => { globalThis.__r = "THREW " + e; });`)
	if want := "stole,victim:AbortError"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWebLocksSynchronousAbortBeatsGrant(t *testing.T) {
	got := runLocks(t, `
		(async () => {
			const ac = new AbortController();
			let ran = false;
			const p = navigator.locks.request("q", { signal: ac.signal }, () => { ran = true; });
			ac.abort();
			const outcome = await p.then(() => "resolved", (e) => e.name);
			globalThis.__r = outcome + "," + ran;
		})().catch((e) => { globalThis.__r = "THREW " + e; });`)
	if want := "AbortError,false"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
