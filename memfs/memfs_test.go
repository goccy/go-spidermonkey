package memfs_test

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"testing"
	"testing/fstest"

	"github.com/goccy/go-spidermonkey/memfs"
)

func TestFSConformance(t *testing.T) {
	fsys := memfs.New()
	if err := fsys.MkdirAll("a/b", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("hello.txt", []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("a/b/deep.txt", []byte("deep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fstest.TestFS(fsys, "hello.txt", "a/b/deep.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	fsys := memfs.New()
	if err := fsys.WriteFile("f.txt", []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := fs.ReadFile(fsys, "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first" {
		t.Errorf("content = %q, want %q", got, "first")
	}

	// Truncate-rewrite replaces, append extends.
	if err := fsys.WriteFile("f.txt", []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := fsys.OpenFile("f.txt", os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.(io.Writer).Write([]byte("+more")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	got, _ = fs.ReadFile(fsys, "f.txt")
	if string(got) != "second+more" {
		t.Errorf("content = %q, want %q", got, "second+more")
	}
}

func TestOpenFileFlags(t *testing.T) {
	fsys := memfs.New()

	if _, err := fsys.OpenFile("missing.txt", os.O_WRONLY, 0o644); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("write without O_CREATE: err = %v, want ErrNotExist", err)
	}
	if err := fsys.WriteFile("f.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.OpenFile("f.txt", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644); !errors.Is(err, fs.ErrExist) {
		t.Errorf("O_EXCL on existing: err = %v, want ErrExist", err)
	}
	if _, err := fsys.OpenFile("no/parent.txt", os.O_WRONLY|os.O_CREATE, 0o644); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("create without parent: err = %v, want ErrNotExist", err)
	}

	// A write-only handle refuses reads; an O_RDWR handle serves both.
	wo, err := fsys.OpenFile("f.txt", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wo.Read(make([]byte, 1)); err == nil {
		t.Error("read on O_WRONLY handle succeeded")
	}
	wo.Close()
	rw, err := fsys.OpenFile("f.txt", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Read(make([]byte, 1)); err != nil {
		t.Errorf("read on O_RDWR handle: %v", err)
	}
	rw.Close()
}

func TestMkdirRemove(t *testing.T) {
	fsys := memfs.New()

	if err := fsys.Mkdir("d", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Mkdir("d", 0o755); !errors.Is(err, fs.ErrExist) {
		t.Errorf("second Mkdir: err = %v, want ErrExist", err)
	}
	if err := fsys.Mkdir("x/y", 0o755); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Mkdir without parent: err = %v, want ErrNotExist", err)
	}
	if err := fsys.WriteFile("d/f.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Remove("d"); err == nil {
		t.Error("Remove of non-empty directory succeeded")
	}
	if err := fsys.Remove("d/f.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Remove("d"); err != nil {
		t.Fatalf("Remove of empty directory: %v", err)
	}
	if _, err := fsys.Open("d"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open after Remove: err = %v, want ErrNotExist", err)
	}
}

func TestRenameMovesSubtree(t *testing.T) {
	fsys := memfs.New()
	if err := fsys.MkdirAll("old/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("old/sub/f.txt", []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Rename("old", "new"); err != nil {
		t.Fatal(err)
	}
	got, err := fs.ReadFile(fsys, "new/sub/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "data" {
		t.Errorf("content = %q, want %q", got, "data")
	}
	if _, err := fsys.Open("old/sub/f.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("old path still readable after Rename: err = %v", err)
	}
}
