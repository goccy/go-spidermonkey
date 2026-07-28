// This file runs Babel's fixture corpus against this runtime.
//
// The run is opt-in:
//
//	BABELTEST=1 go test ./babeltest/ -run TestBabelSuite -v -timeout 2h
//
// Knobs: BABELTEST_FILTER (substring, matched against the Babel package name),
// BABELTEST_WORKERS, BABELTEST_TIMEOUT (per shard), BABELTEST_UPDATE=1
// (regenerate expectations.json), BABELTEST_REPORT=path.
//
// One shard is one Babel package's fixtures, run in one interpreter (see
// runner.go). expectations.json lists the fixtures known to fail; the run FAILS
// on any regression and on any stale expectation.
package babeltest_test

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-spidermonkey/babeltest"
)

const expectationsFile = "expectations.json"

func TestBabelSuite(t *testing.T) {
	if os.Getenv("BABELTEST") == "" {
		t.Skip("set BABELTEST=1 to run Babel's fixture corpus (see the package comment)")
	}
	root := os.Getenv("BABELTEST_ROOT")
	if root == "" {
		root = "."
	}
	if _, err := os.Stat(root + "/node_modules/@babel/core/package.json"); err != nil {
		t.Fatalf("no @babel packages under %s: run `make babeltest-fetch` (%v)", root, err)
	}
	shards, err := babeltest.Shards(root)
	if err != nil {
		t.Fatalf("no Babel checkout: run `make babeltest-fetch` (%v)", err)
	}
	filter := os.Getenv("BABELTEST_FILTER")
	if filter != "" {
		var keep []string
		for _, s := range shards {
			if strings.Contains(s, filter) {
				keep = append(keep, s)
			}
		}
		shards = keep
	}
	if len(shards) == 0 {
		t.Fatalf("no fixture packages selected (filter=%q)", filter)
	}

	opts := babeltest.Options{Root: root}
	if s := os.Getenv("BABELTEST_TIMEOUT"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			t.Fatalf("BABELTEST_TIMEOUT: %v", err)
		}
		opts.Timeout = d
	}
	// Babel shards are memory-hungry, so the default fan-out is modest.
	workers := 4
	if n, err := strconv.Atoi(os.Getenv("BABELTEST_WORKERS")); err == nil && n > 0 {
		workers = n
	}
	t.Logf("running %d fixture packages on %d workers", len(shards), workers)

	jobs := make(chan string)
	results := make(chan []babeltest.Result, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range jobs {
				results <- babeltest.RunShard(context.Background(), opts, s)
			}
		}()
	}
	go func() {
		for _, s := range shards {
			jobs <- s
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	type stat struct{ pass, fail, skip int }
	byPkg := map[string]*stat{}
	failures := map[string]string{}
	skipReasons := map[string]int{}
	done, start := 0, time.Now()
	for batch := range results {
		for _, r := range batch {
			pkg := strings.SplitN(r.Name, "/", 2)[0]
			s := byPkg[pkg]
			if s == nil {
				s = &stat{}
				byPkg[pkg] = s
			}
			switch r.Status {
			case babeltest.StatusPass:
				s.pass++
			case babeltest.StatusSkip:
				s.skip++
				skipReasons[r.Reason]++
			default:
				s.fail++
				failures[r.Name] = firstLine(r.Reason)
			}
		}
		if done++; done%10 == 0 {
			t.Logf("%d/%d packages in %v", done, len(shards), time.Since(start).Round(time.Second))
		}
	}

	var pkgs []string
	for p := range byPkg {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	var totalPass, totalRun, totalSkip int
	for _, p := range pkgs {
		s := byPkg[p]
		totalPass += s.pass
		totalRun += s.pass + s.fail
		totalSkip += s.skip
		if s.fail > 0 {
			t.Logf("%-48s %5d/%5d  (skip %4d)", p, s.pass, s.pass+s.fail, s.skip)
		}
	}
	t.Logf("TOTAL %d/%d fixtures  %.2f%%  (skip %d)  in %v",
		totalPass, totalRun, 100*float64(totalPass)/float64(max(totalRun, 1)), totalSkip,
		time.Since(start).Round(time.Second))
	var reasons []string
	for r := range skipReasons {
		reasons = append(reasons, r)
	}
	sort.Slice(reasons, func(i, j int) bool { return skipReasons[reasons[i]] > skipReasons[reasons[j]] })
	for _, r := range reasons {
		t.Logf("skip %6d  %s", skipReasons[r], r)
	}

	if report := os.Getenv("BABELTEST_REPORT"); report != "" {
		writeJSON(t, report, failures)
	}
	if os.Getenv("BABELTEST_UPDATE") != "" {
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
	for k, detail := range failures {
		if _, ok := expected[k]; !ok {
			regressions = append(regressions, k+": "+detail)
		}
	}
	for k := range expected {
		if _, ok := failures[k]; !ok {
			if filter == "" || strings.Contains(k, filter) {
				stale = append(stale, k)
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
	for i, k := range stale {
		if i == 50 {
			t.Errorf("... and %d more stale expectations", len(stale)-50)
			break
		}
		t.Errorf("expected failure now passes (update %s): %s", expectationsFile, k)
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

func writeJSON(t *testing.T, p string, v map[string]string) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatalf("marshal %s: %v", p, err)
	}
	if err := os.WriteFile(p, append(b, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}
