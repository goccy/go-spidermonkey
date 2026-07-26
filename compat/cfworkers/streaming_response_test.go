package cfworkers_test

// Streaming response delivery: a worker Response wrapping a ReadableStream
// must reach the client chunk-by-chunk (previously the whole body was
// buffered before the first byte went out), and a fetch() Response returned
// straight through (reverse proxy) must stream the upstream body. Also covers
// the request.cf local-dev stub.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/cfworkers"
)

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
