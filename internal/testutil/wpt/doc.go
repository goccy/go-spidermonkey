// Package wpt is the test harness that runs the Web Platform Tests against
// compat/web. It is test tooling, not part of this module's API, which is why
// it lives under internal/.
//
// It has three parts:
//
//   - A loopback server (server.go, handlers.go, permissive.go, tls.go) that
//     stands in for wptserve: it serves the pinned upstream checkout in
//     testdata/wpt/suite at the repository root (a git submodule,
//     materialized by `make wpt-fetch`),
//     re-implements the suite's Python fixture handlers (*.py) in Go, and
//     speaks wptserve's conventions —
//     `.headers` files, `.asis` raw responses, `?pipe=` stages and `.sub.`
//     template substitution.
//
//   - A runner (runner.go) that expands each test file into the cases a
//     browser would run (scope × variant), executes them on a fresh
//     compat/web instance with testharness.js, and collects per-subtest
//     verdicts.
//
//   - A gate (suite_test.go, expectations.json) that compares every verdict
//     against the checked-in expectations, so a regression in ANY subtest —
//     passing or failing — turns the build red. `WPT_UPDATE=1` rewrites the
//     expectations after an intentional change; failures_test.go groups
//     failing subtests by message to show what is worth fixing next.
//
// Run it with `make wpt`, or directly:
//
//	WPT=1 go test ./internal/testutil/wpt -run TestWPTSuite
//	WPT=1 WPT_DIRS=fetch/api WPT_FILTER=cors go test ./internal/testutil/wpt -run TestWPTSuite
//	WPT=1 WPT_BUCKETS=fetch go test ./internal/testutil/wpt -run FailureBuckets -v
package wpt
