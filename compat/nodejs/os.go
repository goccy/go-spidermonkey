package nodejs

// The host ops backing node:os system information (os.cpus/totalmem/
// freemem) and process resource accounting (process.memoryUsage/cpuUsage/
// resourceUsage). SpiderMonkey has no view of the host machine, so these
// values come from the Go runtime (runtime.NumCPU, runtime.MemStats) and the
// interpreter's configured linear-memory ceiling. The JS layers (corelibs.js
// os, runtime.js process) call these lazily, so they reflect live numbers at
// call time rather than install-time snapshots.

import (
	goruntime "runtime"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// defaultTotalMemBytes mirrors internal/newjs.go's defaultMaxMemoryBytes: the
// linear-memory ceiling used when Config.MaxMemoryBytes is left at 0. It is the
// figure os.totalmem() reports when the embedder did not set an explicit cap.
const defaultTotalMemBytes = 256 << 20

func (rt *Runtime) sysOps() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"os_cpus":       rt.opOSCPUs,
		"os_meminfo":    rt.opOSMemInfo,
		"proc_memusage": rt.opProcMemUsage,
		"proc_cpuusage": rt.opProcCPUUsage,
		"proc_resusage": rt.opProcResUsage,
	}
}

// totalMemBytes is the effective linear-memory ceiling: the configured cap, or
// the engine default when unset. It is what os.totalmem() reports.
func totalMemBytes(cfg spidermonkey.Config) uint64 {
	if cfg.MaxMemoryBytes > 0 {
		return uint64(cfg.MaxMemoryBytes)
	}
	return defaultTotalMemBytes
}

// opOSCPUs returns one entry per logical CPU the Go runtime sees. The ARRAY
// LENGTH is the real core count (runtime.NumCPU) — apps size worker pools from
// it — while model/speed are plausible constants and the per-core times are
// zeros (the wasm sandbox exposes no real scheduler accounting).
func (rt *Runtime) opOSCPUs(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	n := goruntime.NumCPU()
	if n < 1 {
		n = 1
	}
	cpus := make([]any, 0, n)
	for i := 0; i < n; i++ {
		cpus = append(cpus, map[string]any{
			"model": "wasm virtual CPU",
			"speed": 2400,
			"times": map[string]any{
				"user": 0, "nice": 0, "sys": 0, "idle": 0, "irq": 0,
			},
		})
	}
	return spidermonkey.ValueOf(cpus), nil
}

// opOSMemInfo reports totalmem (the linear-memory ceiling) and freemem
// (ceiling minus what the Go runtime currently has mapped from the OS). Both
// are non-zero so os.freemem()/os.totalmem() ratios never produce NaN.
func (rt *Runtime) opOSMemInfo(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	total := totalMemBytes(cfg)
	var m goruntime.MemStats
	goruntime.ReadMemStats(&m)
	free := int64(total) - int64(m.Sys)
	if free < 0 {
		free = int64(total) / 8 // ceiling already exceeded by Go's own arena; still report a plausible sliver
	}
	return spidermonkey.ValueOf(map[string]any{
		"total": float64(total),
		"free":  float64(free),
	}), nil
}

// opProcMemUsage maps runtime.MemStats onto Node's process.memoryUsage() shape.
// heapUsed/heapTotal are the live GC figures (leak-guards and ratios depend on
// them being real and non-zero); rss approximates the resident set with the
// total bytes obtained from the OS.
func (rt *Runtime) opProcMemUsage(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	var m goruntime.MemStats
	goruntime.ReadMemStats(&m)
	return spidermonkey.ValueOf(map[string]any{
		"rss":          float64(m.Sys),
		"heapTotal":    float64(m.HeapSys),
		"heapUsed":     float64(m.HeapAlloc),
		"external":     float64(m.HeapIdle),
		"arrayBuffers": 0,
	}), nil
}

// cpuTimesMicros derives a monotonic, non-zero cumulative CPU time from wall
// time elapsed since the runtime started. The wasm sandbox exposes no per-
// process scheduler accounting, so this is best-effort: user gets the bulk,
// system a smaller share, which is enough for cpuUsage(prev) diffs to be
// positive and monotonic (what benchmarking/telemetry code checks for).
func (rt *Runtime) cpuTimesMicros() (user, system float64) {
	elapsed := time.Since(rt.started).Microseconds()
	if elapsed < 0 {
		elapsed = 0
	}
	user = float64(elapsed) * 0.75
	system = float64(elapsed) * 0.25
	return user, system
}

func (rt *Runtime) opProcCPUUsage(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	user, system := rt.cpuTimesMicros()
	return spidermonkey.ValueOf(map[string]any{
		"user":   user,
		"system": system,
	}), nil
}

// opProcResUsage returns Node's ~16-field resourceUsage() object. The fields
// we can source from the Go runtime (CPU times, maxRSS in kilobytes) are
// filled; the rest (page faults, context switches, IPC, fs I/O) are 0 because
// the sandbox has no way to observe them.
func (rt *Runtime) opProcResUsage(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	user, system := rt.cpuTimesMicros()
	var m goruntime.MemStats
	goruntime.ReadMemStats(&m)
	return spidermonkey.ValueOf(map[string]any{
		"userCPUTime":                user,
		"systemCPUTime":              system,
		"maxRSS":                     float64(m.Sys / 1024), // kilobytes, as getrusage reports
		"sharedMemorySize":           0,
		"unsharedDataSize":           0,
		"unsharedStackSize":          0,
		"minorPageFault":             0,
		"majorPageFault":             0,
		"swappedOut":                 0,
		"fsRead":                     0,
		"fsWrite":                    0,
		"ipcSent":                    0,
		"ipcReceived":                0,
		"signalsCount":               0,
		"voluntaryContextSwitches":   0,
		"involuntaryContextSwitches": 0,
	}), nil
}
