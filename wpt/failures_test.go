package wpt_test

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-spidermonkey/wpt"
)

// TestWPTFailureBuckets groups the failing subtests of a directory by their
// message so the largest cause is visible instead of 8,000 individual lines.
// It is a diagnostic, not a gate: it never fails, and it is what decides which
// item of docs/conformance-plan.md is worth doing next.
//
//	WPT=1 WPT_BUCKETS=WebCryptoAPI go test ./wpt -run FailureBuckets -v
//
// WPT_BUCKETS_GREP narrows to messages containing a substring,
// WPT_BUCKETS_TOP sets how many buckets are printed (default 15), and
// WPT_BUCKETS_NAMES=1 keys on the subtest name as well as the message, to
// break one large message bucket back down into the cases it covers.
func TestWPTFailureBuckets(t *testing.T) {
	dirs := os.Getenv("WPT_BUCKETS")
	if os.Getenv("WPT") == "" || dirs == "" {
		t.Skip("set WPT=1 and WPT_BUCKETS=<dir>[,<dir>...]")
	}
	paths, err := wpt.List(suiteDir, strings.Split(dirs, ","))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := wpt.StartServer(suiteDir)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// Digits are collapsed so that "AES-GCM 128" and "AES-GCM 256" land in one
	// bucket: the shared cause is what matters, not the parameter.
	digits := regexp.MustCompile(`\d+`)
	grep := os.Getenv("WPT_BUCKETS_GREP")
	counts := map[string]int{}
	failing := 0
	for _, p := range paths {
		r := wpt.Run(context.Background(), wpt.Options{Root: suiteDir, BaseURL: srv.BaseURL(), SubVars: srv.SubVars()}, p)
		for _, s := range r.Subtests {
			if s.Status == wpt.StatusPass || !strings.Contains(s.Message, grep) {
				continue
			}
			failing++
			// The MESSAGE is the cause; the name is the parameter it happened
			// to fail on. Bucketing by message is what makes 8,000 failures
			// resolve into a handful of things to fix.
			line := s.Message
			if os.Getenv("WPT_BUCKETS_NAMES") != "" {
				line = s.Name + " :: " + line
			}
			if len(line) > 110 {
				line = line[:110]
			}
			counts[digits.ReplaceAllString(line, "N")]++
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

	top := 15
	if v, err := strconv.Atoi(os.Getenv("WPT_BUCKETS_TOP")); err == nil && v > 0 {
		top = v
	}
	t.Logf("%d failing subtests in %s, %d distinct causes", failing, dirs, len(buckets))
	for i, b := range buckets {
		if i == top {
			break
		}
		t.Logf("%6d  %s", b.n, b.msg)
	}
}
