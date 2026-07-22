// Separate module: goja is benchmark-only and must never enter
// go-spidermonkey's own go.mod. Run from this directory.
module github.com/goccy/go-spidermonkey/bench

go 1.26.0

require (
	github.com/dop251/goja v0.0.0-20250630131328-58d95d85e994
	github.com/goccy/go-spidermonkey v0.0.0
)

require (
	github.com/dlclark/regexp2 v1.11.4 // indirect
	github.com/go-sourcemap/sourcemap v2.1.3+incompatible // indirect
	github.com/goccy/spidermonkeywasm2go v0.2.4 // indirect
	github.com/google/pprof v0.0.0-20230207041349-798e818bf904 // indirect
	golang.org/x/text v0.3.8 // indirect
)

replace github.com/goccy/go-spidermonkey => ../
