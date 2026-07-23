// Separate module: goja is benchmark-only and must never enter
// go-spidermonkey's own go.mod. Run from this directory.
module github.com/goccy/go-spidermonkey/bench

go 1.26.0

require (
	github.com/dop251/goja v0.0.0-20250630131328-58d95d85e994
	github.com/goccy/go-spidermonkey v0.0.0
	modernc.org/quickjs v0.21.1
)

require (
	github.com/dlclark/regexp2 v1.11.4 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-sourcemap/sourcemap v2.1.3+incompatible // indirect
	github.com/goccy/spidermonkeywasm2go v0.2.4 // indirect
	github.com/google/pprof v0.0.0-20230207041349-798e818bf904 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.3.8 // indirect
	modernc.org/libc v1.74.1 // indirect
	modernc.org/libquickjs v0.12.10 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

replace github.com/goccy/go-spidermonkey => ../
