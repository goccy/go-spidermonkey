// Package test262 runs the official ECMAScript conformance suite
// (https://github.com/tc39/test262, vendored as the git submodule ./suite)
// against go-spidermonkey.
//
// The run is opt-in because it takes minutes, not seconds:
//
//	git submodule update --init --depth 1 test262/suite
//	TEST262=1 go test ./test262/ -v -timeout 2h
//
// Every test runs in a fresh Interpreter (its own realm), in both sloppy and
// strict mode unless the test's metadata says otherwise, with the suite's
// harness files prepended and a watchdog interrupt as the timeout. Async tests
// are judged by the Test262:AsyncTestComplete convention on captured stdout.
//
// Tests this embedding cannot host are skipped, not failed, and the skip is
// accounted per reason: module-flagged tests (no module loader is wired),
// tests needing $262 host hooks (createRealm, detachArrayBuffer, gc, agent),
// and features this engine build excludes (Intl/ICU, Atomics/SharedArrayBuffer,
// Temporal). The intl402/ directory is excluded wholesale for the same reason.
//
// The checked-in expectations.json lists the tests known to fail; the suite
// FAILS on any regression (an unexpected failure) and on any stale expectation
// (an expected failure that now passes). Regenerate it with TEST262_UPDATE=1.
package test262

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

const (
	suiteDir         = "suite"
	expectationsFile = "expectations.json"
)

// featuresUnsupported are test262 feature tags this embedding cannot run, and
// why. Feature tags are declared per test in its YAML frontmatter.
var featuresUnsupported = map[string]string{
	// No threads in the wasm build: no agents, no shared-memory *blocking*.
	// Single-agent SharedArrayBuffer/Atomics themselves work; the tests
	// skipped here are the ones that spawn agents (they use atomicsHelper.js
	// / $262.agent, caught below) or require a blocking main agent.
	"Atomics.pause":     "no threads in the wasm build",
	"Atomics.waitAsync": "no threads in the wasm build",

	// Not shipped in upstream SpiderMonkey either.
	"ShadowRealm": "not shipped in SpiderMonkey",
}

// pathsUnsupported skips tests, by path prefix, that depend on Unicode data
// the ICU-less engine build does not carry and that no feature tag identifies.
var pathsUnsupported = map[string]string{}

// $262 members that require host hooks we do not have. A test whose source
// touches any of these is skipped (feature tags catch most, this catches the
// rest).
var dollar262Unsupported = []string{
	"$262.agent",
	"$262.AbstractModuleSource",
}

// $262 is installed natively via Interpreter.InstallTest262Hooks.
const prelude = ""

type meta struct {
	flags    map[string]bool
	includes []string
	features []string
	negType  string // negative.type: the constructor name the test must throw
	negPhase string // negative.phase: parse | resolution | runtime
}

// parseMeta extracts the YAML frontmatter between /*--- and ---*/. It parses
// just the subset test262 uses: scalar keys, inline arrays, dash lists for
// flags/includes/features, and the nested negative: block.
func parseMeta(src string) meta {
	m := meta{flags: map[string]bool{}}
	start := strings.Index(src, "/*---")
	if start < 0 {
		return m
	}
	end := strings.Index(src[start:], "---*/")
	if end < 0 {
		return m
	}
	block := src[start+5 : start+end]
	// A few tests use CR or CRLF line terminators for the whole file (that is
	// what they test). Normalize the frontmatter only; the source is untouched.
	block = strings.ReplaceAll(block, "\r\n", "\n")
	block = strings.ReplaceAll(block, "\r", "\n")

	appendTo := func(key string, items []string) {
		switch key {
		case "flags":
			for _, it := range items {
				m.flags[it] = true
			}
		case "includes":
			m.includes = append(m.includes, items...)
		case "features":
			m.features = append(m.features, items...)
		}
	}
	splitInline := func(s string) []string {
		s = strings.Trim(strings.TrimSpace(s), "[]")
		var out []string
		for _, f := range strings.Split(s, ",") {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, f)
			}
		}
		return out
	}

	var key string
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		indented := line != "" && (line[0] == ' ' || line[0] == '\t')
		switch {
		case !indented && strings.Contains(line, ":"):
			k, v, _ := strings.Cut(line, ":")
			key = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			if v != "" && strings.HasPrefix(v, "[") {
				appendTo(key, splitInline(v))
			}
		case strings.HasPrefix(trimmed, "- "):
			appendTo(key, []string{strings.TrimSpace(trimmed[2:])})
		case key == "negative" && indented:
			k, v, ok := strings.Cut(trimmed, ":")
			if !ok {
				continue
			}
			switch strings.TrimSpace(k) {
			case "phase":
				m.negPhase = strings.TrimSpace(v)
			case "type":
				m.negType = strings.TrimSpace(v)
			}
		}
	}
	return m
}

// loadHarness reads every file under suite/harness once, keyed by
// slash-separated relative path ("assert.js", "sm/non262-Math-shell.js", ...)
// exactly as tests name them in `includes:`.
func loadHarness(t *testing.T) map[string]string {
	t.Helper()
	dir := filepath.Join(suiteDir, "harness")
	h := make(map[string]string)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".js") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		h[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("read harness dir: %v", err)
	}
	return h
}

type outcome struct {
	path   string
	status string // "pass" | "fail" | "skip"
	reason string // skip reason or failure detail (one line)
}

// skipReason decides whether this embedding can host the test at all.
func skipReason(relPath, src string, m meta) string {
	for prefix, why := range pathsUnsupported {
		if strings.HasPrefix(relPath, prefix) {
			return "path " + prefix + " (" + why + ")"
		}
	}
	if m.flags["CanBlockIsTrue"] {
		return "agent blocking (no threads)"
	}
	for _, f := range m.features {
		if why, ok := featuresUnsupported[f]; ok {
			return "feature " + f + " (" + why + ")"
		}
	}
	for _, inc := range m.includes {
		if inc == "atomicsHelper.js" {
			return "include " + inc + " (agents need threads)"
		}
	}
	for _, member := range dollar262Unsupported {
		if strings.Contains(src, member) {
			return member + " (host hook not exposed)"
		}
	}
	return ""
}

// buildSource assembles the full script for one run of the test.
func buildSource(src string, m meta, harness map[string]string, strict bool) string {
	var sb strings.Builder
	if strict {
		sb.WriteString("\"use strict\";\n")
	}
	if !m.flags["raw"] {
		sb.WriteString(prelude)
		sb.WriteString(harness["assert.js"])
		sb.WriteString(harness["sta.js"])
		if m.flags["async"] {
			sb.WriteString(harness["doneprintHandle.js"])
		}
		for _, inc := range m.includes {
			sb.WriteString(harness[inc])
		}
	}
	sb.WriteString(src)
	return sb.String()
}

// evalOne runs one assembled script (or module) in a fresh interpreter with a
// watchdog, converting panics from wasm traps into failures.
//
// Module fixtures resolve lazily, the way a host loader's retry protocol
// works: a run that fails with "module not registered: X" (in the error, or in
// captured stdout for async dynamic imports) has X read from the suite and the
// whole test re-run in a FRESH interpreter with every fixture discovered so
// far pre-registered — registration is per-interpreter, and re-running a
// script in a used global would trip redeclaration errors.
func evalOne(src string, relPath string, asModule bool, timeout time.Duration) (spidermonkey.EvalResult, error) {
	spec := filepath.ToSlash(relPath)
	fixtures := map[string]string{}
	var r spidermonkey.EvalResult
	var err error
	for attempt := 0; attempt < 64; attempt++ {
		r, err = evalOnce(src, spec, asModule, fixtures, timeout)
		if err != nil {
			return r, err
		}
		missing, ok := missingModule(r.Error)
		if !ok {
			missing, ok = missingModule(r.Stdout)
		}
		if r.Ok && !ok {
			return r, nil
		}
		if !ok {
			return r, nil
		}
		if _, seen := fixtures[missing]; seen {
			return r, nil // registered but still failing: report as-is
		}
		// A classic script has no referrer private, so its dynamic imports
		// resolve as bare names with any leading ../ folded away. Try the
		// suite root, then every ancestor directory of the test.
		b, rerr := os.ReadFile(filepath.Join(suiteDir, "test", filepath.FromSlash(missing)))
		for d := filepath.Dir(filepath.FromSlash(spec)); rerr != nil && d != "." && d != "/"; d = filepath.Dir(d) {
			b, rerr = os.ReadFile(filepath.Join(suiteDir, "test", d, filepath.FromSlash(missing)))
		}
		if rerr != nil {
			return r, nil // unresolvable: report the module error as-is
		}
		fixtures[missing] = string(b)
	}
	return r, nil
}

func evalOnce(src, spec string, asModule bool, fixtures map[string]string, timeout time.Duration) (r spidermonkey.EvalResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	i, err := spidermonkey.NewInterpreter(spidermonkey.Config{})
	if err != nil {
		return spidermonkey.EvalResult{}, fmt.Errorf("NewInterpreter: %w", err)
	}
	defer i.Close()
	if err := i.InstallTest262Hooks(); err != nil {
		return spidermonkey.EvalResult{}, fmt.Errorf("InstallTest262Hooks: %w", err)
	}
	for name, fsrc := range fixtures {
		// A fixture that fails to compile is left unregistered; the re-run
		// then reports the real import failure.
		if _, err := i.RegisterModule(name, fsrc); err != nil {
			return spidermonkey.EvalResult{}, err
		}
	}
	ip, err := i.PrepareInterrupt()
	if err != nil {
		return spidermonkey.EvalResult{}, fmt.Errorf("PrepareInterrupt: %w", err)
	}
	watchdog := time.AfterFunc(timeout, ip.Fire)
	defer watchdog.Stop()
	if asModule {
		return i.EvalModule(spec, src)
	}
	return i.Eval(src)
}

// missingModule extracts the specifier from a "module not registered: X"
// error, which may be wrapped in an exception stringification.
func missingModule(errText string) (string, bool) {
	const marker = "module not registered: "
	idx := strings.Index(errText, marker)
	if idx < 0 {
		return "", false
	}
	rest := errText[idx+len(marker):]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	return strings.TrimSpace(rest), true
}

// judge decides pass/fail for one run's result.
func judge(m meta, r spidermonkey.EvalResult, err error) (ok bool, detail string) {
	if err != nil {
		return false, "host: " + err.Error()
	}
	if m.negType != "" {
		if r.Ok {
			return false, "expected " + m.negType + " (" + m.negPhase + "), but completed normally"
		}
		if !strings.Contains(r.Error, m.negType) {
			return false, "expected " + m.negType + ", got: " + firstLine(r.Error)
		}
		return true, ""
	}
	if !r.Ok {
		return false, firstLine(r.Error)
	}
	if m.flags["async"] {
		if strings.Contains(r.Stdout, "Test262:AsyncTestComplete") {
			return true, ""
		}
		return false, "async test did not complete: " + firstLine(r.Stdout+r.Stderr)
	}
	return true, ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// runTest executes one test file in every required mode.
func runTest(relPath string, src string, m meta, harness map[string]string, timeout time.Duration) outcome {
	if reason := skipReason(filepath.ToSlash(relPath), src, m); reason != "" {
		return outcome{path: relPath, status: "skip", reason: reason}
	}
	if m.flags["module"] {
		r, err := evalOne(buildSource(src, m, harness, false), relPath, true, timeout)
		if ok, detail := judge(m, r, err); !ok {
			return outcome{path: relPath, status: "fail", reason: "[module] " + detail}
		}
		return outcome{path: relPath, status: "pass"}
	}
	var modes []bool // strict?
	switch {
	case m.flags["raw"], m.flags["noStrict"]:
		modes = []bool{false}
	case m.flags["onlyStrict"]:
		modes = []bool{true}
	default:
		modes = []bool{false, true}
	}
	for _, strict := range modes {
		r, err := evalOne(buildSource(src, m, harness, strict), relPath, false, timeout)
		if ok, detail := judge(m, r, err); !ok {
			mode := "sloppy"
			if strict {
				mode = "strict"
			}
			return outcome{path: relPath, status: "fail", reason: "[" + mode + "] " + detail}
		}
	}
	return outcome{path: relPath, status: "pass"}
}

func TestTest262(t *testing.T) {
	if os.Getenv("TEST262") == "" {
		t.Skip("set TEST262=1 to run the conformance suite (takes minutes); see the package comment")
	}
	if _, err := os.Stat(filepath.Join(suiteDir, "test")); err != nil {
		t.Skip("test262 suite not present: git submodule update --init --depth 1 test262/suite")
	}
	timeout := 120 * time.Second
	if v := os.Getenv("TEST262_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("TEST262_TIMEOUT: %v", err)
		}
		timeout = d
	}
	filter := os.Getenv("TEST262_FILTER")
	harness := loadHarness(t)

	var paths []string
	root := filepath.Join(suiteDir, "test")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(rel, ".js") || strings.Contains(rel, "_FIXTURE") {
			return nil
		}
		if filter != "" && !strings.Contains(rel, filter) {
			return nil
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk suite: %v", err)
	}
	sort.Strings(paths)
	t.Logf("running %d tests on %d workers", len(paths), runtime.GOMAXPROCS(0))

	jobs := make(chan string)
	results := make(chan outcome)
	var wg sync.WaitGroup
	for w := 0; w < runtime.GOMAXPROCS(0); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rel := range jobs {
				b, err := os.ReadFile(filepath.Join(root, rel))
				if err != nil {
					results <- outcome{path: rel, status: "fail", reason: "read: " + err.Error()}
					continue
				}
				src := string(b)
				results <- runTest(rel, src, parseMeta(src), harness, timeout)
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

	counts := map[string]map[string]int{} // area -> status -> n
	skipReasons := map[string]int{}
	failures := map[string]string{}
	done := 0
	start := time.Now()
	for o := range results {
		area := strings.SplitN(o.path, string(filepath.Separator), 2)[0]
		if counts[area] == nil {
			counts[area] = map[string]int{}
		}
		counts[area][o.status]++
		switch o.status {
		case "skip":
			skipReasons[o.reason]++
		case "fail":
			failures[filepath.ToSlash(o.path)] = o.reason
		}
		if done++; done%5000 == 0 {
			t.Logf("%d/%d done in %v", done, len(paths), time.Since(start).Round(time.Second))
		}
	}

	// Report.
	var areas []string
	for a := range counts {
		areas = append(areas, a)
	}
	sort.Strings(areas)
	totalPass, totalRun := 0, 0
	for _, a := range areas {
		c := counts[a]
		run := c["pass"] + c["fail"]
		totalPass += c["pass"]
		totalRun += run
		t.Logf("%-12s pass %5d / run %5d (skip %5d)  %.2f%%",
			a, c["pass"], run, c["skip"], 100*float64(c["pass"])/float64(max(run, 1)))
	}
	t.Logf("TOTAL        pass %5d / run %5d  %.2f%%  in %v",
		totalPass, totalRun, 100*float64(totalPass)/float64(max(totalRun, 1)), time.Since(start).Round(time.Second))
	var reasons []string
	for r := range skipReasons {
		reasons = append(reasons, r)
	}
	sort.Slice(reasons, func(i, j int) bool { return skipReasons[reasons[i]] > skipReasons[reasons[j]] })
	for _, r := range reasons {
		t.Logf("skip %6d  %s", skipReasons[r], r)
	}

	if report := os.Getenv("TEST262_REPORT"); report != "" {
		writeJSON(t, report, failures)
	}

	// Compare against the checked-in expectations: fail on regressions and on
	// stale expectations, so CI is green exactly when the delta is the
	// documented one.
	if os.Getenv("TEST262_UPDATE") != "" {
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
