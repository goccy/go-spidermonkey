package web_test

import (
	"context"
	"strings"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// fetch's Dial/Resolve hooks are fail-closed. Installing compat/web without a
// network policy must NOT open outbound access. This builds JS with a bare
// Config directly (bypassing newWeb's allow-all test defaults).
func TestFetchDeniedByDefault(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { js.Close() })
	w, err := web.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	// A literal-IP target needs Config.Dial (nil ⇒ deny); no DNS involved.
	js.Eval(context.Background(), `
		globalThis.__r = {};
		fetch("http://127.0.0.1:9/").then(() => { __r.ok = true; })
			.catch((e) => { __r.err = String(e && e.message || e); });
	`)
	if evalString(t, js, `String(__r.ok ?? false)`) == "true" {
		t.Fatalf("nil Dial should deny fetch to a literal IP")
	}
	if got := evalString(t, js, `__r.err ?? ""`); !strings.Contains(got, "permission denied") {
		t.Fatalf("nil Dial should deny fetch; got err=%q", got)
	}
}
