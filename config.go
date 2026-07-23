package spidermonkey

import (
	"io"
	"io/fs"
)

// Config configures a JS interpreter. The zero value is a usable, fully
// sandboxed interpreter: no host environment leaks, no host stdio is attached,
// no filesystem is reachable, and the memory cap takes its default.
type Config struct {
	// MaxMemoryBytes caps the interpreter's wasm linear memory — the single
	// ceiling on everything it can allocate (GC heap, engine data, C stack).
	// It sizes the allocation the guest grows into; untouched pages never page
	// in, so headroom is nearly free. Hitting it aborts the guest (the host
	// gets an error and the instance is spent), so it is a hard host-protection
	// backstop, not a limit a script recovers from. Zero means the default.
	MaxMemoryBytes int

	// Env is the environment the guest sees (and that host functions may read
	// via the Config handed to them). nil means an empty environment — the host
	// process os.Environ() is NOT leaked.
	Env []string

	// Stdin, Stdout, Stderr are for HOST FUNCTIONS. SpiderMonkey has no I/O of
	// its own, so these do nothing until a host function uses them (a future
	// Node-compatible console/process). Unset streams are sandboxed (empty
	// stdin, discarded output), never the host process's.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// FS backs the default ES module loader and is available to host functions
	// (a future Node-compatible fs). SpiderMonkey itself has no filesystem API,
	// so it does nothing on its own. nil means modules must be supplied by
	// a custom SetModuleLoader. An FS that also implements WritableFS accepts
	// writes from host functions that need them; a plain fs.FS behaves as a
	// read-only filesystem.
	FS fs.FS

	// The permission hooks below are callback allow-lists consulted by host
	// functions (the compat packages' ops) and, for FSAccess, by the default
	// module loader. SpiderMonkey has no I/O of its own, so a hook only gates
	// capabilities an embedder explicitly installed: the zero Config remains
	// fully sandboxed because there is nothing to allow. A nil hook permits
	// the operations of installed host functions unrestricted.

	// FSAccess, when non-nil, is a per-access whitelist. It receives the guest
	// path and whether the access is a write; returning false denies it (the
	// guest sees a permission error).
	FSAccess func(path string, write bool) bool

	// Dial, when non-nil, is the outbound-connection whitelist. It is called
	// with ("tcp", dotted-quad IP, port) before each connect; returning false
	// denies the connection.
	Dial func(network, ip string, port int) bool

	// Resolve, when non-nil, is the name-resolution whitelist. It is called
	// with the host being resolved before each lookup; returning false denies
	// it. This is where a hostname policy such as "block example.com" is
	// enforced.
	Resolve func(host string) bool

	// Listen, when non-nil, gates inbound sockets. It is called with the
	// network and local address before each listen; returning false denies it.
	Listen func(network, addr string) bool

	// Exec, when non-nil, is the subprocess whitelist. It is called before
	// every process spawn with the executable path and full argv; returning
	// false denies the spawn. Spawning runs a HOST binary, so a sandbox that
	// allows subprocess should set this to a strict allow-list.
	Exec func(path string, argv []string) bool
}
