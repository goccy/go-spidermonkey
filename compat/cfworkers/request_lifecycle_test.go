package cfworkers_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/cfworkers"
)

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
