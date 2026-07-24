package cfworkers_test

// The compat/cfworkers flagship (docs/nodejs-compat-plan.md): the real,
// unmodified Hono framework served through the instance pool behind
// net/http. Opt-in like the test262 suite: skipped unless
// examples/hono/node_modules exists (run `npm ci` in examples/hono).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/cfworkers"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
)

const honoApp = `
	import { Hono } from "hono";

	const app = new Hono();

	app.use("*", async (c, next) => {
		await next();
		c.res.headers.set("x-powered-by", "go-spidermonkey");
	});

	app.get("/", (c) => c.text("Hello Hono!"));
	app.get("/api/items/:id", (c) =>
		c.json({ id: c.req.param("id"), v: c.req.query("v") ?? null, greeting: c.env.GREETING ?? null }));
	app.post("/echo", async (c) => c.text(await c.req.text()));

	export default app;
`

func newHonoServer(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "examples", "hono")
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "hono", "package.json")); err != nil {
		t.Skip("examples/hono/node_modules not installed; run `npm ci` in examples/hono")
	}
	pool, err := cfworkers.NewPool(cfworkers.PoolConfig{
		Size:   1,
		Source: honoApp,
		Config: spidermonkey.Config{FS: os.DirFS(dir)},
		Loader: nodejs.ESMLoader,
		Env: map[string]cfworkers.Binding{
			"GREETING": cfworkers.Static("bound from Go"),
		},
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	srv := httptest.NewServer(pool)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestHonoFlagship(t *testing.T) {
	base := newHonoServer(t)

	// Root route + middleware header.
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "Hello Hono!" {
		t.Fatalf("GET / = %d %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Powered-By"); got != "go-spidermonkey" {
		t.Errorf("middleware header = %q", got)
	}

	// Path params, query params, and the Go env binding through c.env.
	resp2, err := http.Get(base + "/api/items/42?v=hi")
	if err != nil {
		t.Fatal(err)
	}
	var item struct {
		ID, V, Greeting string
	}
	if err := json.NewDecoder(resp2.Body).Decode(&item); err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if item.ID != "42" || item.V != "hi" || item.Greeting != "bound from Go" {
		t.Errorf("GET /api/items/42?v=hi = %+v", item)
	}

	// Request body round trip.
	resp3, err := http.Post(base+"/echo", "text/plain", strings.NewReader("ピンポン"))
	if err != nil {
		t.Fatal(err)
	}
	body3, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if string(body3) != "ピンポン" {
		t.Errorf("POST /echo = %q", body3)
	}

	// Hono's own 404 handling.
	resp4, err := http.Get(base + "/missing")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp4.Body)
	resp4.Body.Close()
	if resp4.StatusCode != 404 {
		t.Errorf("GET /missing = %d, want 404", resp4.StatusCode)
	}
}
