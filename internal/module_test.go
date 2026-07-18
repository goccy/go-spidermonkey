package internal

// ES modules and the Promise job queue are ECMA-262 features the engine
// implements; the host drives module resolution (the loader callback) and the
// job loop. These verify both work through the per-instance surface.

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestPromiseJobsDrainAfterEval(t *testing.T) {
	js, _ := newJS(t)

	// Eval drains the microtask queue before returning, so a resolved Promise's
	// .then runs within the same call (no explicit PumpJobs needed here).
	mustEval(t, js, `globalThis.p = 0; Promise.resolve(21).then(v => { globalThis.p = v * 2; });`)
	if got := evalDisplay(t, js, `globalThis.p`); got != "42" {
		t.Errorf("promise result = %q, want \"42\" (job queue not drained)", got)
	}
}

func TestModuleLoaderCallbackResolvesOnDemand(t *testing.T) {
	js, env := newJS(t)

	// A loader that serves sources on demand — the shape the public API drives
	// from Config.FS. Nothing is pre-registered.
	files := map[string]string{
		"dep":  `export const answer = 40;`,
		"dep2": `export const extra = 2;`,
	}
	var requested []string
	env.loader = func(specifier, referrer string) (string, error) {
		requested = append(requested, specifier)
		src, ok := files[specifier]
		if !ok {
			return "", fmt.Errorf("module not found: %s", specifier)
		}
		return src, nil
	}

	raw, err := js.EvalModule("main",
		`import {answer} from "dep"; import {extra} from "dep2"; globalThis.result = answer + extra;`)
	if err != nil {
		t.Fatalf("EvalModule transport error: %v", err)
	}
	if r := decodeEnvelope(t, raw); !r.Ok {
		t.Fatalf("EvalModule failed: %s", r.Error)
	}
	if got := evalDisplay(t, js, `globalThis.result`); got != "42" {
		t.Errorf("result = %q, want \"42\"", got)
	}

	// The loader was consulted for each imported specifier.
	seen := map[string]bool{}
	for _, s := range requested {
		seen[s] = true
	}
	if !seen["dep"] || !seen["dep2"] {
		t.Errorf("loader saw %v, want it consulted for dep and dep2", requested)
	}
}

func TestModuleLoaderErrorSurfaces(t *testing.T) {
	js, env := newJS(t)
	env.loader = func(specifier, referrer string) (string, error) {
		return "", fmt.Errorf("no such module %q", specifier)
	}
	raw, err := js.EvalModule("main", `import "missing"; globalThis.x = 1;`)
	if err != nil {
		t.Fatalf("EvalModule transport error: %v", err)
	}
	r := decodeEnvelope(t, raw)
	if r.Ok {
		t.Fatalf("expected the import to fail")
	}
	if want := "no such module"; !contains(r.Error, want) {
		t.Errorf("error = %q, want it to contain %q", r.Error, want)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func decodeEnvelope(t *testing.T, raw string) evalResult {
	t.Helper()
	var r evalResult
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("undecodable envelope %q: %v", raw, err)
	}
	return r
}
