// Package babeltest runs Babel's own fixture corpus (babel/babel, checked out
// under ./testdata/suite by `make babeltest-fetch`) through @babel/core ON THIS RUNTIME.
//
// Where test262 measures the engine and the Node/WPT suites measure one API
// surface at a time, this measures the whole stack under a real workload:
// Babel is ~150 packages of ordinary, demanding JavaScript, and each fixture
// makes it parse, transform and print a program whose expected output the
// Babel project itself pins. A mismatch is a defect in this runtime, not an
// opinion about it.
//
// Fidelity note: the fixtures are run against the PUBLISHED @babel/* packages
// at the same version as the pinned checkout (see scripts/babel-suite-deps.sh),
// not against a local build of the monorepo. That keeps the run reproducible
// and hermetic while testing exactly the code Babel ships.
package babeltest

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
)

//go:embed js/fixtures.js
var fixturesJS string

// Status is one fixture's outcome.
type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
	StatusSkip Status = "skip"
)

// Result is one fixture's outcome. Name is "<package>/<suite>/<task>".
type Result struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// Options configures a run.
type Options struct {
	// Root is the babeltest directory: it must contain suite/ (the pinned
	// checkout) and node_modules/ (the matching @babel/* packages).
	Root string
	// Timeout bounds one shard — one Babel package's whole fixture corpus.
	Timeout time.Duration
	// MaxMemoryBytes caps each interpreter. Zero means the runner default.
	MaxMemoryBytes int
}

const (
	// DefaultTimeout covers the largest shard (babel-preset-env, thousands of
	// transforms in one interpreter).
	DefaultTimeout = 30 * time.Minute
	// Babel is a heavy workload: @babel/core plus a preset is tens of MB of
	// source, and a shard transforms hundreds of programs without a fresh realm.
	defaultMemory = 2 << 30
)

// Shards returns the fixture roots to run, one per Babel package that has a
// test/fixtures directory, relative to Root. Sharding per package is what keeps
// one interpreter's working set bounded and isolates a crash to one package.
func Shards(root string) ([]string, error) {
	pkgs, err := os.ReadDir(filepath.Join(root, "testdata", "suite", "packages"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range pkgs {
		if !p.IsDir() {
			continue
		}
		rel := path.Join("testdata", "suite", "packages", p.Name(), "test", "fixtures")
		if fi, err := os.Stat(filepath.Join(root, rel)); err == nil && fi.IsDir() {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out, nil
}

// RunShard runs every fixture under one fixtures root in a single interpreter
// and returns one Result per fixture, named "<package>/<suite>/<task>".
func RunShard(ctx context.Context, opts Options, shard string) (results []Result) {
	label := shardLabel(shard)
	defer func() {
		if p := recover(); p != nil {
			results = append(results, Result{
				Name:   label + "/<host>",
				Status: StatusFail,
				Reason: fmt.Sprintf("host panic: %v\n%s", p, debug.Stack()),
			})
		}
	}()

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	mem := opts.MaxMemoryBytes
	if mem == 0 {
		mem = defaultMemory
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	fail := func(reason string) []Result {
		return []Result{{Name: label + "/<shard>", Status: StatusFail, Reason: reason}}
	}

	out := &lockedBuffer{}
	js, err := spidermonkey.New(spidermonkey.Config{
		// The whole babeltest directory: node_modules for the @babel packages,
		// suite/ for the fixtures. Read-only — nothing a fixture does may touch
		// the checkout.
		FS:             os.DirFS(opts.Root),
		Stdout:         out,
		Stderr:         out,
		MaxMemoryBytes: mem,
		Env:            []string{"BABEL_ENV=test", "NODE_ENV=test"},
	})
	if err != nil {
		return fail("new interpreter: " + err.Error())
	}
	defer js.Close()

	rt, err := nodejs.Install(js, nodejs.Options{Argv: []string{"node", "/babeltest"}})
	if err != nil {
		return fail("nodejs.Install: " + err.Error())
	}
	defer rt.Close()

	// The harness reads its fixtures root off a global and leaves its results on
	// one: it has to import the plugin packages, importing needs await, and a
	// host call cannot await a guest promise. Evaluating the module (whose
	// top-level await runs the whole shard) and reading the result back is the
	// synchronous seam.
	// The root is handed over ABSOLUTE: Babel joins a relative filename onto
	// cwd, which would double the path into every fixture that embeds its own
	// source location (the development JSX transform does).
	if r, err := rt.RunScript(ctx, "globalThis.__babeltest_root = "+jsString("/"+shard)); err != nil {
		return fail("set fixtures root: " + err.Error())
	} else if r.Error != nil {
		return fail("set fixtures root: " + r.Error.Error())
	}
	if r, err := rt.RunModule(ctx, "babeltest-harness.mjs", fixturesJS); err != nil {
		return fail("run fixture harness: " + err.Error() + " | " + tail(out.String()))
	} else if r.Error != nil {
		return fail("run fixture harness: " + r.Error.Error() + " | " + tail(out.String()))
	}

	r, err := rt.RunScript(ctx, "globalThis.__babeltest_result || \"\"")
	if err != nil {
		return fail("collect results: " + err.Error() + " | " + tail(out.String()))
	}
	if r.Error != nil {
		return fail("collect results: " + r.Error.Error() + " | " + tail(out.String()))
	}
	var got []Result
	if err := json.Unmarshal([]byte(r.Value.String()), &got); err != nil {
		return fail("decode results: " + err.Error() + " | " + tail(r.Value.String()))
	}
	for i := range got {
		got[i].Name = label + "/" + got[i].Name
	}
	return got
}

// shardLabel is the Babel package name a fixtures root belongs to.
func shardLabel(shard string) string {
	parts := strings.Split(shard, "/")
	for i, p := range parts {
		if p == "packages" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return shard
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		s = "…" + s[len(s)-400:]
	}
	return strings.ReplaceAll(s, "\n", " | ")
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() > 1<<20 {
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var _ io.Writer = (*lockedBuffer)(nil)
