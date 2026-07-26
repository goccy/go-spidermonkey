// Command nodejs runs unmodified npm packages (lodash, commander) on the
// go-spidermonkey Node runtime — no Node.js binary involved. The packages are
// loaded from ./node_modules exactly as Node would resolve them, and the app
// itself is the script in main.js.
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
var app string

// run boots the Node runtime over ./node_modules and executes main.js, sending
// the script's console output to out.
func run(out io.Writer) error {
	js, err := spidermonkey.New(spidermonkey.Config{FS: os.DirFS("."), Stdout: out})
	if err != nil {
		return err
	}
	defer js.Close()

	rt, err := nodejs.Install(js)
	if err != nil {
		return err
	}
	defer rt.Close()

	r, err := rt.RunScript(context.Background(), app)
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
