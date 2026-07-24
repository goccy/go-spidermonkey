package nodejs_test

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/memfs"
)

// TestBufferFillAndRangeChecks pins the Round-7 Buffer fixes: fill(string) fills
// with the string pattern (not zeros), and the 8-bit accessors throw RangeError
// out of range instead of silently returning undefined / no-op writing.
func TestBufferFillAndRangeChecks(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		r.fillStr = Buffer.alloc(4).fill("x").toString();
		r.fillPat = Buffer.alloc(5).fill("ab").toString();
		r.fillRange = (() => { const b = Buffer.alloc(4); b.fill("z", 1, 3); return b.toString("hex"); })();
		try { Buffer.from([1]).readUInt8(5); r.readOOB = "no-throw"; }
		catch (e) { r.readOOB = e.constructor.name; }
		try { Buffer.alloc(1).writeUInt8(9, 5); r.writeOOB = "no-throw"; }
		catch (e) { r.writeOOB = e.constructor.name; }
	`)
	for expr, want := range map[string]string{
		"r.fillStr":   "xxxx",
		"r.fillPat":   "ababa",
		"r.fillRange": "007a7a00",
		"r.readOOB":   "RangeError",
		"r.writeOOB":  "RangeError",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// TestFSOpenWTruncates verifies openSync(path, "w") + closeSync creates/truncates
// the file even when nothing is written, and createWriteStream honors `start`.
func TestFSOpenWTruncates(t *testing.T) {
	fsys := memfs.New()
	if err := fsys.WriteFile("old.txt", []byte("STALE"), 0o644); err != nil {
		t.Fatal(err)
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runScript(t, rt, `
		const fs = require("fs");
		globalThis.r = {};
		// w + immediate close must truncate the existing file to empty.
		fs.closeSync(fs.openSync("/old.txt", "w"));
		r.truncated = fs.readFileSync("/old.txt", "utf8");
		// w on a new path must create an (empty) file.
		fs.closeSync(fs.openSync("/created.txt", "w"));
		r.created = fs.existsSync("/created.txt");
	`)
	if got := evalStr(t, js, "r.truncated"); got != "" {
		t.Errorf(`openSync("w")+close did not truncate: %q`, got)
	}
	if got := evalStr(t, js, "String(r.created)"); got != "true" {
		t.Errorf(`openSync("w")+close did not create the file`)
	}
}

func TestFSWriteStreamStart(t *testing.T) {
	fsys := memfs.New()
	if err := fsys.WriteFile("f.txt", []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	r, err := rt.RunScript(context.Background(), `
		const fs = require("fs");
		globalThis.r = {};
		const ws = fs.createWriteStream("/f.txt", { flags: "r+", start: 3 });
		ws.on("finish", () => { r.after = fs.readFileSync("/f.txt", "utf8"); });
		ws.write("XY");
		ws.end();
	`)
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("script threw: %v", r.Error)
	}
	if got := evalStr(t, js, "r.after"); got != "012XY56789" {
		t.Errorf("createWriteStream start=3 wrote wrong bytes: %q, want 012XY56789", got)
	}
}

// TestReusedRuntimeExitDoesNotMaskError verifies a process.exit() in one run does
// not make a later run's genuine error read as a clean exit on a reused Runtime.
func TestReusedRuntimeExitDoesNotMaskError(t *testing.T) {
	_, rt := newRuntime(t, spidermonkey.Config{})
	if _, err := rt.RunScript(context.Background(), `process.exit(0);`); err != nil {
		t.Fatalf("first run: %v", err)
	}
	r, err := rt.RunScript(context.Background(), `throw new Error("real failure");`)
	if err != nil {
		t.Fatalf("second run returned a Go error: %v", err)
	}
	if r.Error == nil {
		t.Fatal("second run's genuine throw was masked as a clean exit by the prior process.exit")
	}
	if rt.Exited() {
		t.Error("Exited() still true after a run that did not call process.exit")
	}
}
