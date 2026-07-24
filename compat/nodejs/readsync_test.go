package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/memfs"
)

// readSync with a null position reads from the CURRENT offset and advances, so
// a chunked loop walks the whole file instead of re-reading the first bytes.
func TestReadSyncNullPositionAdvances(t *testing.T) {
	fsys := memfs.New()
	fsys.WriteFile("data.bin", []byte("0123456789"), 0o644)
	js, rt := newRuntime(t, spidermonkey.Config{FS: fsys})
	runScript(t, rt, `
		const fs = require("fs");
		globalThis.r = {};
		const fd = fs.openSync("/data.bin", "r");
		const buf = Buffer.alloc(3);
		let out = "";
		for (;;) {
			const n = fs.readSync(fd, buf, 0, 3, null); // null => advance
			if (n === 0) break;
			out += buf.toString("utf8", 0, n);
		}
		fs.closeSync(fd);
		r.out = out;
	`)
	if got := evalStr(t, js, `r.out`); got != "0123456789" {
		t.Fatalf("chunked readSync = %q, want 0123456789 (null position must advance)", got)
	}
}
