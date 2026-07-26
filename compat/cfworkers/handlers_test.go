package cfworkers_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goccy/go-spidermonkey/compat/cfworkers"
)

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
