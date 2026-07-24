package nodejs_test

import (
	"context"
	"testing"
	"testing/fstest"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestProcessExitCleanTermination verifies process.exit() is reported as a clean
// termination (no evaluation error) with the exit code recorded, and that it
// cannot be swallowed by an uncaughtException handler.
func TestProcessExitCleanTermination(t *testing.T) {
	_, rt := newRuntime(t, spidermonkey.Config{})
	r, err := rt.RunScript(context.Background(), `console.log("bye"); process.exit(3);`)
	if err != nil {
		t.Fatalf("RunScript returned error for a clean exit: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("process.exit surfaced as an evaluation error: %v", r.Error)
	}
	if !rt.Exited() || rt.ExitCode() != 3 {
		t.Fatalf("Exited=%v ExitCode=%d, want true/3", rt.Exited(), rt.ExitCode())
	}
}

func TestProcessExitNotSwallowedByHandler(t *testing.T) {
	_, rt := newRuntime(t, spidermonkey.Config{})
	r, err := rt.RunScript(context.Background(), `
		process.on("uncaughtException", () => { globalThis.__swallowed = true; });
		process.nextTick(() => process.exit(0));
	`)
	if err != nil {
		t.Fatalf("RunScript error: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("unexpected eval error: %v", r.Error)
	}
	if !rt.Exited() {
		t.Fatal("process.exit in nextTick was swallowed by the uncaughtException handler")
	}
}

// TestESMJSONImportIsInert verifies a .json module imported through the ESM
// loader is parsed as data (JSON.parse), not evaluated as JavaScript, so an
// executable payload in the file cannot run on import.
func TestESMJSONImportIsInert(t *testing.T) {
	fsys := fstest.MapFS{
		"side.json": {Data: []byte(`(globalThis.__PWNED = 1, {"x": 2})`)},
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	// side.json's bytes form a JS expression with a side effect. Importing it must
	// NOT run that side effect: JSON.parse rejects the non-JSON and the import
	// throws, and __PWNED is never set.
	_, err := rt.RunModule(context.Background(), "main.mjs", `
		globalThis.r = {};
		try {
			await import("./side.json");
			r.imported = true;
		} catch (e) {
			r.threw = true;
		}
		r.pwned = globalThis.__PWNED ?? "unset";
	`)
	if err != nil {
		t.Fatalf("RunModule error: %v", err)
	}
	if got := evalStr(t, js, "String(r.pwned)"); got != "unset" {
		t.Fatalf("ESM .json import executed its payload: __PWNED = %q", got)
	}
	if evalVal(t, js, "!!r.imported").Bool() {
		t.Error("importing non-JSON .json succeeded; expected a parse error")
	}
}

// TestFSPositionedReadWrite verifies readSync/writeSync with an explicit numeric
// position behave like pread/pwrite: they act at that offset and leave the file
// position unchanged.
func TestFSPositionedReadWrite(t *testing.T) {
	fsys := fstest.MapFS{"f": {Data: []byte("ABCDEFGHIJKLMNOP")}}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	r, err := rt.RunScript(context.Background(), `
		const fs = require("fs");
		globalThis.r = {};
		const fd = fs.openSync("/f", "r");
		const b = Buffer.alloc(4);
		fs.readSync(fd, b, 0, 4, 8);          // positioned read at offset 8
		r.at8 = b.toString();
		fs.readSync(fd, b, 0, 4, null);       // current position must still be 0
		r.at0 = b.toString();
		fs.closeSync(fd);
	`)
	if err != nil {
		t.Fatalf("RunScript error: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	if got := evalStr(t, js, "r.at8"); got != "IJKL" {
		t.Errorf("positioned read = %q, want IJKL", got)
	}
	if got := evalStr(t, js, "r.at0"); got != "ABCD" {
		t.Errorf("positioned read advanced the file position: at0 = %q, want ABCD", got)
	}
}
