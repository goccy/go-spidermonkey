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
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
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

// Case is one runnable test: WPT expands a single source file into one test per
// target scope and per declared variant, and each of those is a separate result.
// Running the file once — which is what this harness used to do — measures a
// fraction of what a browser measures and cannot be compared with it.
type Case struct {
	Path    string // suite-relative source file
	Scope   string // "window", "dedicatedworker", "sharedworker", "serviceworker"
	Variant string // the "?..." a META: variant= line declares, or ""
}

// Key is how a case is named in a report and in the expectations file. It is the
// URL a browser would load, which is what makes it recognisable.
func (c Case) Key() string {
	k := c.Path + c.Variant
	if c.Scope != "" && c.Scope != "window" {
		k += " [" + c.Scope + "]"
	}
	return k
}

// scopesFor returns the scopes a file declares that this runtime can host. The
// default, when no META: global line is given, is window plus dedicatedworker.
func scopesFor(m meta) []string {
	declared := m.globals
	if len(declared) == 0 {
		declared = []string{"window", "dedicatedworker"}
	}
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, g := range declared {
		switch g {
		case "window":
			add("window")
		case "worker":
			// "worker" is shorthand for all three worker scopes. Only the
			// dedicated one is hosted here; the others need a real Worker.
			add("dedicatedworker")
		case "dedicatedworker":
			add("dedicatedworker")
		case "default", "!default":
			add("window")
			add("dedicatedworker")
		case "sharedworker", "serviceworker", "shadowrealm", "jsshell":
			// Not hostable here yet. Left out rather than counted as a pass.
		}
	}
	return out
}

// Expand turns source files into the cases a browser would run: the cross
// product of the scopes and the variants each file declares.
func Expand(root string, paths []string) ([]Case, error) {
	var out []Case
	for _, p := range paths {
		b, err := os.ReadFile(path.Join(root, p))
		if err != nil {
			return nil, err
		}
		m := parseMeta(string(b))
		variants := m.variant
		if len(variants) == 0 {
			variants = []string{""}
		}
		for _, scope := range scopesFor(m) {
			for _, v := range variants {
				out = append(out, Case{Path: p, Scope: scope, Variant: v})
			}
		}
	}
	return out, nil
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
	// HTTPSBaseURL is the same origin over TLS. A ".https." test is loaded from
	// here instead: it asserts on location.protocol and builds https:// URLs, so
	// served over http it fails while constructing a URL rather than for the
	// reason it exists.
	HTTPSBaseURL string
	// RootCAs are the authorities the guest must trust to reach HTTPSBaseURL. The
	// harness mints its own certificate (see tls.go), so without this every https
	// fetch fails the handshake.
	RootCAs *x509.CertPool
	// SubVars are the suite's server-side `{{name}}` substitutions (see
	// Server.SubVars). The runner loads a test and its META scripts from disk
	// rather than through the server, so a ".sub." file arriving that way —
	// common/get-host-info.sub.js above all — needs the same treatment or every
	// URL it builds is unparseable.
	SubVars map[string]string
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

// preludeFor establishes the scope the suite is written against, without
// faking a DOM: testharness.js detects the absence of Window and uses its
// ShellTestEnvironment, `self` is the spelling .any.js files use for the
// global, and `location` gives the tests the base URL every relative fetch of
// a fixture resolves against. Supplying a base URL is an environment
// property, not an API the compat layer is missing — a worker has one too —
// so fetch is wrapped here rather than changed there.
func preludeFor(baseURL, rel, scope string) string {
	href := baseURL + rel
	isWindow := scope == "window"
	isSecure := strings.HasPrefix(baseURL, "https:")
	secureGate := ""
	if !isSecure {
		// The web-locks surface is [SecureContext]; a browser would never have
		// exposed it to this origin, so the harness removes it the way the
		// exposure gate would have. (localhost is a trustworthy origin in real
		// browsers, but the suite's non-secure tests are written for a host that
		// is not — the plain-http origin plays that part here.)
		secureGate = `
globalThis.isSecureContext = false;
delete globalThis.Lock;
delete globalThis.LockManager;
delete Object.getPrototypeOf(globalThis.navigator).locks;
delete globalThis.navigator.locks;
`
	}
	return secureGate + `
globalThis.self = globalThis;
globalThis.GLOBAL = {
  isWindow: () => ` + fmt.Sprint(isWindow) + `,
  isWorker: () => ` + fmt.Sprint(!isWindow) + `,
  isShadowRealm: () => false,
};
` + func() string {
		if baseURL == "" {
			return ""
		}
		return `
(function () {
  // location itself is installed by compat/web (see Options.Location); what is
  // needed here is only that relative URLs resolve against it.
  const base = ` + jsString(href) + `;
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

// scopeFor maps a case's scope name onto the installation's.
func scopeFor(scope string) web.Scope {
	switch scope {
	case "dedicatedworker":
		return web.ScopeDedicatedWorker
	case "sharedworker":
		return web.ScopeSharedWorker
	case "serviceworker":
		return web.ScopeServiceWorker
	}
	return web.ScopeWindow
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
    // Tell the host the verdicts are in. Waiting for the loop to go IDLE instead
    // would mean waiting out every handle the test left open — an unclosed
    // WebSocket keeps the loop alive on purpose, and the file would cost the
    // whole per-file timeout after it had already finished.
    __wpt_done();
  });
})();
`

// importScriptsShimFor satisfies the call a classic-worker test makes for the
// scripts the harness has already loaded (see importedScripts), and THROWS for
// any other — a dependency the static scan missed must surface as a harness
// error, not as a test that quietly ran without it.
func importScriptsShimFor(loaded []string) string {
	list, _ := json.Marshal(loaded)
	return `
(function () {
  // Re-evaluating a pre-loaded script here would re-register testharness and
  // lose the results collected so far, so the call is satisfied, not replayed.
  const loaded = new Set(` + string(list) + `);
  globalThis.importScripts = function importScripts(...urls) {
    for (const u of urls) {
      if (!loaded.has(String(u))) {
        throw new Error("importScripts(" + u + "): not pre-loaded by the harness");
      }
    }
  };
})();
`
}

// Run executes one .any.js test file and returns its per-subtest verdicts.
func Run(ctx context.Context, opts Options, c Case) FileResult {
	rel := c.Path
	start := time.Now()
	res := FileResult{Path: c.Key()}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	mem := opts.MaxMemoryBytes
	if mem == 0 {
		mem = defaultMemory
	}

	srcBytes, err := os.ReadFile(path.Join(opts.Root, rel))
	if err != nil {
		res.Harness, res.Message = string(StatusError), err.Error()
		return res
	}
	src := substituteWPT(rel, srcBytes, opts.SubVars)
	m := parseMeta(string(src))
	if m.long {
		timeout *= 2
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	base := opts.BaseURL
	if opts.HTTPSBaseURL != "" && isHTTPSTest(rel) {
		base = opts.HTTPSBaseURL
	}

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

	// The scope and the base URL are environment facts, and compat/web owns the
	// interfaces they imply: WorkerGlobalScope and its subtype, WorkerNavigator,
	// WorkerLocation, the worker global's postMessage. The prelude used to hand-
	// roll a plain `location` object and the harness used to define marker
	// classes for idlharness; both were the runner standing in for a layer that
	// should have had them.
	w, err := web.InstallWith(js, web.Options{
		RootCAs:  opts.RootCAs,
		Scope:    scopeFor(c.Scope),
		Location: base + rel + c.Variant,
		// One interpreter is one thread here, and a Worker is not hosted yet.
		HardwareConcurrency: 1,
	})
	if err != nil {
		res.Harness, res.Message = string(StatusError), err.Error()
		return res
	}
	defer w.Close()

	// __wpt_done is what the collector calls once the verdicts are in. The run
	// stops there rather than at loop idle: a handle the test left open — an
	// unclosed WebSocket above all — keeps the loop alive by design, and waiting
	// it out would cost the whole per-file timeout after the file had finished.
	var reported atomic.Bool
	if err := js.Global().DefineFunc("__wpt_done",
		func(spidermonkey.Config, []spidermonkey.Value) (spidermonkey.Value, error) {
			reported.Store(true)
			return spidermonkey.Undefined(), nil
		}); err != nil {
		res.Harness, res.Message = string(StatusError), err.Error()
		return res
	}

	// Everything the file needs, in the order WPT loads it: the shell-scope
	// prelude, the harness, the completion collector, the META scripts, then
	// the test itself. They run as separate evaluations so a syntax error in
	// one is reported against that file.
	steps := []struct{ name, src string }{{"<prelude>", preludeFor(base, rel+c.Variant, c.Scope)}}
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
		imported := importedScripts(string(src))
		steps = append(steps, struct{ name, src string }{
			"<importScripts>", importScriptsShimFor(append(imported, "/resources/testharness.js"))})
		scripts = append(imported, scripts...)
	} else if c.Scope != "" && c.Scope != "window" {
		// A worker-scope .any.js reaches a browser as a generated wrapper that
		// pulls testharness and the test in with importScripts. The scripts are
		// already loaded here, but the CALL has to exist: importScripts is part of
		// the worker surface, and a test that uses it directly (workers/ has
		// several) must not fail on its absence.
		steps = append(steps, struct{ name, src string }{
			"<importScripts>", importScriptsShimFor([]string{"/resources/testharness.js"})})
	}
	for _, s := range scripts {
		p := resolveScript(rel, s)
		b, err := os.ReadFile(path.Join(opts.Root, p))
		if err != nil {
			res.Harness, res.Message = string(StatusError), "META script "+s+": "+err.Error()
			return res
		}
		steps = append(steps, struct{ name, src string }{p, string(substituteWPT(p, b, opts.SubVars))})
	}
	steps = append(steps, struct{ name, src string }{rel, string(src)})
	if c.Scope != "" && c.Scope != "window" && !strings.HasSuffix(rel, ".worker.js") {
		// ...and the wrapper ends with done(). In a worker scope testharness
		// waits for an explicit finish (WorkerTestEnvironment sets
		// wait_for_finish), so without this every worker-scope file that does not
		// call done() itself times out instead of reporting.
		steps = append(steps, struct{ name, src string }{"<done>", "done();"})
	}

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

	// Drain timers and promise jobs: async_test/promise_test complete here, and
	// the run stops as soon as they have — not when the loop runs dry.
	waitErr := w.Loop().RunUntil(ctx, reported.Load)

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
	// RFC 6761: any name under .localhost is a loopback name. Tests mint these
	// on the fly (location.href with "www2." prefixed) to get a second origin
	// on the same server, exactly as wptserve's subdomain table does.
	return strings.HasSuffix(host, ".localhost")
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
