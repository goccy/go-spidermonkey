package spidermonkey_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func TestEval(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer js.Close()

	r, err := js.Eval(context.Background(), `1 + 2 * 3`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil || r.Value.String() != "7" {
		t.Fatalf("Eval = %+v, want no error with Value \"7\"", r)
	}
}

func TestEvalThrowIsResultNotError(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	r, err := js.Eval(context.Background(), `throw new Error("boom")`)
	if err != nil {
		t.Fatalf("a JS throw must not be a Go error: %v", err)
	}
	if r.Error == nil {
		t.Fatalf("expected a non-nil Error for a thrown exception")
	}
	if !strings.Contains(r.Error.Error(), "boom") {
		t.Errorf("Error = %q, want it to mention \"boom\"", r.Error)
	}
	var jsErr *spidermonkey.JSError
	if !errors.As(r.Error, &jsErr) {
		t.Errorf("Error should be a *JSError, got %T", r.Error)
	}
}

func TestRegisterFuncReceivesConfigAndArgs(t *testing.T) {
	var out bytes.Buffer
	js, _ := spidermonkey.New(spidermonkey.Config{Stdout: &out})
	defer js.Close()

	// A host-provided console.log-ish that writes to Config.Stdout, defined on
	// the global object.
	err := js.Global().DefineFunc("emit", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
		for _, a := range args {
			cfg.Stdout.Write([]byte(a.String()))
		}
		return spidermonkey.Undefined(), nil
	})
	if err != nil {
		t.Fatal(err)
	}

	r, err := js.Eval(context.Background(), `emit("hello, "); emit("host"); 42`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil || r.Value.String() != "42" {
		t.Fatalf("Eval = %+v", r)
	}
	if got := out.String(); got != "hello, host" {
		t.Errorf("host func output = %q, want \"hello, host\"", got)
	}
}

func TestEvalContextInterruptsRunaway(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := js.Eval(ctx, `while (true) {}`)
	if err != context.DeadlineExceeded {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the interrupt did not stop the loop")
	}
}

func TestEvalModuleFromFS(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{
		FS: fstest.MapFS{
			"dep.js": {Data: []byte(`export const answer = 42;`)},
		},
	})
	defer js.Close()

	r, err := js.EvalModule(context.Background(), "main",
		`import { answer } from "dep.js"; globalThis.result = answer * 2;`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != nil {
		t.Fatalf("EvalModule failed: %v", r.Error)
	}
	got, _ := js.Eval(context.Background(), `globalThis.result`)
	if got.Value.String() != "84" {
		t.Errorf("result = %q, want \"84\"", got.Value)
	}
}

func TestRunJobsIdleReturns(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	// No async work scheduled: RunJobs should yield nothing and return promptly.
	if _, err := js.Eval(context.Background(), `1 + 1`); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for range js.RunJobs(context.Background()) {
			// no async work, so this body should not run
			t.Error("RunJobs yielded although nothing was pending")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunJobs did not return on an idle queue")
	}
}
