package wpt_test

// TestWPTCoverage answers the honest question: of EVERYTHING in the Web
// Platform Tests at the pinned revision, how much runs clean here? The
// denominator is the whole upstream tree — directories that are not checked
// out, .html tests that need a DOM, scopes this runtime cannot host — and
// anything not run counts as failed, the same way Servo's dashboard
// (https://servo.org/wpt/) scores itself against the full suite rather than
// against the parts it attempts.
//
// Two-step use (the run takes minutes, the arithmetic does not):
//
//	WPT=1 WPT_REPORT_ALL=/tmp/all.json go test ./internal/testutil/wpt -run TestWPTSuite
//	WPT_COVERAGE_REPORT=/tmp/all.json go test ./internal/testutil/wpt -run TestWPTCoverage -v
//
// The unit is the TEST FILE. The real WPT manifest expands a file into items
// (scopes, variants) by reading its contents, which a blobless checkout does
// not have for the directories that are not materialized; files are the
// finest unit that can be counted for the WHOLE tree from names alone. A
// file is clean only when every case expanded from it ran with no failing
// subtest.
//
// What counts as a test file follows the manifest's naming rules: anything
// under a resources/, support/ or tools/ directory is support, *-ref.* and
// *-notref.* are reftest references, .any.js/.worker.js/.window.js are the
// script forms, and .html/.htm/.xht/.xhtml/.svg files are tests of one kind
// or another — testharness, reftest, crash, manual or visual, every one of
// which a browser runs and this runtime does not, so every one belongs in
// the denominator. webdriver/tests/*.py (wdspec) are counted too, minus
// their conftest/__init__ plumbing.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

func TestWPTCoverage(t *testing.T) {
	reportPath := os.Getenv("WPT_COVERAGE_REPORT")
	if reportPath == "" {
		t.Skip("set WPT_COVERAGE_REPORT to the WPT_REPORT_ALL output of a full run")
	}

	// The full upstream listing. A blobless clone carries every TREE, so this
	// names the whole suite, not just the sparse directories on disk.
	out, err := exec.Command("git", "-C", suiteDir, "ls-tree", "-r", "HEAD", "--name-only").Output()
	if err != nil {
		t.Fatalf("ls-tree %s: %v", suiteDir, err)
	}

	total := map[string]int{}
	isTest := func(p string) bool {
		for _, seg := range strings.Split(p, "/") {
			if seg == "resources" || seg == "support" || seg == "tools" {
				return false
			}
		}
		base := p[strings.LastIndex(p, "/")+1:]
		stem := base
		if i := strings.LastIndex(stem, "."); i >= 0 {
			stem = stem[:i]
		}
		if strings.HasSuffix(stem, "-ref") || strings.HasSuffix(stem, "-notref") ||
			strings.Contains(base, "-ref.") || strings.Contains(base, "-notref.") {
			return false
		}
		switch {
		case strings.HasSuffix(base, ".any.js"),
			strings.HasSuffix(base, ".worker.js"),
			strings.HasSuffix(base, ".window.js"):
			return true
		case strings.HasSuffix(base, ".html"), strings.HasSuffix(base, ".htm"),
			strings.HasSuffix(base, ".xht"), strings.HasSuffix(base, ".xhtml"),
			strings.HasSuffix(base, ".svg"):
			return true
		case strings.HasSuffix(base, ".py") && strings.HasPrefix(p, "webdriver/tests/"):
			return base != "__init__.py" && base != "conftest.py"
		}
		return false
	}
	allTests := map[string]bool{}
	for _, p := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if isTest(p) {
			allTests[p] = true
			total[topDir(p)]++
		}
	}

	// The run's outcomes, collapsed from cases (file x scope x variant) to
	// files: a file is attempted when any case of it ran, clean when every
	// case ran clean.
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var outcomes map[string]struct {
		Harness  string `json:"harness"`
		Subtests int    `json:"subtests"`
		Failed   int    `json:"failed"`
	}
	if err := json.Unmarshal(raw, &outcomes); err != nil {
		t.Fatal(err)
	}
	attempted := map[string]bool{}
	dirty := map[string]bool{}
	for key, o := range outcomes {
		file := key
		if i := strings.Index(file, " ["); i >= 0 {
			file = file[:i]
		}
		if i := strings.Index(file, "?"); i >= 0 {
			file = file[:i]
		}
		attempted[file] = true
		if o.Harness != "OK" || o.Failed > 0 {
			dirty[file] = true
		}
	}
	run := map[string]int{}
	clean := map[string]int{}
	for f := range attempted {
		if !allTests[f] {
			// A case for a file the manifest heuristic does not count —
			// surface it rather than silently skewing the totals.
			t.Logf("note: ran %s, which the manifest heuristic does not count", f)
			continue
		}
		run[topDir(f)]++
		if !dirty[f] {
			clean[topDir(f)]++
		}
	}

	var dirs []string
	for d := range total {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool { return total[dirs[i]] > total[dirs[j]] })
	var sumTotal, sumRun, sumClean int
	t.Logf("%-28s %7s %7s %7s %8s", "directory", "tests", "run", "clean", "coverage")
	for _, d := range dirs {
		sumTotal += total[d]
		sumRun += run[d]
		sumClean += clean[d]
		t.Logf("%-28s %7d %7d %7d %7.2f%%", d, total[d], run[d], clean[d],
			100*float64(clean[d])/float64(total[d]))
	}
	t.Logf("%-28s %7d %7d %7d %7.2f%%", "TOTAL", sumTotal, sumRun, sumClean,
		100*float64(sumClean)/float64(max(sumTotal, 1)))
	fmt.Printf("WPT coverage (whole suite, unrun counts as failed): %d/%d test files = %.2f%%\n",
		sumClean, sumTotal, 100*float64(sumClean)/float64(max(sumTotal, 1)))
}
