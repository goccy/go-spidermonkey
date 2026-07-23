package nodejs_test

// The Phase 4 flagship (docs/nodejs-compat-plan.md): the real, unmodified
// Express 4 framework serving HTTP through node:http over Go's net/http.
// This exercises the deepest surface yet: ~30 transitive npm packages,
// streams (body parsing), node:crypto (ETag sha1), the full CJS resolver.
// Opt-in: skipped unless examples/nodejs/node_modules has express.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

const expressApp = `
	const express = require("express");
	const app = express();

	app.use(express.json());
	app.use((req, res, next) => {
		res.set("X-Middleware", "ran");
		next();
	});

	app.get("/", (req, res) => {
		res.send("Hello Express!");
	});
	app.get("/users/:id", (req, res) => {
		res.json({ id: req.params.id, v: req.query.v ?? null, ip: typeof req.ip });
	});
	app.post("/echo", (req, res) => {
		res.status(201).json({ got: req.body });
	});
	app.get("/boom", (req, res) => {
		throw new Error("route exploded");
	});
	app.use((req, res) => {
		res.status(404).send("no such route");
	});

	const server = app.listen(0);
	globalThis.__server = server;
	globalThis.PORT = server.address().port;
`

func TestExpressFlagship(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "nodejs")
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "express", "package.json")); err != nil {
		t.Skip("examples/nodejs/node_modules not installed; run `npm ci` in examples/nodejs")
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: os.DirFS(dir)})

	port, _ := startServer(t, js, rt, expressApp)
	base := "http://127.0.0.1:" + port

	// Root route: body, Express headers, middleware header, ETag.
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "Hello Express!" {
		t.Fatalf("GET / = %d %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Powered-By"); got != "Express" {
		t.Errorf("X-Powered-By = %q", got)
	}
	if got := resp.Header.Get("X-Middleware"); got != "ran" {
		t.Errorf("middleware header = %q", got)
	}
	if got := resp.Header.Get("ETag"); got == "" {
		t.Error("no ETag header (node:crypto sha1 path)")
	}
	if got := resp.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}

	// Route params + query string.
	resp2, err := http.Get(base + "/users/42?v=hello")
	if err != nil {
		t.Fatal(err)
	}
	var user struct {
		ID, V, IP string
	}
	if err := json.NewDecoder(resp2.Body).Decode(&user); err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if user.ID != "42" || user.V != "hello" {
		t.Errorf("GET /users/42?v=hello = %+v", user)
	}

	// JSON body parsing (body-parser: streams + raw-body + iconv-lite).
	payload := `{"name":"ノード","n":7,"nested":{"ok":true}}`
	resp3, err := http.Post(base+"/echo", "application/json", bytes.NewReader([]byte(payload)))
	if err != nil {
		t.Fatal(err)
	}
	body3, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != 201 {
		t.Fatalf("POST /echo = %d %s", resp3.StatusCode, body3)
	}
	var echo struct {
		Got struct {
			Name   string `json:"name"`
			N      int    `json:"n"`
			Nested struct {
				OK bool `json:"ok"`
			} `json:"nested"`
		} `json:"got"`
	}
	if err := json.Unmarshal(body3, &echo); err != nil {
		t.Fatalf("bad echo body %s: %v", body3, err)
	}
	if echo.Got.Name != "ノード" || echo.Got.N != 7 || !echo.Got.Nested.OK {
		t.Errorf("echo = %+v", echo.Got)
	}

	// A throwing route: Express's error handler answers 500.
	resp4, err := http.Get(base + "/boom")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp4.Body)
	resp4.Body.Close()
	if resp4.StatusCode != 500 {
		t.Errorf("GET /boom = %d, want 500", resp4.StatusCode)
	}

	// The app's own 404 handler.
	resp5, err := http.Get(base + "/missing")
	if err != nil {
		t.Fatal(err)
	}
	body5, _ := io.ReadAll(resp5.Body)
	resp5.Body.Close()
	if resp5.StatusCode != 404 || string(body5) != "no such route" {
		t.Errorf("GET /missing = %d %q", resp5.StatusCode, body5)
	}
}

func TestExpressConditionalGET(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "nodejs")
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "express", "package.json")); err != nil {
		t.Skip("examples/nodejs/node_modules not installed; run `npm ci` in examples/nodejs")
	}
	js, rt := newRuntime(t, spidermonkey.Config{FS: os.DirFS(dir)})

	port, _ := startServer(t, js, rt, `
		const express = require("express");
		const app = express();
		app.get("/data", (req, res) => res.send("cacheable payload"));
		const server = app.listen(0);
		globalThis.__server = server;
		globalThis.PORT = server.address().port;
	`)
	base := "http://127.0.0.1:" + port

	resp, err := http.Get(base + "/data")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on first response")
	}

	// If-None-Match with the ETag → 304 (the fresh/etag pipeline end-to-end).
	req, _ := http.NewRequest("GET", base+"/data", nil)
	req.Header.Set("If-None-Match", etag)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Errorf("conditional GET = %d, want 304", resp2.StatusCode)
	}
	_ = spidermonkey.Config{}
}
