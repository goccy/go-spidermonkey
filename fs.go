package spidermonkey

import "io/fs"

// WritableFS is the interface upgrade a Config.FS may implement to accept
// writes. Host functions that need to create or modify files assert this
// interface on Config.FS (the fs.ReadDirFS idiom): when the assertion fails,
// the filesystem behaves as a read-only mount and writes surface to the guest
// as permission errors. Reads always go through the plain fs.FS methods.
//
// An implementation is the single point of filesystem access control: it
// enforces its own policy inside these methods and inside the fs.FS read
// methods (return fs.ErrPermission to deny, fs.ErrNotExist to hide). A sheena
// fs.Volume satisfies this interface through a tiny adapter (its OpenFile
// returns sheena's fs.File, which is an io/fs.File; the rest match by
// embedding), so a sheena sandbox's Refuse/Hide/Access rules carry straight
// into the guest.
//
// The memfs package provides an unrestricted in-memory implementation, useful
// in tests and for isolating instances from the host filesystem entirely.
type WritableFS interface {
	fs.FS

	// OpenFile opens name with OS-style flags (os.O_RDONLY, os.O_CREATE,
	// os.O_TRUNC, os.O_APPEND, ...). When the flags request write access, the
	// returned file also implements io.Writer.
	OpenFile(name string, flag int, perm fs.FileMode) (fs.File, error)

	// Mkdir creates a directory. The parent must already exist.
	Mkdir(name string, perm fs.FileMode) error

	// Remove removes a file or an empty directory.
	Remove(name string) error

	// Rename moves oldname to newname, replacing a non-directory target.
	Rename(oldname, newname string) error
}
