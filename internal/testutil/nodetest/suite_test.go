// This file runs the official Node.js test suite (nodejs/node, checked out
// under ./suite by `make nodetest`) against the compat/nodejs runtime.
//
// The run is opt-in because it takes minutes, not seconds:
//
//	NODETEST=1 go test ./nodetest/ -run TestNodeSuite -v -timeout 3h
//
// Knobs: NODETEST_DIRS (comma-separated suite directories, default
// test/parallel,test/es-module), NODETEST_FILTER (substring), NODETEST_WORKERS,
// NODETEST_TIMEOUT (per test), NODETEST_UPDATE=1 (regenerate expectations.json),
// NODETEST_REPORT=path (dump failures as JSON).
//
// Each test runs in a fresh interpreter with the real suite mounted read-only
// and every write absorbed by an in-memory overlay, entered exactly as Node
// enters it, so `require('../common')` and the mustCall assertions that
// common registers on 'exit' judge the run. Tests addressed to something only
// the node binary can answer — its private internals, a respawn of itself, a
// V8 flag, an unimplemented core module — are SKIPPED with a reason and
// accounted per reason (see policy.go), never counted as passes.
//
// expectations.json lists the tests known to fail; the suite FAILS on any
// regression and on any stale expectation.
package nodetest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goccy/go-spidermonkey/internal/testutil/nodetest"
)

const (
	suiteDir         = "../../../testdata/node/suite"
	expectationsFile = "expectations.json"
	quarantineFile   = "quarantine.txt"
)

// loadQuarantine reads the list of tests known to HANG here — they never
// complete, so each one costs the whole per-test timeout and nothing else. They
// are skipped by default so a run finishes in a predictable time, and the list
// is the explicit, shrinkable record of what needs individual fixing.
func loadQuarantine(t *testing.T) map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(quarantineFile)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[line] = true
	}
	return out
}

// writeQuarantine rewrites the list, sorted, with the header that explains it.
func writeQuarantine(t *testing.T, paths map[string]bool) {
	t.Helper()
	names := make([]string, 0, len(paths))
	for p := range paths {
		names = append(names, p)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(`# Node.js tests that HANG on this runtime: they never complete, so each one
# costs the entire per-test timeout and tells us nothing beyond "still broken".
# They are skipped by default, which is what keeps a run's duration predictable;
# every other test is expected to finish.
#
# This is a list to SHRINK, not a permanent allowance. Each entry needs its own
# diagnosis — the usual cause is a handle the event loop never releases, so the
# test's own logic finished but the runtime never decides it is done.
#
# Regenerate (adds newly-hanging tests):  NODETEST_QUARANTINE_UPDATE=1
# Work on the quarantined set only:       NODETEST_QUARANTINE=only
# Ignore the list entirely:               NODETEST_QUARANTINE=all
`)
	for _, n := range names {
		b.WriteString(n)
		b.WriteString("\n")
	}
	if err := os.WriteFile(quarantineFile, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write %s: %v", quarantineFile, err)
	}
}

func TestNodeSuite(t *testing.T) {
	if os.Getenv("NODETEST") == "" {
		t.Skip("set NODETEST=1 to run the Node.js test suite (see the package comment)")
	}
	root := os.Getenv("NODETEST_ROOT")
	if root == "" {
		root = suiteDir
	}
	if _, err := os.Stat(filepath.Join(root, "test", "common", "index.js")); err != nil {
		t.Fatalf("no Node checkout at %s: run `make nodetest-fetch` (%v)", root, err)
	}

	dirs := []string{"test/parallel", "test/es-module"}
	if s := os.Getenv("NODETEST_DIRS"); s != "" {
		dirs = strings.Split(s, ",")
	}
	var paths []string
	for _, d := range dirs {
		p, err := nodetest.List(root, strings.TrimSpace(d))
		if err != nil {
			t.Fatalf("list %s: %v", d, err)
		}
		paths = append(paths, p...)
	}
	filter := os.Getenv("NODETEST_FILTER")
	if filter != "" {
		var keep []string
		for _, p := range paths {
			if strings.Contains(p, filter) {
				keep = append(keep, p)
			}
		}
		paths = keep
	}
	sort.Strings(paths)
	// NODETEST_SHARD=i/n runs only every n-th test, so the suite can be spread
	// over separate PROCESSES. That is not just parallelism: a test that blocks
	// in a host call is abandoned with its interpreter still live (see
	// JS.Close), and enough of those in one process eventually stop it making
	// progress. Sharding bounds the blast radius of that to one shard.
	if sh := os.Getenv("NODETEST_SHARD"); sh != "" {
		i, n, ok := strings.Cut(sh, "/")
		idx, err1 := strconv.Atoi(i)
		total, err2 := strconv.Atoi(n)
		if !ok || err1 != nil || err2 != nil || total <= 0 || idx < 0 || idx >= total {
			t.Fatalf("NODETEST_SHARD=%q: want i/n with 0 <= i < n", sh)
		}
		var keep []string
		for j, p := range paths {
			if j%total == idx {
				keep = append(keep, p)
			}
		}
		paths = keep
		t.Logf("shard %d of %d: %d tests", idx, total, len(paths))
	}
	quarantined := loadQuarantine(t)
	// heldBack counts the quarantined tests IN THIS SELECTION — a shard must not
	// count the other shards' hangs in its own target.
	heldBack := 0
	mode := os.Getenv("NODETEST_QUARANTINE")
	switch mode {
	case "all": // run everything, quarantine included
	case "only":
		var keep []string
		for _, p := range paths {
			if quarantined[p] {
				keep = append(keep, p)
			}
		}
		paths = keep
		t.Logf("quarantine-only: %d tests", len(paths))
	default:
		var keep []string
		for _, p := range paths {
			if quarantined[p] {
				heldBack++
				continue
			}
			keep = append(keep, p)
		}
		paths = keep
		if heldBack > 0 {
			t.Logf("skipping %d quarantined test(s) that hang; see %s", heldBack, quarantineFile)
		}
	}
	if len(paths) == 0 {
		t.Fatalf("no tests selected (dirs=%v filter=%q)", dirs, filter)
	}

	opts := nodetest.Options{Root: root}
	if s := os.Getenv("NODETEST_TIMEOUT"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			t.Fatalf("NODETEST_TIMEOUT: %v", err)
		}
		opts.Timeout = d
	}
	workers := 8
	if n, err := strconv.Atoi(os.Getenv("NODETEST_WORKERS")); err == nil && n > 0 {
		workers = n
	}
	t.Logf("running %d tests from %v on %d workers", len(paths), dirs, workers)

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = nodetest.DefaultTimeout
	}
	// inFlight names the tests currently running, so a stalled run reports WHICH
	// test stalled instead of just stopping.
	var flightMu sync.Mutex
	inFlight := map[string]time.Time{}
	jobs := make(chan string)
	results := make(chan nodetest.Result, workers)
	// An abandoned call's goroutine keeps SPINNING inside translated wasm code
	// the scheduler cannot preempt, so each abandonment can pin one P for the
	// rest of the process. Granting the scheduler a replacement P keeps the
	// other workers running; the cap bounds a pathological shard. The real fix
	// is engine-side (docs/engine-followups.md: the stack-quota check the
	// interrupt relies on is a no-op under translation).
	var extraProcs atomic.Int32
	grantProc := func() {
		if extraProcs.Add(1) <= 16 {
			runtime.GOMAXPROCS(runtime.GOMAXPROCS(0) + 1)
		}
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				// The per-test context deadline is the normal watchdog, but it can
				// only stop code the engine will interrupt: a test blocked in a host
				// op or on an Atomics.wait that is never notified ignores it. Rather
				// than let one such test stall the whole run, report it and move on,
				// abandoning the goroutine.
				flightMu.Lock()
				inFlight[p] = time.Now()
				flightMu.Unlock()
				done := make(chan nodetest.Result, 1)
				go func(p string) { done <- nodetest.Run(context.Background(), opts, p) }(p)
				select {
				case r := <-done:
					flightMu.Lock()
					delete(inFlight, p)
					flightMu.Unlock()
					if strings.Contains(r.Reason, "deadline exceeded") {
						// The interrupt did not stop it; the goroutine was
						// abandoned mid-flight (see nodetest.Run) and may be
						// pinning a P.
						grantProc()
					}
					results <- r
				case <-time.After(timeout + 30*time.Second):
					flightMu.Lock()
					delete(inFlight, p)
					flightMu.Unlock()
					// Name it as it happens: a test that has to be abandoned is the
					// one worth looking at, and waiting for the summary hides it
					// behind however long the rest of the run takes.
					t.Logf("hard timeout, abandoned: %s", p)
					grantProc()
					results <- nodetest.Result{Path: p, Status: nodetest.StatusFail,
						Reason: "hard timeout: uninterruptible block; abandoned"}
				}
			}
		}()
	}
	go func() {
		for _, p := range paths {
			jobs <- p
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	// Report what is still running whenever the run goes quiet, so a stall names
	// the tests responsible instead of just stopping.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tick := time.NewTicker(60 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				flightMu.Lock()
				var slow []string
				for p, since := range inFlight {
					if d := time.Since(since); d > timeout {
						slow = append(slow, fmt.Sprintf("%s (%v)", p, d.Round(time.Second)))
					}
				}
				flightMu.Unlock()
				if len(slow) > 0 {
					sort.Strings(slow)
					t.Logf("still running past the deadline: %s", strings.Join(slow, ", "))
				}
			}
		}
	}()

	counts := map[string]map[nodetest.Status]int{} // directory -> status -> n
	skipReasons := map[string]int{}
	// Skips split two ways, because the denominator matters: what can never work
	// here versus what is simply not built yet (see policy.go). The second group
	// belongs in the target, so it is reported as such rather than filed away.
	unimplemented := map[string]int{}
	impossible := 0
	failures := map[string]string{}
	// How long the tests that actually COMPLETE take. The per-test timeout is
	// what a run costs — the handful of tests that hang dominate the wall clock
	// entirely — so it should be set from this distribution, not guessed.
	var completed []nodetest.Result
	timedOut := map[string]bool{}
	done := 0
	start := time.Now()
	for r := range results {
		area := filepath.ToSlash(filepath.Dir(r.Path))
		if counts[area] == nil {
			counts[area] = map[nodetest.Status]int{}
		}
		counts[area][r.Status]++
		switch r.Status {
		case nodetest.StatusSkip:
			skipReasons[r.Reason]++
			switch r.Scope {
			case nodetest.Impossible:
				impossible++
			case nodetest.Unimplemented:
				unimplemented[r.Reason]++
			}
		case nodetest.StatusFail:
			failures[filepath.ToSlash(r.Path)] = firstLine(r.Reason)
		}
		if r.Status != nodetest.StatusSkip && !strings.Contains(r.Reason, "deadline exceeded") &&
			!strings.Contains(r.Reason, "hard timeout") {
			completed = append(completed, r)
		} else if r.Status == nodetest.StatusFail {
			timedOut[filepath.ToSlash(r.Path)] = true
		}
		if done++; done%250 == 0 {
			flightMu.Lock()
			var slow []string
			for p, since := range inFlight {
				if d := time.Since(since); d > timeout {
					slow = append(slow, fmt.Sprintf("%s (%v)", p, d.Round(time.Second)))
				}
			}
			flightMu.Unlock()
			sort.Strings(slow)
			t.Logf("%d/%d done in %v%s", done, len(paths), time.Since(start).Round(time.Second),
				func() string {
					if len(slow) == 0 {
						return ""
					}
					return " | over deadline: " + strings.Join(slow, ", ")
				}())
		}
	}

	var areas []string
	for a := range counts {
		areas = append(areas, a)
	}
	sort.Strings(areas)
	totalPass, totalRun := 0, 0
	for _, a := range areas {
		c := counts[a]
		run := c[nodetest.StatusPass] + c[nodetest.StatusFail]
		totalPass += c[nodetest.StatusPass]
		totalRun += run
		t.Logf("%-18s pass %5d / run %5d (skip %5d)  %.2f%%",
			a, c[nodetest.StatusPass], run, c[nodetest.StatusSkip],
			100*float64(c[nodetest.StatusPass])/float64(max(run, 1)))
	}
	// Three numbers, because only the last one measures progress toward the goal:
	//   run       — what executed today
	//   target    — every test except the ones that can never work here; the
	//               quarantined hangs and the unimplemented capabilities are
	//               inside it, since both are work to do
	//   impossible— V8 surfaces, native addons, Node's private internals
	unimplementedTotal := 0
	for _, n := range unimplemented {
		unimplementedTotal += n
	}
	target := totalRun + unimplementedTotal + heldBack
	t.Logf("TOTAL              pass %5d / run %5d  %.2f%%  in %v",
		totalPass, totalRun, 100*float64(totalPass)/float64(max(totalRun, 1)),
		time.Since(start).Round(time.Second))
	t.Logf("TARGET             pass %5d / %5d  %.2f%%  (everything except %d that can never run here)",
		totalPass, target, 100*float64(totalPass)/float64(max(target, 1)), impossible)
	t.Logf("  of the target, %d are unimplemented capabilities and %d hang (quarantined)",
		unimplementedTotal, heldBack)
	var unimplNames []string
	for k := range unimplemented {
		unimplNames = append(unimplNames, k)
	}
	sort.Slice(unimplNames, func(i, j int) bool {
		return unimplemented[unimplNames[i]] > unimplemented[unimplNames[j]]
	})
	for i, k := range unimplNames {
		if i == 12 {
			break
		}
		t.Logf("  todo %5d  %s", unimplemented[k], k)
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].Took > completed[j].Took })
	if len(completed) > 0 {
		p50 := completed[len(completed)/2].Took
		t.Logf("completed-test time: slowest %v, median %v — the per-test timeout "+
			"should sit above the slowest, since only the tests that HANG pay it",
			completed[0].Took.Round(time.Millisecond), p50.Round(time.Millisecond))
		for i, r := range completed {
			if i == 10 {
				break
			}
			t.Logf("  slow %8v  %s", r.Took.Round(time.Millisecond), r.Path)
		}
	}
	var reasons []string
	for r := range skipReasons {
		reasons = append(reasons, r)
	}
	sort.Slice(reasons, func(i, j int) bool { return skipReasons[reasons[i]] > skipReasons[reasons[j]] })
	for _, r := range reasons {
		t.Logf("skip %6d  %s", skipReasons[r], r)
	}

	if len(timedOut) > 0 {
		t.Logf("%d test(s) hit the timeout; add them with NODETEST_QUARANTINE_UPDATE=1", len(timedOut))
	}
	if mode == "only" {
		// The point of this mode: find entries that have been fixed. Anything that
		// COMPLETES here no longer belongs in the list.
		var fixed []string
		for _, r := range completed {
			fixed = append(fixed, filepath.ToSlash(r.Path))
		}
		sort.Strings(fixed)
		for _, p := range fixed {
			t.Logf("quarantined test now completes; remove it from %s: %s", quarantineFile, p)
		}
	}
	if os.Getenv("NODETEST_QUARANTINE_UPDATE") != "" {
		for p := range timedOut {
			quarantined[p] = true
		}
		writeQuarantine(t, quarantined)
		t.Logf("quarantine now lists %d test(s)", len(quarantined))
		return
	}
	if report := os.Getenv("NODETEST_REPORT"); report != "" {
		writeJSON(t, report, failures)
	}
	if mode := os.Getenv("NODETEST_UPDATE"); mode != "" {
		out := failures
		if mode == "merge" {
			// A SHARD's results, merged into the existing file. The suite cannot
			// always be regenerated in one process — a long single-process run
			// stops making progress (see docs/engine-followups.md), and sharding
			// is the documented way around it — so a whole-file rewrite from one
			// shard would delete every other shard's expectations.
			out = map[string]string{}
			if b, err := os.ReadFile(expectationsFile); err == nil {
				if err := json.Unmarshal(b, &out); err != nil {
					t.Fatalf("parse %s: %v", expectationsFile, err)
				}
			}
			for _, p := range paths {
				delete(out, filepath.ToSlash(p)) // this shard re-decides its own
			}
			for p, detail := range failures {
				out[p] = detail
			}
		}
		writeJSON(t, expectationsFile, out)
		t.Logf("wrote %d expected failures to %s", len(out), expectationsFile)
		return
	}

	expected := map[string]string{}
	if b, err := os.ReadFile(expectationsFile); err == nil {
		if err := json.Unmarshal(b, &expected); err != nil {
			t.Fatalf("parse %s: %v", expectationsFile, err)
		}
	} else if filter == "" {
		t.Logf("no %s: reporting only, not judging", expectationsFile)
		return
	}
	var regressions, stale []string
	for p, detail := range failures {
		if _, ok := expected[p]; !ok {
			regressions = append(regressions, p+": "+detail)
		}
	}
	// Staleness is only meaningful for tests this invocation actually RAN.
	// Without that, every shard would report the other shards' expectations as
	// stale, and a filtered run would report the whole file.
	ran := make(map[string]bool, len(paths))
	for _, p := range paths {
		ran[filepath.ToSlash(p)] = true
	}
	for p := range expected {
		if _, ok := failures[p]; !ok && ran[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(regressions)
	sort.Strings(stale)
	for i, r := range regressions {
		if i == 50 {
			t.Errorf("... and %d more unexpected failures", len(regressions)-50)
			break
		}
		t.Errorf("unexpected failure: %s", r)
	}
	for i, p := range stale {
		if i == 50 {
			t.Errorf("... and %d more stale expectations", len(stale)-50)
			break
		}
		t.Errorf("expected failure now passes (update %s): %s", expectationsFile, p)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return strings.TrimSpace(s)
}

func writeJSON(t *testing.T, path string, v map[string]string) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
