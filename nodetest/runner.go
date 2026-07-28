// Package nodetest runs the official Node.js test suite (nodejs/node, checked
// out under ./suite by `make nodetest-fetch`) against the compat/nodejs
// runtime. It is the Node-side counterpart of the test262 conformance run:
// every test executes in a fresh interpreter, the outcome is judged the way
// Node's own runner judges it (exit status plus the assertions common.mustCall
// registers on 'exit'), and the checked-in expectations file turns the run into
// a regression gate rather than a pass-rate report.
package nodetest

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
)

// Status is the outcome of one test file.
type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
	StatusSkip Status = "skip"
)

// Result is one test file's outcome, with enough detail to classify a failure
// without re-running it.
type Result struct {
	Path   string        // suite-relative path, e.g. "test/parallel/test-path.js"
	Status Status        // pass, fail or skip
	Reason string        // failure message or skip reason
	Output string        // captured stdout+stderr, trimmed
	Took   time.Duration // wall time
}

// Options configures one run.
type Options struct {
	// Root is the Node checkout root (the directory containing ./test).
	Root string
	// Timeout bounds a single test file. Zero means DefaultTimeout.
	Timeout time.Duration
	// MaxMemoryBytes caps each interpreter's linear memory. Zero means the
	// runner's default.
	MaxMemoryBytes int
}

const (
	// DefaultTimeout is Node's own per-test budget scaled for a wasm engine.
	DefaultTimeout = 60 * time.Second
	// A Node test is small; the cap is deliberately modest because a test that
	// blocks in a host op is ABANDONED with its interpreter still alive (see
	// JS.Close), so every such test holds its cap for the rest of the run.
	defaultMemory = 192 << 20
)

// skipMarker is what a Node test prints when it decides it cannot run in the
// current configuration; common.skip() prints it and exits 0.
const skipMarker = "1..0 # Skipped:"

// Run executes one test file and reports how it went. The file runs as the
// entry CommonJS module of a fresh interpreter, so `require('../common')`
// resolves against the real suite exactly as it does under Node.
//
// A host-side panic (a wasm trap, a bug in a compat host function) is converted
// into a failure for THIS test rather than being allowed to abort the whole
// run — the message is reported verbatim so it stays visible.
func Run(ctx context.Context, opts Options, rel string) (res Result) {
	defer func() {
		if p := recover(); p != nil {
			res.Path, res.Status = rel, StatusFail
			res.Reason = fmt.Sprintf("host panic: %v\n%s", p, debug.Stack())
		}
	}()
	return run(ctx, opts, rel)
}

func run(ctx context.Context, opts Options, rel string) Result {
	start := time.Now()
	res := Result{Path: rel}
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
		res.Status, res.Reason = StatusFail, err.Error()
		return res
	}
	if why := SkipReason(rel, string(src)); why != "" {
		res.Status, res.Reason = StatusSkip, why
		return res
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out := &lockedBuffer{}
	fsys := newUnionFS(os.DirFS(opts.Root))
	js, newErr := spidermonkey.New(spidermonkey.Config{
		FS:             fsys,
		Stdout:         out,
		Stderr:         out,
		MaxMemoryBytes: mem,
		Env: []string{
			"NODE_TEST_DIR=/tmp",
			"TEST_SERIAL_ID=0",
		},
		// The suite is a local, offline run: loopback is the only network it
		// legitimately needs, and nothing may reach the outside world.
		Resolve: func(host string) bool { return isLoopbackName(host) },
		Dial: func(network, host, ip string, port int) bool {
			return isLoopbackName(host) || isLoopbackIP(ip)
		},
		Listen: func(network, addr string) bool { return true },
	})
	if newErr != nil {
		res.Status = StatusFail
		res.Reason = "new interpreter: " + newErr.Error()
		res.Took = time.Since(start)
		return res
	}
	defer js.Close()

	rt, err := nodejs.Install(js, nodejs.Options{Argv: []string{"node", "/" + rel}})
	if err != nil {
		res.Status = StatusFail
		res.Reason = "nodejs.Install: " + err.Error()
		res.Took = time.Since(start)
		return res
	}
	defer rt.Close()

	// Materialize the writable scratch directory the suite's tmpdir helper
	// expects (NODE_TEST_DIR above); it lives in the union's upper layer only.
	fsys.Mkdir("tmp", 0o755)

	// A .mjs test is an ES module and must be evaluated as one; everything else
	// is CommonJS and is entered through require() by absolute path, which is
	// what gives it the __filename/__dirname its `require('../common')` needs.
	var runErr, evalErr error
	if strings.HasSuffix(rel, ".mjs") {
		var mr spidermonkey.ModuleResult
		mr, runErr = rt.RunModule(ctx, rel, string(src))
		evalErr = mr.Error
	} else {
		var r spidermonkey.Result
		r, runErr = rt.RunScript(ctx, "require("+quote("/"+rel)+");")
		evalErr = r.Error
	}
	res.Took = time.Since(start)
	res.Output = strings.TrimSpace(out.String())

	switch {
	case runErr != nil:
		res.Status = StatusFail
		res.Reason = runErr.Error()
	case evalErr != nil:
		res.Status = StatusFail
		res.Reason = evalErr.Error()
	case rt.ExitCode() != 0:
		res.Status = StatusFail
		res.Reason = fmt.Sprintf("exited with code %d", rt.ExitCode())
	case strings.Contains(res.Output, skipMarker):
		res.Status = StatusSkip
		res.Reason = skipLine(res.Output)
	default:
		res.Status = StatusPass
	}
	return res
}

func skipLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, skipMarker) {
			return strings.TrimSpace(strings.SplitN(line, skipMarker, 2)[1])
		}
	}
	return ""
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

func isLoopbackName(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1", "":
		return true
	}
	return false
}

func isLoopbackIP(ip string) bool {
	return strings.HasPrefix(ip, "127.") || ip == "::1"
}

// List returns the suite-relative paths of every test file under dir, sorted.
func List(root, dir string) ([]string, error) {
	entries, err := os.ReadDir(path.Join(root, dir))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "test-") {
			continue
		}
		if !strings.HasSuffix(name, ".js") && !strings.HasSuffix(name, ".mjs") {
			continue
		}
		out = append(out, path.Join(dir, name))
	}
	return out, nil
}

// lockedBuffer collects guest output from the interpreter's goroutines.
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
