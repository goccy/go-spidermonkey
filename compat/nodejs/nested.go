package nodejs

// nested.go: spawning "node" from inside this runtime.
//
// A large part of Node's own test suite re-executes the node binary —
// `spawn(process.execPath, ['-e', code])` and friends — to check flag handling,
// exit codes and stdio. There is no binary here, but there does not need to be
// one: a child "node" is a FRESH INTERPRETER on a goroutine, wired to the same
// pipes an OS process would have been. Deno passes those tests because its
// binary answers as node; this is the same answer for an embedded runtime.
//
// It is a real capability rather than test scaffolding: tooling that shells out
// to `node` (npm scripts, a framework's own build step) needs exactly this.

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// defaultExecPath is what process.execPath reports, and therefore what a test
// passes to spawn when it means "this runtime again".
const defaultExecPath = "/usr/local/bin/node"

// isSelfExec reports whether a command names this runtime. Both the full
// execPath and a bare "node" count: tests use either.
func isSelfExec(file string) bool {
	if file == defaultExecPath {
		return true
	}
	switch path.Base(file) {
	case "node", "node.exe":
		return true
	}
	return false
}

// nestedArgs is one parsed child "node" command line.
type nestedArgs struct {
	eval      string   // -e/--eval source, or ""
	print     bool     // -p/--print: print the completion value
	module    bool     // --input-type=module
	script    string   // script path, or "" when eval/stdin is used
	fromStdin bool     // the script is "-", i.e. read from stdin
	argv      []string // what process.argv should be, after argv[1]
	version   bool     // -v/--version
}

// parseNestedArgs reads the subset of node's command line the suite uses. An
// unrecognized flag is IGNORED rather than treated as a script path: a flag we
// do not implement should not turn into "cannot find module '--foo'".
func parseNestedArgs(argv []string) nestedArgs {
	var a nestedArgs
	i := 0
	for ; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "-e" || arg == "--eval":
			if i+1 < len(argv) {
				i++
				a.eval = argv[i]
			}
		case strings.HasPrefix(arg, "--eval="):
			a.eval = strings.TrimPrefix(arg, "--eval=")
		case arg == "-p" || arg == "--print":
			a.print = true
			if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				i++
				a.eval = argv[i]
			}
		case strings.HasPrefix(arg, "--print="):
			a.print = true
			a.eval = strings.TrimPrefix(arg, "--print=")
		case arg == "--input-type=module":
			a.module = true
		case arg == "-v" || arg == "--version":
			a.version = true
		case arg == "-":
			a.fromStdin = true
		case arg == "--":
			// Everything after "--" is program arguments.
			a.argv = append(a.argv, argv[i+1:]...)
			return a
		case strings.HasPrefix(arg, "-"):
			// An unimplemented flag: skipped, not mistaken for a script.
		default:
			if a.script == "" && a.eval == "" {
				a.script = arg
			} else {
				a.argv = append(a.argv, arg)
				continue
			}
			a.argv = append(a.argv, argv[i+1:]...)
			return a
		}
	}
	return a
}

// nestedResult is what a child "node" produced.
type nestedResult struct {
	exitCode int
	err      error // a host-side failure, not the script's own error
}

// runNested runs one child "node" to completion in a fresh interpreter.
//
// The child gets the parent's filesystem and permission hooks — it is the same
// sandbox, not an escape from it — plus its own environment and argv.
func (rt *Runtime) runNested(cfg spidermonkey.Config, argv []string, env []string, cwd string,
	stdin io.Reader, stdout, stderr io.Writer, timeout time.Duration,
) nestedResult {
	a := parseNestedArgs(argv)

	childCfg := cfg
	childCfg.Stdin = stdin
	childCfg.Stdout = stdout
	childCfg.Stderr = stderr
	if env != nil {
		childCfg.Env = env
	}

	if a.version {
		fmt.Fprintln(stdout, "v20.0.0")
		return nestedResult{exitCode: 0}
	}

	js, err := spidermonkey.New(childCfg)
	if err != nil {
		return nestedResult{exitCode: 1, err: err}
	}
	defer js.Close()

	scriptArgv := append([]string{defaultExecPath}, a.argv...)
	if a.script != "" {
		scriptArgv = append([]string{defaultExecPath, a.script}, a.argv...)
	}
	child, err := Install(js, Options{Argv: scriptArgv})
	if err != nil {
		return nestedResult{exitCode: 1, err: err}
	}
	defer child.Close()

	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	source := a.eval
	switch {
	case a.fromStdin && stdin != nil:
		b, rerr := io.ReadAll(stdin)
		if rerr != nil {
			return nestedResult{exitCode: 1, err: rerr}
		}
		source = string(b)
	case a.script != "":
		// The script is entered as the main module, which is what gives it
		// __filename, __dirname and a working relative require().
		p := a.script
		if cwd != "" && !path.IsAbs(p) {
			p = path.Join(cwd, p)
		}
		source = "require(" + jsQuote("/"+strings.TrimPrefix(path.Clean(p), "/")) + ");"
	case source == "":
		// No script and no eval: node reads stdin, and with none it exits 0.
		return nestedResult{exitCode: 0}
	}

	var jsErr error
	if a.module && a.script == "" {
		r, runErr := child.RunModule(ctx, "main.mjs", source)
		jsErr = firstErr(runErr, r.Error)
	} else {
		src := source
		if a.print {
			// -p prints the completion value; a throw still goes to stderr.
			src = "globalThis.console.log(" + source + ")"
		}
		r, runErr := child.RunScript(ctx, src)
		jsErr = firstErr(runErr, r.Error)
	}

	if child.Exited() {
		return nestedResult{exitCode: child.ExitCode()}
	}
	if jsErr != nil {
		// An uncaught exception is exit code 1 with the error on stderr, as in
		// node — not a host-side failure.
		fmt.Fprintln(stderr, jsErr.Error())
		return nestedResult{exitCode: 1}
	}
	return nestedResult{exitCode: child.ExitCode()}
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func jsQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString("\\n")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
