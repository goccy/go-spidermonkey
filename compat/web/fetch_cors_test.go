package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// Nothing enforced the cross-origin rules: every fetch resolved regardless of
// mode or of what the server permitted, so a page could read a response it has
// no right to. This pins the four outcomes the Fetch spec defines, plus the
// two headers a user agent is required to send and did not.
func TestFetchCrossOriginRules(t *testing.T) {
	var gotOrigin, gotReferer, gotUA string
	mux := http.NewServeMux()
	mux.HandleFunc("/open", func(w http.ResponseWriter, r *http.Request) {
		gotOrigin, gotReferer, gotUA = r.Header.Get("Origin"), r.Header.Get("Referer"), r.Header.Get("User-Agent")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write([]byte("open"))
	})
	mux.HandleFunc("/closed", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("closed"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	js, err := spidermonkey.New(spidermonkey.Config{
		Resolve: func(host string) bool { return true },
		Dial:    func(network, host, ip string, port int) bool { return true },
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

	// The environment's own URL. Everything below is cross-origin to it, since
	// the test server is on its own port.
	if _, err := js.Eval(context.Background(), `globalThis.location = { href: "http://example.test/page/index.html", origin: "http://example.test" };`); err != nil {
		t.Fatal(err)
	}

	if _, err := js.Eval(context.Background(), `
		globalThis.__done = (async () => {
			const base = "`+srv.URL+`";
			const out = [];
			const step = async (name, f) => {
				try { out.push(name + "=" + await f()); }
				catch (e) { out.push(name + "!" + (e && e.name ? e.name : String(e))); }
			};
			// A server that permits the origin: the response is readable and its
			// type says it came through CORS.
			await step("cors-allowed", async () => {
				const r = await fetch(base + "/open");
				return r.type + ":" + r.status + ":" + (await r.text());
			});
			// A server that does not: the fetch REJECTS, it does not resolve with
			// an unreadable response.
			await step("cors-denied", async () => {
				const r = await fetch(base + "/closed");
				return "READ:" + (await r.text());
			});
			// no-cors is allowed to make the request but not to see the answer.
			await step("no-cors", async () => {
				const r = await fetch(base + "/closed", { mode: "no-cors" });
				return r.type + ":" + r.status + ":" + ((await r.text()) === "" ? "empty" : "LEAKED");
			});
			// same-origin refuses before the request is made.
			await step("same-origin", async () => {
				const r = await fetch(base + "/open", { mode: "same-origin" });
				return "SENT";
			});
			// A data: URL is fetched by its own scheme handler, not through CORS.
			// Its origin is "null", so judging it by origin rejects every one.
			await step("data-url", async () => {
				const r = await fetch("data:text/plain,hi");
				return r.status + ":" + (await r.text());
			});
			globalThis.__r = out.join(" | ");
		})();
	`); err != nil {
		t.Fatalf("eval: %v", err)
	}
	if err := w.Wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}

	got := evalString(t, js, `String(globalThis.__r)`)
	want := "cors-allowed=cors:200:open | cors-denied!TypeError | no-cors=opaque:0:empty | same-origin!TypeError | data-url=200:hi"
	if got != want {
		t.Errorf("cross-origin rules =\n %s\nwant\n %s", got, want)
	}

	// A user agent states who it is and where it came from.
	if gotOrigin != "http://example.test" {
		t.Errorf("Origin = %q, want http://example.test", gotOrigin)
	}
	// Cross-origin under the default policy: the origin only, never the path.
	// It is written as a URL, so the origin carries its root path — which is
	// exactly what the suite compares against.
	if gotReferer != "http://example.test/" {
		t.Errorf("Referer = %q, want the bare origin (strict-origin-when-cross-origin)", gotReferer)
	}
	if gotUA == "" {
		t.Error("User-Agent was not sent")
	}
}
