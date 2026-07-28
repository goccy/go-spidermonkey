package nodejs_test

import (
	"testing"
	"testing/fstest"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// process.chdir() moves the working directory, and a RELATIVE path resolves
// against it — the two halves have to arrive together. chdir used to throw
// outright; making it succeed while every relative read still resolved against
// the filesystem root would have been worse than refusing it, so fs paths go
// through one resolver.
func TestProcessChdirAndRelativePaths(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{FS: fstest.MapFS{
		"app/sub/file.txt": {Data: []byte("contents")},
		"app/other.txt":    {Data: []byte("other")},
	}})
	runScript(t, rt, `
		const fs = require("fs");
		globalThis.r = [];
		r.push("start:" + process.cwd());
		process.chdir("/app");
		r.push("abs:" + process.cwd());
		r.push("read-rel:" + fs.readFileSync("other.txt", "utf8"));
		process.chdir("sub");
		r.push("rel:" + process.cwd());
		r.push("read-after:" + fs.readFileSync("file.txt", "utf8"));
		r.push("read-abs:" + fs.readFileSync("/app/other.txt", "utf8"));
		try { process.chdir("/nope"); r.push("missing:NO-THROW"); }
		catch (e) { r.push("missing:" + e.code); }
		try { process.chdir("/app/other.txt"); r.push("file:NO-THROW"); }
		catch (e) { r.push("file:" + e.code); }
	`)
	want := "start:/ | abs:/app | read-rel:other | rel:/app/sub | read-after:contents | " +
		"read-abs:other | missing:ENOENT | file:ENOTDIR"
	if got := evalStr(t, js, `r.join(" | ")`); got != want {
		t.Errorf("chdir behaviour =\n %s\nwant\n %s", got, want)
	}
}
