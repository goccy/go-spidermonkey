// Command nextjs serves an unmodified Next.js 15 App Router PRODUCTION build
// inside the go-spidermonkey Node runtime — no Node.js binary is involved at
// serve time. The app is built ahead of time with real Node (`next build`; SWC
// natives are build-time only); the server itself is the standard Next.js
// custom server in main.js.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
)

//go:embed main.js
var server string

// start boots the runtime and evaluates the Next.js custom server (main.js).
// The caller drives the event loop with rt.Wait; the running server is
// reachable as a Go http.Handler via rt.HTTPHandler.
func start(out, errOut io.Writer, env ...string) (*spidermonkey.JS, *nodejs.Runtime, error) {
	js, err := spidermonkey.New(spidermonkey.Config{
		FS:             os.DirFS("."),
		Env:            append([]string{"NODE_ENV=production"}, env...),
		MaxMemoryBytes: 2 << 30,
		Stdout:         out,
		Stderr:         errOut,
		// The network hooks are fail-closed (nil denies); allow the loopback
		// listen the server needs.
		Dial:    func(network, host, ip string, port int) bool { return true },
		Resolve: func(host string) bool { return true },
		Listen:  func(network, addr string) bool { return true },
	})
	if err != nil {
		return nil, nil, err
	}
	rt, err := nodejs.Install(js)
	if err != nil {
		js.Close()
		return nil, nil, err
	}
	if r, err := js.Eval(context.Background(), server); err != nil {
		rt.Close()
		js.Close()
		return nil, nil, err
	} else if r.Error != nil {
		rt.Close()
		js.Close()
		return nil, nil, r.Error
	}
	return js, rt, nil
}

func main() {
	js, rt, err := start(os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer js.Close()
	defer rt.Close()
	// Serve until the process is stopped.
	if err := rt.Wait(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
