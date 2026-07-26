// Command hono serves the real, unmodified Hono framework on the
// go-spidermonkey Cloudflare-Workers compat layer — the instance pool behind
// net/http — with no Node.js or Workers runtime involved. The app itself is the
// Worker module in main.js; the `hono` package is loaded from ./node_modules
// exactly as a bundler would resolve it.
package main

import (
	_ "embed"
	"fmt"
	"log"
	"net/http"
	"os"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/cfworkers"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
)

//go:embed main.js
var honoApp string

// newHandler builds a cfworkers pool that serves the Hono app. GREETING is
// bound from Go and read by the worker as c.env.GREETING.
func newHandler() (http.Handler, func() error, error) {
	pool, err := cfworkers.NewPool(cfworkers.PoolConfig{
		Size:   1,
		Source: honoApp,
		Config: spidermonkey.Config{FS: os.DirFS(".")},
		Loader: nodejs.ESMLoader,
		Env: map[string]cfworkers.Binding{
			"GREETING": cfworkers.Static("bound from Go"),
		},
	})
	if err != nil {
		return nil, nil, err
	}
	return pool, pool.Close, nil
}

func main() {
	h, closePool, err := newHandler()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer closePool()

	const addr = ":8787"
	log.Printf("Hono listening on http://localhost%s", addr)
	if err := http.ListenAndServe(addr, h); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
