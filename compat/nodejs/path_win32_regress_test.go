package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// path.win32 used to be an alias of the posix object (sep "/", no drive or
// UNC handling). It must implement real Windows semantics while path (and
// path.posix) keep posix behavior.
func TestPathWin32BasicOperations(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const path = require("path");
		const w = path.win32;
		globalThis.r = {};
		r.sep = w.sep;
		r.delimiter = w.delimiter;
		r.join = w.join("C:\\temp", "foo");
		r.joinMixed = w.join("C:/temp", "foo/bar", "..", "baz");
		r.joinRel = w.join("a", "b");
		r.absDrive = w.isAbsolute("C:\\temp");
		r.absDriveRel = w.isAbsolute("C:temp");
		r.absUNC = w.isAbsolute("\\\\server\\share");
		r.absSlash = w.isAbsolute("/foo");
		r.absPlain = w.isAbsolute("foo\\bar");
		r.base = w.basename("C:\\temp\\file.txt");
		r.baseSuffix = w.basename("C:\\temp\\file.txt", ".txt");
		r.baseDriveRel = w.basename("C:file.txt");
		r.baseMixed = w.basename("C:/temp/dir\\file.txt");
		r.dir = w.dirname("C:\\temp\\file.txt");
		r.dirRoot = w.dirname("C:\\temp");
		r.dirOfRoot = w.dirname("C:\\");
		r.ext = w.extname("C:\\temp\\archive.tar.gz");
	`)
	for expr, want := range map[string]string{
		"r.sep":          `\`,
		"r.delimiter":    ";",
		"r.join":         `C:\temp\foo`,
		"r.joinMixed":    `C:\temp\foo\baz`,
		"r.joinRel":      `a\b`,
		"r.absDrive":     "true",
		"r.absDriveRel":  "false",
		"r.absUNC":       "true",
		"r.absSlash":     "true",
		"r.absPlain":     "false",
		"r.base":         "file.txt",
		"r.baseSuffix":   "file",
		"r.baseDriveRel": "file.txt",
		"r.baseMixed":    "file.txt",
		"r.dir":          `C:\temp`,
		"r.dirRoot":      `C:\`,
		"r.dirOfRoot":    `C:\`,
		"r.ext":          ".gz",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestPathWin32ResolveNormalizeMixedSeparators(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const w = require("path").win32;
		globalThis.r = {};
		r.resolve = w.resolve("C:\\temp", "foo");
		r.resolveMixed = w.resolve("C:/temp", "foo/bar", "..\\baz");
		r.resolveRooted = w.resolve("C:\\a", "\\b", "c");
		r.resolveOtherDrive = w.resolve("D:\\x", "C:\\temp", "y");
		r.normalize = w.normalize("C:\\temp\\\\foo\\..\\bar");
		r.normalizeMixed = w.normalize("C:/a/b/../c");
		r.normalizeTrailing = w.normalize("C:\\temp\\foo\\");
		r.normalizeUNC = w.normalize("\\\\server\\share\\a\\..\\b");
		r.normalizeRel = w.normalize("a/b/../c");
	`)
	for expr, want := range map[string]string{
		"r.resolve":           `C:\temp\foo`,
		"r.resolveMixed":      `C:\temp\foo\baz`,
		"r.resolveRooted":     `C:\b\c`,
		"r.resolveOtherDrive": `C:\temp\y`,
		"r.normalize":         `C:\temp\bar`,
		"r.normalizeMixed":    `C:\a\c`,
		"r.normalizeTrailing": `C:\temp\foo\`,
		"r.normalizeUNC":      `\\server\share\b`,
		"r.normalizeRel":      `a\c`,
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestPathWin32ParseFormatRelative(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const w = require("path").win32;
		globalThis.r = {};
		const p = w.parse("C:\\temp\\file.txt");
		r.parse = [p.root, p.dir, p.base, p.ext, p.name].join("|");
		const u = w.parse("\\\\server\\share\\dir\\x.js");
		r.parseUNC = [u.root, u.dir, u.base, u.ext, u.name].join("|");
		const rel = w.parse("file.txt");
		r.parseBare = [rel.root, rel.dir, rel.base].join("|");
		r.format = w.format({ dir: "C:\\temp", base: "f.txt" });
		r.formatRoot = w.format({ root: "C:\\", base: "f.txt" });
		r.formatNameExt = w.format({ dir: "C:\\temp", name: "f", ext: ".txt" });
		r.relative = w.relative("C:\\a\\b", "C:\\a\\c\\d");
		r.relativeCase = w.relative("C:\\A\\b", "c:\\a\\c");
		r.relativeDrives = w.relative("C:\\a", "D:\\b");
		r.relativeSame = w.relative("C:\\a\\b", "C:\\a\\b");
	`)
	for expr, want := range map[string]string{
		"r.parse":          `C:\|C:\temp|file.txt|.txt|file`,
		"r.parseUNC":       `\\server\share\|\\server\share\dir|x.js|.js|x`,
		"r.parseBare":      `||file.txt`,
		"r.format":         `C:\temp\f.txt`,
		"r.formatRoot":     `C:\f.txt`,
		"r.formatNameExt":  `C:\temp\f.txt`,
		"r.relative":       `..\c\d`,
		"r.relativeCase":   `..\c`,
		"r.relativeDrives": `D:\b`,
		"r.relativeSame":   "",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// The default path export stays posix, and both flavors cross-reference each
// other like Node (path.posix.win32 === path.win32, path.win32.posix ===
// path.posix); path/win32 and path/posix resolve to the right objects.
func TestPathWin32PosixCrossReferences(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const path = require("path");
		globalThis.r = {};
		r.defaultIsPosix = path.join("a", "b") === "a/b" && path.sep === "/";
		r.posixSelf = path.posix === path;
		r.distinct = path.win32 !== path;
		r.posixWin32 = path.posix.win32 === path.win32;
		r.win32Posix = path.win32.posix === path.posix;
		r.win32Self = path.win32.win32 === path.win32;
		r.modWin32 = require("path/win32") === path.win32;
		r.modPosix = require("path/posix") === path.posix;
	`)
	for _, expr := range []string{
		"r.defaultIsPosix", "r.posixSelf", "r.distinct", "r.posixWin32",
		"r.win32Posix", "r.win32Self", "r.modWin32", "r.modPosix",
	} {
		if got := evalStr(t, js, expr); got != "true" {
			t.Errorf("%s = %s, want true", expr, got)
		}
	}
}
