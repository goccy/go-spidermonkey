package nodetest_test

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-spidermonkey/nodetest"
)

// TestNodeFailureBuckets groups tests by the FIRST line their run printed, so a
// few hundred failures resolve into the handful of missing APIs behind them.
// It is a diagnostic, not a gate: it never fails, and it is what decides which
// gap is worth closing next.
//
//	NODETEST=1 NODETEST_BUCKETS=quarantine go test ./nodetest -run FailureBuckets -v
//
// NODETEST_BUCKETS takes "quarantine" (the hanging list), "all", or a
// comma-separated list of suite-relative paths. NODETEST_BUCKETS_TOP sets how
// many buckets are printed (default 20).
func TestNodeFailureBuckets(t *testing.T) {
	sel := os.Getenv("NODETEST_BUCKETS")
	if os.Getenv("NODETEST") == "" || sel == "" {
		t.Skip("set NODETEST=1 and NODETEST_BUCKETS=quarantine|all|<paths>")
	}
	var paths []string
	switch sel {
	case "quarantine":
		b, err := os.ReadFile("quarantine.txt")
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
				paths = append(paths, line)
			}
		}
	case "all":
		for _, dir := range []string{"test/parallel", "test/es-module"} {
			p, err := nodetest.List("suite", dir)
			if err != nil {
				t.Fatal(err)
			}
			paths = append(paths, p...)
		}
	default:
		paths = strings.Split(sel, ",")
	}

	timeout := 8 * time.Second
	if s := os.Getenv("NODETEST_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			timeout = d
		}
	}

	digits := regexp.MustCompile(`\d+`)
	counts := map[string]int{}
	example := map[string]string{}
	hung := 0
	for _, rel := range paths {
		r := nodetest.Run(context.Background(), nodetest.Options{Root: "suite", Timeout: timeout}, rel)
		if r.Status == nodetest.StatusPass {
			continue
		}
		if strings.Contains(r.Reason, "deadline") {
			hung++
		}
		line := strings.TrimSpace(strings.SplitN(r.Output, "\n", 2)[0])
		if line == "" {
			line = "(no output) " + r.Reason
		}
		if len(line) > 110 {
			line = line[:110]
		}
		key := digits.ReplaceAllString(line, "N")
		counts[key]++
		if example[key] == "" {
			example[key] = rel
		}
	}

	type bucket struct {
		msg string
		n   int
	}
	buckets := make([]bucket, 0, len(counts))
	for k, v := range counts {
		buckets = append(buckets, bucket{k, v})
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].n > buckets[j].n })

	top := 20
	if v, err := strconv.Atoi(os.Getenv("NODETEST_BUCKETS_TOP")); err == nil && v > 0 {
		top = v
	}
	t.Logf("%d tests, %d still hang, %d distinct first-errors", len(paths), hung, len(buckets))
	for i, b := range buckets {
		if i == top {
			break
		}
		t.Logf("%5d  %s\n         e.g. %s", b.n, b.msg, example[b.msg])
	}
}
