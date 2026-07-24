package nodejs

// fsextra.go: the fs operations beyond the basic read/write/stat set —
// copyFile, rm (recursive), cp, mkdtemp, realpath, and the fd-based
// openSync/readSync/writeSync/closeSync surface. All gated by
// Config.FSAccess; writes require a WritableFS.

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"strconv"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func (rt *Runtime) fsExtraOps() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"fs_copyfile": rt.opFSCopyFile,
		"fs_rm":       rt.opFSRm,
		"fs_mkdtemp":  rt.opFSMkdtemp,
		"fs_open":     rt.opFSOpen,
		"fs_read_fd":  rt.opFSReadFD,
		"fs_write_fd": rt.opFSWriteFD,
		"fs_close_fd": rt.opFSCloseFD,
		"fs_fstat":    rt.opFSFstat,
	}
}

func (rt *Runtime) opFSCopyFile(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("fs_copyfile: (src, dest) required")
	}
	src := guestPath(args[0].String())
	dst := guestPath(args[1].String())
	if err := checkAccess(cfg, src, false); err != nil {
		return fsErrValue(err), nil
	}
	if err := checkAccess(cfg, dst, true); err != nil {
		return fsErrValue(err), nil
	}
	wfs, err := writableFS(cfg)
	if err != nil {
		return fsErrValue(err), nil
	}
	data, err := readFile(cfg.FS, src)
	if err != nil {
		return fsErrValue(err), nil
	}
	f, err := wfs.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fsErrValue(err), nil
	}
	if _, err := f.(interface{ Write([]byte) (int, error) }).Write(data); err != nil {
		f.Close()
		return fsErrValue(err), nil
	}
	if err := f.Close(); err != nil {
		return fsErrValue(err), nil
	}
	return spidermonkey.ValueOf(map[string]any{"ok": true}), nil
}

// opFSRm(path, recursive, force) removes a file or (recursive) a subtree.
func (rt *Runtime) opFSRm(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("fs_rm: (path, recursive, force) required")
	}
	p := guestPath(args[0].String())
	recursive := args[1].Bool()
	force := args[2].Bool()
	if err := checkAccess(cfg, p, true); err != nil {
		return fsErrValue(err), nil
	}
	wfs, err := writableFS(cfg)
	if err != nil {
		return fsErrValue(err), nil
	}
	info, err := fs.Stat(cfg.FS, p)
	if err != nil {
		if force {
			return spidermonkey.ValueOf(map[string]any{"ok": true}), nil
		}
		return fsErrValue(err), nil
	}
	if info.IsDir() && recursive {
		if err := removeAll(wfs, p); err != nil {
			return fsErrValue(err), nil
		}
	} else if err := wfs.Remove(p); err != nil {
		if !force {
			return fsErrValue(err), nil
		}
	}
	return spidermonkey.ValueOf(map[string]any{"ok": true}), nil
}

// removeAll deletes a directory subtree depth-first.
func removeAll(wfs spidermonkey.WritableFS, dir string) error {
	entries, err := fs.ReadDir(wfs, dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		child := path.Join(dir, e.Name())
		if e.IsDir() {
			if err := removeAll(wfs, child); err != nil {
				return err
			}
		} else if err := wfs.Remove(child); err != nil {
			return err
		}
	}
	return wfs.Remove(dir)
}

// opFSMkdtemp(prefix) creates prefix+XXXXXX and returns the path.
func (rt *Runtime) opFSMkdtemp(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("fs_mkdtemp: prefix required")
	}
	prefix := guestPath(args[0].String())
	wfs, err := writableFS(cfg)
	if err != nil {
		return fsErrValue(err), nil
	}
	// Deterministic-ish suffix from a monotonic counter (Math.random is
	// unavailable and time is fine here as it is host-side).
	for i := 0; i < 1000; i++ {
		rt.mu.Lock()
		rt.nextFD++
		seq := rt.nextFD
		rt.mu.Unlock()
		candidate := prefix + strconv.FormatInt(seq, 36) + "xxxxx"[:5]
		if err := checkAccess(cfg, candidate, true); err != nil {
			return fsErrValue(err), nil
		}
		if _, statErr := fs.Stat(wfs, candidate); statErr == nil {
			continue
		}
		if err := wfs.Mkdir(candidate, 0o700); err != nil {
			return fsErrValue(err), nil
		}
		return spidermonkey.ValueOf("/" + candidate), nil
	}
	return fsErrValue(fmt.Errorf("mkdtemp: exhausted attempts")), nil
}

// opFSOpen(path, flags) -> fd | err.
func (rt *Runtime) opFSOpen(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("fs_open: (path, flags) required")
	}
	p := guestPath(args[0].String())
	flags := args[1].String()
	write := flags != "r"
	if err := checkAccess(cfg, p, write); err != nil {
		return fsErrValue(err), nil
	}
	var data []byte
	switch flags {
	case "r", "r+":
		b, err := readFile(cfg.FS, p)
		if err != nil {
			return fsErrValue(err), nil
		}
		data = b
	case "a", "a+":
		if b, err := readFile(cfg.FS, p); err == nil {
			data = b
		}
	case "w", "w+":
		// truncate
	default:
		return fsErrValue(fmt.Errorf("unsupported flags %q", flags)), nil
	}
	rt.mu.Lock()
	rt.nextFD++
	fd := rt.nextFD
	rt.fds[fd] = &openFile{
		path:   p,
		data:   data,
		write:  write,
		append: flags == "a" || flags == "a+",
		pos:    0,
		cfg:    cfg,
	}
	rt.mu.Unlock()
	return spidermonkey.ValueOf(fd), nil
}

// opFSReadFD(fd, length, position) -> {data, bytesRead}.
func (rt *Runtime) opFSReadFD(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("fs_read_fd: fd required")
	}
	rt.mu.Lock()
	f := rt.fds[int64(args[0].Float())]
	rt.mu.Unlock()
	if f == nil {
		return fsErrValue(fmt.Errorf("EBADF")), nil
	}
	pos := f.pos
	if len(args) > 2 && args[2].Object() == nil && !args[2].IsUndefined() {
		pos = int64(args[2].Float())
	}
	length := int64(len(f.data)) - pos
	if len(args) > 1 && !args[1].IsUndefined() {
		if l := int64(args[1].Float()); l < length {
			length = l
		}
	}
	if pos >= int64(len(f.data)) || length <= 0 {
		return rt.readFDResult(nil, 0)
	}
	chunk := f.data[pos : pos+length]
	f.pos = pos + length
	return rt.readFDResult(chunk, len(chunk))
}

func (rt *Runtime) readFDResult(data []byte, n int) (spidermonkey.Value, error) {
	obj, err := rt.js.NewObject()
	if err != nil {
		return nil, err
	}
	u8, err := rt.js.NewBytes(data)
	if err != nil {
		return nil, err
	}
	defer u8.Free()
	obj.Set("data", u8)
	obj.Set("bytesRead", spidermonkey.ValueOf(n))
	return rt.trackReturn(obj), nil
}

func (rt *Runtime) opFSWriteFD(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("fs_write_fd: (fd, data) required")
	}
	rt.mu.Lock()
	f := rt.fds[int64(args[0].Float())]
	rt.mu.Unlock()
	if f == nil {
		return fsErrValue(fmt.Errorf("EBADF")), nil
	}
	data, err := valueBytes(args[1])
	if err != nil {
		return nil, err
	}
	if f.append || f.pos >= int64(len(f.data)) {
		f.data = append(f.data, data...)
	} else {
		grow := f.pos + int64(len(data)) - int64(len(f.data))
		if grow > 0 {
			f.data = append(f.data, make([]byte, grow)...)
		}
		copy(f.data[f.pos:], data)
	}
	f.pos += int64(len(data))
	f.dirty = true
	return spidermonkey.ValueOf(len(data)), nil
}

func (rt *Runtime) opFSCloseFD(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	rt.mu.Lock()
	fd := int64(args[0].Float())
	f := rt.fds[fd]
	delete(rt.fds, fd)
	rt.mu.Unlock()
	if f == nil || !f.dirty {
		return spidermonkey.ValueOf(map[string]any{"ok": true}), nil
	}
	wfs, err := writableFS(f.cfg)
	if err != nil {
		return fsErrValue(err), nil
	}
	fh, err := wfs.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fsErrValue(err), nil
	}
	if _, err := fh.(interface{ Write([]byte) (int, error) }).Write(f.data); err != nil {
		fh.Close()
		return fsErrValue(err), nil
	}
	if err := fh.Close(); err != nil {
		return fsErrValue(err), nil
	}
	return spidermonkey.ValueOf(map[string]any{"ok": true}), nil
}

func (rt *Runtime) opFSFstat(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("fs_fstat: fd required")
	}
	rt.mu.Lock()
	f := rt.fds[int64(args[0].Float())]
	rt.mu.Unlock()
	if f == nil {
		return fsErrValue(fmt.Errorf("EBADF")), nil
	}
	return spidermonkey.ValueOf(map[string]any{
		"size": len(f.data), "dir": false, "mode": uint32(0o644), "mtimeMs": 0,
	}), nil
}
