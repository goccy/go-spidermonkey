package spidermonkey

import (
	"fmt"
	"sync/atomic"
)

// funcKeyCounter numbers the hidden dispatch keys NewFunction / DefineFunc
// mint. The NUL prefix keeps them out of any guest-visible name space, so an
// anonymous function can never collide with (or be shadowed by) another
// definition. Process-global for simplicity: uniqueness is sufficient, the
// per-interpreter map does not require density.
var funcKeyCounter atomic.Uint64

func nextFnKey() string { return fmt.Sprintf("\x00fn:%d", funcKeyCounter.Add(1)) }

// NewFunction returns a fresh guest function object backed by fn — the Go
// analogue of syscall/js's FuncOf. Unlike DefineFunc it attaches to nothing:
// the embedder composes it into any structure — an object property (Set), a
// callback argument (Call), an underlyingSource.pull — and identity is
// preserved wherever it flows. name is only the function's `name` property.
//
// The caller owns the handle (Free releases the host's pin; the guest keeps
// the function alive as long as it references it). The Go-side registration
// lives for the interpreter's lifetime — it cannot be released earlier, since
// the guest may hold and call the function long after the host dropped its
// handle.
func (js *JS) NewFunction(name string, fn Func) (*Object, error) {
	key := nextFnKey()
	js.env.funcs[key] = fn
	h, err := js.raw.NewFunction(name, key, 0)
	if err != nil {
		delete(js.env.funcs, key)
		return nil, err
	}
	return &Object{js: js, handle: h, callable: true}, nil
}
