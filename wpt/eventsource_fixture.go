package wpt

// eventsource_fixture.go: the eventsource/ fixtures the .any.js tests use,
// ported from eventsource/resources/*.py. The cookie-based ones
// (status-reconnect.py, reconnect-fail.py) are used only by .window.js tests
// and need a cookie jar the runtime does not have; they stay unported until
// that surface exists.

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// esMessageHandler ports message.py: one response whose MIME type, payload and
// terminator the query string chooses. It is the whole parsing corpus's
// fixture — every format-*.any.js drives the parser through it.
func esMessageHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	mimeType := q.Get("mime")
	if mimeType == "" {
		mimeType = "text/event-stream"
	}
	message := "data: data"
	if v, ok := q["message"]; ok && len(v) > 0 {
		message = v[0]
	}
	newline := "\n\n"
	if q.Get("newline") == "none" {
		newline = ""
	}
	if s, err := strconv.Atoi(q.Get("sleep")); err == nil && s > 0 {
		time.Sleep(time.Duration(s) * time.Millisecond)
	}
	// Set verbatim, not via textproto canonicalization: a deliberately bogus
	// MIME type ("x bogus") is the test's whole point.
	w.Header()["Content-Type"] = []string{mimeType}
	_, _ = w.Write([]byte(message + newline + "\n"))
	return true
}

// esMessage2Handler ports message2.py: an endless stream that repeats a block
// of well-formed and deliberately malformed fields every two seconds. The
// test reads the first few events and closes; the handler just has to keep
// the stream alive (and flushed — an unflushed infinite stream delivers
// nothing at all) until the client goes away.
func esMessage2Handler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	for {
		_, err := fmt.Fprint(w, "data:msg\ndata: msg\n\n:\nfalsefield:msg\n\nfalsefield:msg\nData:data\n\ndata\n\ndata:end\n\n")
		if err != nil {
			return true
		}
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			return true
		case <-time.After(2 * time.Second):
		}
	}
}

// esLastEventIDHandler ports last-event-id.py: the first request is told an id
// (U+2026 by default — deliberately non-ASCII) and a short retry; the
// reconnection must carry that id back, and gets it echoed as data.
func esLastEventIDHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Content-Type", "text/event-stream")
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", last)
		return true
	}
	idvalue := r.URL.Query().Get("idvalue")
	if idvalue == "" {
		idvalue = "\u2026"
	}
	_, _ = fmt.Fprintf(w, "id: %s\nretry: 200\ndata: hello\n\n", idvalue)
	return true
}

// esCORSHandler ports cors.py: it wraps another fixture with the CORS headers
// the query names. Only the delegates the .any.js corpus reaches are wired;
// the rest (status-reconnect, redirect) belong to .window.js tests that also
// need a cookie jar, and fall through as not-found so their absence is loud.
func esCORSHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	origin := r.URL.Query().Get("origin")
	if origin == "" {
		origin = r.Header.Get("Origin")
	}
	credentials := r.URL.Query().Get("credentials")
	if credentials == "" {
		credentials = "true"
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Credentials", credentials)
	switch r.URL.Query().Get("run") {
	case "message":
		return esMessageHandler(st, w, r)
	case "cache-control":
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", r.Header.Get("Cache-Control"))
		return true
	}
	w.WriteHeader(http.StatusNotFound)
	return true
}

// xhrDelayHandler ports xhr/resources/delay.py: it answers after the requested
// number of milliseconds. Several XHR tests use it to make a request that is
// still outstanding when a timeout or an abort fires; without it the request
// 404s at once and the test races its own timer. The wait is abandoned as soon
// as the client goes away, so a test that asks for twenty seconds and aborts
// after five milliseconds costs five milliseconds.
func xhrDelayHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	ms, err := strconv.ParseFloat(r.URL.Query().Get("ms"), 64)
	if err != nil {
		ms = 500
	}
	select {
	case <-time.After(time.Duration(ms) * time.Millisecond):
	case <-r.Context().Done():
		return true
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "YO")
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("TEST_DELAY"))
	return true
}
