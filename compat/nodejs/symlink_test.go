package nodejs_test

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	gofs "github.com/goccy/go-spidermonkey/fs"
)

// fs.symlink threw ENOSYS, so 31 suite files could not even set up their
// fixtures. Links live in the runtime rather than in the filesystem — this
// embedding's FS is an abstract fs.FS with no link concept — which means a
// link is visible to the program that made it and to nothing else. That is the
// limitation being pinned here alongside the behaviour.
func TestSymlinks(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{FS: gofs.NewMemFS()})

	if _, err := rt.RunScript(context.Background(), `
		const fs = require("fs");
		const out = [];
		const t = (n, f) => { try { out.push(n + "=" + f()); } catch (e) { out.push(n + "!" + (e.code || e.name)); } };
		fs.mkdirSync("/d", { recursive: true });
		fs.writeFileSync("/d/real.txt", "hello");

		t("create", () => { fs.symlinkSync("/d/real.txt", "/d/link.txt"); return "ok"; });
		// Reading THROUGH the link is the whole point.
		t("read", () => fs.readFileSync("/d/link.txt", "utf8"));
		t("readlink", () => fs.readlinkSync("/d/link.txt"));
		// lstat sees the link, stat sees what it points at.
		t("lstat", () => fs.lstatSync("/d/link.txt").isSymbolicLink());
		t("lstat-file", () => fs.lstatSync("/d/link.txt").isFile());
		t("stat", () => fs.statSync("/d/link.txt").isFile());
		// A relative target resolves against the link's own directory.
		t("relative", () => { fs.symlinkSync("real.txt", "/d/rel.txt"); return fs.readFileSync("/d/rel.txt", "utf8"); });
		// A link to a DIRECTORY works for paths beneath it.
		t("dir", () => { fs.symlinkSync("/d", "/dl"); return fs.readFileSync("/dl/real.txt", "utf8"); });
		// A cycle is ELOOP, not a hang.
		t("loop", () => { fs.symlinkSync("/a", "/b"); fs.symlinkSync("/b", "/a"); return fs.readFileSync("/a", "utf8"); });
		t("exists", () => { fs.symlinkSync("/d/real.txt", "/d/link.txt"); return "NO-THROW"; });
		t("notalink", () => fs.readlinkSync("/d/real.txt"));
		globalThis.__r = out.join(" | ");
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}

	got := evalStr(t, js, `globalThis.__r`)
	want := "create=ok | read=hello | readlink=/d/real.txt | lstat=true | lstat-file=false | stat=true | " +
		"relative=hello | dir=hello | loop!ELOOP | exists!EEXIST | notalink!EINVAL"
	if got != want {
		t.Errorf("symlinks =\n %s\nwant\n %s", got, want)
	}
}
