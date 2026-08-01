// This file runs the Web Platform Tests (web-platform-tests/wpt, checked out
// under ./suite by `make wpt-fetch`) against the compat/web vocabulary.
//
// The run is opt-in:
//
//	WPT=1 go test ./wpt/ -run TestWPTSuite -v -timeout 1h
//
// Knobs: WPT_DIRS (comma-separated suite directories, default DefaultDirs),
// WPT_FILTER (substring), WPT_WORKERS, WPT_TIMEOUT (per file), WPT_SHARD=i/n
// (every n-th case, for the sequential shards `make wpt` runs), WPT_UPDATE=1
// (regenerate expectations.json — whole file, so only from an UNSHARDED run;
// shards use WPT_UPDATE=merge), WPT_REPORT=path.
//
// Only the .any.js / .worker.js forms are runnable without a browser. Which
// DIRECTORIES are in the default set is a separate question, and the answer is
// not "the ones whose APIs exist here" — see DefaultDirs. The suite's own
// testharness.js drives each file in its shell environment (no DOM is faked), a
// loopback server serves the fixtures the tests fetch over both http and https,
// and results are judged PER SUBTEST so one unimplemented corner cannot hide
// the rest of a file.
//
// Two rates are reported. The subtest rate is not worth quoting on its own:
// WebCryptoAPI declares over 70% of every subtest in the corpus, so a whole new
// API moves it by a tenth of a point. The CLEAN CASE rate — cases that ran with
// no failing subtest — is the fair weighting, and it is also what the other
// runtimes publish, so it is the only one of the two that can be compared.
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
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goccy/go-spidermonkey/internal/testutil/wpt"
)

const (
	suiteDir         = "../../../testdata/wpt/suite"
	expectationsFile = "expectations.json"
)

// DefaultDirs are the WPT directories this runtime is measured against: the
// union of what Bun and Deno cover, plus anything else already implemented here.
//
// The list is deliberately NOT "the directories whose APIs we implement". That
// was the earlier rule, and it inverted what a conformance run is for: a missing
// API had no failing tests, so the score went up by not implementing something,
// and the number was quoted as "WPT" while covering a subset chosen to flatter
// it. A directory where everything fails is the point — it is the statement of
// work remaining.
//
// Top-level directories are named, not narrower paths, so that a subdirectory
// added upstream is picked up rather than silently missed.
var DefaultDirs = []string{
	// Covered by both Bun and Deno.
	"console",
	"compression",
	"dom",
	"encoding",
	"fetch",
	"FileAPI",
	"hr-time",
	"html",
	"mimesniff",
	"streams",
	"url",
	"urlpattern",
	"user-timing",
	"webmessaging",
	"WebCryptoAPI",
	// Covered by Deno, not yet implemented here. Listed because that is what
	// makes the gap visible.
	"css",
	"eventsource",
	"schema",
	"service-workers",
	"wasm",
	"web-locks",
	"webidl",
	"webstorage",
	"websockets",
	"workers",
	"xhr",
	// Implemented here; Deno does not track it.
	"performance-timeline",
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
	// One source file becomes one case per target scope and per variant, which is
	// what a browser runs. See wpt.Expand.
	cases, err := wpt.Expand(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	filter := os.Getenv("WPT_FILTER")
	if filter != "" {
		var keep []wpt.Case
		for _, c := range cases {
			if strings.Contains(c.Path, filter) {
				keep = append(keep, c)
			}
		}
		cases = keep
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].Key() < cases[j].Key() })
	// WPT_SHARD=i/n keeps every n-th case, so the suite can run as separate
	// SEQUENTIAL processes (make wpt): each shard's peak memory is returned to
	// the OS at process exit, and a stall (docs/engine-followups.md) costs one
	// shard, not the run. Pair with WPT_UPDATE=merge, never =1: a whole-file
	// rewrite from one shard deletes every other shard's expectations.
	if sh := os.Getenv("WPT_SHARD"); sh != "" {
		i, n, ok := strings.Cut(sh, "/")
		idx, err1 := strconv.Atoi(i)
		total, err2 := strconv.Atoi(n)
		if !ok || err1 != nil || err2 != nil || total <= 0 || idx < 0 || idx >= total {
			t.Fatalf("WPT_SHARD=%q: want i/n with 0 <= i < n", sh)
		}
		var keep []wpt.Case
		for j, c := range cases {
			if j%total == idx {
				keep = append(keep, c)
			}
		}
		cases = keep
	}
	if len(cases) == 0 {
		t.Fatalf("no tests selected (dirs=%v filter=%q)", dirs, filter)
	}

	// The tests fetch their own fixtures; serving the checkout over loopback is
	// what gives them an origin to fetch from (and exercises fetch itself).
	srv, err := wpt.StartServer(root)
	if err != nil {
		t.Fatalf("start fixture server: %v", err)
	}
	defer srv.Close()

	opts := wpt.Options{
		Root: root, BaseURL: srv.BaseURL(), HTTPSBaseURL: srv.HTTPSBaseURL(),
		RootCAs: srv.RootCAs(), SubVars: srv.SubVars(),
	}
	replacer := stabilizer(srv.SubVars())
	// Volatile ports AND the coordinates of our own evaluated source, both of
	// which change for reasons unrelated to any outcome.
	stable := func(s string) string { return evalFrame.ReplaceAllString(replacer.Replace(s), "<eval>") }
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
	t.Logf("running %d cases from %d files in %d directories on %d workers",
		len(cases), len(paths), len(dirs), workers)

	jobs := make(chan wpt.Case)
	results := make(chan wpt.FileResult, workers)
	// An abandoned call's goroutine keeps spinning inside translated wasm code
	// the scheduler cannot preempt (see the nodetest driver for the long form);
	// grant the scheduler a replacement P per abandonment, within a cap.
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
			for c := range jobs {
				r := wpt.Run(context.Background(), opts, c)
				if r.Harness == string(wpt.StatusError) && strings.Contains(r.Message, "deadline exceeded") {
					grantProc()
				}
				results <- r
			}
		}()
	}
	go func() {
		for _, c := range cases {
			jobs <- c
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	type stat struct{ pass, fail, files, clean, broken, skipped int }
	byArea := map[string]*stat{}
	// One line per FILE, not per subtest. Per-subtest keys are the precise form,
	// but this suite has ~22,000 known failures — mostly whole APIs that are not
	// implemented — and 6 MB of them is a file nobody can read or review. Each
	// line therefore carries the count AND a digest of exactly WHICH subtests
	// failed, so a swap (one fixed, one broken, same count) still shows up; the
	// run itself prints the individual failures, and WPT_REPORT dumps them all.
	failures := map[string]string{}
	// The full status+name+message lines behind each digest, kept only in memory
	// and printed only for a failure the expectations did not predict. A flake
	// that strikes once per full run is undebuggable from a digest; this is what
	// turns its next occurrence into a cause.
	failureDetail := map[string][]string{}
	harnessReasons := map[string]int{}
	// WPT_REPORT_ALL captures EVERY case, passing ones included — the input
	// the coverage report (coverage_test.go) is computed from.
	type caseOutcome struct {
		Harness  string `json:"harness"`
		Subtests int    `json:"subtests"`
		Failed   int    `json:"failed"`
	}
	allOutcomes := map[string]caseOutcome{}
	done, start := 0, time.Now()
	for r := range results {
		if done++; done%100 == 0 {
			t.Logf("%d/%d cases in %v", done, len(cases), time.Since(start).Round(time.Second))
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
			harnessReasons[r.Harness+": "+bucket(stable(r.Message))]++
			failures[r.Path] = r.Harness + ": " + bucket(stable(r.Message))
			allOutcomes[r.Path] = caseOutcome{Harness: r.Harness, Subtests: len(r.Subtests)}
			continue
		}
		if r.Harness == "SKIP" {
			allOutcomes[r.Path] = caseOutcome{Harness: r.Harness}
		}
		var failed []string
		for _, sub := range r.Subtests {
			if sub.Status == wpt.StatusPass {
				s.pass++
				continue
			}
			s.fail++
			failed = append(failed, string(sub.Status)+" "+stable(sub.Name))
			failureDetail[r.Path] = append(failureDetail[r.Path],
				string(sub.Status)+" "+stable(sub.Name)+": "+bucket(stable(sub.Message)))
		}
		if r.Harness == "OK" {
			allOutcomes[r.Path] = caseOutcome{Harness: r.Harness, Subtests: len(r.Subtests), Failed: len(failed)}
		}
		if len(failed) == 0 {
			s.clean++
		} else {
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
	var totalPass, totalFail, totalCases int
	for _, a := range areas {
		s := byArea[a]
		totalPass += s.pass
		totalFail += s.fail
		totalCases += s.files
		t.Logf("%-34s subtests %6d/%6d %6.2f%%   clean cases %4d/%4d %6.2f%%  (harness-error %3d, skipped %3d)",
			a, s.pass, s.pass+s.fail, 100*float64(s.pass)/float64(max(s.pass+s.fail, 1)),
			s.clean, s.files, 100*float64(s.clean)/float64(max(s.files, 1)),
			s.broken, s.skipped)
	}
	t.Logf("TOTAL                    subtests %6d/%6d  %.2f%%  in %v",
		totalPass, totalPass+totalFail,
		100*float64(totalPass)/float64(max(totalPass+totalFail, 1)),
		time.Since(start).Round(time.Second))
	// The subtest rate is dominated by whichever directory declares the most
	// assertions — WebCryptoAPI alone is over 70% of them — so it moves barely at
	// all when a whole API is implemented. The rate below counts CASES that ran
	// clean, which is both a fair weighting and the number the other runtimes
	// publish: it is what "Deno passes 62.2% of WPT" means.
	clean := totalCases - len(failures)
	t.Logf("TOTAL                    clean cases %5d/%5d  %.2f%%  (a case is a file in one scope with one variant)",
		clean, totalCases, 100*float64(clean)/float64(max(totalCases, 1)))
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
	if report := os.Getenv("WPT_REPORT_ALL"); report != "" {
		writeJSON(t, report, allOutcomes)
	}
	if report := os.Getenv("WPT_DETAIL"); report != "" {
		// Every failing subtest's status, name and message, per case — the raw
		// material for asking "which missing piece costs the most".
		writeJSON(t, report, failureDetail)
	}
	if mode := os.Getenv("WPT_UPDATE"); mode != "" {
		out := failures
		if mode == "merge" {
			// A SHARD's results, merged into the existing file: this run
			// re-decides only the cases it ran, and keeps the rest.
			out = map[string]string{}
			if b, err := os.ReadFile(expectationsFile); err == nil {
				if err := json.Unmarshal(b, &out); err != nil {
					t.Fatalf("parse %s: %v", expectationsFile, err)
				}
			}
			for _, c := range cases {
				delete(out, c.Key())
			}
			for k, detail := range failures {
				out[k] = detail
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
	ran := make(map[string]bool, len(cases))
	for _, c := range cases {
		ran[c.Key()] = true
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
		key, _, _ := strings.Cut(r, ": ")
		for _, line := range failureDetail[key] {
			t.Errorf("    %s", line)
		}
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

// stabilizer removes this process's own volatile values from a subtest name or
// a harness message. The two loopback ports are assigned by the kernel at
// start-up, and a ".sub." test's subtest name usually carries the URL it
// fetched — so the name, and any digest of it, differed on every run for a
// reason that had nothing to do with the outcome, and expectations.json could
// never be clean. The ports are known structurally (this process assigned
// them), so each goes back to the token it was substituted from.
// evalFrame matches a stack frame inside code this harness evaluated. The line
// and column are positions in compat/web's own JavaScript, so every edit there
// changes them — which made unrelated files report a "changed outcome" and made
// expectations.json impossible to keep clean. The FRAME is the information; its
// coordinates inside our own source are not.
var evalFrame = regexp.MustCompile(`<eval>:\d+:\d+`)

func stabilizer(vars map[string]string) *strings.Replacer {
	// EVERY listener's port, not just the plain-HTTP ones: a subtest name built
	// from get_host_info().HTTPS_ORIGIN carries the TLS port, and a name built
	// from a WebSocket URL carries the ws one. One token per distinct value —
	// ports[http][0] and ports[ws][0] are the same listener here, so they
	// deliberately collapse to the same token.
	seen := map[string]string{}
	var pairs []string
	for _, name := range []string{
		"ports[http][0]", "ports[http][1]",
		"ports[https][0]", "ports[https][1]",
		"ports[ws][0]", "ports[wss][0]", "ports[h2][0]",
	} {
		v := vars[name]
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue // already covered by the first variable that named this port
		}
		token := ":{{port" + strconv.Itoa(len(seen)) + "}}"
		seen[v] = token
		pairs = append(pairs, ":"+v, token)
	}
	return strings.NewReplacer(pairs...)
}

func bucket(s string) string {
	s = strings.ReplaceAll(s, "\n", " | ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return strings.TrimSpace(s)
}

func writeJSON(t *testing.T, p string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatalf("marshal %s: %v", p, err)
	}
	if err := os.WriteFile(p, append(b, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}
