package nodejs_test

import (
	"bytes"
	"context"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/fs"
	gofs "github.com/goccy/go-spidermonkey/fs"
	"testing"
	"testing/fstest"
)

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

// TestFSOpenWTruncates verifies openSync(path, "w") + closeSync creates/truncates
// the file even when nothing is written, and createWriteStream honors `start`.
func TestFSOpenWTruncates(t *testing.T) {
	fsys := fs.NewMemFS()
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
	fsys := fs.NewMemFS()
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

// TestFSWriteFDPositionCapped verifies a wildly large write position is rejected
// (EFBIG) rather than densely allocating gigabytes of host memory.
func TestFSWriteFDPositionCapped(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{FS: fs.NewMemFS()})
	runScript(t, rt, `
		const fs = require("fs");
		globalThis.r = {};
		const fd = fs.openSync("/x", "w");
		try { fs.writeSync(fd, Buffer.from("a"), 0, 1, 50_000_000_000); r.threw = false; }
		catch { r.threw = true; }
		fs.closeSync(fd);
	`)
	if got := evalStr(t, js, "String(r.threw)"); got != "true" {
		t.Errorf("huge write position not rejected: %q", got)
	}
}

// TestFSWriteEncoding verifies writeFileSync decodes a string per the encoding
// option (hex/base64), not as UTF-8.
func TestFSWriteEncoding(t *testing.T) {
	fsys := fs.NewMemFS()
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runScript(t, rt, `
		const fs = require("fs");
		globalThis.r = {};
		fs.writeFileSync("/hex.bin", "deadbeef", "hex");
		r.hexLen = fs.readFileSync("/hex.bin").length;
		r.hex = fs.readFileSync("/hex.bin").toString("hex");
		fs.writeFileSync("/b64.bin", "AQID", { encoding: "base64" });
		r.b64 = fs.readFileSync("/b64.bin").toString("hex");
	`)
	if got := evalStr(t, js, "String(r.hexLen)"); got != "4" {
		t.Errorf("hex write length = %q, want 4", got)
	}
	if got := evalStr(t, js, "r.hex"); got != "deadbeef" {
		t.Errorf("hex write = %q, want deadbeef", got)
	}
	if got := evalStr(t, js, "r.b64"); got != "010203" {
		t.Errorf("base64 write = %q, want 010203", got)
	}
}

// TestWriteFileSyncAppendFlag verifies fs.writeFileSync honors { flag: "a" }
// (append) instead of truncating — the common append-a-log-line pattern.
func TestWriteFileSyncAppendFlag(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{FS: fs.NewMemFS()})
	runScript(t, rt, `
		globalThis.r = {};
		const fs = require("fs");
		fs.writeFileSync("log.txt", "line1\n");
		fs.writeFileSync("log.txt", "line2\n", { flag: "a" });
		r.appended = fs.readFileSync("log.txt", "utf8");
		fs.writeFileSync("log.txt", "fresh\n"); // default "w" still truncates
		r.truncated = fs.readFileSync("log.txt", "utf8");
	`)
	if got := evalStr(t, js, `r.appended`); got != "line1\nline2\n" {
		t.Errorf("append flag = %q, want line1\\nline2\\n (flag:a truncated)", got)
	}
	if got := evalStr(t, js, `r.truncated`); got != "fresh\n" {
		t.Errorf("default write = %q, want fresh\\n (default should truncate)", got)
	}
}

// TestFSFdCallbackAPI verifies fs.open/fstat/read/close (the graceful-fs path)
// and fs.promises.open (FileHandle) work.
func TestFSFdCallbackAPI(t *testing.T) {
	fsys := fs.NewMemFS()
	fsys.MkdirAll("data", 0o755)
	fsys.WriteFile("data/x.txt", []byte("hello"), 0o644)
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runScript(t, rt, `
		globalThis.r = {};
		const fs = require("fs");
		fs.open("data/x.txt", "r", (err, fd) => {
			r.fdType = typeof fd;
			fs.fstat(fd, (e, st) => {
				r.size = st.size;
				fs.close(fd, () => { r.closed = true; });
			});
		});
		(async () => {
			const fh = await require("fs/promises").open("data/x.txt", "r");
			r.fhRead = (await fh.readFile("utf8"));
			await fh.close();
		})();
	`)
	if got := evalStr(t, js, `r.fdType`); got != "number" {
		t.Errorf("fs.open fd type = %q, want number", got)
	}
	if got := evalStr(t, js, `String(r.size)`); got != "5" {
		t.Errorf("fs.fstat size = %q, want 5", got)
	}
	if got := evalStr(t, js, `String(r.closed)`); got != "true" {
		t.Errorf("fs.close callback didn't fire: %q", got)
	}
	if got := evalStr(t, js, `r.fhRead`); got != "hello" {
		t.Errorf("FileHandle.readFile = %q, want hello", got)
	}
}

// TestFSStatModeTypeBits verifies stats.mode includes the file-type bits so
// (mode & S_IFMT) === S_IFDIR / S_IFREG works.
func TestFSStatModeTypeBits(t *testing.T) {
	fsys := fs.NewMemFS()
	fsys.MkdirAll("data", 0o755)
	fsys.WriteFile("data/x.txt", []byte("hi"), 0o644)
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runScript(t, rt, `
		globalThis.r = {};
		const fs = require("fs");
		const C = fs.constants;
		r.dirIsDir = (fs.statSync("data").mode & C.S_IFMT) === C.S_IFDIR;
		r.fileIsReg = (fs.statSync("data/x.txt").mode & C.S_IFMT) === C.S_IFREG;
	`)
	if got := evalStr(t, js, `String(r.dirIsDir)`); got != "true" {
		t.Errorf("dir S_IFDIR check = %q, want true", got)
	}
	if got := evalStr(t, js, `String(r.fileIsReg)`); got != "true" {
		t.Errorf("file S_IFREG check = %q, want true", got)
	}
}

// TestCreateReadStreamRange verifies createReadStream honors an inclusive
// {start,end} range and streams the exact bytes.
func TestCreateReadStreamRange(t *testing.T) {
	fsys := fs.NewMemFS()
	fsys.MkdirAll("data", 0o755)
	fsys.WriteFile("data/abc.txt", []byte("ABCDEFGHIJ"), 0o644)
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runScript(t, rt, `
		globalThis.r = {};
		const fs = require("fs");
		const rs = fs.createReadStream("data/abc.txt", { start: 2, end: 5 });
		const chunks = [];
		rs.on("data", (c) => chunks.push(c));
		rs.on("end", () => { r.range = Buffer.concat(chunks).toString(); });
	`)
	if got := evalStr(t, js, `r.range`); got != "CDEF" {
		t.Errorf("createReadStream range [2,5] = %q, want CDEF", got)
	}
}

// TestCreateReadStreamDestroyClosesFd verifies destroy()ing a createReadStream
// mid-read closes the underlying fd (no fd/host-memory leak).
func TestCreateReadStreamDestroyClosesFd(t *testing.T) {
	fsys := fs.NewMemFS()
	fsys.MkdirAll("data", 0o755)
	fsys.WriteFile("data/big.txt", bytes.Repeat([]byte("x"), 200*1024), 0o644) // > hwm
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runScript(t, rt, `
		globalThis.r = {};
		const fs = require("fs");
		const rs = fs.createReadStream("data/big.txt");
		let fd;
		rs.on("open", (openedFd) => { fd = openedFd; });
		rs.once("data", () => {
			rs.destroy(); // abort mid-stream
			process.nextTick(() => { try { fs.fstatSync(fd); r.fdState = "open"; } catch (e) { r.fdState = e.code; } });
		});
	`)
	if got := evalStr(t, js, `r.fdState`); got != "EBADF" {
		t.Errorf("fd after createReadStream destroy = %q, want EBADF (fd leaked)", got)
	}
}

func TestFSExtra(t *testing.T) {
	fsys := fs.NewMemFS()
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
	fsys := fs.NewMemFS()
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
	fsys := fs.NewMemFS()
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

// The exclusive open modifier ('x' in wx/xw/ax/...) must fail with EEXIST when
// the target exists — Node never silently overwrites under an exclusive flag.
// Regression: writeFileSync only inspected flag[0] === 'a', so { flag: "wx" }
// clobbered existing files.
func TestWriteFileExclusiveFlagEEXIST(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{FS: fs.NewMemFS()})
	runScript(t, rt, `
		globalThis.r = {};
		const fs = require("fs");
		fs.writeFileSync("/f.txt", "original");

		const codeOf = (fn) => { try { fn(); return "no-throw"; } catch (e) { return e.code; } };
		r.wx = codeOf(() => fs.writeFileSync("/f.txt", "clobber", { flag: "wx" }));
		r.xw = codeOf(() => fs.writeFileSync("/f.txt", "clobber", { flag: "xw" }));
		r.ax = codeOf(() => fs.appendFileSync("/f.txt", "clobber", { flag: "ax" }));
		r.openWx = codeOf(() => fs.openSync("/f.txt", "wx"));
		r.survived = fs.readFileSync("/f.txt", "utf8");

		// Error shape matches the other fs errors (syscall + path populated).
		try { fs.writeFileSync("/f.txt", "x", { flag: "wx" }); } catch (e) {
			r.syscall = e.syscall;
			r.path = e.path;
			r.msgHasCode = e.message.includes("EEXIST");
		}

		// Exclusive on a NEW file must still create it.
		fs.writeFileSync("/new.txt", "fresh", { flag: "wx" });
		r.created = fs.readFileSync("/new.txt", "utf8");
		const fd = fs.openSync("/new2.txt", "wx");
		fs.writeSync(fd, "fd-fresh");
		fs.closeSync(fd);
		r.fdCreated = fs.readFileSync("/new2.txt", "utf8");

		// promises + callback forms surface the same EEXIST.
		(async () => {
			r.promise = await fs.promises.writeFile("/f.txt", "x", { flag: "wx" }).then(() => "no-throw", (e) => e.code);
			r.appendPromise = await fs.promises.appendFile("/f.txt", "x", { flag: "ax" }).then(() => "no-throw", (e) => e.code);
		})();
		fs.writeFile("/f.txt", "x", { flag: "wx" }, (err) => { r.callback = err ? err.code : "no-throw"; });
	`)
	for expr, want := range map[string]string{
		"r.wx":            "EEXIST",
		"r.xw":            "EEXIST",
		"r.ax":            "EEXIST",
		"r.openWx":        "EEXIST",
		"r.survived":      "original",
		"r.syscall":       "open",
		"r.path":          "/f.txt",
		"r.msgHasCode":    "true",
		"r.created":       "fresh",
		"r.fdCreated":     "fd-fresh",
		"r.promise":       "EEXIST",
		"r.appendPromise": "EEXIST",
		"r.callback":      "EEXIST",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// fs.realpath* must fail with ENOENT for a missing path (Node stats the
// target); an existing path still resolves. Regression: realpathSync was a
// bare path.resolve, so missing paths "resolved" successfully.
func TestRealpathMissingPathENOENT(t *testing.T) {
	fsys := fs.NewMemFS()
	fsys.MkdirAll("dir", 0o755)
	fsys.WriteFile("dir/f.txt", []byte("x"), 0o644)
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runScript(t, rt, `
		globalThis.r = {};
		const fs = require("fs");
		r.existing = fs.realpathSync("/dir/../dir/f.txt");
		try { fs.realpathSync("/nope.txt"); r.missing = "no-throw"; } catch (e) { r.missing = e.code; }
		(async () => {
			r.promise = await fs.promises.realpath("/nope.txt").then((p) => "resolved:" + p, (e) => e.code);
			r.promiseOk = await fs.promises.realpath("/dir/f.txt");
		})();
		fs.realpath("/nope.txt", (err) => { r.callback = err ? err.code : "no-throw"; });
	`)
	for expr, want := range map[string]string{
		"r.existing":  "/dir/f.txt",
		"r.missing":   "ENOENT",
		"r.promise":   "ENOENT",
		"r.promiseOk": "/dir/f.txt",
		"r.callback":  "ENOENT",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// readdir with { recursive: true } must walk subdirectories and return paths
// relative to the starting directory joined with '/' ("sub/b.txt"), and
// withFileTypes Dirents must carry the containing directory as parentPath.
// Regression: the option was ignored, returning only the top level.
func TestReaddirRecursive(t *testing.T) {
	fsys := fs.NewMemFS()
	fsys.MkdirAll("data/sub/deep", 0o755)
	fsys.WriteFile("data/a.txt", []byte("a"), 0o644)
	fsys.WriteFile("data/sub/b.txt", []byte("b"), 0o644)
	fsys.WriteFile("data/sub/deep/c.txt", []byte("c"), 0o644)
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runScript(t, rt, `
		globalThis.r = {};
		const fs = require("fs");
		r.flat = fs.readdirSync("/data").sort().join(",");
		r.rec = fs.readdirSync("/data", { recursive: true }).sort().join(",");
		const ents = fs.readdirSync("/data", { recursive: true, withFileTypes: true });
		r.recTypes = ents.map((e) => e.parentPath + "|" + e.name + "|" + (e.isDirectory() ? "d" : "f")).sort().join(",");
		(async () => {
			r.promise = (await fs.promises.readdir("/data", { recursive: true })).sort().join(",");
		})();
	`)
	for expr, want := range map[string]string{
		"r.flat": "a.txt,sub",
		"r.rec":  "a.txt,sub,sub/b.txt,sub/deep,sub/deep/c.txt",
		"r.recTypes": "/data/sub/deep|c.txt|f,/data/sub|b.txt|f,/data/sub|deep|d," +
			"/data|a.txt|f,/data|sub|d",
		"r.promise": "a.txt,sub,sub/b.txt,sub/deep,sub/deep/c.txt",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// chmod and utimes must persist metadata that subsequent stats reflect (mtime
// movement matters for touch-based cache invalidation). Regression: both were
// silent no-ops even on memfs, which can store the metadata.
func TestChmodUtimesPersistMetadata(t *testing.T) {
	fsys := fs.NewMemFS()
	fsys.WriteFile("f.txt", []byte("data"), 0o644)
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runScript(t, rt, `
		globalThis.r = {};
		const fs = require("fs");

		fs.chmodSync("/f.txt", 0o600);
		r.mode600 = (fs.statSync("/f.txt").mode & 0o777).toString(8);
		fs.chmodSync("/f.txt", "755"); // octal string form
		r.mode755 = (fs.statSync("/f.txt").mode & 0o777).toString(8);
		r.stillFile = fs.statSync("/f.txt").isFile();

		// utimes: numbers are epoch seconds; Date works too.
		fs.utimesSync("/f.txt", 1000000000, 1000000000);
		r.mtimeSecs = String(fs.statSync("/f.txt").mtimeMs);
		fs.utimesSync("/f.txt", new Date(2000000000000), new Date(2000000000000));
		r.mtimeDate = String(fs.statSync("/f.txt").mtimeMs);
		fs.lutimesSync("/f.txt", 3000000000, 3000000000); // symlink-less alias
		r.mtimeL = String(fs.statSync("/f.txt").mtimeMs);

		// Missing targets fail with ENOENT like Node.
		try { fs.chmodSync("/nope", 0o600); r.chmodMissing = "no-throw"; } catch (e) { r.chmodMissing = e.code; }
		try { fs.utimesSync("/nope", 1, 1); r.utimesMissing = "no-throw"; } catch (e) { r.utimesMissing = e.code; }

		// promises + callback forms (separate file for the callback so the
		// microtask orderings can't overwrite each other's timestamps).
		fs.writeFileSync("/f2.txt", "cb");
		(async () => {
			await fs.promises.chmod("/f.txt", 0o640);
			r.promiseMode = (fs.statSync("/f.txt").mode & 0o777).toString(8);
			await fs.promises.utimes("/f.txt", 4000000000, 4000000000);
			r.promiseMtime = String(fs.statSync("/f.txt").mtimeMs);
		})();
		fs.utimes("/f2.txt", 5, 5, (err) => {
			r.cbErr = err === null ? "null" : String(err && err.code);
			r.cbMtime = String(fs.statSync("/f2.txt").mtimeMs);
		});
	`)
	for expr, want := range map[string]string{
		"r.mode600":       "600",
		"r.mode755":       "755",
		"r.stillFile":     "true",
		"r.mtimeSecs":     "1000000000000",
		"r.mtimeDate":     "2000000000000",
		"r.mtimeL":        "3000000000000",
		"r.chmodMissing":  "ENOENT",
		"r.utimesMissing": "ENOENT",
		"r.promiseMode":   "640",
		"r.promiseMtime":  "4000000000000",
		"r.cbErr":         "null",
		"r.cbMtime":       "5000",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// fs.promises.readFile/writeFile honor { signal } (a pre-aborted signal
// rejects with an AbortError and writes nothing); stat { bigint: true }
// returns BigInt fields including the *Ns nanosecond variants; and
// FileHandle.truncate resizes the open file.
func TestPromisesSignalBigintStatAndHandleTruncate(t *testing.T) {
	fsys := fs.NewMemFS()
	fsys.WriteFile("f.txt", []byte("hello world"), 0o644)
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runScript(t, rt, `
		globalThis.r = {};
		const fs = require("fs");
		(async () => {
			// Pre-aborted signal -> AbortError rejection, file untouched.
			const ac = new AbortController();
			ac.abort();
			r.readAbort = await fs.promises.readFile("/f.txt", { signal: ac.signal })
				.then(() => "no-throw", (e) => e.name + "/" + e.code);
			r.writeAbort = await fs.promises.writeFile("/f.txt", "clobber", { signal: ac.signal })
				.then(() => "no-throw", (e) => e.name + "/" + e.code);
			r.untouched = fs.readFileSync("/f.txt", "utf8");
			// A live (unaborted) signal doesn't interfere.
			const ac2 = new AbortController();
			r.liveRead = String(await fs.promises.readFile("/f.txt", { signal: ac2.signal, encoding: "utf8" }));

			// bigint stats.
			fs.utimesSync("/f.txt", 1234, 1234);
			const st = fs.statSync("/f.txt", { bigint: true });
			r.sizeType = typeof st.size;
			r.size = String(st.size);
			r.mtimeMsType = typeof st.mtimeMs;
			r.mtimeMs = String(st.mtimeMs);
			r.mtimeNs = String(st.mtimeNs);
			r.atimeNsType = typeof st.atimeNs;
			r.modeType = typeof st.mode;
			r.isFile = st.isFile();
			const stNum = fs.statSync("/f.txt");
			r.plainStillNumber = typeof stNum.size;
			r.promiseBigint = typeof (await fs.promises.stat("/f.txt", { bigint: true })).size;

			// FileHandle.truncate: shrink, then zero-extend.
			const fh = await fs.promises.open("/f.txt", "r+");
			await fh.truncate(5);
			await fh.close();
			r.shrunk = fs.readFileSync("/f.txt", "utf8");
			const fh2 = await fs.promises.open("/f.txt", "r+");
			await fh2.truncate(7);
			await fh2.close();
			const grown = fs.readFileSync("/f.txt");
			r.grownLen = grown.length;
			r.grownTail = grown[5] + "," + grown[6];
			// path-level truncate too.
			fs.truncateSync("/f.txt", 2);
			r.pathTrunc = fs.readFileSync("/f.txt", "utf8");
		})();
	`)
	for expr, want := range map[string]string{
		"r.readAbort":        "AbortError/ABORT_ERR",
		"r.writeAbort":       "AbortError/ABORT_ERR",
		"r.untouched":        "hello world",
		"r.liveRead":         "hello world",
		"r.sizeType":         "bigint",
		"r.size":             "11",
		"r.mtimeMsType":      "bigint",
		"r.mtimeMs":          "1234000",
		"r.mtimeNs":          "1234000000000",
		"r.atimeNsType":      "bigint",
		"r.modeType":         "bigint",
		"r.isFile":           "true",
		"r.plainStillNumber": "number",
		"r.promiseBigint":    "bigint",
		"r.shrunk":           "hello",
		"r.grownLen":         "7",
		"r.grownTail":        "0,0",
		"r.pathTrunc":        "he",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

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

// readSync with a null position reads from the CURRENT offset and advances, so
// a chunked loop walks the whole file instead of re-reading the first bytes.
func TestReadSyncNullPositionAdvances(t *testing.T) {
	fsys := fs.NewMemFS()
	fsys.WriteFile("data.bin", []byte("0123456789"), 0o644)
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runScript(t, rt, `
		const fs = require("fs");
		globalThis.r = {};
		const fd = fs.openSync("/data.bin", "r");
		const buf = Buffer.alloc(3);
		let out = "";
		for (;;) {
			const n = fs.readSync(fd, buf, 0, 3, null); // null => advance
			if (n === 0) break;
			out += buf.toString("utf8", 0, n);
		}
		fs.closeSync(fd);
		r.out = out;
	`)
	if got := evalStr(t, js, `r.out`); got != "0123456789" {
		t.Fatalf("chunked readSync = %q, want 0123456789 (null position must advance)", got)
	}
}
