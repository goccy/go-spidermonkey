package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/memfs"
)

func TestFSExtra(t *testing.T) {
	fsys := memfs.New()
	fsys.MkdirAll("src/nested", 0o755)
	fsys.WriteFile("src/a.txt", []byte("alpha"), 0o644)
	fsys.WriteFile("src/nested/b.txt", []byte("beta"), 0o644)
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})

	runScript(t, rt, `
		const fs = require("fs");
		globalThis.r = {};

		// copyFile
		fs.copyFileSync("/src/a.txt", "/src/a-copy.txt");
		r.copied = fs.readFileSync("/src/a-copy.txt", "utf8");

		// cp (recursive directory)
		fs.cpSync("/src", "/dst", { recursive: true });
		r.cpNested = fs.readFileSync("/dst/nested/b.txt", "utf8");

		// mkdtemp
		const tmp = fs.mkdtempSync("/tmp-");
		r.tmpExists = fs.existsSync(tmp);
		fs.writeFileSync(tmp + "/f.txt", "temp");
		r.tmpRead = fs.readFileSync(tmp + "/f.txt", "utf8");

		// rm recursive
		fs.rmSync("/dst", { recursive: true });
		r.dstGone = !fs.existsSync("/dst");

		// rm force on missing = no throw
		fs.rmSync("/does-not-exist", { force: true });
		r.forceOk = true;

		// readdir withFileTypes
		const entries = fs.readdirSync("/src", { withFileTypes: true });
		r.dirents = entries.map((e) => e.name + ":" + (e.isDirectory() ? "d" : "f")).sort().join(",");
	`)
	for expr, want := range map[string]string{
		"r.copied":    "alpha",
		"r.cpNested":  "beta",
		"r.tmpExists": "true",
		"r.tmpRead":   "temp",
		"r.dstGone":   "true",
		"r.forceOk":   "true",
		"r.dirents":   "a-copy.txt:f,a.txt:f,nested:d",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestFSFileDescriptors(t *testing.T) {
	fsys := memfs.New()
	fsys.WriteFile("data.bin", []byte("0123456789"), 0o644)
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})

	runScript(t, rt, `
		const fs = require("fs");
		globalThis.r = {};

		// Read via fd with position.
		const fd = fs.openSync("/data.bin", "r");
		const buf = Buffer.alloc(4);
		const n = fs.readSync(fd, buf, 0, 4, 2);
		r.read = buf.slice(0, n).toString("utf8");
		r.fstatSize = fs.fstatSync(fd).size;
		fs.closeSync(fd);

		// Write via fd.
		const wfd = fs.openSync("/out.bin", "w");
		fs.writeSync(wfd, Buffer.from("hello "));
		fs.writeSync(wfd, "world");
		fs.closeSync(wfd);
		r.written = fs.readFileSync("/out.bin", "utf8");

		// Append flag.
		const afd = fs.openSync("/out.bin", "a");
		fs.writeSync(afd, "!");
		fs.closeSync(afd);
		r.appended = fs.readFileSync("/out.bin", "utf8");
	`)
	for expr, want := range map[string]string{
		"r.read":      "2345",
		"r.fstatSize": "10",
		"r.written":   "hello world",
		"r.appended":  "hello world!",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestFSStreams(t *testing.T) {
	fsys := memfs.New()
	fsys.WriteFile("big.txt", []byte("line one\nline two\nline three"), 0o644)
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})

	runScript(t, rt, `
		const fs = require("fs");
		globalThis.r = {};
		const rs = fs.createReadStream("/big.txt", "utf8");
		let content = "";
		rs.on("data", (c) => { content += c; });
		rs.on("end", () => { r.read = content; });

		const ws = fs.createWriteStream("/written.txt");
		ws.write("streamed ");
		ws.end("content");
		ws.on("finish", () => { r.wrote = fs.readFileSync("/written.txt", "utf8"); });
	`)
	if got := evalStr(t, js, `r.read`); got != "line one\nline two\nline three" {
		t.Errorf("read stream = %q", got)
	}
	if got := evalStr(t, js, `r.wrote`); got != "streamed content" {
		t.Errorf("write stream = %q", got)
	}
}
