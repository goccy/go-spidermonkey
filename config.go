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
	// a custom SetModuleLoader.
	FS fs.FS
}
