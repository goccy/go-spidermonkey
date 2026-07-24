package nodejs_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// The http client must run off the loop goroutine: a slow response must not
// freeze timers or other work. A ~150ms response races a 10ms timer; the timer
// has to fire first, which is only possible if the request does not block the
// loop.
func TestHTTPClientDoesNotBlockLoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Write([]byte("slow-body"))
	}))
	defer srv.Close()

	js, rt := newRuntime(t, spidermonkey.Config{})
	js.Global().Set("BASE", spidermonkey.ValueOf(srv.URL))
	if _, err := rt.RunScript(context.Background(), `
		globalThis.__order = [];
		const http = require("http");
		http.get(BASE, (res) => {
			let body = "";
			res.on("data", (c) => { body += c; });
			res.on("end", () => { __order.push("response:" + body); });
		});
		setTimeout(() => { __order.push("timer"); }, 10);
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if got := evalStr(t, js, `__order.join(",")`); got != "timer,response:slow-body" {
		t.Fatalf("order = %q, want timer,response:slow-body (client blocked the loop)", got)
	}
}
