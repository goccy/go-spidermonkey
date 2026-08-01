package cfworkers_test

import (
	"context"
	"encoding/json"
	"fmt"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/cfworkers"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
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

func newPool(t *testing.T, cfg cfworkers.PoolConfig) *cfworkers.Pool {
	t.Helper()
	pool, err := cfworkers.NewPool(cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func serve(t *testing.T, pool *cfworkers.Pool) string {
	t.Helper()
	srv := httptest.NewServer(pool)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestScheduledHandler(t *testing.T) {
	pool := newPool(t, cfworkers.PoolConfig{
		Size: 1,
		Source: `
			globalThis.__ran = null;
			export default {
				async scheduled(event, env, ctx) {
					globalThis.__ran = event.cron;
				},
				async fetch(req) {
					return new Response(globalThis.__ran ?? "not-run");
				},
			};
		`,
	})

	if err := pool.Scheduled(context.Background(), "*/5 * * * *", 1720000000000); err != nil {
		t.Fatalf("Scheduled: %v", err)
	}
	// The same warm instance reports the cron the scheduled handler saw.
	srv := serve(t, pool)
	resp, err := http.Get(srv)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "*/5 * * * *" {
		t.Errorf("scheduled handler ran with cron = %q", body)
	}
}

func TestScheduledMissingHandlerErrors(t *testing.T) {
	pool := newPool(t, cfworkers.PoolConfig{
		Size:   1,
		Source: `export default { async fetch() { return new Response("ok"); } };`,
	})
	err := pool.Scheduled(context.Background(), "* * * * *", 0)
	if err == nil || !strings.Contains(err.Error(), "no scheduled") {
		t.Errorf("expected missing-handler error, got %v", err)
	}
}

func TestQueueHandler(t *testing.T) {
	pool := newPool(t, cfworkers.PoolConfig{
		Size: 1,
		Source: `
			globalThis.__processed = [];
			export default {
				async queue(batch, env, ctx) {
					for (const msg of batch.messages) {
						globalThis.__processed.push(batch.queue + ":" + JSON.stringify(msg.body));
						msg.ack();
					}
				},
				async fetch(req) {
					return Response.json(globalThis.__processed);
				},
			};
		`,
	})

	if err := pool.Queue(context.Background(), "my-queue", []any{
		map[string]any{"task": "a"},
		map[string]any{"task": "b"},
	}); err != nil {
		t.Fatalf("Queue: %v", err)
	}
	srv := serve(t, pool)
	resp, err := http.Get(srv)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	want := `["my-queue:{\"task\":\"a\"}","my-queue:{\"task\":\"b\"}"]`
	if strings.TrimSpace(string(body)) != want {
		t.Errorf("queue processed = %s, want %s", body, want)
	}
}

func TestCacheAPI(t *testing.T) {
	pool := newPool(t, cfworkers.PoolConfig{
		Size: 1,
		Source: `
			export default {
				async fetch(req) {
					const url = new URL(req.url);
					const cache = caches.default;
					const key = "https://example.com/data";
					if (url.pathname === "/put") {
						await cache.put(key, new Response("cached-value", { headers: { "X-Cache": "stored" } }));
						return new Response("stored");
					}
					if (url.pathname === "/get") {
						const hit = await cache.match(key);
						if (!hit) return new Response("MISS", { status: 404 });
						return new Response(await hit.text() + ":" + hit.headers.get("X-Cache"));
					}
					if (url.pathname === "/named") {
						const c = await caches.open("v1");
						await c.put(key, new Response("named-value"));
						const hit = await c.match(key);
						return new Response(await hit.text());
					}
					return new Response("?", { status: 404 });
				},
			};
		`,
	})
	srv := serve(t, pool)

	// Put then get on the same warm instance.
	if _, err := http.Get(srv + "/put"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(srv + "/get")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "cached-value:stored" {
		t.Errorf("cache get = %q", body)
	}

	resp2, err := http.Get(srv + "/named")
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if string(body2) != "named-value" {
		t.Errorf("named cache = %q", body2)
	}
}

func TestWebSocketPair(t *testing.T) {
	pool := newPool(t, cfworkers.PoolConfig{
		Size: 1,
		Source: `
			export default {
				async fetch(req) {
					const pair = new WebSocketPair();
					const client = pair[0], server = pair[1];
					server.accept();
					const received = [];
					server.addEventListener("message", (e) => { received.push(e.data); });
					// Simulate a client message.
					client.send("hello from client");
					// Give the microtask queue a turn, then report.
					await Promise.resolve();
					await Promise.resolve();
					return Response.json({
						isServerOpen: server.readyState === 1,
						received,
					});
				},
			};
		`,
	})
	srv := serve(t, pool)
	resp, err := http.Get(srv)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"isServerOpen":true`) {
		t.Errorf("server socket not open: %s", body)
	}
	if !strings.Contains(string(body), "hello from client") {
		t.Errorf("message not delivered across the pair: %s", body)
	}
}

// TestCryptoKeyCachedAcrossRequests verifies an imported/generated CryptoKey
// cached in a module global keeps working on later requests to the same pooled
// instance — the "import once, reuse" pattern that real Cloudflare Workers
// support (module globals persist on a warm isolate). Request 1 caches an HMAC
// key and signs; request 2 reuses the SAME cached key and must still sign,
// rather than failing with "unknown key handle" (which a per-request key wipe
// would cause). Anti-forgery rests on the unguessable random handle within the
// single trust domain, not on wiping keys between requests.
func TestCryptoKeyCachedAcrossRequests(t *testing.T) {
	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1, // force reuse of one instance across both requests
		Source: `
			let cachedKey; // module global: persists across requests like a warm isolate
			export default {
				async fetch(req, env, ctx) {
					if (!cachedKey) {
						cachedKey = await crypto.subtle.importKey(
							"raw", new TextEncoder().encode("secret-hmac-key"),
							{ name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
					}
					try {
						const sig = await crypto.subtle.sign("HMAC", cachedKey, new TextEncoder().encode("data"));
						return new Response("ok:" + new Uint8Array(sig).length);
					} catch (e) {
						return new Response("fail:" + (e && e.message), { status: 500 });
					}
				},
			};
		`,
	})

	for i := 0; i < 3; i++ {
		resp, err := http.Get(srv.URL + "/")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "ok:32" {
			t.Fatalf("request %d: status=%d body=%q, want 200 ok:32 (cached CryptoKey broke on reuse)", i, resp.StatusCode, body)
		}
	}
}

// TestHandlerTimerThrowStillResponds verifies a handler that returns a response
// but leaves a timer that throws still delivers the response (200), rather than
// the timer throw aborting the loop and turning it into a 500.
func TestHandlerTimerThrowStillResponds(t *testing.T) {
	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1,
		Source: `
			export default {
				async fetch(req, env, ctx) {
					setTimeout(() => { throw new Error("boom"); }, 0);
					return new Response("ok");
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
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (timer throw must not clobber the response)", resp.StatusCode)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want ok", body)
	}
}

// TestFireAndForgetFetchDoesNotCorruptNextRequest verifies a handler that issues
// a fetch without awaiting it (and without ctx.waitUntil) doesn't hang, leak, or
// make a LATER request on the same pooled instance fail — the async fetch's late
// completion must be cancelled/drained at the request boundary.
func TestFireAndForgetFetchDoesNotCorruptNextRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("data"))
	}))
	defer upstream.Close()

	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1, // one instance, reused across the two requests
		Config: spidermonkey.Config{
			Dial:    func(network, host, ip string, port int) bool { return true },
			Resolve: func(host string) bool { return true },
		},
		Env: map[string]cfworkers.Binding{"UP": cfworkers.Static(upstream.URL)},
		Source: `
			export default {
				async fetch(req, env, ctx) {
					// Fire-and-forget: start a fetch, read its body, never await.
					fetch(env.UP).then((r) => r.text());
					return new Response("ok");
				},
			};
		`,
	})

	for i := 0; i < 3; i++ {
		resp, err := http.Get(srv.URL + "/")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "ok" {
			t.Fatalf("request %d: status=%d body=%q, want 200/ok", i, resp.StatusCode, body)
		}
	}
}

// TestFetchResponsePassthrough verifies a handler can return an upstream fetch()
// Response straight through (the reverse-proxy pattern) — status, headers, and
// body must survive.
func TestFetchResponsePassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "origin")
		w.WriteHeader(201)
		io.WriteString(w, "proxied body")
	}))
	defer upstream.Close()

	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1,
		Config: spidermonkey.Config{
			Dial:    func(network, host, ip string, port int) bool { return true },
			Resolve: func(host string) bool { return true },
		},
		Env: map[string]cfworkers.Binding{
			"UPSTREAM": cfworkers.Static(upstream.URL),
		},
		Source: `
			export default {
				async fetch(req, env, ctx) {
					return fetch(env.UPSTREAM);
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
	if resp.StatusCode != 201 {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Upstream"); got != "origin" {
		t.Errorf("X-Upstream = %q, want origin", got)
	}
	if string(body) != "proxied body" {
		t.Errorf("body = %q, want %q", body, "proxied body")
	}
}

// TestLeftoverTimerDoesNotCorruptNextRequest verifies a handler that leaves a
// setInterval armed (which itself spawns fetches) does not corrupt the next
// request on a reused instance. The request-boundary reset must quiesce the
// timer before draining, so it can't fire mid-drain and spawn a fetch whose
// completion lands on the next request's loop (driving its pending accounting
// negative -> a spurious 500).
func TestLeftoverTimerDoesNotCorruptNextRequest(t *testing.T) {
	// A slow upstream guarantees a fire-and-forget fetch is still in flight at
	// request end, so the drain RunUntil actually runs (the window the leftover
	// timer would otherwise fire into).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Write([]byte("data"))
	}))
	defer upstream.Close()

	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1, // one instance, reused across requests
		Config: spidermonkey.Config{
			Dial:    func(network, host, ip string, port int) bool { return true },
			Resolve: func(host string) bool { return true },
		},
		Env: map[string]cfworkers.Binding{"UP": cfworkers.Static(upstream.URL)},
		Source: `
			export default {
				async fetch(req, env, ctx) {
					fetch(env.UP).then((r) => r.text()).catch(() => {}); // in-flight at request end
					setInterval(() => { fetch(env.UP).then((r) => r.text()).catch(() => {}); }, 5); // never cleared
					return new Response("ok:" + new URL(req.url).pathname);
				},
			};
		`,
	})

	for i := 0; i < 5; i++ {
		path := "/r" + string(rune('0'+i))
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "ok:"+path {
			t.Fatalf("request %d: status=%d body=%q, want 200 ok:%s", i, resp.StatusCode, body, path)
		}
	}
}

// TestBackgroundTimerThrowDoesNotFailResponse verifies an uncaught exception in a
// background setTimeout callback does not tear down the loop and fail an
// otherwise-valid in-flight response (Workers report it out-of-band).
func TestBackgroundTimerThrowDoesNotFailResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(40 * time.Millisecond)
		w.Write([]byte("payload"))
	}))
	defer upstream.Close()

	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1,
		Config: spidermonkey.Config{
			Dial:    func(network, host, ip string, port int) bool { return true },
			Resolve: func(host string) bool { return true },
		},
		Env: map[string]cfworkers.Binding{"UP": cfworkers.Static(upstream.URL)},
		Source: `
			export default {
				async fetch(req, env, ctx) {
					// Background work throws while the response is still pending.
					setTimeout(() => { throw new Error("bg boom"); }, 5);
					const r = await fetch(env.UP);
					return new Response(await r.text());
				},
			};
		`,
	})

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "payload" {
		t.Fatalf("status=%d body=%q, want 200 payload (background throw sank the response)", resp.StatusCode, body)
	}
}

// ctx.waitUntil() must NOT delay delivery of the response to the client: the
// canonical Workers idiom schedules background work (analytics/logging) and
// returns immediately. Regression: the response was written but never flushed
// before the blocking waitUntil drain loop, so net/http buffered it and the
// client waited the full waitUntil duration.
func TestWaitUntilDoesNotBlockResponse(t *testing.T) {
	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1,
		Source: `
			const sleep = (ms) => new Promise((res) => setTimeout(res, ms));
			export default {
				async fetch(req, env, ctx) {
					ctx.waitUntil(sleep(1500)); // long background task
					return new Response("ok");
				},
			};
		`,
	})
	start := time.Now()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	elapsed := time.Since(start)
	if string(body) != "ok" || resp.StatusCode != 200 {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	// The response must arrive well before the 1.5s background task settles.
	if elapsed > 800*time.Millisecond {
		t.Fatalf("response blocked on waitUntil: took %v (background task is 1.5s)", elapsed)
	}
}

// Streaming response delivery: a worker Response wrapping a ReadableStream
// must reach the client chunk-by-chunk (previously the whole body was
// buffered before the first byte went out), and a fetch() Response returned
// straight through (reverse proxy) must stream the upstream body. Also covers
// the request.cf local-dev stub.

// firstByteAndTotal GETs url and reports the time-to-first-byte, the total
// time until EOF, and the full body.
func firstByteAndTotal(t *testing.T, url string) (ttfb, total time.Duration, body string) {
	t.Helper()
	start := time.Now()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	var all []byte
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if ttfb == 0 {
				ttfb = time.Since(start)
			}
			all = append(all, buf[:n]...)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Fatalf("reading body: %v", rerr)
		}
	}
	return ttfb, time.Since(start), string(all)
}

// A worker Response wrapping a ReadableStream that enqueues "first", waits
// 300ms, enqueues "second", closes: the client must see "first" well before
// the stream completes (buffered delivery would make TTFB ≈ total).
func TestStreamedResponseFirstByteBeforeCompletion(t *testing.T) {
	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1,
		Source: `
			export default {
				async fetch(req) {
					const stream = new ReadableStream({
						start(c) {
							c.enqueue("first");
							setTimeout(() => { c.enqueue("second"); c.close(); }, 300);
						},
					});
					return new Response(stream, { headers: { "content-type": "text/plain" } });
				},
			};
		`,
	})

	ttfb, total, body := firstByteAndTotal(t, srv.URL+"/")
	if body != "firstsecond" {
		t.Fatalf("body = %q, want firstsecond", body)
	}
	if total < 300*time.Millisecond {
		t.Fatalf("total = %v; the stream's 300ms gap should dominate", total)
	}
	if ttfb >= 200*time.Millisecond {
		t.Errorf("TTFB = %v (total %v); first chunk must be delivered before the stream completes", ttfb, total)
	}
}

// The reverse-proxy pattern: the handler returns fetch(upstream) straight
// through. A slow upstream that flushes early must stream through to the
// client with an immediate first byte.
func TestPassthroughProxyStreamsUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		time.Sleep(400 * time.Millisecond)
		io.WriteString(w, "second")
	}))
	defer upstream.Close()

	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1,
		Config: spidermonkey.Config{
			Resolve: func(host string) bool { return true },
			Dial:    func(network, host, ip string, port int) bool { return true },
		},
		Env: map[string]cfworkers.Binding{
			"UPSTREAM": cfworkers.Static(upstream.URL),
		},
		Source: `
			export default {
				fetch(req, env) { return fetch(env.UPSTREAM); },
			};
		`,
	})

	ttfb, total, body := firstByteAndTotal(t, srv.URL+"/")
	if body != "firstsecond" {
		t.Fatalf("proxied body = %q, want firstsecond", body)
	}
	if total < 400*time.Millisecond {
		t.Fatalf("total = %v; the upstream's 400ms gap should dominate", total)
	}
	if ttfb >= 250*time.Millisecond {
		t.Errorf("TTFB = %v (total %v); passthrough proxying must stream, not buffer", ttfb, total)
	}
}

// Buffered bodies (strings) keep working alongside the streaming path, and a
// streamed response does not poison the pooled instance for the next request.
func TestStreamedThenBufferedOnSameInstance(t *testing.T) {
	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1,
		Source: `
			export default {
				async fetch(req) {
					const u = new URL(req.url);
					if (u.pathname === "/stream") {
						return new Response(new ReadableStream({
							start(c) { c.enqueue("s1"); c.enqueue(new TextEncoder().encode("s2")); c.close(); },
						}));
					}
					return new Response("plain");
				},
			};
		`,
	})
	_, _, body := firstByteAndTotal(t, srv.URL+"/stream")
	if body != "s1s2" {
		t.Fatalf("streamed body = %q, want s1s2", body)
	}
	_, _, body2 := firstByteAndTotal(t, srv.URL+"/plain")
	if body2 != "plain" {
		t.Fatalf("buffered body after a streamed one = %q, want plain", body2)
	}
}

// ctx.waitUntil work still drains after a streamed response completes: the
// same (only) pooled instance must observe the background write afterwards.
func TestStreamedResponseStillDrainsWaitUntil(t *testing.T) {
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
					return new Response(new ReadableStream({
						start(c) { c.enqueue("streamed"); c.close(); },
					}));
				},
			};
		`,
	})
	_, _, body := firstByteAndTotal(t, srv.URL+"/")
	if body != "streamed" {
		t.Fatalf("streamed body = %q", body)
	}
	_, _, flag := firstByteAndTotal(t, srv.URL+"/check")
	if flag != "done" {
		t.Errorf("flag after streamed response = %q, want done (waitUntil must still drain)", flag)
	}
}

// An erroring stream aborts the connection mid-body instead of ending it
// cleanly (the client must be able to tell truncation from success).
func TestStreamedResponseErrorAbortsConnection(t *testing.T) {
	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1,
		Source: `
			export default {
				async fetch(req) {
					const u = new URL(req.url);
					if (u.pathname === "/ok") return new Response("ok");
					return new Response(new ReadableStream({
						start(c) {
							c.enqueue("partial");
							setTimeout(() => c.error(new Error("stream blew up")), 20);
						},
					}));
				},
			};
		`,
	})
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rerr == nil {
		t.Errorf("erroring stream ended cleanly; want an aborted/truncated body read")
	}
	// The instance must survive for the next request.
	resp2, err := http.Get(srv.URL + "/ok")
	if err != nil {
		t.Fatalf("GET /ok after aborted stream: %v", err)
	}
	body, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 || string(body) != "ok" {
		t.Errorf("instance poisoned after stream error: %d %q", resp2.StatusCode, body)
	}
}

// request.cf: the workerd-local-dev-style stub must exist with sensibly typed
// fields so worker code reading request.cf.country etc. does not throw.
func TestRequestCFStub(t *testing.T) {
	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1,
		Source: `
			export default {
				async fetch(req) {
					return Response.json({
						hasCF: typeof req.cf === "object" && req.cf !== null,
						colo: req.cf.colo,
						country: req.cf.country,
						city: typeof req.cf.city,
						continent: typeof req.cf.continent,
						latitude: typeof req.cf.latitude,
						longitude: typeof req.cf.longitude,
						timezone: req.cf.timezone,
						httpProtocol: typeof req.cf.httpProtocol,
						tlsVersion: typeof req.cf.tlsVersion,
						tlsCipher: typeof req.cf.tlsCipher,
						asn: typeof req.cf.asn,
						asOrganization: typeof req.cf.asOrganization,
						requestPriority: typeof req.cf.requestPriority,
						clientTcpRtt: typeof req.cf.clientTcpRtt,
					});
				},
			};
		`,
	})
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["hasCF"] != true {
		t.Fatalf("request.cf missing: %v", got)
	}
	if got["colo"] != "DEV" || got["country"] != "XX" || got["timezone"] != "Etc/UTC" {
		t.Errorf("cf stub values = colo:%v country:%v timezone:%v", got["colo"], got["country"], got["timezone"])
	}
	for _, k := range []string{"city", "continent", "latitude", "longitude", "httpProtocol", "tlsVersion", "tlsCipher", "asOrganization", "requestPriority"} {
		if got[k] != "string" {
			t.Errorf("typeof cf.%s = %v, want string", k, got[k])
		}
	}
	for _, k := range []string{"asn", "clientTcpRtt"} {
		if got[k] != "number" {
			t.Errorf("typeof cf.%s = %v, want number", k, got[k])
		}
	}
}

// Streamed responses must not leak per-request state on the pooled instance:
// the streaming callbacks are shared per-worker (a NewFunction registration
// is permanent for the interpreter's lifetime), with a per-request sink
// cleared after serve. Previously three new functions were registered per
// streamed request (~1.8KB each, pinning that request's ResponseWriter
// forever). The bound here is generous to stay CI-safe while still failing
// on any per-request registration leak.
func TestStreamedResponsesDoNotAccumulateHostState(t *testing.T) {
	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1,
		Source: `
			export default {
				async fetch() {
					const rs = new ReadableStream({
						start(c) { c.enqueue("chunk"); c.close(); },
					});
					return new Response(rs);
				},
			};
		`,
	})
	get := func() {
		resp, err := http.Get(srv.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != "chunk" {
			t.Fatalf("body = %q", b)
		}
	}
	// Warm up (glue caches, first-request setup), then measure.
	for i := 0; i < 50; i++ {
		get()
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	const n = 400
	for i := 0; i < n; i++ {
		get()
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	perReq := int64(after.HeapAlloc-before.HeapAlloc) / n
	if perReq > 1024 {
		t.Errorf("streamed requests retain %d bytes each on the pooled instance (want ~0; leak regression)", perReq)
	}
}

// request.cf must survive the new Request(req) copy pattern (workerd keeps it).
func TestRequestCfSurvivesRequestCopy(t *testing.T) {
	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1,
		Source: `
			export default {
				async fetch(req) {
					const copy = new Request(req);
					return Response.json({ country: copy.cf?.country ?? null });
				},
			};
		`,
	})
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Country *string `json:"country"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Country == nil || *out.Country == "" {
		t.Error("request.cf lost by new Request(req) copy")
	}
}
