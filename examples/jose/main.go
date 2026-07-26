// Command jose runs the real, unmodified jose npm package on the
// go-spidermonkey WEB (WinterTC) layer — no Node.js binary involved. It signs
// and verifies an HS256 JWT using the WebCrypto surface; the app itself is the
// ES module in main.js. The package is loaded from ./node_modules exactly as an
// ESM resolver would find it.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
	"github.com/goccy/go-spidermonkey/compat/web"
)

//go:embed main.js
var app string

// run boots the WEB (WinterTC) layer over ./node_modules and evaluates main.js
// as an ES module, sending its console output to out.
func run(out io.Writer) error {
	js, err := spidermonkey.New(spidermonkey.Config{FS: os.DirFS("."), Stdout: out})
	if err != nil {
		return err
	}
	defer js.Close()

	w, err := web.Install(js)
	if err != nil {
		return err
	}
	defer w.Close()

	js.SetModuleLoader(nodejs.ESMLoader)

	r, err := js.EvalModule(context.Background(), "main.js", app)
	if err != nil {
		return err
	}
	return r.Error
}

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
