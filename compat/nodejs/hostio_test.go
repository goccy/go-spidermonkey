package nodejs_test

import (
	"bytes"
	"context"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
	"github.com/goccy/go-spidermonkey/memfs"
	"testing"
	"time"
)

func TestProcessStdin(t *testing.T) {
	stdin := bytes.NewBufferString("line one\nline two\n")
	js, err := spidermonkey.New(spidermonkey.Config{Stdin: stdin})
	if err != nil {
		t.Fatal(err)
	}
	defer js.Close()
	rt, err := nodejs.Install(js)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if _, err := js.Eval(context.Background(), `
		globalThis.r = { lines: [] };
		const readline = require("readline");
		const rl = readline.createInterface({ input: process.stdin });
		rl.on("line", (l) => r.lines.push(l));
	`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt.Wait(ctx)
	if got := evalStr(t, js, "r.lines.join('|')"); got != "line one|line two" {
		t.Errorf("readline over stdin = %q", got)
	}
}

func TestFSWatch(t *testing.T) {
	fsys := memfs.New()
	fsys.WriteFile("watched.txt", []byte("v1"), 0o644)
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	if _, err := js.Eval(context.Background(), `
		const fs = require("fs");
		globalThis.r = { changes: 0 };
		const w = fs.watch("/watched.txt", () => { r.changes++; if (r.changes >= 1) w.close(); });
	`); err != nil {
		t.Fatal(err)
	}
	// Mutate the file from Go after a beat.
	go func() { time.Sleep(300 * time.Millisecond); fsys.WriteFile("watched.txt", []byte("v2-changed"), 0o644) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt.Wait(ctx)
	if got := evalStr(t, js, "String(r.changes >= 1)"); got != "true" {
		t.Errorf("fs.watch did not observe the change (changes=%s)", evalStr(t, js, "String(r.changes)"))
	}
}

func TestPunycode(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const punycode = require("punycode");
		globalThis.r = {};
		r.enc = punycode.encode("bücher");        // known: bcher-kva
		r.dec = punycode.decode("bcher-kva");
		r.ascii = punycode.toASCII("münchen.de");
		r.uni = punycode.toUnicode("xn--mnchen-3ya.de");
	`)
	for expr, want := range map[string]string{
		"r.enc":   "bcher-kva",
		"r.dec":   "bücher",
		"r.ascii": "xn--mnchen-3ya.de",
		"r.uni":   "münchen.de",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestProcessSignal(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := js.Eval(context.Background(), `
		globalThis.r = {};
		process.on("SIGUSR1", () => { r.got = "SIGUSR1"; });
	`); err != nil {
		t.Fatal(err)
	}
	// Deliver the signal from a separate goroutine, but that goroutine only
	// touches the OS (p.Signal) — never the wasm instance. A single SpiderMonkey
	// instance is NOT safe for concurrent cross-goroutine entry (one JSContext /
	// linear memory / set of globals), so ONLY the main goroutine may enter it:
	// drive the loop here with rt.Wait and assert after it returns. The signal
	// handler keeps the loop alive (AddPending), so Wait blocks until the context
	// deadline; by then the posted handler has run and set r.got.
	go func() {
		time.Sleep(200 * time.Millisecond)
		p, _ := osFindProcess()
		p.Signal(sigUSR1())
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rt.Wait(ctx)
	if got := evalStr(t, js, "String(r.got ?? '')"); got != "SIGUSR1" {
		t.Fatalf("SIGUSR1 handler did not fire; got %q", got)
	}
}
