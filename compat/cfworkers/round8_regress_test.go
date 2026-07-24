package cfworkers_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/cfworkers"
)

// TestCryptoKeyCrossRequestIsolation verifies a key created in one request cannot
// be addressed — via a forged globalThis.CryptoKey carrying that request's host
// handle — by a later request on the same pooled instance. Request 1 leaks its
// HMAC key handle; request 2 forges a CryptoKey around it and must fail to
// export, because the key table is cleared between requests.
func TestCryptoKeyCrossRequestIsolation(t *testing.T) {
	srv := newPoolServer(t, cfworkers.PoolConfig{
		Size: 1, // force reuse of one instance across both requests
		Source: `
			export default {
				async fetch(req, env, ctx) {
					const u = new URL(req.url);
					if (u.pathname === "/make") {
						const k = await crypto.subtle.generateKey(
							{ name: "HMAC", hash: "SHA-256" }, true, ["sign"]);
						return new Response(String(k._h));
					}
					// /steal?h=<handle>: forge a CryptoKey around the leaked handle.
					const h = Number(u.searchParams.get("h"));
					const forged = new CryptoKey("secret", true,
						{ name: "HMAC", hash: { name: "SHA-256" } }, ["sign"], h);
					try {
						await crypto.subtle.exportKey("raw", forged);
						return new Response("LEAKED");
					} catch (e) {
						return new Response("isolated");
					}
				},
			};
		`,
	})

	// Request 1: create a key and leak its host handle.
	resp1, err := http.Get(srv.URL + "/make")
	if err != nil {
		t.Fatal(err)
	}
	handle, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()

	// Request 2 (same instance): forge a CryptoKey around that handle.
	resp2, err := http.Get(srv.URL + "/steal?h=" + string(handle))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if string(got) != "isolated" {
		t.Fatalf("cross-request key access = %q, want isolated (handle %q)", got, handle)
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
