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
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-spidermonkey/nodetest"
)

const (
	suiteDir         = "suite"
	expectationsFile = "expectations.json"
)

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
					results <- r
				case <-time.After(timeout + 30*time.Second):
					flightMu.Lock()
					delete(inFlight, p)
					flightMu.Unlock()
					// Name it as it happens: a test that has to be abandoned is the
					// one worth looking at, and waiting for the summary hides it
					// behind however long the rest of the run takes.
					t.Logf("hard timeout, abandoned: %s", p)
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
	failures := map[string]string{}
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
		case nodetest.StatusFail:
			failures[filepath.ToSlash(r.Path)] = firstLine(r.Reason)
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
	t.Logf("TOTAL              pass %5d / run %5d  %.2f%%  in %v",
		totalPass, totalRun, 100*float64(totalPass)/float64(max(totalRun, 1)),
		time.Since(start).Round(time.Second))
	var reasons []string
	for r := range skipReasons {
		reasons = append(reasons, r)
	}
	sort.Slice(reasons, func(i, j int) bool { return skipReasons[reasons[i]] > skipReasons[reasons[j]] })
	for _, r := range reasons {
		t.Logf("skip %6d  %s", skipReasons[r], r)
	}

	if report := os.Getenv("NODETEST_REPORT"); report != "" {
		writeJSON(t, report, failures)
	}
	if os.Getenv("NODETEST_UPDATE") != "" {
		writeJSON(t, expectationsFile, failures)
		t.Logf("wrote %d expected failures to %s", len(failures), expectationsFile)
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
	for p := range expected {
		if _, ok := failures[p]; !ok {
			if filter == "" || strings.Contains(p, filter) {
				stale = append(stale, p)
			}
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
