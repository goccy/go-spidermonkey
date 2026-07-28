package wpt

// handlers.go: Go implementations of the suite's own Python fixture handlers.
//
// A large part of fetch/api asks a server-side script to report what it
// received — which headers arrived, what method was used, what the body was —
// and then asserts on the reply. Those scripts are wptserve handlers written in
// Python, and without a Python server they were served as source text: the
// tests read `from wptserve.utils import isomorphic_decode` where they expected
// a header value, and reported it as "expected 1.1 but got <python source>".
//
// The handlers below are the ones the suite leans on most, ported from
// resources/*.py. Each is small and entirely defined by its query string, so a
// faithful port is a matter of reading the original — which is checked out
// alongside, so any drift is visible. A path with no handler here still falls
// through to being served as a file, exactly as before.

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// stash is wptserve's per-server key/value store: a handler puts a value under
// a token and a later request takes it. It is how the suite checks that a
// preflight happened at all, since a preflight's own response is not visible
// to the test.
type stash struct {
	mu sync.Mutex
	m  map[string]string
}

func newStash() *stash { return &stash{m: map[string]string{}} }

func (s *stash) put(token, value string) {
	s.mu.Lock()
	s.m[token] = value
	s.mu.Unlock()
}

// take reads and REMOVES, as wptserve's does.
func (s *stash) take(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[token]
	delete(s.m, token)
	return v, ok
}

// wptHandler is one ported fixture. It returns false to fall through to
// serving the file, which is what an unported handler does.
type wptHandler func(st *stash, w http.ResponseWriter, r *http.Request) bool

// handlerFor maps a request path to its port. The paths are the suite's, so
// they are the exact strings the tests fetch.
func handlerFor(rel string) wptHandler {
	switch rel {
	case "fetch/api/resources/inspect-headers.py":
		return inspectHeadersHandler
	case "fetch/api/resources/status.py":
		return statusHandler
	case "fetch/api/resources/method.py":
		return methodHandler
	case "fetch/api/resources/echo-content.py":
		return echoContentHandler
	case "fetch/api/resources/stash-put.py":
		return stashPutHandler
	case "fetch/api/resources/stash-take.py":
		return stashTakeHandler
	case "fetch/api/resources/clean-stash.py":
		return cleanStashHandler
	case "fetch/api/resources/dump-authorization-header.py":
		return dumpAuthorizationHandler
	}
	return nil
}

// q reads the first value of a query parameter, with a default.
func q(r *http.Request, name, def string) string {
	if v := r.URL.Query().Get(name); v != "" {
		return v
	}
	if _, present := r.URL.Query()[name]; present {
		return "" // present but empty: the suite distinguishes these
	}
	return def
}

func has(r *http.Request, name string) bool {
	_, ok := r.URL.Query()[name]
	return ok
}

// headerOr returns a request header, or a placeholder the suite expects when
// the header is absent.
func headerOr(r *http.Request, name, absent string) string {
	if v := r.Header.Get(name); v != "" {
		return v
	}
	return absent
}

// inspectHeadersHandler echoes selected request headers back as x-request-*,
// which is how a test observes what the fetch actually sent.
func inspectHeadersHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	var checked []string
	if v := r.URL.Query().Get("headers"); v != "" {
		checked = strings.Split(v, "|")
		for _, h := range checked {
			if got := r.Header.Get(h); got != "" {
				w.Header().Add("x-request-"+h, got)
			}
		}
	}
	if has(r, "cors") {
		if origin := r.Header.Get("Origin"); origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		} else {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, HEAD")
		exposed := make([]string, len(checked))
		for i, h := range checked {
			exposed[i] = "x-request-" + h
		}
		w.Header().Set("Access-Control-Expose-Headers", strings.Join(exposed, ", "))
		if v := r.URL.Query().Get("allow_headers"); v != "" {
			w.Header().Set("Access-Control-Allow-Headers", v)
		} else {
			names := make([]string, 0, len(r.Header))
			for k := range r.Header {
				names = append(names, k)
			}
			w.Header().Set("Access-Control-Allow-Headers", strings.Join(names, ", "))
		}
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	return true
}

// statusHandler returns whatever status, text and body the query asks for.
func statusHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	code, err := strconv.Atoi(q(r, "code", "200"))
	if err != nil || code < 100 || code > 599 {
		code = 200
	}
	// An explicit empty Content-Type, which is what the Python handler sends
	// when `type` is absent. Leaving the header off entirely would let Go sniff
	// one, and the tests read it back.
	w.Header()["Content-Type"] = []string{q(r, "type", "")}
	w.Header().Set("X-Request-Method", r.Method)
	w.WriteHeader(code)
	io.WriteString(w, q(r, "content", ""))
	return true
}

// methodHandler reports the method and the entity headers, and echoes the body.
func methodHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	if has(r, "cors") {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, FOO")
		w.Header().Set("Access-Control-Allow-Headers", "x-test, x-foo")
		w.Header().Set("Access-Control-Expose-Headers", "x-request-method")
	}
	w.Header().Set("x-request-method", r.Method)
	w.Header().Set("x-request-content-type", headerOr(r, "Content-Type", "NO"))
	w.Header().Set("x-request-content-length", headerOr(r, "Content-Length", "NO"))
	w.Header().Set("x-request-content-encoding", headerOr(r, "Content-Encoding", "NO"))
	w.Header().Set("x-request-content-language", headerOr(r, "Content-Language", "NO"))
	w.Header().Set("x-request-content-location", headerOr(r, "Content-Location", "NO"))
	body, _ := io.ReadAll(r.Body)
	w.WriteHeader(http.StatusOK)
	w.Write(body)
	return true
}

// echoContentHandler returns the request body verbatim, with the method and
// entity headers alongside so the test can check both.
func echoContentHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("X-Request-Method", r.Method)
	w.Header().Set("X-Request-Content-Length", headerOr(r, "Content-Length", "NO"))
	w.Header().Set("X-Request-Content-Type", headerOr(r, "Content-Type", "NO"))
	// Explicit, so nothing sniffs the echoed body.
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write(body)
	return true
}

func corsWildcard(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "*")
	w.Header().Set("Access-Control-Allow-Headers", "*")
}

// stashPutHandler stores the request body under the query's key; stashTake
// reads it back once. Together they let a test observe a request whose
// response it never sees.
func stashPutHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	corsWildcard(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "done")
		return true
	}
	body, _ := io.ReadAll(r.Body)
	st.put(r.URL.Query().Get("key"), string(body))
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "done")
	return true
}

func stashTakeHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	corsWildcard(w)
	v, ok := st.take(r.URL.Query().Get("key"))
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	if !ok {
		io.WriteString(w, "null")
		return true
	}
	// wptserve returns the stored value as JSON, which for a string means it
	// arrives quoted.
	fmt.Fprintf(w, "%q", v)
	return true
}

func cleanStashHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	corsWildcard(w)
	_, ok := st.take(r.URL.Query().Get("token"))
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	if ok {
		io.WriteString(w, "1")
	} else {
		io.WriteString(w, "0")
	}
	return true
}

// dumpAuthorizationHandler reports whether (and how) credentials were sent.
func dumpAuthorizationHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	} else {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
	w.Header().Set("Access-Control-Allow-Headers", "Authorization")
	w.Header().Set("Content-Type", "text/plain")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return true
	}
	w.WriteHeader(http.StatusOK)
	if v := r.Header.Get("Authorization"); v != "" {
		io.WriteString(w, v)
	} else {
		io.WriteString(w, "none")
	}
	return true
}
