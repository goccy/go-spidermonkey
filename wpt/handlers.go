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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
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
	case "fetch/api/resources/preflight.py":
		return preflightHandler
	case "fetch/api/resources/redirect.py":
		return redirectHandler
	case "fetch/api/resources/trickle.py":
		return trickleHandler
	case "fetch/api/resources/cache.py":
		return cacheHandler
	case "fetch/api/resources/authentication.py":
		return authenticationHandler
	case "fetch/api/resources/redirect-empty-location.py":
		return emptyLocationHandler
	case "fetch/api/resources/infinite-slow-response.py":
		return infiniteSlowHandler
	case "fetch/api/request/resources/cache.py":
		return requestCacheHandler
	case "fetch/http-cache/resources/http-cache.py":
		return httpCacheHandler
	case "fetch/stale-while-revalidate/resources/stale-script.py":
		return staleScriptHandler
	case "eventsource/resources/message.py":
		return esMessageHandler
	case "eventsource/resources/message2.py":
		return esMessage2Handler
	case "eventsource/resources/last-event-id.py":
		return esLastEventIDHandler
	case "eventsource/resources/cors.py":
		return esCORSHandler
	case "fetch/cross-origin-resource-policy/resources/hello.py":
		return corpHelloHandler
	case "fetch/metadata/resources/echo-as-json.py":
		return secFetchEchoHandler
	case "xhr/resources/delay.py":
		return xhrDelayHandler
	case "xhr/resources/content.py":
		return xhrContentHandler
	case "xhr/resources/access-control-basic-allow.py":
		return xhrAllowOriginHandler
	case "xhr/resources/access-control-basic-allow-star.py":
		return xhrAllowStarHandler
	case "xhr/resources/corsenabled.py":
		return xhrCORSEnabledHandler
	case "xhr/resources/redirect-cors.py":
		return xhrRedirectCORSHandler
	case "xhr/resources/reset-token.py":
		return xhrResetTokenHandler
	case "xhr/resources/echo-content-cors.py":
		return xhrEchoContentCORSHandler
	case "xhr/resources/bad-chunk-encoding.py":
		return xhrBadChunkEncodingHandler
	// These are the same fixtures fetch/api uses, under xhr's own resources.
	case "xhr/resources/status.py":
		return statusHandler
	case "xhr/resources/redirect.py":
		return redirectHandler
	case "xhr/resources/trickle.py":
		return trickleHandler
	case "xhr/resources/dump-authorization-header.py":
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

// preflightHandler is resources/preflight.py: on OPTIONS it records that a
// preflight happened (under the query's token, in the stash) and answers with
// whatever the query says to allow; on the real request it reports what it
// recorded through x-* headers, since a bodyless response has nowhere else to
// put it.
func preflightHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	query := r.URL.Query()
	token := query.Get("token")
	w.Header().Set("Content-Type", "text/plain")
	if origins := query.Get("origin"); origins != "" {
		for _, o := range strings.Split(origins, ", ") {
			w.Header().Add("Access-Control-Allow-Origin", o)
		}
	} else {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
	if has(r, "clear-stash") {
		_, ok := st.take(token)
		w.WriteHeader(http.StatusOK)
		if ok {
			io.WriteString(w, "1")
		} else {
			io.WriteString(w, "0")
		}
		return true
	}
	if has(r, "credentials") {
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	}

	if r.Method == http.MethodOptions {
		if r.Header.Get("Access-Control-Request-Method") == "" {
			http.Error(w, "ERROR: No access-control-request-method in preflight!", http.StatusBadRequest)
			return true
		}
		if r.Header.Get("Accept") != "*/*" {
			http.Error(w, "ERROR: Invalid access in preflight!", http.StatusBadRequest)
			return true
		}
		if v := query.Get("max_age"); v != "" {
			w.Header().Set("Access-Control-Max-Age", v)
		}
		if v := query.Get("allow_headers"); v != "" {
			w.Header().Set("Access-Control-Allow-Headers", v)
		}
		if v := query.Get("allow_methods"); v != "" {
			w.Header().Set("Access-Control-Allow-Methods", v)
		}
		if token != "" {
			// ABSENT is not the same as empty: the original stores None when the
			// preflight carried no Access-Control-Request-Headers, and then omits
			// the reporting header entirely — which is what the test reads as null.
			// Storing "" for both made every no-unsafe-header preflight report an
			// empty string instead.
			control := "\x00absent"
			if v, ok := r.Header["Access-Control-Request-Headers"]; ok && len(v) > 0 {
				control = v[0]
			}
			st.put(token, strings.Join([]string{
				control,
				"1",
				r.Header.Get("Referer"),
				r.Header.Get("User-Agent"),
			}, "\n"))
		}
		status := http.StatusOK
		if v := query.Get("preflight_status"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				status = n
			}
		}
		w.WriteHeader(status)
		return true
	}

	// The real request: report what the preflight recorded.
	controlRequestHeaders, didPreflight, preflightReferrer, preflightUA := "\x00absent", "0", "", ""
	if token != "" {
		if v, ok := st.take(token); ok {
			parts := strings.SplitN(v, "\n", 4)
			for len(parts) < 4 {
				parts = append(parts, "")
			}
			controlRequestHeaders, didPreflight, preflightReferrer, preflightUA = parts[0], parts[1], parts[2], parts[3]
		}
	}
	if has(r, "checkUserAgentHeaderInPreflight") && r.Header.Get("User-Agent") != preflightUA {
		http.Error(w, "ERROR: No user-agent header in preflight", http.StatusBadRequest)
		return true
	}
	w.Header().Set("Access-Control-Expose-Headers",
		"x-did-preflight, x-control-request-headers, x-referrer, x-preflight-referrer, x-origin")
	w.Header().Set("x-did-preflight", didPreflight)
	if controlRequestHeaders != "\x00absent" {
		w.Header().Set("x-control-request-headers", controlRequestHeaders)
	}
	w.Header().Set("x-preflight-referrer", preflightReferrer)
	w.Header().Set("x-referrer", r.Header.Get("Referer"))
	w.Header().Set("x-origin", r.Header.Get("Origin"))
	if token != "" {
		st.put(token, strings.Join([]string{controlRequestHeaders, didPreflight, preflightReferrer, preflightUA}, "\n"))
	}
	w.WriteHeader(http.StatusOK)
	return true
}

// redirectHandler is resources/redirect.py. The redirect family is written
// against its two behaviours: it counts the hops it has served under a stash
// token, and it carries the whole query string forward so the next hop behaves
// the same way.
func redirectHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	query := r.URL.Query()
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Pragma", "no-cache")
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	} else {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}

	token := query.Get("token")
	count, preflight := 0, "0"
	if token != "" {
		if v, ok := st.take(token); ok {
			parts := strings.SplitN(v, "\n", 2)
			count, _ = strconv.Atoi(parts[0])
			if len(parts) > 1 {
				preflight = parts[1]
			}
		}
	}

	if r.Method == http.MethodOptions {
		if v := query.Get("allow_headers"); v != "" {
			w.Header().Set("Access-Control-Allow-Headers", v)
		}
		preflight = "1"
		// A preflight is NOT redirected unless the test asks for it.
		if !has(r, "redirect_preflight") {
			if token != "" {
				st.put(token, fmt.Sprintf("%d\n%s", count, preflight))
			}
			w.WriteHeader(http.StatusOK)
			return true
		}
	}

	status := 302
	if v := query.Get("redirect_status"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			status = n
		}
	}
	count++

	if loc := query.Get("location"); loc != "" {
		target := loc
		if !has(r, "simple") {
			if u, err := url.Parse(loc); err == nil && (u.Scheme == "" || u.Scheme == "http" || u.Scheme == "https") {
				// Carry the query forward so the next hop behaves the same, and
				// vary it by count so a redirect LOOP keeps changing URL.
				sep := "?"
				if strings.Contains(target, "?") {
					sep = "&"
				}
				forwarded := url.Values{}
				for k, vs := range query {
					if len(vs) > 0 {
						forwarded.Set(k, vs[0])
					}
				}
				forwarded.Set("count", strconv.Itoa(count))
				target += sep + forwarded.Encode()
			}
		}
		w.Header().Set("Location", target)
	}
	if v := query.Get("redirect_referrerpolicy"); v != "" {
		w.Header().Set("Referrer-Policy", v)
	}
	if v := query.Get("delay"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 && ms < 5000 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
	}
	if token != "" {
		st.put(token, fmt.Sprintf("%d\n%s", count, preflight))
		if v := query.Get("max_count"); v != "" {
			if maxCount, err := strconv.Atoi(v); err == nil && count > maxCount {
				// Stop redirecting and report the hop count. -1 because the last
				// one is not a redirection.
				w.WriteHeader(http.StatusOK)
				io.WriteString(w, strconv.Itoa(count-1))
				return true
			}
		}
	}
	w.WriteHeader(status)
	return true
}

// trickleHandler is resources/trickle.py: it dribbles a body out in `count`
// chunks `ms` apart, which is how the streaming and abort tests get a response
// that is still arriving when they act on it.
func trickleHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	ms := 500
	if v := r.URL.Query().Get("ms"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			ms = n
		}
	}
	count := 50
	if v := r.URL.Query().Get("count"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			count = n
		}
	}
	// Bound what a single fixture can cost: the suite's own values are small,
	// and a test that asks for minutes of dribbling would stall the run.
	if ms < 0 {
		ms = 0
	}
	if ms > 1000 {
		ms = 1000
	}
	if count < 0 || count > 200 {
		count = 50
	}
	delay := time.Duration(ms) * time.Millisecond
	io.Copy(io.Discard, r.Body)
	time.Sleep(delay)
	if !has(r, "notype") {
		w.Header().Set("Content-Type", "text/plain")
	}
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	time.Sleep(delay)
	for i := 0; i < count; i++ {
		io.WriteString(w, "TEST_TRICKLE\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(delay)
	}
	return true
}

// cacheHandler is resources/cache.py: a fixed body behind an ETag, so the
// conditional-request tests have something to revalidate against.
func cacheHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	const etag = `"123abc"`
	if r.Header.Get("If-None-Match") == etag {
		w.Header().Set("X-HTTP-STATUS", "304")
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "lorem ipsum dolor sit amet")
	return true
}

// authenticationHandler is resources/authentication.py: it challenges with
// Basic auth and accepts exactly one credential pair, which is how the
// credentials tests tell "sent" from "not sent".
func authenticationHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	user, password, _ := r.BasicAuth()
	if user == "user" && password == "password" {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "Authentication done")
		return true
	}
	realm := "test"
	if v := r.URL.Query().Get("realm"); v != "" {
		realm = v
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`"`)
	w.WriteHeader(http.StatusUnauthorized)
	io.WriteString(w, "Please login with credentials 'user' and 'password'")
	return true
}

// emptyLocationHandler is resources/redirect-empty-location.py: a 302 whose
// Location is empty, which a client must treat as a network error rather than
// as a redirect to the current URL.
func emptyLocationHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	w.Header()["Location"] = []string{""}
	w.WriteHeader(http.StatusFound)
	return true
}

// infiniteSlowHandler is resources/infinite-slow-response.py: a response that
// never ends, so the abort tests have something to abort. It stops when the
// client goes away rather than running forever.
func infiniteSlowHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	if key := r.URL.Query().Get("stateKey"); key != "" {
		st.put(key, "open")
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	// An initial 2k so the client believes the body has started.
	io.WriteString(w, strings.Repeat(".", 2048))
	if flusher != nil {
		flusher.Flush()
	}
	// Bounded so a test that never aborts cannot stall the whole run: the
	// suite's own cases abort within a second.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			return true
		case <-r.Context().Done():
			if key := r.URL.Query().Get("stateKey"); key != "" {
				st.put(key, "aborted")
			}
			return true
		case <-time.After(100 * time.Millisecond):
			io.WriteString(w, ".")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// requestCacheHandler is fetch/api/request/resources/cache.py. The whole
// request-cache family drives it: each fetch records what conditional headers
// arrived, and a final "querystate" request reads the record back as JSON. The
// response itself is shaped by the query — an ETag, a Date, a Cache-Control —
// so the client has something to revalidate against.
func requestCacheHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	query := r.URL.Query()
	token := query.Get("token")

	if has(r, "querystate") {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		if v, ok := st.take(token); ok {
			io.WriteString(w, v)
		} else {
			io.WriteString(w, "[]")
		}
		return true
	}

	// Record this request's conditional headers, appending to the JSON array
	// the querystate call will read.
	state := map[string]string{}
	if v := r.Header.Get("If-None-Match"); v != "" {
		state["If-None-Match"] = v
	}
	if v := r.Header.Get("If-Modified-Since"); v != "" {
		state["If-Modified-Since"] = v
	}
	if v := r.Header.Get("Pragma"); v != "" {
		state["Pragma"] = v
	}
	if v := r.Header.Get("Cache-Control"); v != "" {
		state["Cache-Control"] = v
	}
	if token != "" {
		prev, _ := st.take(token)
		entry, _ := json.Marshal(state)
		if prev == "" || prev == "[]" {
			st.put(token, "["+string(entry)+"]")
		} else {
			st.put(token, strings.TrimSuffix(prev, "]")+","+string(entry)+"]")
		}
	}

	tag := query.Get("tag")
	if tag != "" {
		tag = `"` + tag + `"`
		w.Header().Set("ETag", tag)
	}
	if v := query.Get("date"); v != "" {
		w.Header().Set("Last-Modified", v)
	}
	if v := query.Get("expires"); v != "" {
		w.Header().Set("Expires", v)
	}
	if v := query.Get("vary"); v != "" {
		w.Header().Set("Vary", v)
	}
	if v := query.Get("cache_control"); v != "" {
		w.Header().Set("Cache-Control", v)
	}
	if v := query.Get("redirect"); v != "" {
		w.Header().Set("Location", v)
		w.WriteHeader(http.StatusFound)
		return true
	}
	// A matching conditional is a 304, which is the point of the fixture.
	if tag != "" && r.Header.Get("If-None-Match") == tag && !has(r, "ignore") {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, query.Get("content"))
	return true
}
