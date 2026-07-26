package nodejs_test

import (
	"bytes"
	"context"
	"testing"
	"testing/fstest"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/fs"
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
