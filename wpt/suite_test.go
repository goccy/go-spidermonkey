// This file runs the Web Platform Tests (web-platform-tests/wpt, checked out
// under ./suite by `make wpt-fetch`) against the compat/web vocabulary.
//
// The run is opt-in:
//
//	WPT=1 go test ./wpt/ -run TestWPTSuite -v -timeout 1h
//
// Knobs: WPT_DIRS (comma-separated suite directories, default DefaultDirs),
// WPT_FILTER (substring), WPT_WORKERS, WPT_TIMEOUT (per file), WPT_UPDATE=1
// (regenerate expectations.json), WPT_REPORT=path.
//
// Only the .any.js / .worker.js forms are runnable without a browser, and only
// the directories whose APIs this embedding actually provides are in the
// default set — a DOM-dependent directory would produce nothing but noise. The
// suite's own testharness.js drives each file in its shell environment (no DOM
// is faked), a loopback HTTP server serves the fixtures the tests fetch, and
// results are judged PER SUBTEST so one unimplemented corner cannot hide the
// rest of a file.
//
// expectations.json records one line per FILE — the number of failing subtests
// and a digest of which ones — and the run FAILS on any change to it: a new
// failing file, a file that stopped failing, or the same count with a different
// set of subtests. Per-subtest keys would be the precise form, but this suite
// has ~22,000 known failures (mostly whole APIs that are not implemented) and
// 6 MB of them is a file nobody can review; the digest keeps the precision
// without the bulk, and WPT_REPORT still dumps the detail.
package wpt_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-spidermonkey/wpt"
)

const (
	suiteDir         = "suite"
	expectationsFile = "expectations.json"
)

// DefaultDirs are the WPT directories whose APIs compat/web implements. Adding
// a capability adds its directory here; nothing is listed that could only ever
// report "not implemented".
var DefaultDirs = []string{
	"url",
	"encoding",
	"streams",
	"WebCryptoAPI",
	"console",
	"hr-time",
	"performance-timeline",
	"FileAPI",
	"urlpattern",
	"fetch/api",
	"fetch/data-urls",
	"dom/events",
	"dom/abort",
	"html/webappapis/microtask-queuing",
	"html/webappapis/timers",
	"html/webappapis/structured-clone",
	"html/webappapis/atob",
	// Directories Deno also tracks whose APIs this embedding claims to provide.
	// They are listed even where the score is currently zero: a capability that
	// is documented as present and scores nothing is exactly what a conformance
	// run exists to surface (CompressionStream and MessageChannel turned out to
	// live only in compat/nodejs, though both are web APIs).
	"compression",
	"user-timing",
	"webmessaging",
	"mimesniff",
}

func TestWPTSuite(t *testing.T) {
	if os.Getenv("WPT") == "" {
		t.Skip("set WPT=1 to run the Web Platform Tests (see the package comment)")
	}
	root := os.Getenv("WPT_ROOT")
	if root == "" {
		root = suiteDir
	}
	if _, err := os.Stat(path.Join(root, "resources", "testharness.js")); err != nil {
		t.Fatalf("no WPT checkout at %s: run `make wpt-fetch` (%v)", root, err)
	}

	dirs := DefaultDirs
	if s := os.Getenv("WPT_DIRS"); s != "" {
		dirs = strings.Split(s, ",")
	}
	paths, err := wpt.List(root, dirs)
	if err != nil {
		t.Fatal(err)
	}
	filter := os.Getenv("WPT_FILTER")
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

	// The tests fetch their own fixtures; serving the checkout over loopback is
	// what gives them an origin to fetch from (and exercises fetch itself).
	srv, err := wpt.StartServer(root)
	if err != nil {
		t.Fatalf("start fixture server: %v", err)
	}
	defer srv.Close()

	opts := wpt.Options{Root: root, BaseURL: srv.BaseURL()}
	if s := os.Getenv("WPT_TIMEOUT"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			t.Fatalf("WPT_TIMEOUT: %v", err)
		}
		opts.Timeout = d
	}
	workers := 8
	if n, err := strconv.Atoi(os.Getenv("WPT_WORKERS")); err == nil && n > 0 {
		workers = n
	}
	t.Logf("running %d files from %d directories on %d workers", len(paths), len(dirs), workers)

	jobs := make(chan string)
	results := make(chan wpt.FileResult, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				results <- wpt.Run(context.Background(), opts, p)
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

	type stat struct{ pass, fail, files, broken, skipped int }
	byArea := map[string]*stat{}
	// One line per FILE, not per subtest. Per-subtest keys are the precise form,
	// but this suite has ~22,000 known failures — mostly whole APIs that are not
	// implemented — and 6 MB of them is a file nobody can read or review. Each
	// line therefore carries the count AND a digest of exactly WHICH subtests
	// failed, so a swap (one fixed, one broken, same count) still shows up; the
	// run itself prints the individual failures, and WPT_REPORT dumps them all.
	failures := map[string]string{}
	harnessReasons := map[string]int{}
	done, start := 0, time.Now()
	for r := range results {
		if done++; done%100 == 0 {
			t.Logf("%d/%d files in %v", done, len(paths), time.Since(start).Round(time.Second))
		}
		area := topDir(r.Path)
		s := byArea[area]
		if s == nil {
			s = &stat{}
			byArea[area] = s
		}
		s.files++
		switch r.Harness {
		case "OK":
		case "SKIP":
			s.skipped++
			harnessReasons["skip: "+r.Message]++
		default:
			s.broken++
			harnessReasons[r.Harness+": "+bucket(r.Message)]++
			failures[r.Path] = r.Harness + ": " + bucket(r.Message)
			continue
		}
		var failed []string
		for _, sub := range r.Subtests {
			if sub.Status == wpt.StatusPass {
				s.pass++
				continue
			}
			s.fail++
			failed = append(failed, string(sub.Status)+" "+sub.Name)
		}
		if len(failed) > 0 {
			sort.Strings(failed)
			sum := sha256.Sum256([]byte(strings.Join(failed, "\n")))
			failures[r.Path] = fmt.Sprintf("%d/%d subtests fail (%x)",
				len(failed), len(r.Subtests), sum[:6])
		}
	}

	var areas []string
	for a := range byArea {
		areas = append(areas, a)
	}
	sort.Strings(areas)
	var totalPass, totalFail int
	for _, a := range areas {
		s := byArea[a]
		totalPass += s.pass
		totalFail += s.fail
		t.Logf("%-24s subtests %6d/%6d  %.2f%%  (files %4d, harness-error %3d, skipped %3d)",
			a, s.pass, s.pass+s.fail, 100*float64(s.pass)/float64(max(s.pass+s.fail, 1)),
			s.files, s.broken, s.skipped)
	}
	t.Logf("TOTAL                    subtests %6d/%6d  %.2f%%  in %v",
		totalPass, totalPass+totalFail,
		100*float64(totalPass)/float64(max(totalPass+totalFail, 1)),
		time.Since(start).Round(time.Second))
	var reasons []string
	for r := range harnessReasons {
		reasons = append(reasons, r)
	}
	sort.Slice(reasons, func(i, j int) bool { return harnessReasons[reasons[i]] > harnessReasons[reasons[j]] })
	for i, r := range reasons {
		if i == 30 {
			break
		}
		t.Logf("harness %5d  %s", harnessReasons[r], r)
	}

	if report := os.Getenv("WPT_REPORT"); report != "" {
		writeJSON(t, report, failures)
	}
	if os.Getenv("WPT_UPDATE") != "" {
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
	var regressions, changed, stale []string
	for k, detail := range failures {
		want, ok := expected[k]
		if !ok {
			regressions = append(regressions, k+": "+detail)
			continue
		}
		// The value carries the count and a digest of WHICH subtests failed, so a
		// difference here is a change in outcome even when the count matches. It
		// may be an improvement — that still has to be recorded, or the file stops
		// describing the run.
		if want != detail {
			changed = append(changed, k+": was "+want+", now "+detail)
		}
	}
	// Staleness is only meaningful for files this invocation actually RAN: a
	// narrowed run (WPT_DIRS, WPT_FILTER) must not report every other directory's
	// expectations as stale.
	ran := make(map[string]bool, len(paths))
	for _, p := range paths {
		ran[p] = true
	}
	for k := range expected {
		if _, ok := failures[k]; !ok && ran[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(regressions)
	sort.Strings(changed)
	sort.Strings(stale)
	for i, c := range changed {
		if i == 50 {
			t.Errorf("... and %d more changed outcomes", len(changed)-50)
			break
		}
		t.Errorf("outcome changed (update %s): %s", expectationsFile, c)
	}
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

// topDir is the reporting bucket for a test path: its first two path segments
// where the suite nests (html/webappapis), otherwise the first.
func topDir(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) > 2 && (parts[0] == "html" || parts[0] == "fetch" || parts[0] == "dom") {
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

func bucket(s string) string {
	s = strings.ReplaceAll(s, "\n", " | ")
	if len(s) > 200 {
		s = s[:200] + "…"
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
