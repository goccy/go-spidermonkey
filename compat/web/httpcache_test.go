package web_test

// The HTTP cache is tested by counting what reaches the origin. That is the only
// thing a cache is for: every assertion below is about a request that did or did
// not happen, not about the bytes that came back.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// countingOrigin serves a body that never changes, with the freshness the caller
// asks for, and records every request it sees.
func countingOrigin(t *testing.T, cacheControl string) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var hits, conditional atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			conditional.Add(1)
		}
		w.Header().Set("ETag", `"v1"`)
		if cacheControl != "" {
			w.Header().Set("Cache-Control", cacheControl)
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("body"))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &conditional
}

// fetchTwice runs two fetches of the same URL under the given cache mode and
// returns what the guest saw.
func fetchTwice(t *testing.T, url, mode string) string {
	t.Helper()
	js, err := spidermonkey.New(spidermonkey.Config{
		Resolve: func(string) bool { return true },
		Dial:    func(string, string, string, int) bool { return true },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	w, err := web.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer w.Close()

	script := fmt.Sprintf(`
		globalThis.__r = "?";
		(async () => {
			const opts = %q === "" ? undefined : { cache: %q };
			// Both bodies are read. A response whose body is never read is an
			// incomplete load, and the cache does not store one — the bytes were
			// never accepted, so there is nothing to have stored.
			const a = await fetch(%q, opts);
			const abody = await a.text();
			const b = await fetch(%q, opts);
			globalThis.__r = a.status + "," + b.status + "," + (await b.text()) + "," + abody;
		})().catch((e) => { globalThis.__r = "THREW " + e.message; });
	`, mode, mode, url, url)
	if _, err := js.Eval(context.Background(), script); err != nil {
		t.Fatalf("eval: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	r, err := js.Eval(context.Background(), `String(globalThis.__r)`)
	if err != nil {
		t.Fatal(err)
	}
	return r.Value.String()
}

func TestHTTPCacheModes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cacheControl  string
		mode          string
		wantHits      int32
		wantCondition int32
		note          string
	}{
		{"fresh default serves the second from store", "max-age=300", "", 1, 0,
			"a fresh entry means the network is not touched at all"},
		{"stale default revalidates", "max-age=0", "", 2, 1,
			"a stale entry is revalidated, not refetched"},
		{"no-store never stores", "max-age=300", "no-store", 2, 0,
			"neither request may consult or write the cache"},
		{"reload always goes out unconditionally", "max-age=300", "reload", 2, 0,
			"reload skips the stored response and sends no validator"},
		{"no-cache revalidates a fresh entry", "max-age=300", "no-cache", 2, 1,
			"no-cache means do not use a stored response WITHOUT checking"},
		{"force-cache uses a stale entry", "max-age=0", "force-cache", 1, 0,
			"force-cache takes what is stored however stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits, conditional := countingOrigin(t, tc.cacheControl)
			got := fetchTwice(t, srv.URL+"/x", tc.mode)
			if got != "200,200,body,body" {
				t.Fatalf("fetch results = %q, want \"200,200,body,body\"", got)
			}
			if h := hits.Load(); h != tc.wantHits {
				t.Errorf("origin saw %d requests, want %d (%s)", h, tc.wantHits, tc.note)
			}
			if c := conditional.Load(); c != tc.wantCondition {
				t.Errorf("origin saw %d conditional requests, want %d (%s)", c, tc.wantCondition, tc.note)
			}
		})
	}
}

// TestHTTPCacheOnlyIfCached pins the one mode that turns a miss into a failure.
func TestHTTPCacheOnlyIfCached(t *testing.T) {
	srv, hits, _ := countingOrigin(t, "max-age=300")

	js, err := spidermonkey.New(spidermonkey.Config{
		Resolve: func(string) bool { return true },
		Dial:    func(string, string, string, int) bool { return true },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	w, err := web.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer w.Close()

	if _, err := js.Eval(context.Background(), fmt.Sprintf(`
		globalThis.__r = "?";
		(async () => {
			// Nothing is stored yet, so this must be a network error.
			let first = "resolved";
			try { await fetch(%q, { cache: "only-if-cached", mode: "same-origin" }); }
			catch (e) { first = "rejected"; }
			// Store it, then ask again: now it comes from the cache.
			const warm = await fetch(%q);
			await warm.text();
			const again = await fetch(%q, { cache: "only-if-cached", mode: "same-origin" });
			globalThis.__r = first + "," + again.status;
		})().catch((e) => { globalThis.__r = "THREW " + e.message; });
	`, srv.URL+"/y", srv.URL+"/y", srv.URL+"/y")); err != nil {
		t.Fatalf("eval: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	r, err := js.Eval(context.Background(), `String(globalThis.__r)`)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Value.String(); got != "rejected,200" {
		t.Errorf("only-if-cached = %q, want \"rejected,200\"", got)
	}
	// One request only: the miss never went out, and the third came from store.
	if h := hits.Load(); h != 1 {
		t.Errorf("origin saw %d requests, want 1", h)
	}
}
