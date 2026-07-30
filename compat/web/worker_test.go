package web_test

// A Worker is a real thread, so these tests are about what crosses between two
// of them: messages both ways, a worker's own timers running while the parent
// waits, an uncaught throw arriving as an error event, and terminate actually
// ending it.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// workerServer serves each named script at /<name>.
func workerServer(t *testing.T, scripts map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		src, ok := scripts[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write([]byte(src))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runWorker evaluates script with BASE bound to the server origin, and returns
// String(globalThis.__r) plus whatever reached the configured stdout.
func runWorker(t *testing.T, base, script string) (string, string) {
	t.Helper()
	var out bytes.Buffer
	js, err := spidermonkey.New(spidermonkey.Config{
		Stdout:  &out,
		Stderr:  &out,
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

	full := fmt.Sprintf("globalThis.__r = %q;\nconst BASE = %q;\n%s", "?", base, script)
	if r, err := js.Eval(context.Background(), full); err != nil {
		t.Fatalf("eval: %v", err)
	} else if r.Error != nil {
		t.Fatalf("threw: %v", r.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	r, err := js.Eval(context.Background(), `String(globalThis.__r)`)
	if err != nil {
		t.Fatal(err)
	}
	return r.Value.String(), out.String()
}

func TestWorkerMessagesBothWays(t *testing.T) {
	srv := workerServer(t, map[string]string{
		"/echo.js": `
			onmessage = (e) => {
				postMessage("worker saw: " + e.data + " in " + (self instanceof DedicatedWorkerGlobalScope ? "a worker" : "?"));
			};`,
	})
	got, _ := runWorker(t, srv.URL, `
		const w = new Worker(BASE + "/echo.js");
		w.onmessage = (e) => { globalThis.__r = e.data + "|trusted:" + e.isTrusted; w.terminate(); };
		w.postMessage("hello");`)
	if want := "worker saw: hello in a worker|trusted:true"; got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestWorkerRunsItsOwnTimers(t *testing.T) {
	srv := workerServer(t, map[string]string{
		"/timer.js": `
			// The worker's own event loop must run its timers while the parent waits.
			let n = 0;
			const h = setInterval(() => {
				if (++n === 3) { clearInterval(h); postMessage("ticked " + n); }
			}, 5);`,
	})
	got, _ := runWorker(t, srv.URL, `
		const w = new Worker(BASE + "/timer.js");
		w.onmessage = (e) => { globalThis.__r = e.data; w.terminate(); };`)
	if want := "ticked 3"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWorkerUncaughtThrowReachesTheParent(t *testing.T) {
	srv := workerServer(t, map[string]string{
		"/boom.js": `throw new Error("worker exploded");`,
	})
	got, _ := runWorker(t, srv.URL, `
		const w = new Worker(BASE + "/boom.js");
		w.onerror = (e) => {
			e.preventDefault();
			globalThis.__r = (e instanceof ErrorEvent) + ":" + e.message;
			w.terminate();
		};`)
	if want := "true:worker exploded"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWorkerConsoleReachesTheParentStreams(t *testing.T) {
	srv := workerServer(t, map[string]string{
		"/say.js": `console.log("from the worker"); postMessage("done");`,
	})
	got, out := runWorker(t, srv.URL, `
		const w = new Worker(BASE + "/say.js");
		w.onmessage = () => { globalThis.__r = "ok"; w.terminate(); };`)
	if got != "ok" {
		t.Fatalf("worker did not report: %q", got)
	}
	if !bytes.Contains([]byte(out), []byte("from the worker")) {
		t.Errorf("the worker's console output did not reach the parent: %q", out)
	}
}

func TestWorkerMissingScriptIsAnErrorEvent(t *testing.T) {
	srv := workerServer(t, map[string]string{})
	// The constructor returns before the script is known to be missing, so the
	// failure is an event and never a throw.
	got, _ := runWorker(t, srv.URL, `
		let threw = "no";
		let w;
		try { w = new Worker(BASE + "/absent.js"); } catch (e) { threw = e.name; }
		w.onerror = () => { globalThis.__r = "error event, constructor threw: " + threw; };`)
	if want := "error event, constructor threw: no"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWorkerTerminateStopsIt(t *testing.T) {
	srv := workerServer(t, map[string]string{
		// A worker that would keep posting forever if it were not stopped.
		"/spam.js": `setInterval(() => postMessage("tick"), 2);`,
	})
	// What terminate guarantees is that the thread STOPS, not that it stops
	// between two particular messages: a message already in flight when
	// terminate is called still arrives. So the test measures quiet AFTER the
	// call — count at the moment of terminate versus count a while later.
	got, _ := runWorker(t, srv.URL, `
		const w = new Worker(BASE + "/spam.js");
		let seen = 0, atTerminate = -1;
		w.onmessage = () => {
			seen++;
			if (atTerminate < 0) {
				w.terminate();
				atTerminate = seen;
				setTimeout(() => {
					// A few late arrivals are the messages already queued; what must
					// not happen is the interval continuing to produce them.
					globalThis.__r = (seen - atTerminate <= 2) ? "quiet" : "still running: " + (seen - atTerminate);
				}, 150);
			}
		};`)
	if want := "quiet"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWorkerLocationComesFromTheRealParser(t *testing.T) {
	srv := workerServer(t, map[string]string{
		"/loc.js": `postMessage([location.protocol, location.pathname, location.search, self.name].join("|"));`,
	})
	got, _ := runWorker(t, srv.URL, `
		const w = new Worker(BASE + "/loc.js?x=1");
		w.onmessage = (e) => { globalThis.__r = e.data; w.terminate(); };`)
	if want := "http:|/loc.js|?x=1|worker-1"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
