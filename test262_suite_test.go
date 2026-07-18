// This file runs the official ECMAScript conformance suite
// (https://github.com/tc39/test262, vendored as the git submodule
// ./test262/suite) against go-spidermonkey's PUBLIC API.
//
// The run is opt-in because it takes minutes, not seconds:
//
//	git submodule update --init --depth 1 test262/suite
//	TEST262=1 go test . -run TestTest262 -v -timeout 2h
//
// Every test runs in a fresh interpreter (its own realm), in both sloppy and
// strict mode unless the test's metadata says otherwise, with the suite's
// harness files prepended and a context deadline as the watchdog. Async tests
// are judged by the Test262:AsyncTestComplete convention on captured stdout.
//
// The host surface is assembled the way any embedder would assemble it:
//   - module fixtures load on demand through SetModuleLoader,
//   - $262.agent is the Go adapter over the public Agents API
//     (see agent262_test.go) — real agents on real goroutine-backed threads,
//   - only the $262 engine hooks that cannot exist outside the engine
//     (createRealm, detachArrayBuffer, gc, IsHTMLDDA, evalScript) come from
//     the test-only InstallTest262Hooks seam (export_test.go).
//
// Tests this embedding cannot host are skipped, not failed, and the skip is
// accounted per reason. The checked-in test262/expectations.json lists the
// tests known to fail; the suite FAILS on any regression (an unexpected
// failure) and on any stale expectation (an expected failure that now
// passes). Regenerate it with TEST262_UPDATE=1.
package spidermonkey_test

import (
	"context"
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
	"unicode"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

const (
	suiteDir         = "test262/suite"
	expectationsFile = "test262/expectations.json"
)

// featureProbes maps a test262 feature tag to a JS expression that is true when
// THIS engine build actually provides the feature. The suite probes each once
// at startup (detectAbsentFeatures) and skips ONLY the features that come back
// absent — never a hand-maintained "not shipped" assertion, which silently goes
// stale. (Atomics.pause, long skipped as "not shipped", is in fact shipped in
// the current build; the probe now runs its tests instead of hiding them.)
//
// An absent feature that a stock SpiderMonkey shell also lacks (ShadowRealm is
// behind an off-by-default pref there) is a fair skip — neither engine counts
// those tests. An absent feature that upstream DOES ship would be an engine
// build gap to fix, not something to hide; the same-commit jsshell comparison
// is what surfaces that.
var featureProbes = map[string]string{
	"ShadowRealm":   `typeof ShadowRealm !== "undefined"`,
	"Atomics.pause": `typeof Atomics === "object" && typeof Atomics.pause === "function"`,
}

// featuresUnsupported is populated by detectAbsentFeatures at the start of the
// run: feature tag -> why it is skipped. Empty until then.
var featuresUnsupported = map[string]string{}

// detectAbsentFeatures probes every gated feature once against a fresh
// interpreter and returns the skip-reason map for those this build does not
// provide, so the skip set reflects the engine's real surface.
func detectAbsentFeatures(t *testing.T) map[string]string {
	t.Helper()
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("feature probe: New: %v", err)
	}
	defer js.Close()
	absent := map[string]string{}
	for feat, expr := range featureProbes {
		r, err := js.Eval(context.Background(), expr)
		if err != nil {
			t.Fatalf("feature probe %s: %v", feat, err)
		}
		if r.Error != nil || !r.Value.Bool() {
			absent[feat] = "not provided by this engine build"
		}
	}
	return absent
}

// pathsUnsupported skips tests, by path prefix, that depend on Unicode data
// the engine build does not carry and that no feature tag identifies.
var pathsUnsupported = map[string]string{}

// $262 members that require host hooks we do not have. A test whose source
// touches any of these is skipped (feature tags catch most, this catches the
// rest).
var dollar262Unsupported = []string{
	"$262.AbstractModuleSource",
}

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
	if m.flags["CanBlockIsFalse"] {
		// These tests only apply where the main agent CANNOT block; this
		// embedding sets JS_SetFutexCanWait on every context (shell-like, not
		// a browser main thread), so upstream's shell skips them too.
		return "flag CanBlockIsFalse (main agent can block here)"
	}
	for _, f := range m.features {
		if why, ok := featuresUnsupported[f]; ok {
			return "feature " + f + " (" + why + ")"
		}
	}
	for _, member := range dollar262Unsupported {
		if strings.Contains(src, member) {
			return member + " (host hook not exposed)"
		}
	}
	return ""
}

// buildSources assembles the harness prelude and the test source for one run.
// They are evaluated as SEPARATE scripts (test262 INTERPRETING.md): the
// test's "use strict" directive scopes to the test alone, and sloppy-mode
// harness helpers (the staging/sm shells rely on this) stay sloppy even when
// the test runs strict.
func buildSources(src string, m meta, harness map[string]string, strict bool) (string, string) {
	var hb strings.Builder
	if !m.flags["raw"] {
		hb.WriteString(harness["assert.js"])
		hb.WriteString(harness["sta.js"])
		if m.flags["async"] {
			hb.WriteString(harness["doneprintHandle.js"])
		}
		for _, inc := range m.includes {
			hb.WriteString(harness[inc])
		}
	}
	test := src
	if strict {
		test = "\"use strict\";\n" + src
	}
	return hb.String(), test
}

// runResult is one evaluation's outcome in the form judge consumes.
type runResult struct {
	ok      bool
	errText string
	stdout  string
	stderr  string
}

// Linear-memory caps the runner uses. Most tests fit the first comfortably;
// a handful of parser-stress tests (an 8 MB source in one eval) need the
// second. Linear memory only ever grows, and a large cap makes every
// interpreter slower to build (the allocation is sized to the cap), so pay
// for the headroom only where a test actually ran out.
var memCaps = []int{256 << 20, 1 << 30}

// evalOne runs one assembled script (or module) in a fresh interpreter with a
// context deadline as the watchdog, converting panics from wasm traps into
// failures.
func evalOne(harnessSrc, testSrc, relPath string, asModule bool, timeout time.Duration) (runResult, error) {
	r, err := evalOnce(harnessSrc, testSrc, relPath, asModule, timeout, memCaps[0])
	// "out of memory" can surface as the thrown error, or — when the test
	// CATCHES it, as the parser-stress tests do — inside the assertion text
	// it then fails with. Either way the verdict is "this test needs more
	// linear memory than the default cap", so re-run it with the big one.
	if err == nil && !r.ok && strings.Contains(r.errText, "out of memory") {
		return evalOnce(harnessSrc, testSrc, relPath, asModule, timeout, memCaps[1])
	}
	return r, err
}

// fixtureLoader serves module sources from the suite on demand — the loader
// half of the host's module protocol. The engine resolves ./ and ../ against
// the importing module's specifier before calling us, so a resolved specifier
// maps straight under suite/test; a dynamic import from a classic SCRIPT has
// no referrer, so its specifier arrives bare — try every ancestor directory
// of the test for those.
func fixtureLoader(spec string) func(cfg spidermonkey.Config, specifier, referrer string) (string, error) {
	return func(cfg spidermonkey.Config, specifier, referrer string) (string, error) {
		b, err := os.ReadFile(filepath.Join(suiteDir, "test", filepath.FromSlash(specifier)))
		for d := filepath.Dir(filepath.FromSlash(spec)); err != nil && d != "." && d != "/"; d = filepath.Dir(d) {
			b, err = os.ReadFile(filepath.Join(suiteDir, "test", d, filepath.FromSlash(specifier)))
		}
		if err != nil {
			return "", fmt.Errorf("module not found: %s", specifier)
		}
		return string(b), nil
	}
}

func evalOnce(harnessSrc, testSrc, relPath string, asModule bool, timeout time.Duration, memBytes int) (rr runResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	spec := filepath.ToSlash(relPath)

	js, err := spidermonkey.New(spidermonkey.Config{MaxMemoryBytes: memBytes})
	if err != nil {
		return runResult{}, fmt.Errorf("New: %w", err)
	}
	defer js.Close()
	// The WHOLE $262 (createRealm, detachArrayBuffer, gc, evalScript,
	// IsHTMLDDA, setTimeout, agent) composed host-side on the public API +
	// engine primitives — no engine-side test262 hook exists.
	h262, err := newHarness262(js)
	if err != nil {
		return runResult{}, fmt.Errorf("newHarness262: %w", err)
	}
	// Guest output (console is a host func) is collected in h262; surface it on
	// every exit path so judge can see the async marker and any diagnostics.
	defer func() { rr.stdout = h262.stdout(); rr.stderr = h262.stderr() }()
	js.SetModuleLoader(fixtureLoader(spec))

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// A context deadline surfaces as (partial result, ctx.Err()); the partial
	// envelope carries "JS execution interrupted", which is the failure the
	// judge should see — not a host error. Guest output lands in h262 (console
	// is a host func), read at the end.
	fold := func(errv error, jsErr error) (runResult, error) {
		if errv != nil && jsErr == nil {
			return rr, errv
		}
		rr.ok = jsErr == nil
		if jsErr != nil {
			rr.errText = jsErr.Error()
		}
		return rr, nil
	}

	// The harness runs as its own (sloppy) script; the test as another, so a
	// "use strict" directive on the test cannot leak into harness helpers.
	if harnessSrc != "" {
		hr, herr := js.Eval(ctx, harnessSrc)
		if out, ferr := fold(herr, hr.Error); ferr != nil || !out.ok {
			out.errText = "harness: " + out.errText
			return out, ferr
		}
	}
	if asModule {
		mr, merr := js.EvalModule(ctx, spec, testSrc)
		if out, ferr := fold(merr, mr.Error); ferr != nil || !out.ok {
			return out, ferr
		}
	} else {
		er, eerr := js.Eval(ctx, testSrc)
		if out, ferr := fold(eerr, er.Error); ferr != nil || !out.ok {
			return out, ferr
		}
	}

	// Drive the event loop: interleave host setTimeout callbacks with
	// engine job-queue steps until the async marker lands, everything is idle
	// (no engine work pending, no timer armed), or the deadline passes. A
	// waitAsync resolution crosses threads as a Dispatchable and surfaces as
	// engine-pending ("2"); a setTimeout(fn,0) is a host timer the engine
	// knows nothing about, so the harness fires it here.
	marker := func() bool { return strings.Contains(h262.stdout(), "Test262:AsyncTest") }
	for !marker() && ctx.Err() == nil {
		ranTimer := h262.fireDueTimers(ctx)

		pr, perr := spidermonkey.PumpJobs(ctx, js)
		if perr != nil {
			if ctx.Err() != nil {
				break
			}
			return rr, perr
		}
		if pr.Err != nil {
			rr.ok = false
			rr.errText = pr.Err.Error()
			return rr, nil
		}
		if marker() {
			break
		}
		if ranTimer {
			continue // a timer fired; it may have queued jobs
		}
		if pr.Code == "0" && !h262.pendingTimers() {
			break // idle: no engine work, no armed timer
		}
		// "2" (engine work pending) or a timer armed but not yet due.
		select {
		case <-ctx.Done():
		case <-time.After(time.Millisecond):
		}
	}
	return rr, nil
}

// judge decides pass/fail for one run's result.
func judge(m meta, r runResult, err error) (ok bool, detail string) {
	if err != nil {
		return false, "host: " + err.Error()
	}
	if m.negType != "" {
		phase := m.negPhase
		if phase == "" {
			phase = "runtime"
		}
		if r.ok {
			return false, "expected " + m.negType + " at " + phase + ", but completed normally"
		}
		// A parse- or resolution-phase test whose body actually executed trips
		// the harness $DONOTEVALUATE guard, which throws this sentinel. The code
		// running AT ALL is a phase violation — the error was required before
		// evaluation — regardless of what type a later statement would raise. So
		// this must fail even though the sentinel is not the expected type; the
		// old substring match happened to reject it too, but for the wrong
		// reason and with a misleading message.
		if strings.Contains(r.errText, "This statement should not be evaluated") {
			return false, "phase " + phase + ": source executed, but " + m.negType +
				" was required before evaluation"
		}
		// Match the thrown value's CONSTRUCTOR NAME exactly, not a substring of
		// the whole message+stack. Substring matching is lenient: it passes a
		// test that threw the WRONG error whenever the expected type name merely
		// appears in the message (e.g. assert.throws's "Expected a TypeError..."
		// Test262Error) or in a stack frame.
		if got := errConstructor(r.errText); got != m.negType {
			if got == "" {
				got = "a non-Error value"
			}
			return false, "expected " + m.negType + ", got " + got + ": " + firstLine(r.errText)
		}
		return true, ""
	}
	if !r.ok {
		return false, firstLine(r.errText)
	}
	if m.flags["async"] {
		if strings.Contains(r.stdout, "Test262:AsyncTestComplete") {
			return true, ""
		}
		return false, "async test did not complete: " + firstLine(r.stdout+r.stderr)
	}
	return true, ""
}

// errConstructor extracts the constructor name of a thrown value from its
// engine stringification: "Name: message\n<stack>", or a bare "Name" when the
// message is empty. A thrown non-Error (string, number, the $DONOTEVALUATE
// sentinel) has no such identifier prefix and yields "".
func errConstructor(errText string) string {
	line := firstLine(errText)
	// "Name: message", bare "Name", or "Name:" when the message is empty and
	// firstLine trimmed the trailing space. Split on the first colon, not
	// ": ", so an empty-message error still yields its constructor name.
	if i := strings.IndexByte(line, ':'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if isIdentifier(line) {
		return line
	}
	return ""
}

// isIdentifier reports whether s is a single JS-identifier-shaped token (the
// shape every standard error constructor name has).
func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || r == '$' || unicode.IsLetter(r):
		case i > 0 && unicode.IsDigit(r):
		default:
			return false
		}
	}
	return true
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
		hsrc, tsrc := buildSources(src, m, harness, false)
		r, err := evalOne(hsrc, tsrc, relPath, true, timeout)
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
		hsrc, tsrc := buildSources(src, m, harness, strict)
		r, err := evalOne(hsrc, tsrc, relPath, false, timeout)
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
		t.Skip("set TEST262=1 to run the conformance suite (takes minutes); see the file comment")
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
	// Ground the feature-skip set in what this engine build actually exposes,
	// rather than a static list that drifts out of date.
	featuresUnsupported = detectAbsentFeatures(t)
	var probed []string
	for f := range featuresUnsupported {
		probed = append(probed, f)
	}
	sort.Strings(probed)
	t.Logf("skipping absent features: %v", probed)

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
				// Hard deadline around the whole test: the in-engine
				// watchdog interrupt cannot break a main agent blocked in
				// Atomics.wait (the interrupt check runs at loop headers,
				// not inside a futex park), so a test whose expected notify
				// never arrives would wedge this worker forever. Abandon
				// it — the goroutine and its interpreter leak, bounded by
				// the number of such tests — and report a failure that
				// names the real problem.
				if os.Getenv("TEST262_TRACE") != "" {
					fmt.Fprintf(os.Stderr, "START %s\n", rel)
				}
				ch := make(chan outcome, 1)
				go func() { ch <- runTest(rel, src, parseMeta(src), harness, timeout) }()
				select {
				case o := <-ch:
					if os.Getenv("TEST262_TRACE") != "" {
						fmt.Fprintf(os.Stderr, "END %s\n", rel)
					}
					results <- o
				case <-time.After(timeout + 30*time.Second):
					results <- outcome{path: rel, status: "fail",
						reason: "hard timeout: uninterruptible block (Atomics.wait never notified); abandoned"}
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
