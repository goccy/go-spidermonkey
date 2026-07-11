package spidermonkey

// White-box tests for the second line of the filesystem sandbox: the WASI layer.
// A guest script cannot name a file (see TestGuestHasNoFilesystemAPI), but the
// engine's wasi-libc still imports path_open, and base.DefaultWASI preopens the
// host "/", so the plumbing to reach the host filesystem exists. These tests
// drive path_open directly — the same entry point that import would reach — to
// prove the Config.FSAccess policy actually gates it: denied by default, and
// allowed only for exactly what a supplied hook permits.

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/spidermonkeywasm2go/base"
)

// wasi errno values returned by path_open (see wasi-libc errno.h).
const (
	wasiESUCCESS int32 = 0
	wasiEACCES   int32 = 2
)

func mustInterp(t *testing.T, cfg Config) *Interpreter {
	t.Helper()
	i, err := NewInterpreter(cfg)
	if err != nil {
		t.Fatalf("NewInterpreter: %v", err)
	}
	t.Cleanup(func() {
		if err := i.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return i
}

// openPath drives WASI path_open against the interpreter's host for guestPath
// and returns the wasi errno (0 == opened). The preopen root is "/", so
// guestPath is the absolute host path. A successful open is closed again
// immediately. The path and result words are scribbled into a scratch region at
// the very top of linear memory and restored afterwards, so driving the syscall
// cannot disturb the live engine.
func openPath(t *testing.T, i *Interpreter, guestPath string, write bool) int32 {
	t.Helper()
	g := i.m.g
	var rights int64 = 1 << 1 // fd_read
	if write {
		rights = 1 << 6 // fd_write
	}
	var errno int32
	base.AccessMemory(g, func(mem []byte) {
		outPtr := int32(len(mem)) - 8
		pathPtr := outPtr - int32(len(guestPath)) - 8
		if pathPtr < 0 {
			t.Fatalf("linear memory too small (%d bytes) for the test path", len(mem))
		}
		saved := append([]byte(nil), mem[pathPtr:outPtr+4]...)
		copy(mem[pathPtr:], guestPath)
		errno = i.wasi.Path_open(g, 3 /*preopen dirFd*/, 0, pathPtr, int32(len(guestPath)), 0, rights, 0, 0, outPtr)
		if errno == wasiESUCCESS {
			i.wasi.Fd_close(g, int32(binary.LittleEndian.Uint32(mem[outPtr:])))
		}
		copy(mem[pathPtr:], saved)
	})
	return errno
}

// The default policy (Config.FSAccess nil) denies every file open, even though
// the WASI host preopens the real "/". Without this, a path_open would open the
// host file.
func TestFilesystemDeniedByDefault(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(p, []byte("classified"), 0o600); err != nil {
		t.Fatal(err)
	}
	i := mustInterp(t, Config{})
	if e := openPath(t, i, p, false); e != wasiEACCES {
		t.Fatalf("default policy: opening %s returned errno %d, want EACCES(%d) — the guest reached the host filesystem", p, e, wasiEACCES)
	}
}

// A supplied FSAccess hook is a real gate: it permits exactly what it returns
// true for and denies the rest, and it is consulted with the guest path.
func TestFilesystemAccessHookGrantsAndDenies(t *testing.T) {
	dir := t.TempDir()
	allowed := filepath.Join(dir, "ok.txt")
	blocked := filepath.Join(dir, "no.txt")
	for _, p := range []string{allowed, blocked} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var seen []string
	i := mustInterp(t, Config{
		FSAccess: func(path string, write bool) bool {
			seen = append(seen, path)
			return path == allowed
		},
	})

	if e := openPath(t, i, allowed, false); e != wasiESUCCESS {
		t.Errorf("allowed path %s: errno %d, want success", allowed, e)
	}
	if e := openPath(t, i, blocked, false); e != wasiEACCES {
		t.Errorf("blocked path %s: errno %d, want EACCES(%d)", blocked, e, wasiEACCES)
	}
	if len(seen) != 2 || seen[0] != allowed || seen[1] != blocked {
		t.Errorf("hook was consulted with %v, want [%q %q]", seen, allowed, blocked)
	}
}

// The hook sees whether the open is for writing, so a policy can be read-only.
func TestFilesystemAccessHookSeesWriteFlag(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotWrite, called bool
	i := mustInterp(t, Config{
		FSAccess: func(path string, write bool) bool { called, gotWrite = true, write; return true },
	})
	if e := openPath(t, i, p, true); e != wasiESUCCESS {
		t.Fatalf("write open of %s: errno %d, want success", p, e)
	}
	if !called || !gotWrite {
		t.Errorf("hook write flag: called=%v write=%v, want called with write=true", called, gotWrite)
	}
}
