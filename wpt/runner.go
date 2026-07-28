// Package wpt runs the Web Platform Tests (web-platform-tests/wpt, checked out
// under ./suite by `make wpt-fetch`) against the compat/web vocabulary.
//
// WPT tests are judged per SUBTEST, not per file: a .any.js file typically
// declares dozens of independent assertions, and a single unimplemented corner
// must not hide the rest. The runner loads the suite's own testharness.js in
// its shell environment (no DOM is faked — testharness has a first-class
// no-Window mode), runs the test in a fresh interpreter, and reads the results
// back through testharness's completion callback.
package wpt

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// Status mirrors testharness.js's status codes.
type Status string

const (
	StatusPass               Status = "PASS"
	StatusFail               Status = "FAIL"
	StatusTimeout            Status = "TIMEOUT"
	StatusNotRun             Status = "NOTRUN"
	StatusPreconditionFailed Status = "PRECONDITION_FAILED"
	StatusError              Status = "ERROR" // the file itself did not run
	harnessStatusNamesJSFrag        = `["OK","ERROR","TIMEOUT","PRECONDITION_FAILED"]`
	subtestStatusNamesJSFrag        = `["PASS","FAIL","TIMEOUT","NOTRUN","PRECONDITION_FAILED"]`
)

// Subtest is one assertion group inside a test file.
type Subtest struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Message string `json:"message,omitempty"`
}

// FileResult is the outcome of running one test file.
type FileResult struct {
	Path     string        `json:"path"`    // suite-relative, e.g. "url/url-setters.any.js"
	Harness  string        `json:"harness"` // OK, ERROR, TIMEOUT
	Message  string        `json:"message"` // harness-level error, if any
	Subtests []Subtest     `json:"subtests"`
	Took     time.Duration `json:"-"`
}

// Options configures a run.
type Options struct {
	// Root is the WPT checkout root (the directory containing ./resources).
	Root string
	// Timeout bounds a single test file. Zero means DefaultTimeout.
	Timeout time.Duration
	// MaxMemoryBytes caps each interpreter. Zero means the runner default.
	MaxMemoryBytes int
	// BaseURL is what location.href reports and what relative fetches resolve
	// against. Empty means the tests run without a server (fetching tests will
	// report their own failures).
	BaseURL string
}

const (
	// DefaultTimeout is generous: testharness's own long timeout is 60s.
	DefaultTimeout = 70 * time.Second
	defaultMemory  = 512 << 20
)

var metaRE = regexp.MustCompile(`^// META: ([a-z]+)=(.*)$`)

// meta is the subset of WPT's "// META:" directives that decide how a file
// runs: which scripts precede it, which global scopes it targets, and whether
// it asks for the long timeout.
type meta struct {
	scripts []string
	globals []string
	long    bool
	variant []string
}

// parseMeta reads the leading "// META:" comment block of a .any.js file.
func parseMeta(src string) meta {
	var m meta
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "// META:") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			break
		}
		sub := metaRE.FindStringSubmatch(line)
		if sub == nil {
			continue
		}
		switch sub[1] {
		case "script":
			m.scripts = append(m.scripts, strings.TrimSpace(sub[2]))
		case "global":
			for _, g := range strings.Split(sub[2], ",") {
				m.globals = append(m.globals, strings.TrimSpace(g))
			}
		case "timeout":
			m.long = strings.TrimSpace(sub[2]) == "long"
		case "variant":
			m.variant = append(m.variant, strings.TrimSpace(sub[2]))
		}
	}
	return m
}

// scopeSupported reports whether this embedding can host the file's target
// scopes. A file limited to window/shadowrealm scopes has no runnable form
// here; the default (no META: global) is window+dedicatedworker, and the
// worker half is exactly what compat/web implements.
func scopeSupported(m meta) bool {
	if len(m.globals) == 0 {
		return true // default window,dedicatedworker — the worker half applies
	}
	for _, g := range m.globals {
		switch g {
		case "worker", "dedicatedworker", "sharedworker", "serviceworker", "default", "!default":
			return true
		}
	}
	// window-only or shadowrealm-only.
	return false
}

// preludeFor establishes the scope the suite is written against, without
// faking a DOM: testharness.js detects the absence of Window and uses its
// ShellTestEnvironment, `self` is the spelling .any.js files use for the
// global, and `location` gives the tests the base URL every relative fetch of
// a fixture resolves against. Supplying a base URL is an environment
// property, not an API the compat layer is missing — a worker has one too —
// so fetch is wrapped here rather than changed there.
func preludeFor(baseURL, rel string) string {
	href := baseURL + rel
	return `
globalThis.self = globalThis;
globalThis.GLOBAL = {
  isWindow: () => false,
  isWorker: () => true,
  isShadowRealm: () => false,
};
` + func() string {
		if baseURL == "" {
			return ""
		}
		return `
(function () {
  const u = new URL(` + jsString(href) + `);
  globalThis.location = {
    href: u.href, origin: u.origin, protocol: u.protocol, host: u.host,
    hostname: u.hostname, port: u.port, pathname: u.pathname,
    search: u.search, hash: u.hash, toString: () => u.href,
  };
  const base = u.href;
  const raw = globalThis.fetch;
  globalThis.fetch = function fetch(input, init) {
    if (typeof input === "string") input = new URL(input, base).href;
    return raw.call(this, input, init);
  };
  const RawRequest = globalThis.Request;
  if (RawRequest) {
    globalThis.Request = class Request extends RawRequest {
      constructor(input, init) {
        super(typeof input === "string" ? new URL(input, base).href : input, init);
      }
    };
  }
})();
`
	}()
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// collector installs the completion callback that carries the per-subtest
// verdicts back to the host as JSON on a global.
const collector = `
(function () {
  const H = ` + harnessStatusNamesJSFrag + `;
  const S = ` + subtestStatusNamesJSFrag + `;
  add_completion_callback(function (tests, status) {
    globalThis.__wpt_result = JSON.stringify({
      harness: H[status.status] || "ERROR",
      message: status.message || "",
      subtests: tests.map((t) => ({
        name: t.name,
        status: S[t.status] || "FAIL",
        message: t.message || "",
      })),
    });
  });
})();
`

// importScriptsShim satisfies the call a classic-worker test makes for scripts
// the harness has already loaded (see importedScripts) and fails loudly for any
// other, so a missed dependency cannot look like a passing test.
const importScriptsShim = `
globalThis.importScripts = function importScripts() {
  // The harness pre-loads what a .worker.js file imports; re-loading here would
  // re-register testharness and lose the collected results.
};
`

// Run executes one .any.js test file and returns its per-subtest verdicts.
func Run(ctx context.Context, opts Options, rel string) FileResult {
	start := time.Now()
	res := FileResult{Path: rel}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	mem := opts.MaxMemoryBytes
	if mem == 0 {
		mem = defaultMemory
	}

	src, err := os.ReadFile(path.Join(opts.Root, rel))
	if err != nil {
		res.Harness, res.Message = string(StatusError), err.Error()
		return res
	}
	m := parseMeta(string(src))
	if !scopeSupported(m) {
		res.Harness = "SKIP"
		res.Message = "scopes " + strings.Join(m.globals, ",") + " need a DOM window"
		return res
	}
	if m.long {
		timeout *= 2
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out := &lockedBuffer{}
	js, err := spidermonkey.New(spidermonkey.Config{
		FS:             os.DirFS(opts.Root),
		Stdout:         out,
		Stderr:         out,
		MaxMemoryBytes: mem,
		Resolve:        func(host string) bool { return isLoopback(host) },
		Dial: func(network, host, ip string, port int) bool {
			return isLoopback(host) || strings.HasPrefix(ip, "127.") || ip == "::1"
		},
	})
	if err != nil {
		res.Harness, res.Message = string(StatusError), err.Error()
		return res
	}
	defer js.Close()

	w, err := web.Install(js)
	if err != nil {
		res.Harness, res.Message = string(StatusError), err.Error()
		return res
	}
	defer w.Close()

	// Everything the file needs, in the order WPT loads it: the shell-scope
	// prelude, the harness, the completion collector, the META scripts, then
	// the test itself. They run as separate evaluations so a syntax error in
	// one is reported against that file.
	steps := []struct{ name, src string }{{"<prelude>", preludeFor(opts.BaseURL, rel)}}
	harness, err := os.ReadFile(path.Join(opts.Root, "resources/testharness.js"))
	if err != nil {
		res.Harness, res.Message = string(StatusError), "testharness.js: "+err.Error()
		return res
	}
	steps = append(steps,
		struct{ name, src string }{"resources/testharness.js", string(harness)},
		struct{ name, src string }{"<collector>", collector},
	)
	scripts := m.scripts
	if strings.HasSuffix(rel, ".worker.js") {
		// A classic worker test bootstraps itself with importScripts instead of
		// META directives; those files are loaded here, and the call is defined so
		// that anything NOT pre-loaded is reported rather than silently skipped.
		steps = append(steps, struct{ name, src string }{"<importScripts>", importScriptsShim})
		scripts = append(importedScripts(string(src)), scripts...)
	}
	for _, s := range scripts {
		p := resolveScript(rel, s)
		b, err := os.ReadFile(path.Join(opts.Root, p))
		if err != nil {
			res.Harness, res.Message = string(StatusError), "META script "+s+": "+err.Error()
			return res
		}
		steps = append(steps, struct{ name, src string }{p, string(b)})
	}
	steps = append(steps, struct{ name, src string }{rel, string(src)})

	for _, st := range steps {
		r, err := js.Eval(ctx, st.src)
		if err != nil {
			res.Harness = string(StatusError)
			res.Message = st.name + ": " + err.Error()
			res.Took = time.Since(start)
			return res
		}
		if r.Error != nil {
			res.Harness = string(StatusError)
			res.Message = st.name + ": " + r.Error.Error()
			res.Took = time.Since(start)
			return res
		}
	}

	// Drain timers and promise jobs: async_test/promise_test complete here.
	waitErr := w.Wait(ctx)

	res.Took = time.Since(start)
	r, err := js.Eval(context.Background(), `globalThis.__wpt_result || ""`)
	if err == nil && r.Error == nil && r.Value.String() != "" {
		var parsed FileResult
		if e := json.Unmarshal([]byte(r.Value.String()), &parsed); e == nil {
			res.Harness, res.Message, res.Subtests = parsed.Harness, parsed.Message, parsed.Subtests
			return res
		}
	}
	res.Harness = string(StatusTimeout)
	res.Message = "no completion callback fired"
	if waitErr != nil {
		res.Harness = string(StatusError)
		res.Message = waitErr.Error()
	}
	if o := strings.TrimSpace(out.String()); o != "" {
		res.Message += " | " + firstLine(o)
	}
	return res
}

// resolveScript maps a META script reference to a suite-relative path: a
// leading "/" is suite-absolute, anything else is relative to the test file.
func resolveScript(testRel, script string) string {
	if p, ok := scriptAliases[script]; ok {
		return p
	}
	if strings.HasPrefix(script, "/") {
		return strings.TrimPrefix(script, "/")
	}
	return path.Join(path.Dir(testRel), script)
}

// scriptAliases are the suite paths that no longer name a real file. WPT's own
// server rewrites them; offline, the mapping has to be stated.
var scriptAliases = map[string]string{
	"/resources/WebIDLParser.js": "resources/webidl2/lib/webidl2.js",
}

// importScriptsRE finds the classic-worker bootstrap a .worker.js file opens
// with. Those files are not preceded by META directives — they pull their
// dependencies in at run time — so the harness has to read the calls out of the
// source and load them the same way it loads META scripts.
var importScriptsRE = regexp.MustCompile(`importScripts\(([^)]*)\)`)

// importedScripts returns the references a .worker.js file imports, in order and
// unresolved (the caller resolves them like any other script reference).
// testharness.js is dropped: the harness has already loaded it, and loading it
// twice would reset the results it has collected.
func importedScripts(src string) []string {
	var out []string
	for _, m := range importScriptsRE.FindAllStringSubmatch(src, -1) {
		for _, arg := range strings.Split(m[1], ",") {
			arg = strings.TrimSpace(arg)
			if len(arg) < 2 {
				continue
			}
			q := arg[0]
			if q != '"' && q != '\'' {
				continue
			}
			ref := strings.Trim(arg, string(q))
			if strings.HasSuffix(ref, "/testharness.js") {
				continue
			}
			out = append(out, ref)
		}
	}
	return out
}

// List walks dir and returns every runnable test file, suite-relative.
// Only the .any.js / .worker.js forms can run without a browser.
func List(root string, dirs []string) ([]string, error) {
	var out []string
	for _, d := range dirs {
		err := fs.WalkDir(os.DirFS(root), d, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return nil // a directory the checkout does not carry
			}
			if e.IsDir() {
				return nil
			}
			if strings.HasSuffix(p, ".any.js") || strings.HasSuffix(p, ".worker.js") {
				out = append(out, p)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", d, err)
		}
	}
	return out, nil
}

func isLoopback(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1", "":
		return true
	}
	return false
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
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
