package wpt

// xhr_fixture.go: the xhr/resources/*.py handlers the .any.js corpus reaches.
//
// XHR's tests are almost all of the shape "ask the server what it received, and
// assert on the reply", so without these the directory measures nothing about
// XMLHttpRequest — a request that 404s tells you only that the fixture is
// missing. Each port is small and entirely defined by its query string, and the
// original is checked out alongside, so drift is visible.

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// xhrContentHandler ports content.py: it echoes the body (or a `content` query
// argument) and reports what it received in x-request-* headers.
func xhrContentHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	ctype := "text/plain"
	if label := r.URL.Query().Get("response_charset_label"); label != "" {
		ctype += ";charset=" + label
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Request-Method", r.Method)
	w.Header().Set("X-Request-Query", queryOrNo(r))
	w.Header().Set("X-Request-Content-Length", headerOr(r, "Content-Length", "NO"))
	w.Header().Set("X-Request-Content-Type", headerOr(r, "Content-Type", "NO"))
	if v, ok := r.URL.Query()["content"]; ok && len(v) > 0 {
		_, _ = io.WriteString(w, v[0])
		return true
	}
	body, _ := io.ReadAll(r.Body)
	_, _ = w.Write(body)
	return true
}

// queryOrNo is the raw query string, or "NO" when there was none — the
// distinction several tests assert on.
func queryOrNo(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return "NO"
	}
	return r.URL.RawQuery
}

// xhrAllowOriginHandler ports access-control-basic-allow.py: it echoes the
// request's Origin and allows credentials.
func xhrAllowOriginHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Access-Control-Allow-Credentials", "true")
	w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
	_, _ = io.WriteString(w, "PASS: Cross-domain access allowed.")
	return true
}

// xhrAllowStarHandler ports access-control-basic-allow-star.py.
func xhrAllowStarHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_, _ = io.WriteString(w, "PASS: Cross-domain access allowed.")
	return true
}

// xhrCORSEnabledHandler ports corsenabled.py: a permissive CORS endpoint that
// reports what it received, including the body, in exposed headers.
func xhrCORSEnabledHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, FOO")
	h.Set("Access-Control-Allow-Headers", "x-test, x-foo")
	h.Set("Access-Control-Expose-Headers",
		"x-request-method, x-request-content-type, x-request-query, x-request-content-length, x-request-data")
	if d := r.URL.Query().Get("delay"); d != "" {
		if secs, err := strconv.Atoi(d); err == nil {
			select {
			case <-time.After(time.Duration(secs) * time.Second):
			case <-r.Context().Done():
				return true
			}
		}
	}
	if _, ok := r.URL.Query()["safelist_content_type"]; ok {
		h.Add("Access-Control-Allow-Headers", "content-type")
	}
	body, _ := io.ReadAll(r.Body)
	h.Set("X-Request-Method", r.Method)
	h.Set("X-Request-Query", queryOrNo(r))
	h.Set("X-Request-Content-Length", headerOr(r, "Content-Length", "NO"))
	h.Set("X-Request-Content-Type", headerOr(r, "Content-Type", "NO"))
	h.Set("X-Request-Data", string(body))
	_, _ = io.WriteString(w, "Test")
	return true
}

// xhrRedirectCORSHandler ports redirect-cors.py: a redirect whose CORS headers
// the query decides, so a test can drive each way a redirected cross-origin
// request can be refused.
func xhrRedirectCORSHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	location := q.Get("location")
	if _, ok := q["allow_origin"]; ok {
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
	}
	if v := q.Get("allow_header"); v != "" {
		w.Header().Set("Access-Control-Allow-Headers", v)
	}
	switch r.Method {
	case http.MethodOptions:
		w.Header().Set("Access-Control-Allow-Methods", "GET")
		w.Header().Set("Access-Control-Max-Age", "1")
		if _, ok := q["redirect_preflight"]; ok {
			w.Header().Set("Location", location)
			w.WriteHeader(http.StatusFound)
			return true
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusFound)
	}
	return true
}

// xhrResetTokenHandler ports reset-token.py: it clears a stash token, which is
// how a preflight-cache test starts from a known state.
func xhrResetTokenHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
	st.put(r.URL.Query().Get("token"), "")
	_, _ = io.WriteString(w, "PASS")
	return true
}

// xhrEchoContentCORSHandler ports echo-content-cors.py.
func xhrEchoContentCORSHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	h := w.Header()
	h.Set("X-Request-Method", r.Method)
	h.Set("X-Request-Content-Length", headerOr(r, "Content-Length", "NO"))
	h.Set("X-Request-Content-Type", headerOr(r, "Content-Type", "NO"))
	h.Set("Access-Control-Allow-Credentials", "true")
	// text/plain so nothing sniffs the response into another type.
	h.Set("Content-Type", "text/plain")
	origin := q.Get("origin")
	if origin == "" {
		origin = r.Header.Get("Origin")
	}
	if origin != "" {
		h.Set("Access-Control-Allow-Origin", origin)
	}
	reqHeaders := q.Get("origin")
	if reqHeaders == "" {
		reqHeaders = r.Header.Get("Access-Control-Request-Headers")
	}
	if reqHeaders != "" {
		h.Set("Access-Control-Allow-Headers", reqHeaders)
	}
	body, _ := io.ReadAll(r.Body)
	_, _ = w.Write(body)
	return true
}

// xhrBadChunkEncodingHandler ports bad-chunk-encoding.py: a chunked response
// that stops mid-stream, so the client must report a network error rather than
// the truncated body.
func xhrBadChunkEncodingHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Transfer-Encoding", "chunked")
	hj, ok := w.(http.Hijacker)
	if !ok {
		return true
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return true
	}
	defer conn.Close()
	// Written by hand because the point is a response that is NOT well formed:
	// a valid chunk, then a length with no body, then the connection closing.
	_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\n\r\n")
	_, _ = buf.WriteString("5\r\nTEST\n\r\n")
	_, _ = buf.WriteString("5\r\n")
	_ = buf.Flush()
	return true
}

// xhrHeadersHandler ports headers.py and headers-basic.py-style echoes: it
// returns the header names and values the query names.
func xhrHeadersHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Content-Type", "text/plain")
	var out []string
	for name := range r.URL.Query() {
		out = append(out, fmt.Sprintf("%s: %s", name, headerOr(r, name, "NO")))
	}
	_, _ = io.WriteString(w, strings.Join(out, "\n"))
	return true
}
