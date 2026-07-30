package web_test

// EventSource is tested against a live server because everything interesting
// is in the stream: field parsing across chunk boundaries, the reconnection
// carrying Last-Event-ID, and the difference between a failed connection
// (wrong answer, no retry) and a dropped one (stream end, retry).

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

// runES evaluates script with URL_ bound and returns String(globalThis.__r).
func runES(t *testing.T, url, script string) string {
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

	full := fmt.Sprintf("globalThis.__r = %q;\nconst URL_ = %q;\n%s", "?", url, script)
	if r, err := js.Eval(context.Background(), full); err != nil {
		t.Fatalf("eval: %v", err)
	} else if r.Error != nil {
		t.Fatalf("threw: %v", r.Error)
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

func TestEventSourceStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Field parsing in one stream: comment, custom type, id, multi-line data,
		// case-sensitive field name, a value whose single leading space is syntax.
		fmt.Fprint(w, ": comment\n")
		fmt.Fprint(w, "event: greet\ndata: hi\n\n")
		fmt.Fprint(w, "id: 42\ndata:one\ndata:  two\n\n")
		fmt.Fprint(w, "Data: not-a-data-field\ndata\n\n")
	}))
	defer srv.Close()

	got := runES(t, srv.URL, `
		const es = new EventSource(URL_);
		const seen = [];
		es.addEventListener("greet", (e) => seen.push("greet:" + e.data + ":" + e.lastEventId));
		es.onmessage = (e) => seen.push("msg:" + JSON.stringify(e.data) + ":" + e.lastEventId);
		es.onerror = () => {
			// The stream ended; everything has been delivered.
			es.close();
			globalThis.__r = seen.join("|") + "|state:" + es.readyState;
		};`)
	want := `greet:hi:|msg:"one\n two":42|msg:"":42|state:2`
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestEventSourceReconnectCarriesLastEventID(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if hits.Add(1) == 1 {
			// First connection: set an id and a short retry, then end the stream.
			fmt.Fprint(w, "id: seen-1\nretry: 50\ndata: first\n\n")
			return
		}
		// The reconnection must announce where it left off.
		fmt.Fprintf(w, "data: back:%s\n\n", r.Header.Get("Last-Event-ID"))
	}))
	defer srv.Close()

	got := runES(t, srv.URL, `
		const es = new EventSource(URL_);
		const seen = [];
		es.onmessage = (e) => {
			seen.push(e.data);
			if (seen.length === 2) {
				es.close();
				globalThis.__r = seen.join("|");
			}
		};`)
	if want := "first|back:seen-1"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEventSourceFailsOnWrongAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"wrong content type", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "data: x\n\n")
		}},
		{"non-200 status", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusNoContent)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			got := runES(t, srv.URL, `
				const es = new EventSource(URL_);
				es.onopen = () => { globalThis.__r = "OPENED"; };
				es.onmessage = () => { globalThis.__r = "GOT MESSAGE"; };
				es.onerror = () => { globalThis.__r = "error, state=" + es.readyState; };`)
			// A wrong answer FAILS the connection: readyState CLOSED, no retry.
			if want := "error, state=2"; got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

func TestEventSourceConstructorRejectsBadURL(t *testing.T) {
	got := runES(t, "http://unused.invalid/", `
		try {
			new EventSource("http://this is not a url/");
			globalThis.__r = "no-throw";
		} catch (e) {
			globalThis.__r = e.name;
		}`)
	if got != "SyntaxError" {
		t.Errorf("bogus URL threw %q, want SyntaxError", got)
	}
}
