package nodetest_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/goccy/go-spidermonkey/internal/testutil/nodetest"
)

// TestNodeOne runs a single test file and prints everything it produced. It is
// the first step in diagnosing any entry of quarantine.txt: the suite driver
// reports only a status, and what a hanging test PRINTED is usually what names
// the missing API.
//
//	NODETEST_ONE=test/parallel/test-http-raw-headers.js go test ./nodetest -run TestNodeOne -v
func TestNodeOne(t *testing.T) {
	rel := os.Getenv("NODETEST_ONE")
	if rel == "" {
		t.Skip("set NODETEST_ONE=<suite-relative path>")
	}
	timeout := 8 * time.Second
	if s := os.Getenv("NODETEST_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			timeout = d
		}
	}
	r := nodetest.Run(context.Background(), nodetest.Options{Root: "testdata/suite", Timeout: timeout}, rel)
	t.Logf("status=%v reason=%s\n--- output ---\n%s", r.Status, r.Reason, r.Output)
}
