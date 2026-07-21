module github.com/goccy/go-spidermonkey

go 1.25.0

require github.com/goccy/spidermonkeywasm2go v0.2.1

// The raw-bytes bridge (js_bytes_new / js_bytes_read) is not in a published
// bundle yet; build against the locally regenerated one until the next
// spidermonkeywasm2go release, then drop this replace.
replace github.com/goccy/spidermonkeywasm2go => ../spidermonkey-wasm/build/wasm2go/internal/internal/wasm2go
