// octane_test.go — the Octane 2.0 lane of the engine comparison.
//
// Octane (https://github.com/chromium/octane) is the de-facto suite for
// comparing embedded JS engines: Richards, DeltaBlue, Crypto, RayTrace,
// EarleyBoyer, RegExp, Splay, NavierStokes, pdf.js, Mandreel, GB
// Emulator, CodeLoad, Box2D, zlib and a TypeScript compiler run —
// covering property access, GC, regexp, parsing and numeric kernels far
// beyond what fib/loop microbenches see.
//
// The suite sources are NOT vendored; fetch once with
//
//	./testdata/fetch-octane.sh
//
// and the test skips (with that instruction) when they are absent.
//
// Engines: go-spidermonkey (this project), goja and modernc.org/quickjs
// (the pure-Go peers), plus the host's node when available (the
// JIT-enabled ceiling, not a peer). Each suite runs in a FRESH VM so a
// crash or feature gap in one suite cannot poison another, and errors
// are reported per suite instead of failing the harness — this is a
// measurement, not a conformance gate.
//
//	GOARCH=arm64 go test -c -o octane.test . && ./octane.test \
//	  -test.run TestOctane -test.v -test.timeout 4h
//
// RUN THE HARNESS NATIVELY and record the arch: on Apple Silicon hosts
// whose default Go toolchain is darwin/amd64, a plain `go test` runs
// every Go engine under Rosetta and understates them all (goja most) —
// while external lanes (node) stay native, silently skewing ratios.
//
// OCTANE_SUITES=name1,name2 | all   selects suites (default: the classic
// nine that avoid multi-megabyte parse workloads); OCTANE_ENGINES
// likewise selects engines. Scores are Octane points, higher is better;
// the overall line is the geometric mean over the suites that ran, and
// is labelled partial unless every selected suite scored.
package jsbench

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
	spidermonkey "github.com/goccy/go-spidermonkey"
	quickjs "modernc.org/quickjs"
)

const octaneDir = "testdata/octane"

// suiteTimeout bounds one suite on one engine. Interpreters are 10-50x
// slower than JITs here; the big-parse suites need the headroom.
const suiteTimeout = 10 * time.Minute

// octaneSuites maps suite name → source files, in run.js load order.
var octaneSuites = []struct {
	name  string
	files []string
}{
	{"Richards", []string{"richards.js"}},
	{"DeltaBlue", []string{"deltablue.js"}},
	{"Crypto", []string{"crypto.js"}},
	{"RayTrace", []string{"raytrace.js"}},
	{"EarleyBoyer", []string{"earley-boyer.js"}},
	{"RegExp", []string{"regexp.js"}},
	{"Splay", []string{"splay.js"}},
	{"NavierStokes", []string{"navier-stokes.js"}},
	{"CodeLoad", []string{"code-load.js"}},
	// The big-parse / typed-array heavy suites, opt-in via
	// OCTANE_SUITES=all (multi-megabyte sources; minutes per engine on
	// interpreters).
	{"PdfJS", []string{"pdfjs.js"}},
	{"Mandreel", []string{"mandreel.js"}},
	{"Gameboy", []string{"gbemu-part1.js", "gbemu-part2.js"}},
	{"Box2D", []string{"box2d.js"}},
	{"zlib", []string{"zlib.js", "zlib-data.js"}},
	{"Typescript", []string{"typescript.js", "typescript-input.js", "typescript-compiler.js"}},
}

// classicNine is the default subset: every suite whose source parses in
// well under a second even on an interpreter.
var classicNine = []string{
	"Richards", "DeltaBlue", "Crypto", "RayTrace", "EarleyBoyer",
	"RegExp", "Splay", "NavierStokes", "CodeLoad",
}

// octaneDriver runs the (single) registered suite and returns
// "name=score;...;Octane=total" as the script completion value.
const octaneDriver = `
(function () {
	var out = [];
	BenchmarkSuite.RunSuites({
		NotifyResult: function (name, result) { out.push(name + "=" + result); },
		NotifyError: function (name, error) { out.push(name + "=ERROR:" + String(error)); },
		NotifyScore: function (score) { out.push("Octane=" + score); },
	});
	return out.join(";");
})()
`

// octanePrelude provides the d8-shell surface some suites expect —
// zlib's emscripten preamble selects its shell environment by
// `typeof print` and then captures the shell's `read` (under node it
// selects process.stdout/require instead, so the shims are inert
// there). Output is benchmark noise, and `read` is only captured, never
// called — zlib's input data is embedded — so a throwing stub keeps an
// accidental future call loud.
const octanePrelude = `var print = typeof print === "undefined" ? function () {} : print;
var read = typeof read === "undefined" ? function () { throw new Error("shell read() not supported"); } : read;
` + "\n"

// octaneSource assembles the prelude + base.js + the suite files + the
// driver.
func octaneSource(tb testing.TB, files []string) string {
	var b strings.Builder
	b.WriteString(octanePrelude)
	for _, f := range append([]string{"base.js"}, files...) {
		data, err := os.ReadFile(filepath.Join(octaneDir, f))
		if err != nil {
			tb.Fatalf("read %s: %v", f, err)
		}
		b.Write(data)
		b.WriteString("\n")
	}
	b.WriteString(octaneDriver)
	return b.String()
}

// parseSuiteScore extracts this suite's score from the driver output.
// One suite per VM, so the first non-Octane entry is the score line.
func parseSuiteScore(out string) (float64, error) {
	for _, part := range strings.Split(out, ";") {
		name, val, ok := strings.Cut(part, "=")
		if !ok || name == "Octane" {
			continue
		}
		if strings.HasPrefix(val, "ERROR:") {
			return 0, fmt.Errorf("%s: %s", name, strings.TrimPrefix(val, "ERROR:"))
		}
		var score float64
		if _, err := fmt.Sscanf(val, "%f", &score); err != nil {
			return 0, fmt.Errorf("%s: unparsable score %q", name, val)
		}
		return score, nil
	}
	return 0, fmt.Errorf("no suite result in %q", out)
}

// Engine lanes. Each runs one assembled source in a fresh VM and
// returns the raw driver output.

func runSpiderMonkey(src string) (string, error) {
	// Octane's GC/parse-heavy suites (Splay, CodeLoad, the big-parse
	// set) exceed the default wasm-memory cap; give the guest the same
	// order of headroom a browser tab would have. Untouched pages never
	// page in, so this costs nothing on the small suites.
	js, err := spidermonkey.New(spidermonkey.Config{MaxMemoryBytes: 1 << 30})
	if err != nil {
		return "", err
	}
	defer js.Close()
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer cancel()
	r, err := js.Eval(ctx, src)
	if err != nil {
		return "", err
	}
	if r.Error != nil {
		return "", r.Error
	}
	return r.Value.String(), nil
}

func runGoja(src string) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("goja panic: %v", r)
		}
	}()
	vm := goja.New()
	timer := time.AfterFunc(suiteTimeout, func() { vm.Interrupt("suite timeout") })
	defer timer.Stop()
	v, err := vm.RunString(src)
	if err != nil {
		return "", err
	}
	return v.String(), nil
}

func runQuickJS(src string) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("quickjs panic: %v", r)
		}
	}()
	vm, err := quickjs.NewVM()
	if err != nil {
		return "", err
	}
	defer vm.Close()
	timer := time.AfterFunc(suiteTimeout, func() { vm.Interrupt() })
	defer timer.Stop()
	v, err := vm.Eval(src, quickjs.EvalGlobal)
	if err != nil {
		return "", err
	}
	return fmt.Sprint(v), nil
}

func runNode(src string) (string, error) {
	node, err := exec.LookPath("node")
	if err != nil {
		return "", fmt.Errorf("node not in PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
	defer cancel()
	// The driver's completion value is not printed by node; wrap it.
	cmd := exec.CommandContext(ctx, node, "-")
	cmd.Stdin = strings.NewReader(src[:strings.LastIndex(src, "(function () {")] +
		"console.log(" + src[strings.LastIndex(src, "(function () {"):] + ")")
	b, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(b)))
	}
	return strings.TrimSpace(string(b)), nil
}

var octaneEngines = []struct {
	name string
	run  func(string) (string, error)
}{
	{"go-spidermonkey", runSpiderMonkey},
	{"goja", runGoja},
	{"quickjs", runQuickJS},
	{"node", runNode},
}

func selectedSet(env string, all []string, def []string) map[string]bool {
	sel := map[string]bool{}
	v := os.Getenv(env)
	switch v {
	case "":
		for _, n := range def {
			sel[n] = true
		}
	case "all":
		for _, n := range all {
			sel[n] = true
		}
	default:
		for _, n := range strings.Split(v, ",") {
			sel[strings.TrimSpace(n)] = true
		}
	}
	return sel
}

func TestOctane(t *testing.T) {
	if _, err := os.Stat(filepath.Join(octaneDir, "base.js")); err != nil {
		t.Skipf("octane sources not present — run ./testdata/fetch-octane.sh first (%v)", err)
	}
	var suiteNames, engineNames []string
	for _, s := range octaneSuites {
		suiteNames = append(suiteNames, s.name)
	}
	for _, e := range octaneEngines {
		engineNames = append(engineNames, e.name)
	}
	suites := selectedSet("OCTANE_SUITES", suiteNames, classicNine)
	engines := selectedSet("OCTANE_ENGINES", engineNames, engineNames)

	type lane struct {
		scores map[string]float64
		errs   map[string]string
	}
	results := map[string]*lane{}
	for _, e := range octaneEngines {
		if !engines[e.name] {
			continue
		}
		results[e.name] = &lane{scores: map[string]float64{}, errs: map[string]string{}}
		for _, s := range octaneSuites {
			if !suites[s.name] {
				continue
			}
			src := octaneSource(t, s.files)
			start := time.Now()
			out, err := e.run(src)
			if err == nil {
				var score float64
				score, err = parseSuiteScore(out)
				if err == nil {
					results[e.name].scores[s.name] = score
					t.Logf("%-16s %-13s %10.1f  (%.1fs)", e.name, s.name, score, time.Since(start).Seconds())
					continue
				}
			}
			results[e.name].errs[s.name] = err.Error()
			t.Logf("%-16s %-13s %10s  (%.1fs) %v", e.name, s.name, "ERROR", time.Since(start).Seconds(), err)
		}
	}

	// Summary: per-engine geometric mean over its scored suites.
	t.Log("---- geometric means (Octane points, higher is better) ----")
	var names []string
	for n := range results {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		l := results[n]
		if len(l.scores) == 0 {
			t.Logf("%-16s no suites completed", n)
			continue
		}
		logSum := 0.0
		for _, s := range l.scores {
			logSum += math.Log(s)
		}
		partial := ""
		if len(l.errs) > 0 {
			partial = fmt.Sprintf("  (partial: %d/%d suites)", len(l.scores), len(l.scores)+len(l.errs))
		}
		t.Logf("%-16s %10.1f%s", n, math.Exp(logSum/float64(len(l.scores))), partial)
	}
}
