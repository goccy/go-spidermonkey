package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/fs"
)

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
