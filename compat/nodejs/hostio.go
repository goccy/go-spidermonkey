package nodejs

// hostio.go: process.stdin (Config.Stdin), OS signal delivery
// (process.on('SIGINT'|'SIGTERM'|...)), and fs.watch polling. These bridge
// host-side, event-loop-integrated sources into the guest.

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

type ioState struct {
	mu          sync.Mutex
	watchers    map[int64]chan struct{}
	nextWatch   int64
	sigOnce     sync.Once
	sigCh       chan os.Signal
	stdinOnce   sync.Once
	stdinActive bool
}

func newIOState() *ioState {
	return &ioState{watchers: map[int64]chan struct{}{}}
}

func (rt *Runtime) ioOps() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"stdin_start":  rt.opStdinStart,
		"signal_watch": rt.opSignalWatch,
		"fs_watch":     rt.opFSWatch,
		"fs_unwatch":   rt.opFSUnwatch,
	}
}

// opStdinStart(onData, onEnd) reads Config.Stdin on a goroutine, posting
// chunks and end onto the loop. No-op when no Stdin is configured.
func (rt *Runtime) opStdinStart(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 || cfg.Stdin == nil {
		return spidermonkey.ValueOf(false), nil
	}
	onData := args[0].Object()
	onEnd := args[1].Object()
	started := false
	rt.io.stdinOnce.Do(func() {
		started = true
		rt.loop.AddPending()
		go func() {
			buf := make([]byte, 8<<10)
			for {
				n, err := cfg.Stdin.Read(buf)
				if n > 0 {
					chunk := append([]byte(nil), buf[:n]...)
					rt.loop.Post(func() error {
						if onData != nil {
							u8, e := rt.js.NewBytes(chunk)
							if e == nil {
								onData.Call(u8)
								u8.Free()
							}
						}
						return nil
					})
				}
				if err != nil {
					rt.loop.Post(func() error {
						if onEnd != nil {
							onEnd.Call()
						}
						if onData != nil {
							onData.Free()
						}
						if onEnd != nil {
							onEnd.Free()
						}
						return nil
					})
					rt.loop.DonePending()
					return
				}
			}
		}()
	})
	return spidermonkey.ValueOf(started), nil
}

var signalNumbers = map[string]os.Signal{
	"SIGINT":  syscall.SIGINT,
	"SIGTERM": syscall.SIGTERM,
	"SIGHUP":  syscall.SIGHUP,
	"SIGUSR1": syscall.SIGUSR1,
	"SIGUSR2": syscall.SIGUSR2,
	"SIGQUIT": syscall.SIGQUIT,
}

// opSignalWatch(onSignal) installs an OS signal handler that posts the signal
// name onto the loop. Called once, when the first signal listener is added.
func (rt *Runtime) opSignalWatch(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	onSignal := args[0].Object()
	rt.io.sigOnce.Do(func() {
		rt.io.sigCh = make(chan os.Signal, 4)
		sigs := make([]os.Signal, 0, len(signalNumbers))
		for _, s := range signalNumbers {
			sigs = append(sigs, s)
		}
		signal.Notify(rt.io.sigCh, sigs...)
		rt.loop.AddPending()
		go func() {
			for s := range rt.io.sigCh {
				name := signalToName(s)
				rt.loop.Post(func() error {
					if onSignal != nil {
						onSignal.Call(spidermonkey.ValueOf(name))
					}
					return nil
				})
			}
			rt.loop.DonePending()
		}()
	})
	return spidermonkey.Undefined(), nil
}

func signalToName(s os.Signal) string {
	for name, sig := range signalNumbers {
		if sig == s {
			return name
		}
	}
	return s.String()
}

// opFSWatch(path, onChange) polls the file/dir mtime (memfs and read-only FS
// have no OS inotify), posting a 'change' when it moves. Returns a watcher id.
func (rt *Runtime) opFSWatch(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 || cfg.FS == nil {
		return spidermonkey.ValueOf(int64(0)), nil
	}
	p := guestPath(args[0].String())
	onChange := args[1].Object()

	rt.io.mu.Lock()
	rt.io.nextWatch++
	id := rt.io.nextWatch
	stop := make(chan struct{})
	rt.io.watchers[id] = stop
	rt.io.mu.Unlock()

	snapshot := func() map[string]int64 {
		out := map[string]int64{}
		if info, err := fs.Stat(cfg.FS, p); err == nil {
			out[p] = info.ModTime().UnixNano()
			if info.IsDir() {
				if entries, derr := fs.ReadDir(cfg.FS, p); derr == nil {
					for _, e := range entries {
						if fi, ferr := e.Info(); ferr == nil {
							out[e.Name()] = fi.ModTime().UnixNano()
						}
					}
				}
			}
		}
		return out
	}

	rt.loop.AddPending()
	go func() {
		prev := snapshot()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				rt.loop.DonePending()
				return
			case <-ticker.C:
				cur := snapshot()
				changes := diffSnapshots(prev, cur)
				prev = cur
				for _, name := range changes {
					n := name
					rt.loop.Post(func() error {
						if onChange != nil {
							onChange.Call(spidermonkey.ValueOf("change"), spidermonkey.ValueOf(n))
						}
						return nil
					})
				}
			}
		}
	}()
	return spidermonkey.ValueOf(id), nil
}

func diffSnapshots(prev, cur map[string]int64) []string {
	var changed []string
	for k, v := range cur {
		if pv, ok := prev[k]; !ok || pv != v {
			changed = append(changed, k)
		}
	}
	for k := range prev {
		if _, ok := cur[k]; !ok {
			changed = append(changed, k)
		}
	}
	return changed
}

func (rt *Runtime) opFSUnwatch(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	id := int64(args[0].Float())
	rt.io.mu.Lock()
	stop := rt.io.watchers[id]
	delete(rt.io.watchers, id)
	rt.io.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	return spidermonkey.Undefined(), nil
}

func (rt *Runtime) closeIO() {
	rt.io.mu.Lock()
	for id, stop := range rt.io.watchers {
		delete(rt.io.watchers, id)
		close(stop)
	}
	rt.io.mu.Unlock()
	if rt.io.sigCh != nil {
		signal.Stop(rt.io.sigCh)
	}
}

var _ = io.EOF
var _ = fmt.Sprint
