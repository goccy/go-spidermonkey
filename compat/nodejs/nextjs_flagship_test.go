package nodejs_test

// The stretch flagship: an unmodified Next.js 15 App Router production server
// running inside go-spidermonkey. The app is BUILT on the host with real Node
// (`make setup && next build` in examples/nextjs — SWC natives are build-time
// only); the engine runs the production server via the custom-server API.
// Opt-in: skipped unless examples/nextjs has node_modules AND .next.
//
// Prerendered pages, Route Handlers, static assets and Next's own 404 all work.
// DYNAMIC SSR does not, for an engine-side reason recorded at the end of this
// test and in docs/engine-followups.md item 8.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// syncBuffer: guest writes land from the loop goroutine while the test reads
// from its own.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const nextServerScript = `
	process.env.NODE_ENV = "production";
	globalThis.__boot = "requiring";
	const http = require("http");
	const next = require("next");
	globalThis.__boot = "creating";
	const app = next({ dev: false, dir: "/" });
	const handle = app.getRequestHandler();
	globalThis.__boot = "preparing";
	app.prepare().then(() => {
		const server = http.createServer((req, res) => {
			Promise.resolve(handle(req, res)).catch((e) => {
				if (!res.headersSent) { res.statusCode = 500; }
				try { res.end("handler error: " + (e && e.message)); } catch {}
			});
		});
		server.listen(0);
		globalThis.__server = server;
		globalThis.PORT = server.address().port;
		globalThis.__boot = "ready";
	}).catch((e) => {
		globalThis.__bootErr = e instanceof Error ? e.name + ": " + e.message + "\n" + (e.stack || "") : String(e);
	});
`

func TestNextJSFlagship(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "nextjs")
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "next", "package.json")); err != nil {
		t.Skip("examples/nextjs/node_modules not installed; run `npm ci` in examples/nextjs")
	}
	if _, err := os.Stat(filepath.Join(dir, ".next", "BUILD_ID")); err != nil {
		t.Skip("examples/nextjs/.next missing; run `npm run build` in examples/nextjs")
	}
	var stderr, stdout syncBuffer
	js, rt := newRuntime(t, spidermonkey.Config{
		FS:             os.DirFS(dir),
		Env:            []string{"NODE_ENV=production"},
		MaxMemoryBytes: 2 << 30,
		Stdout:         &stdout,
		Stderr:         &stderr,
	})
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("guest stdout:\n%s", stdout.String())
			t.Logf("guest stderr:\n%s", stderr.String())
		}
	})

	ctx := context.Background()
	r, err := js.Eval(ctx, nextServerScript)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("server script threw (boot stage %s): %v", evalStr(t, js, `String(globalThis.__boot)`), r.Error)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- rt.Wait(context.Background()) }()
	t.Cleanup(func() {
		js.Eval(context.Background(), `if (globalThis.__server) __server.close()`)
		select {
		case <-waitDone:
		case <-time.After(15 * time.Second):
			t.Error("event loop did not stop after server.close()")
		}
	})

	// app.prepare() may need loop turns; poll for readiness.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if bootErr := evalStr(t, js, `globalThis.__bootErr ?? ""`); bootErr != "" {
			t.Fatalf("next boot failed: %s", bootErr)
		}
		if evalStr(t, js, `String(globalThis.PORT ?? "")`) != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("next server did not become ready (stage %s)", evalStr(t, js, `String(globalThis.__boot)`))
		}
		time.Sleep(100 * time.Millisecond)
	}
	base := "http://127.0.0.1:" + evalStr(t, js, `String(PORT)`)

	client := &http.Client{Timeout: 20 * time.Second}
	get := func(path string) (int, string, http.Header) {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v (stderr so far:\n%s)", path, err, stderr.String())
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(body), resp.Header
	}

	// Statically prerendered Server Component route. This is the App Router
	// serving a real build: routing, the prerender manifest, and the RSC
	// payload alongside the HTML.
	status2, body2, headers := get("/about")
	if status2 != 200 || !strings.Contains(body2, "Statically generated About (App Router)") {
		t.Errorf("GET /about = %d: %.300s", status2, body2)
	}
	// A "use client" boundary means Next ships a client chunk for hydration.
	if !strings.Contains(body2, "/_next/static/chunks/") {
		t.Errorf("no client chunk referenced — the page was not hydratable")
	}
	if got := headers.Get("X-Powered-By"); got != "Next.js" {
		t.Errorf("X-Powered-By = %q", got)
	}

	// Route Handler (App Router API).
	status3, body3, _ := get("/api/hello")
	if status3 != 200 || !strings.Contains(body3, `"hello":"from route handler"`) {
		t.Errorf("GET /api/hello = %d: %s", status3, body3)
	}

	// A built static asset out of .next/static.
	if i := strings.Index(body2, "/_next/static/"); i >= 0 {
		end := strings.IndexAny(body2[i:], `"'`)
		asset := body2[i : i+end]
		status4, body4, _ := get(asset)
		if status4 != 200 || len(body4) == 0 {
			t.Errorf("GET %s = %d: %.600s", asset, status4, body4)
		}
	} else {
		t.Error("no /_next/static asset referenced in the prerendered body")
	}

	// Dynamic SSR — the Server Component that runs per request — does NOT work
	// on Next.js 15, and the reason is engine-side. Next enters its request
	// store with
	//
	//	workUnitAsyncStorage.run(store, renderToReadableStream, …)
	//
	// where renderToReadableStream returns a stream SYNCHRONOUSLY and the render
	// happens later, as the stream is pulled. Without engine async-context hooks
	// an AsyncLocalStorage store cannot survive into those continuations (see
	// docs/engine-followups.md item 8), so the store is already gone by the time
	// React asks for it and every dynamic render 500s.
	//
	// Asserted rather than skipped, so that the day the engine gains those hooks
	// this test fails and says so instead of quietly staying disabled.
	statusDyn, _, _ := get("/")
	if statusDyn != 500 {
		t.Errorf("GET / = %d, want 500 while AsyncLocalStorage cannot cross an "+
			"async boundary. If this now works, engine async-context hooks have "+
			"landed: restore the dynamic-SSR assertions and close item 8.", statusDyn)
	}

	// Next's own 404.
	status5, _, _ := get("/definitely-missing")
	if status5 != 404 {
		t.Errorf("GET /definitely-missing = %d, want 404", status5)
	}
}
