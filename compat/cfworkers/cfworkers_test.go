package cfworkers_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/cfworkers"
)

func newPoolServer(t *testing.T, cfg cfworkers.PoolConfig) *httptest.Server {
	t.Helper()
	pool, err := cfworkers.NewPool(cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	srv := httptest.NewServer(pool)
	t.Cleanup(srv.Close)
	return srv
}

func TestPoolServesWorker(t *testing.T) {
	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1,
		Env: map[string]cfworkers.Binding{
			"GREETING": cfworkers.Static("hello from Go"),
		},
		Source: `
			export default {
				async fetch(req, env, ctx) {
					const u = new URL(req.url);
					if (u.pathname === "/echo") {
						return Response.json({
							method: req.method,
							path: u.pathname,
							q: u.searchParams.get("q"),
							header: req.headers.get("x-test"),
							body: await req.text(),
							greeting: env.GREETING ?? null,
						});
					}
					return new Response("not found", { status: 404 });
				},
			};
		`,
	})

	req, _ := http.NewRequest("POST", srv.URL+"/echo?q=42", strings.NewReader("ping"))
	req.Header.Set("X-Test", "present")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	var got struct {
		Method, Path, Q, Header, Body, Greeting string
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := got
	want.Method, want.Path, want.Q, want.Header, want.Body, want.Greeting =
		"POST", "/echo", "42", "present", "ping", "hello from Go"
	if got != want {
		t.Errorf("echo = %+v, want %+v", got, want)
	}

	// Non-matching route: the worker's own 404.
	resp2, err := http.Get(srv.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != 404 || string(body2) != "not found" {
		t.Errorf("fallback = %d %q", resp2.StatusCode, body2)
	}
}

func TestPoolAsyncHandlerUsesTimers(t *testing.T) {
	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1,
		Source: `
			const sleep = (ms) => new Promise((res) => setTimeout(res, ms));
			export default {
				async fetch(req) {
					await sleep(20);
					return new Response("slept");
				},
			};
		`,
	})
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "slept" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
}

func TestPoolWaitUntilRunsAfterResponse(t *testing.T) {
	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1,
		Source: `
			let flag = "unset";
			const sleep = (ms) => new Promise((res) => setTimeout(res, ms));
			export default {
				async fetch(req, env, ctx) {
					const u = new URL(req.url);
					if (u.pathname === "/check") return new Response(flag);
					ctx.waitUntil(sleep(20).then(() => { flag = "done"; }));
					return new Response("scheduled");
				},
			};
		`,
	})
	resp1, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	if string(body1) != "scheduled" {
		t.Fatalf("first response = %q", body1)
	}
	// The pool drains waitUntil before reuse, so the same (only) instance
	// must observe the flag on the next request.
	resp2, err := http.Get(srv.URL + "/check")
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if string(body2) != "done" {
		t.Errorf("flag after waitUntil = %q, want \"done\"", body2)
	}
}

func TestPoolConcurrency(t *testing.T) {
	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 2,
		Source: `
			const sleep = (ms) => new Promise((res) => setTimeout(res, ms));
			export default {
				async fetch(req) {
					await sleep(100);
					return new Response("ok");
				},
			};
		`,
	})

	const requests = 4
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(srv.URL + "/")
			if err != nil {
				errs <- err
				return
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || string(body) != "ok" {
				errs <- fmt.Errorf("got %d %q", resp.StatusCode, body)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// 4 requests × 100ms with 2 instances is 2 batches (~200ms); serial
	// execution would need ~400ms. The bound proves genuine parallelism
	// while leaving slack for slow machines.
	if elapsed := time.Since(start); elapsed >= 380*time.Millisecond {
		t.Errorf("4 requests took %v; pool of 2 did not serve them in parallel", elapsed)
	}
}

func TestPoolHandlerErrorIs500(t *testing.T) {
	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1,
		Source: `
			export default {
				async fetch() { throw new Error("kaboom"); },
			};
		`,
	})
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 500 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "kaboom") {
		t.Errorf("body = %q, want the thrown message", body)
	}
}

func TestPoolNonResponseReturnIs500(t *testing.T) {
	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size:   1,
		Source: `export default { async fetch() { return 42; } };`,
	})
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

func TestPoolFunctionBinding(t *testing.T) {
	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1,
		Env: map[string]cfworkers.Binding{
			"DOUBLE": func(js *spidermonkey.JS) (spidermonkey.Value, error) {
				return js.NewFunction("double", func(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
					if len(args) < 1 {
						return nil, fmt.Errorf("double: argument required")
					}
					return spidermonkey.ValueOf(args[0].Float() * 2), nil
				})
			},
		},
		Source: `
			export default {
				async fetch(req, env) {
					return new Response(String(env.DOUBLE(21)));
				},
			};
		`,
	})
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "42" {
		t.Errorf("env.DOUBLE(21) served %q, want \"42\"", body)
	}
}

func TestPoolBootErrors(t *testing.T) {
	if _, err := cfworkers.NewPool(cfworkers.PoolConfig{
		Size:   1,
		Source: `export default {};`,
	}); err == nil || !strings.Contains(err.Error(), "fetch handler") {
		t.Errorf("missing-handler boot err = %v", err)
	}
	if _, err := cfworkers.NewPool(cfworkers.PoolConfig{
		Size:   1,
		Source: `export default { fetch() {} }; throw new Error("boot failure");`,
	}); err == nil || !strings.Contains(err.Error(), "boot failure") {
		t.Errorf("throwing-module boot err = %v", err)
	}
}

// A worker returning an out-of-range status (Response.error() uses 0) must not
// panic net/http's WriteHeader and poison the pooled instance: it becomes a
// clean 500 and the same instance keeps serving.
func TestPoolInvalidStatusReturns500AndSurvives(t *testing.T) {
	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1,
		Source: `
			export default {
				async fetch(req) {
					const u = new URL(req.url);
					if (u.pathname === "/bad") return new Response("x", { status: 0 });
					return new Response("ok");
				},
			};
		`,
	})
	resp, err := http.Get(srv.URL + "/bad")
	if err != nil {
		t.Fatalf("GET /bad: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("GET /bad status = %d, want 500", resp.StatusCode)
	}
	// The instance must still be usable.
	resp2, err := http.Get(srv.URL + "/ok")
	if err != nil {
		t.Fatalf("GET /ok after bad: %v", err)
	}
	body, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 || string(body) != "ok" {
		t.Errorf("instance poisoned: GET /ok = %d %q", resp2.StatusCode, body)
	}
}
