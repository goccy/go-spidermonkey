// Package memfs provides a writable in-memory filesystem implementing
// spidermonkey.WritableFS. It backs tests and fully isolated interpreters:
// giving two interpreters separate memfs instances isolates their file views
// completely, with no host filesystem involved.
//
// Paths follow io/fs conventions: slash-separated, unrooted, "." is the root
// directory. Parent directories are not created implicitly — use Mkdir or
// MkdirAll first, as a Node-style guest would expect ENOENT otherwise.
package memfs

import (
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

var _ spidermonkey.WritableFS = (*FS)(nil)

type node struct {
	data    []byte
	mode    fs.FileMode // fs.ModeDir set for directories
	modTime time.Time
}

// FS is a writable in-memory filesystem. The zero value is not usable; call
// New. FS is safe for concurrent use.
type FS struct {
	mu    sync.RWMutex
	nodes map[string]*node
}

// New returns an empty filesystem containing only the root directory.
func New() *FS {
	return &FS{nodes: map[string]*node{
		".": {mode: fs.ModeDir | 0o755, modTime: time.Now()},
	}}
}

// Open opens the named file or directory for reading.
func (fsys *FS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	fsys.mu.RLock()
	defer fsys.mu.RUnlock()
	n, ok := fsys.nodes[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	if n.mode.IsDir() {
		return &dirHandle{info: infoOf(name, n), entries: fsys.entriesLocked(name)}, nil
	}
	return &readHandle{info: infoOf(name, n), data: append([]byte(nil), n.data...)}, nil
}

// OpenFile opens name with OS-style flags (os.O_CREATE, os.O_TRUNC,
// os.O_APPEND, ...). When the flags request write access the returned file
// also implements io.Writer. Creating a file requires its parent directory
// to exist.
func (fsys *FS) OpenFile(name string, flag int, perm fs.FileMode) (fs.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) == 0 {
		return fsys.Open(name)
	}
	if !fs.ValidPath(name) || name == "." {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	n, ok := fsys.nodes[name]
	switch {
	case ok:
		if n.mode.IsDir() {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
		}
		if flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0 {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrExist}
		}
		if flag&os.O_TRUNC != 0 {
			n.data = nil
		}
	case flag&os.O_CREATE == 0:
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	default:
		if err := fsys.checkParentLocked("open", name); err != nil {
			return nil, err
		}
		n = &node{mode: perm.Perm()}
		fsys.nodes[name] = n
	}
	n.modTime = time.Now()
	h := &writeHandle{fsys: fsys, name: name, flag: flag}
	if flag&os.O_APPEND != 0 {
		h.pos = len(n.data)
	}
	return h, nil
}

// Mkdir creates a directory. The parent must already exist.
func (fsys *FS) Mkdir(name string, perm fs.FileMode) error {
	if !fs.ValidPath(name) || name == "." {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrInvalid}
	}
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	if _, ok := fsys.nodes[name]; ok {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrExist}
	}
	if err := fsys.checkParentLocked("mkdir", name); err != nil {
		return err
	}
	fsys.nodes[name] = &node{mode: fs.ModeDir | perm.Perm(), modTime: time.Now()}
	return nil
}

// MkdirAll creates name and any missing parents.
func (fsys *FS) MkdirAll(name string, perm fs.FileMode) error {
	if !fs.ValidPath(name) {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrInvalid}
	}
	if name == "." {
		return nil
	}
	if parent := path.Dir(name); parent != "." {
		if err := fsys.MkdirAll(parent, perm); err != nil {
			return err
		}
	}
	err := fsys.Mkdir(name, perm)
	if err != nil {
		fsys.mu.RLock()
		n, ok := fsys.nodes[name]
		fsys.mu.RUnlock()
		if ok && n.mode.IsDir() {
			return nil // already a directory — MkdirAll is idempotent
		}
	}
	return err
}

// Remove removes a file or an empty directory.
func (fsys *FS) Remove(name string) error {
	if !fs.ValidPath(name) || name == "." {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrInvalid}
	}
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	n, ok := fsys.nodes[name]
	if !ok {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
	}
	if n.mode.IsDir() {
		for p := range fsys.nodes {
			if path.Dir(p) == name {
				return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrInvalid}
			}
		}
	}
	delete(fsys.nodes, name)
	return nil
}

// Rename moves oldname to newname, replacing a non-directory target. Moving
// a directory moves its whole subtree.
func (fsys *FS) Rename(oldname, newname string) error {
	if !fs.ValidPath(oldname) || oldname == "." {
		return &fs.PathError{Op: "rename", Path: oldname, Err: fs.ErrInvalid}
	}
	if !fs.ValidPath(newname) || newname == "." {
		return &fs.PathError{Op: "rename", Path: newname, Err: fs.ErrInvalid}
	}
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	n, ok := fsys.nodes[oldname]
	if !ok {
		return &fs.PathError{Op: "rename", Path: oldname, Err: fs.ErrNotExist}
	}
	if target, ok := fsys.nodes[newname]; ok && target.mode.IsDir() {
		return &fs.PathError{Op: "rename", Path: newname, Err: fs.ErrExist}
	}
	if err := fsys.checkParentLocked("rename", newname); err != nil {
		return err
	}
	delete(fsys.nodes, oldname)
	fsys.nodes[newname] = n
	if n.mode.IsDir() {
		prefix := oldname + "/"
		for p, child := range fsys.nodes {
			if strings.HasPrefix(p, prefix) {
				delete(fsys.nodes, p)
				fsys.nodes[newname+"/"+p[len(prefix):]] = child
			}
		}
	}
	return nil
}

// WriteFile creates or truncates name with data (parent must exist).
func (fsys *FS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	f, err := fsys.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.(io.Writer).Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// checkParentLocked verifies the parent of name exists and is a directory.
func (fsys *FS) checkParentLocked(op, name string) error {
	parent := path.Dir(name)
	p, ok := fsys.nodes[parent]
	if !ok {
		return &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
	}
	if !p.mode.IsDir() {
		return &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	return nil
}

func (fsys *FS) entriesLocked(dir string) []fs.DirEntry {
	var entries []fs.DirEntry
	for p, n := range fsys.nodes {
		if p != "." && path.Dir(p) == dir {
			entries = append(entries, fs.FileInfoToDirEntry(infoOf(p, n)))
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries
}

func infoOf(name string, n *node) fileInfo {
	return fileInfo{name: path.Base(name), size: int64(len(n.data)), mode: n.mode, modTime: n.modTime}
}

type fileInfo struct {
	name    string
	size    int64
	mode    fs.FileMode
	modTime time.Time
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) Mode() fs.FileMode  { return i.mode }
func (i fileInfo) ModTime() time.Time { return i.modTime }
func (i fileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fileInfo) Sys() any           { return nil }

// readHandle serves reads from a snapshot taken at Open, so a concurrent
// writer never mutates data under a reader.
type readHandle struct {
	info fileInfo
	data []byte
	pos  int
}

func (h *readHandle) Stat() (fs.FileInfo, error) { return h.info, nil }
func (h *readHandle) Close() error               { return nil }
func (h *readHandle) Read(p []byte) (int, error) {
	if h.pos >= len(h.data) {
		return 0, io.EOF
	}
	n := copy(p, h.data[h.pos:])
	h.pos += n
	return n, nil
}

type dirHandle struct {
	info    fileInfo
	entries []fs.DirEntry
	pos     int
}

func (h *dirHandle) Stat() (fs.FileInfo, error) { return h.info, nil }
func (h *dirHandle) Close() error               { return nil }
func (h *dirHandle) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: h.info.name, Err: fs.ErrInvalid}
}

func (h *dirHandle) ReadDir(count int) ([]fs.DirEntry, error) {
	rest := h.entries[h.pos:]
	if count <= 0 {
		h.pos = len(h.entries)
		return rest, nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	if count > len(rest) {
		count = len(rest)
	}
	h.pos += count
	return rest[:count:count], nil
}

// writeHandle applies writes straight to the live node under the FS lock, so
// the write is visible as soon as Write returns.
type writeHandle struct {
	fsys *FS
	name string
	flag int
	pos  int
}

func (h *writeHandle) node() (*node, error) {
	n, ok := h.fsys.nodes[h.name]
	if !ok {
		return nil, &fs.PathError{Op: "write", Path: h.name, Err: fs.ErrNotExist}
	}
	return n, nil
}

func (h *writeHandle) Write(p []byte) (int, error) {
	h.fsys.mu.Lock()
	defer h.fsys.mu.Unlock()
	n, err := h.node()
	if err != nil {
		return 0, err
	}
	if h.flag&os.O_APPEND != 0 {
		h.pos = len(n.data)
	}
	if grow := h.pos + len(p) - len(n.data); grow > 0 {
		n.data = append(n.data, make([]byte, grow)...)
	}
	copy(n.data[h.pos:], p)
	h.pos += len(p)
	n.modTime = time.Now()
	return len(p), nil
}

func (h *writeHandle) Read(p []byte) (int, error) {
	if h.flag&(os.O_WRONLY|os.O_RDWR) == os.O_WRONLY {
		return 0, &fs.PathError{Op: "read", Path: h.name, Err: fs.ErrInvalid}
	}
	h.fsys.mu.RLock()
	defer h.fsys.mu.RUnlock()
	n, err := h.node()
	if err != nil {
		return 0, err
	}
	if h.pos >= len(n.data) {
		return 0, io.EOF
	}
	c := copy(p, n.data[h.pos:])
	h.pos += c
	return c, nil
}

func (h *writeHandle) Stat() (fs.FileInfo, error) {
	h.fsys.mu.RLock()
	defer h.fsys.mu.RUnlock()
	n, err := h.node()
	if err != nil {
		return nil, err
	}
	return infoOf(h.name, n), nil
}

func (h *writeHandle) Close() error { return nil }
