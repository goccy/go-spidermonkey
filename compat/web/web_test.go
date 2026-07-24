package web_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

func newWeb(t *testing.T, cfg spidermonkey.Config) (*spidermonkey.JS, *web.Web) {
	t.Helper()
	// fetch's Resolve/Dial hooks are fail-closed (nil denies). Default the
	// unset ones to allow-all so a test exercising one hook (or none) still
	// reaches the network; a test verifying denial sets its own hook.
	if cfg.Dial == nil {
		cfg.Dial = func(network, host, ip string, port int) bool { return true }
	}
	if cfg.Resolve == nil {
		cfg.Resolve = func(host string) bool { return true }
	}
	js, err := spidermonkey.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { js.Close() })
	w, err := web.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	return js, w
}

func eval(t *testing.T, js *spidermonkey.JS, src string) spidermonkey.Value {
	t.Helper()
	r, err := js.Eval(context.Background(), src)
	if err != nil {
		t.Fatalf("Eval(%s): %v", src, err)
	}
	if r.Error != nil {
		t.Fatalf("Eval(%s) threw: %v", src, r.Error)
	}
	return r.Value
}

func evalString(t *testing.T, js *spidermonkey.JS, src string) string {
	t.Helper()
	return eval(t, js, src).String()
}

func TestTextEncoderDecoderRoundTrip(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	if got := evalString(t, js, `
		const enc = new TextEncoder();
		const dec = new TextDecoder();
		["hello", "こんにちは", "🎉🚀", "aé中😀z"]
			.map(s => dec.decode(enc.encode(s)) === s).join(",")
	`); got != "true,true,true,true" {
		t.Errorf("round trips = %s", got)
	}
	// Known byte sequences.
	if got := evalString(t, js, `Array.from(new TextEncoder().encode("é€😀")).join(",")`); got != "195,169,226,130,172,240,159,152,128" {
		t.Errorf("encode bytes = %s", got)
	}
	// A lone surrogate encodes as U+FFFD per spec.
	if got := evalString(t, js, `Array.from(new TextEncoder().encode("\ud800")).join(",")`); got != "239,191,189" {
		t.Errorf("lone surrogate bytes = %s", got)
	}
	// Invalid UTF-8 decodes to replacement chars (and throws in fatal mode).
	if got := evalString(t, js, `new TextDecoder().decode(new Uint8Array([0x61, 0xff, 0x62]))`); got != "a�b" {
		t.Errorf("invalid decode = %q", got)
	}
	if got := evalString(t, js, `
		(() => { try { new TextDecoder("utf-8", { fatal: true }).decode(new Uint8Array([0xff])); return "no-throw"; }
		catch (e) { return e.constructor.name; } })()
	`); got != "TypeError" {
		t.Errorf("fatal decode: %s, want TypeError", got)
	}
}

func TestBase64(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	if got := evalString(t, js, `btoa("hello")`); got != "aGVsbG8=" {
		t.Errorf("btoa = %q", got)
	}
	if got := evalString(t, js, `atob("aGVsbG8=")`); got != "hello" {
		t.Errorf("atob = %q", got)
	}
	// Compare guest-side: a string containing NUL cannot cross the bridge
	// intact (known engine issue), so return the verdict, not the data.
	if got := evalString(t, js, `String(atob(btoa("\x00\xff binary!")) === "\x00\xff binary!")`); got != "true" {
		t.Errorf("binary round trip = %s", got)
	}
	if got := evalString(t, js, `
		(() => { try { btoa("日本語"); return "no-throw"; } catch (e) { return e.name; } })()
	`); got != "InvalidCharacterError" {
		t.Errorf("btoa out of range: %s", got)
	}
	if got := evalString(t, js, `
		(() => { try { atob("!!!"); return "no-throw"; } catch (e) { return e.name; } })()
	`); got != "InvalidCharacterError" {
		t.Errorf("atob invalid: %s", got)
	}
}

func TestURL(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	if got := evalString(t, js, `
		const u = new URL("https://user:pw@Example.COM:8080/a/b/../c?x=1&y=2#frag");
		[u.protocol, u.hostname, u.port, u.host, u.pathname, u.search, u.hash, u.username, u.password, u.origin].join("|")
	`); got != "https:|example.com|8080|example.com:8080|/a/c|?x=1&y=2|#frag|user|pw|https://example.com:8080" {
		t.Errorf("parse = %s", got)
	}
	// Default port is dropped; empty path becomes "/".
	if got := evalString(t, js, `new URL("https://example.com:443").href`); got != "https://example.com/" {
		t.Errorf("default port = %s", got)
	}
	// Base resolution.
	for _, tc := range [][2]string{
		{`new URL("d", "https://h/a/b/c").href`, "https://h/a/b/d"},
		{`new URL("../d", "https://h/a/b/c").href`, "https://h/a/d"},
		{`new URL("/d?q=1", "https://h/a/b/c?x=2#f").href`, "https://h/d?q=1"},
		{`new URL("?q=1", "https://h/a/b").href`, "https://h/a/b?q=1"},
		{`new URL("#s", "https://h/a?x=1").href`, "https://h/a?x=1#s"},
		{`new URL("//other.io/p", "https://h/a").href`, "https://other.io/p"},
	} {
		if got := evalString(t, js, tc[0]); got != tc[1] {
			t.Errorf("%s = %s, want %s", tc[0], got, tc[1])
		}
	}
	if got := evalString(t, js, `[URL.canParse("https://ok/"), URL.canParse("::nope::")].join(",")`); got != "true,false" {
		t.Errorf("canParse = %s", got)
	}
	if got := evalString(t, js, `new URL("http://[::1]:8080/x").host`); got != "[::1]:8080" {
		t.Errorf("ipv6 host = %s", got)
	}
}

func TestURLSearchParams(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	if got := evalString(t, js, `
		const p = new URLSearchParams("a=1&b=2&a=3");
		[p.get("a"), p.getAll("a").join("+"), p.has("b"), String(p.get("zz"))].join("|")
	`); got != "1|1+3|true|null" {
		t.Errorf("parse = %s", got)
	}
	if got := evalString(t, js, `
		const q = new URLSearchParams();
		q.append("k", "v 1");
		q.append("k", "v2");
		q.set("k", "one");
		q.append("z", "&=?");
		q.toString()
	`); got != "k=one&z=%26%3D%3F" {
		t.Errorf("mutate/encode = %s", got)
	}
	if got := evalString(t, js, `new URLSearchParams("a=x+y%21").get("a")`); got != "x y!" {
		t.Errorf("decode = %q", got)
	}
	// searchParams stays linked to its URL.
	if got := evalString(t, js, `
		const u2 = new URL("https://h/p?a=1");
		u2.searchParams.set("a", "9");
		u2.searchParams.append("b", "2");
		u2.href
	`); got != "https://h/p?a=9&b=2" {
		t.Errorf("linked params = %s", got)
	}
}

func TestCrypto(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	uuid := evalString(t, js, `crypto.randomUUID()`)
	if ok, _ := regexp.MatchString(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, uuid); !ok {
		t.Errorf("randomUUID = %q, not a v4 UUID", uuid)
	}
	if uuid2 := evalString(t, js, `crypto.randomUUID()`); uuid2 == uuid {
		t.Errorf("two randomUUID calls returned the same value %q", uuid)
	}
	if got := evalString(t, js, `
		const arr = new Uint8Array(64);
		const same = crypto.getRandomValues(arr) === arr;
		[same, arr.some(b => b !== 0)].join(",")
	`); got != "true,true" {
		t.Errorf("getRandomValues = %s", got)
	}
	if got := evalString(t, js, `
		(() => { try { crypto.getRandomValues(new Uint8Array(70000)); return "no-throw"; } catch (e) { return e.name; } })()
	`); got != "QuotaExceededError" {
		t.Errorf("quota = %s", got)
	}
}

func TestConsole(t *testing.T) {
	var stdout, stderr bytes.Buffer
	js, _ := newWeb(t, spidermonkey.Config{Stdout: &stdout, Stderr: &stderr})

	eval(t, js, `
		console.log("hello", 42, { a: 1, b: [true, null] });
		console.error("boom");
		console.warn("careful");
	`)
	if got, want := stdout.String(), "hello 42 { a: 1, b: [ true, null ] }\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if got, want := stderr.String(), "boom\ncareful\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

func TestSmallGlobals(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	// structuredClone: deep copy, JSON-limited.
	if got := evalString(t, js, `
		const orig = { a: 1, nested: { b: [1, 2] } };
		const c = structuredClone(orig);
		c.nested.b.push(3);
		[orig.nested.b.length, c.nested.b.length, c.a].join(",")
	`); got != "2,3,1" {
		t.Errorf("structuredClone = %s", got)
	}
	// queueMicrotask runs before Eval returns (Eval drains microtasks).
	if got := evalString(t, js, `
		globalThis.micro = [];
		queueMicrotask(() => micro.push("task"));
		micro.push("sync");
		micro.join(",")
	`); got != "sync" {
		t.Errorf("microtask ran synchronously: %s", got)
	}
	if got := evalString(t, js, `micro.join(",")`); got != "sync,task" {
		t.Errorf("microtask did not run: %s", got)
	}
	// performance.now is monotonic non-negative.
	if got := evalString(t, js, `
		const t0 = performance.now();
		const t1 = performance.now();
		[typeof t0 === "number", t0 >= 0, t1 >= t0].join(",")
	`); got != "true,true,true" {
		t.Errorf("performance.now = %s", got)
	}
}

func TestAbortController(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	if got := evalString(t, js, `
		const c = new AbortController();
		const seen = [];
		c.signal.addEventListener("abort", () => seen.push("listener"));
		c.signal.onabort = () => seen.push("onabort");
		const before = c.signal.aborted;
		c.abort();
		c.abort(); // second abort is a no-op
		[before, c.signal.aborted, c.signal.reason.name, seen.join("+")].join("|")
	`); got != "false|true|AbortError|onabort+listener" {
		t.Errorf("abort = %s", got)
	}
	if got := evalString(t, js, `AbortSignal.abort("why").reason`); got != "why" {
		t.Errorf("AbortSignal.abort reason = %q", got)
	}
}

func TestTimersOrderAndWait(t *testing.T) {
	js, w := newWeb(t, spidermonkey.Config{})

	eval(t, js, `
		globalThis.order = [];
		setTimeout(() => order.push("late"), 30);
		setTimeout(() => order.push("early"), 5);
		setTimeout(() => order.push("zero"));
	`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalString(t, js, `order.join(",")`); got != "zero,early,late" {
		t.Errorf("order = %s", got)
	}
}

func TestTimersIntervalAndClear(t *testing.T) {
	js, w := newWeb(t, spidermonkey.Config{})

	eval(t, js, `
		globalThis.n = 0;
		const id = setInterval(() => { n++; if (n === 3) clearInterval(id); }, 5);
		globalThis.cancelled = false;
		const dead = setTimeout(() => { cancelled = true; }, 10);
		clearTimeout(dead);
	`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := eval(t, js, `n`).Int(); got != 3 {
		t.Errorf("interval ran %d times, want 3", got)
	}
	if eval(t, js, `cancelled`).Bool() {
		t.Error("cleared timeout still fired")
	}
}

func TestTimersNestedAndArgs(t *testing.T) {
	js, w := newWeb(t, spidermonkey.Config{})

	eval(t, js, `
		globalThis.got = [];
		setTimeout((a, b) => {
			got.push(a + b);
			setTimeout(x => got.push(x), 5, "nested");
		}, 5, "out", "er");
	`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalString(t, js, `got.join(",")`); got != "outer,nested" {
		t.Errorf("got = %s", got)
	}
}

func TestTimerPromiseIntegration(t *testing.T) {
	js, w := newWeb(t, spidermonkey.Config{})

	// A promise resolved from a timer: microtasks must drain after the
	// callback so dependent .then chains complete during Wait.
	eval(t, js, `
		globalThis.result = "";
		const sleep = (ms) => new Promise((res) => setTimeout(res, ms));
		(async () => {
			await sleep(5);
			result += "a";
			await sleep(5);
			result += "b";
		})();
	`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := evalString(t, js, `result`); got != "ab" {
		t.Errorf("result = %q, want \"ab\"", got)
	}
}

func TestTimerCallbackThrowStopsLoop(t *testing.T) {
	js, w := newWeb(t, spidermonkey.Config{})

	eval(t, js, `setTimeout(() => { throw new Error("boom in timer"); }, 1);`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := w.Wait(ctx)
	if err == nil {
		t.Fatal("Wait returned nil although a timer callback threw")
	}
	if !strings.Contains(err.Error(), "boom in timer") {
		t.Errorf("err = %v, want the thrown message", err)
	}
}

func TestWaitIdleReturnsImmediately(t *testing.T) {
	_, w := newWeb(t, spidermonkey.Config{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Error("Wait on an idle loop did not return promptly")
	}
}

func newJSONServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"web","n":7}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchJSONWithDialAllowList(t *testing.T) {
	srv := newJSONServer(t)
	var dialed []string
	js, _ := newWeb(t, spidermonkey.Config{
		Dial: func(network, host, ip string, port int) bool {
			dialed = append(dialed, fmt.Sprintf("%s/%s:%d", network, ip, port))
			return ip == "127.0.0.1"
		},
	})
	eval(t, js, `globalThis.BASE = `+fmt.Sprintf("%q", srv.URL))

	eval(t, js, `
		globalThis.__r = {};
		fetch(BASE + "/json")
			.then(res => { __r.status = res.status; __r.ct = res.headers.get("Content-Type"); return res.json(); })
			.then(j => { __r.name = j.name; __r.n = j.n; })
			.catch(e => { __r.err = String(e); });
	`)
	if got := evalString(t, js, `__r.err ?? ""`); got != "" {
		t.Fatalf("fetch rejected: %s", got)
	}
	if got := eval(t, js, `__r.status`).Int(); got != 200 {
		t.Errorf("status = %d", got)
	}
	if got := evalString(t, js, `__r.name`); got != "web" {
		t.Errorf("name = %q", got)
	}
	if got := eval(t, js, `__r.n`).Int(); got != 7 {
		t.Errorf("n = %d", got)
	}
	if len(dialed) == 0 {
		t.Error("Dial hook was never consulted")
	}
}

func TestFetchDialReceivesHost(t *testing.T) {
	// A localhost server addressed by name: the Dial hook must receive the
	// requested host ("localhost") alongside the resolved IP, so a
	// host-scoped policy ("localhost only on this port") can be enforced.
	srv := newJSONServer(t)
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	var gotHost, gotIP string
	var gotPort int
	js, _ := newWeb(t, spidermonkey.Config{
		Dial: func(network, host, ip string, p int) bool {
			gotHost, gotIP, gotPort = host, ip, p
			// Host-scoped rule: allow only "localhost".
			return host == "localhost"
		},
	})
	eval(t, js, `globalThis.BASE = "http://localhost:`+port+`"`)
	eval(t, js, `
		globalThis.__r = {};
		fetch(BASE + "/json").then(res => res.json()).then(j => { __r.name = j.name; })
			.catch(e => { __r.err = String(e); });
	`)
	if got := evalString(t, js, `__r.err ?? ""`); got != "" {
		t.Fatalf("host-allowed fetch rejected: %s", got)
	}
	if gotHost != "localhost" {
		t.Errorf("Dial host = %q, want localhost", gotHost)
	}
	if gotIP == "" || gotIP == "localhost" {
		t.Errorf("Dial ip = %q, want a resolved IP distinct from the host", gotIP)
	}
	wantPort, _ := strconv.Atoi(port)
	if gotPort != wantPort {
		t.Errorf("Dial port = %d, want %d", gotPort, wantPort)
	}
}

func TestFetchDialDenied(t *testing.T) {
	srv := newJSONServer(t)
	js, _ := newWeb(t, spidermonkey.Config{
		Dial: func(network, host, ip string, port int) bool { return false },
	})
	eval(t, js, `globalThis.BASE = `+fmt.Sprintf("%q", srv.URL))

	eval(t, js, `
		globalThis.__r = {};
		fetch(BASE + "/json").then(() => { __r.resolved = true; }).catch(e => { __r.err = String(e); });
	`)
	if eval(t, js, `__r.resolved === true`).Bool() {
		t.Fatal("fetch resolved although Dial denied the connection")
	}
	if got := evalString(t, js, `__r.err ?? ""`); !strings.Contains(got, "permission denied") {
		t.Errorf("rejection = %q, want a permission-denied error", got)
	}
}

func TestFetchResolveHook(t *testing.T) {
	srv := newJSONServer(t)
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]

	var resolved []string
	allow := true
	js, _ := newWeb(t, spidermonkey.Config{
		Resolve: func(host string) bool {
			resolved = append(resolved, host)
			return allow
		},
	})
	eval(t, js, `globalThis.BASE = "http://localhost:`+port+`"`)

	eval(t, js, `
		globalThis.__r = {};
		fetch(BASE + "/json").then(res => res.json()).then(j => { __r.name = j.name; })
			.catch(e => { __r.err = String(e); });
	`)
	if got := evalString(t, js, `__r.err ?? ""`); got != "" {
		t.Fatalf("allowed fetch rejected: %s", got)
	}
	if got := evalString(t, js, `__r.name`); got != "web" {
		t.Errorf("name = %q", got)
	}
	if len(resolved) == 0 || resolved[0] != "localhost" {
		t.Fatalf("Resolve consulted with %v, want [localhost]", resolved)
	}

	allow = false
	eval(t, js, `
		globalThis.__r2 = {};
		fetch(BASE + "/json").then(() => { __r2.resolved = true; }).catch(e => { __r2.err = String(e); });
	`)
	if eval(t, js, `__r2.resolved === true`).Bool() {
		t.Fatal("fetch resolved although Resolve denied the lookup")
	}
	if got := evalString(t, js, `__r2.err ?? ""`); !strings.Contains(got, "permission denied") {
		t.Errorf("rejection = %q, want a permission-denied error", got)
	}
}

func TestFetchPreAbortedSignalRejects(t *testing.T) {
	srv := newJSONServer(t)
	js, _ := newWeb(t, spidermonkey.Config{})
	eval(t, js, `globalThis.BASE = `+fmt.Sprintf("%q", srv.URL))

	eval(t, js, `
		globalThis.__r = {};
		const c = new AbortController();
		c.abort();
		fetch(BASE + "/json", { signal: c.signal })
			.then(() => { __r.resolved = true; })
			.catch(e => { __r.err = String(e); });
	`)
	if eval(t, js, `__r.resolved === true`).Bool() {
		t.Fatal("fetch resolved although the signal was already aborted")
	}
	if got := evalString(t, js, `__r.err ?? ""`); got == "" {
		t.Error("no rejection recorded for the aborted fetch")
	}
}

func TestURLAgainstGoReference(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	// Cross-check a handful of URLs against net/url where the models agree.
	for _, raw := range []string{
		"https://example.com/a/b?x=1#f",
		"http://example.com:9090/",
		"https://u:p@h.io/path",
	} {
		ref, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		got := evalString(t, js, fmt.Sprintf(`new URL(%q).pathname`, raw))
		if got != ref.Path {
			t.Errorf("pathname(%s) = %q, want %q", raw, got, ref.Path)
		}
	}
}
