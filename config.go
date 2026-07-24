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

	// FS is the filesystem the guest sees: it backs the default ES module
	// loader and every host-function file operation (the compat packages'
	// node:fs). SpiderMonkey itself has no filesystem API, so it does nothing
	// on its own. nil means modules must be supplied by a custom
	// SetModuleLoader and no filesystem is reachable.
	//
	// FS is ALSO the single point of filesystem access control: an
	// implementation enforces its own policy inside its Open/OpenFile/Stat/…
	// methods (deny a path with fs.ErrPermission, hide it with
	// fs.ErrNotExist), exactly as a sheena fs.Volume does with its
	// Refuse/Hide/Access rules. There is deliberately no separate access
	// callback — a plain fs.FS grants read access to everything it exposes
	// (the embedder chose to expose it), and a policy-carrying FS restricts
	// reads and writes uniformly from one place. An FS that also implements
	// WritableFS accepts writes; a plain fs.FS is read-only (writes surface
	// as EROFS).
	FS fs.FS

	// The permission hooks below are callback allow-lists consulted by host
	// functions (the compat packages' ops). SpiderMonkey has no I/O of its
	// own, so a hook only gates capabilities an embedder explicitly installed.
	//
	// Every hook is FAIL-CLOSED: a nil hook DENIES the capability it guards.
	// Installing a network/subprocess surface (e.g. compat/web's fetch or
	// compat/nodejs's net/http/tls/child_process) is therefore not enough — the
	// guest reaches it only for the specific hosts/commands the matching hook
	// permits, and a hook left nil blocks that capability entirely rather than
	// opening it. The zero Config is doubly sandboxed: nothing is installed AND
	// every hook denies. (Filesystem access is governed by FS itself, above,
	// not by a hook here.)

	// Dial is the outbound-connection whitelist, called before each connect:
	// host is the name the guest requested and that resolved to ip (or "" for a
	// literal-IP dial with no preceding lookup), ip is the dotted-quad/IPv6
	// being dialed, and port is the port. Returning false denies the
	// connection; a nil Dial denies ALL outbound connections. Passing host lets
	// a policy match host and port jointly (e.g. "example.com only on 443") — an
	// IP alone cannot be tied back to the name resolved. The connection is made
	// to the exact ip that was approved (the name is resolved once, before the
	// hook), so a DNS answer cannot smuggle a different address past the
	// allow-list. This mirrors wasm2go's WASI dial hook
	// (func(network, host, ip string, port int) bool).
	Dial func(network, host, ip string, port int) bool

	// Resolve is the name-resolution whitelist. It is called with the host
	// being resolved before each lookup; returning false denies it, and a nil
	// Resolve denies ALL name resolution (so a hostname connection needs both
	// Resolve and Dial; a literal-IP connection needs only Dial). This is where
	// a hostname policy such as "block example.com" is enforced.
	Resolve func(host string) bool

	// Listen gates inbound sockets. It is called with the network and local
	// address before each listen; returning false denies it, and a nil Listen
	// denies ALL inbound sockets.
	Listen func(network, addr string) bool

	// Exec is the subprocess whitelist. It is called before every process spawn
	// with the executable path and full argv; returning false denies the spawn,
	// and a nil Exec denies ALL spawns. Spawning runs a HOST binary, so a
	// sandbox that allows subprocess should set this to a strict allow-list.
	Exec func(path string, argv []string) bool
}
