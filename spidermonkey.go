// Package spidermonkey runs untrusted JavaScript on Mozilla's SpiderMonkey
// engine from pure Go. The engine is exactly ECMA-262: host surfaces like
// console and setTimeout are not built in — an embedder adds them with
// Global().DefineFunc.
package spidermonkey

import (
	"context"
	"iter"
	"sync"
	"time"

	"github.com/goccy/go-spidermonkey/internal"
)

// JS is one isolated SpiderMonkey interpreter: its own wasm instance (its own
// linear memory) and one runtime. Several run concurrently and in isolation.
type JS struct {
	raw    *internal.JS
	env    *hostEnv
	cfg    Config
	global *Object // cached; resolved lazily by Global()

	agentsOnce sync.Once
	agents     *Agents // the agent cluster; resolved lazily by Agents()
}

// New creates a sandboxed interpreter from cfg. The zero Config is a usable,
// fully sandboxed interpreter.
func New(cfg Config) (*JS, error) {
	env := &hostEnv{cfg: cfg, funcs: map[string]Func{}}
	// The default module loader reads Config.FS. Without an FS (and without a
	// custom loader) leave it unset, so an unresolved import falls through to
	// the "module not registered" failure rather than a hard error.
	if cfg.FS != nil {
		env.loader = defaultModuleLoader
	}
	raw, err := internal.New(internal.Options{
		Env:       env,
		Environ:   cfg.Env,
		Stdin:     cfg.Stdin,
		Stdout:    cfg.Stdout,
		Stderr:    cfg.Stderr,
		MaxMemory: cfg.MaxMemoryBytes,
	})
	if err != nil {
		return nil, err
	}
	js := &JS{raw: raw, env: env, cfg: cfg}
	env.js = js // objects crossing into host callbacks decode against this JS
	return js, nil
}

// Eval runs src as a classic script to synchronous completion — draining the
// microtask queue — and returns the result. ctx aborts a runaway script: on
// cancellation the script is interrupted and Eval returns ctx.Err(). Eval does
// NOT drive timers or other async work; use RunJobs for that. A JavaScript throw
// is reported in Result (OK=false, Error), not as a Go error.
func (js *JS) Eval(ctx context.Context, src string) (Result, error) {
	raw, err := js.raw.EvalContext(ctx, src)
	if err != nil {
		// A context cancellation carries the interrupted envelope in raw
		// (partial output, "interrupted"); a transport error carries none.
		if raw != "" {
			r, _ := parseResult(js, raw)
			return r, err
		}
		return Result{}, err
	}
	return parseResult(js, raw)
}

// RunJobs drives the ECMA-262 job queue, yielding the result of each pump that
// ran work and ending when the queue is idle or ctx is done. Pending-but-not-due
// work (a timer, an Atomics.waitAsync timeout) is awaited internally, bounded by
// ctx, so every yielded Result reflects real progress. Break to stop early:
//
//	for r, err := range js.RunJobs(ctx) {
//		if err != nil { break }
//		// r.Stdout, r.OK, ...
//	}
func (js *JS) RunJobs(ctx context.Context) iter.Seq2[Result, error] {
	return func(yield func(Result, error) bool) {
		for {
			if ctx.Err() != nil {
				return
			}
			raw, err := js.raw.RunJobsContext(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return // cancelled — stop cleanly, not an error
				}
				yield(Result{}, err)
				return
			}
			e, perr := parseEnvelope(raw)
			if perr != nil {
				yield(Result{}, perr)
				return
			}
			// The pump encodes progress in the envelope's result: "2" = engine
			// work still pending (wait and pump again), "0" = idle. RunJobs
			// drains ready microtasks to exhaustion each step, so "0" is final.
			switch e.Result {
			case "0":
				return
			case "2":
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Millisecond):
				}
				continue
			}
			if !yield(Result{Error: jsErr(e)}, nil) {
				return
			}
		}
	}
}

// Close destroys the interpreter and releases its resources. Agents blocked
// in receive are released first (they unwind with an error) so the engine's
// shutdown join can complete.
func (js *JS) Close() error {
	if js.agents != nil {
		js.agents.close()
	}
	if js.global != nil {
		js.raw.FreeObject(js.global.handle)
		js.global = nil
	}
	return js.raw.Close()
}
