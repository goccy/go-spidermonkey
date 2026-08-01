package nodetest

import (
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	memfs "github.com/goccy/go-spidermonkey/fs"
)

// unionFS presents a read-only lower filesystem (the checked-out suite) with
// an in-memory upper layer that absorbs every write. Tests get a writable
// tree — tmpdirs, fixtures they rewrite — without mutating the checkout and
// without copying 80 MB per test.
type unionFS struct {
	lower fs.FS
	upper *memfs.MemFS

	mu      sync.Mutex
	deleted map[string]bool // paths removed from the union
}

func newUnionFS(lower fs.FS) *unionFS {
	return &unionFS{lower: lower, upper: memfs.NewMemFS(), deleted: map[string]bool{}}
}

func (u *unionFS) isDeleted(name string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.deleted[name] {
		return true
	}
	for d := range u.deleted {
		if strings.HasPrefix(name, d+"/") {
			return true
		}
	}
	return false
}

func (u *unionFS) markDeleted(name string, yes bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if yes {
		u.deleted[name] = true
	} else {
		delete(u.deleted, name)
	}
}

func (u *unionFS) Open(name string) (fs.File, error) {
	if f, err := u.upper.Open(name); err == nil {
		if fi, e := f.Stat(); e == nil && fi.IsDir() {
			f.Close()
			return u.openMergedDir(name)
		}
		return f, nil
	}
	if u.isDeleted(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	f, err := u.lower.Open(name)
	if err != nil {
		return nil, err
	}
	if fi, e := f.Stat(); e == nil && fi.IsDir() {
		f.Close()
		return u.openMergedDir(name)
	}
	return f, nil
}

// openMergedDir returns a directory handle whose entries are the union of both
// layers, so a tmpdir created in memory is visible next to checked-out files.
func (u *unionFS) openMergedDir(name string) (fs.File, error) {
	seen := map[string]fs.DirEntry{}
	var lowerInfo fs.FileInfo
	if !u.isDeleted(name) {
		if ents, err := fs.ReadDir(u.lower, name); err == nil {
			for _, e := range ents {
				if !u.isDeleted(path.Join(name, e.Name())) {
					seen[e.Name()] = e
				}
			}
			if fi, err := fs.Stat(u.lower, name); err == nil {
				lowerInfo = fi
			}
		}
	}
	upperOK := false
	if ents, err := fs.ReadDir(u.upper, name); err == nil {
		upperOK = true
		for _, e := range ents {
			seen[e.Name()] = e
		}
	}
	if lowerInfo == nil && !upperOK {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	ents := make([]fs.DirEntry, 0, len(names))
	for _, n := range names {
		ents = append(ents, seen[n])
	}
	return &mergedDir{name: path.Base(name), entries: ents}, nil
}

func (u *unionFS) Stat(name string) (fs.FileInfo, error) {
	if fi, err := fs.Stat(u.upper, name); err == nil {
		return fi, nil
	}
	if u.isDeleted(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return fs.Stat(u.lower, name)
}

func (u *unionFS) ReadDir(name string) ([]fs.DirEntry, error) {
	f, err := u.openMergedDir(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.(*mergedDir).ReadDir(-1)
}

func (u *unionFS) ReadFile(name string) ([]byte, error) {
	f, err := u.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readAll(f)
}

// OpenFile routes writes to the upper layer, copying the lower file up first
// when the open is not a truncating create (so append/read-write see history).
func (u *unionFS) OpenFile(name string, flag int, perm fs.FileMode) (fs.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) == 0 {
		return u.Open(name)
	}
	if _, err := fs.Stat(u.upper, name); err != nil {
		if flag&os.O_TRUNC == 0 && !u.isDeleted(name) {
			if data, err := fs.ReadFile(u.lower, name); err == nil {
				u.upper.MkdirAll(path.Dir(name), 0o755)
				u.upper.WriteFile(name, data, 0o644)
			}
		}
	}
	u.markDeleted(name, false)
	return u.upper.OpenFile(name, flag, perm)
}

func (u *unionFS) Mkdir(name string, perm fs.FileMode) error {
	// The parent may live entirely in the lower layer; materialize it.
	if dir := path.Dir(name); dir != "." {
		u.upper.MkdirAll(dir, 0o755)
	}
	u.markDeleted(name, false)
	return u.upper.Mkdir(name, perm)
}

func (u *unionFS) Remove(name string) error {
	err := u.upper.Remove(name)
	if err == nil {
		u.markDeleted(name, true)
		return nil
	}
	if _, e := fs.Stat(u.lower, name); e == nil && !u.isDeleted(name) {
		u.markDeleted(name, true)
		return nil
	}
	return err
}

func (u *unionFS) Rename(oldname, newname string) error {
	if _, err := fs.Stat(u.upper, oldname); err != nil {
		data, e := fs.ReadFile(u.lower, oldname)
		if e != nil {
			return &fs.PathError{Op: "rename", Path: oldname, Err: fs.ErrNotExist}
		}
		u.upper.MkdirAll(path.Dir(oldname), 0o755)
		if err := u.upper.WriteFile(oldname, data, 0o644); err != nil {
			return err
		}
	}
	if dir := path.Dir(newname); dir != "." {
		u.upper.MkdirAll(dir, 0o755)
	}
	if err := u.upper.Rename(oldname, newname); err != nil {
		return err
	}
	u.markDeleted(oldname, true)
	u.markDeleted(newname, false)
	return nil
}

func (u *unionFS) Chmod(name string, mode fs.FileMode) error { return u.upper.Chmod(name, mode) }

type mergedDir struct {
	name    string
	entries []fs.DirEntry
	off     int
}

func (d *mergedDir) Stat() (fs.FileInfo, error) { return dirInfo{d.name}, nil }
func (d *mergedDir) Close() error               { return nil }
func (d *mergedDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: fs.ErrInvalid}
}

func (d *mergedDir) ReadDir(count int) ([]fs.DirEntry, error) {
	if count <= 0 {
		rest := d.entries[d.off:]
		d.off = len(d.entries)
		return rest, nil
	}
	if d.off >= len(d.entries) {
		return nil, nil
	}
	end := min(d.off+count, len(d.entries))
	out := d.entries[d.off:end]
	d.off = end
	return out, nil
}

type dirInfo struct{ name string }

func (i dirInfo) Name() string       { return i.name }
func (i dirInfo) Size() int64        { return 0 }
func (i dirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o755 }
func (i dirInfo) ModTime() time.Time { return time.Time{} }
func (i dirInfo) IsDir() bool        { return true }
func (i dirInfo) Sys() any           { return nil }

func readAll(f fs.File) ([]byte, error) { return io.ReadAll(f) }
