package spidermonkey_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestGuestHasNoFilesystemAPI is the first line of the filesystem sandbox: a
// guest script has no way to name a file at all. SpiderMonkey the engine exposes
// no filesystem API — ECMAScript has no file I/O — and go-spidermonkey installs
// only print and console. The read / readline / snarf / load / os.file helpers
// people associate with SpiderMonkey come from its standalone `js` shell, not
// the embeddable engine, and are deliberately absent here. If any of these
// became reachable, a script could begin to probe the host, so pin them to
// undefined.
func TestGuestHasNoFilesystemAPI(t *testing.T) {
	i := newInterp(t, spidermonkey.Config{})
	for _, name := range []string{
		"read", "readline", "readlineBuf", "readRelativeToScript",
		"snarf", "load", "loadRelativeToScript", "os",
		"require", "importScripts", "fetch", "XMLHttpRequest",
	} {
		if r := eval(t, i, "typeof "+name); !r.Ok || r.Result != "undefined" {
			t.Errorf("guest can see %q (typeof = %q); a script must have no file API", name, r.Result)
		}
	}
}
