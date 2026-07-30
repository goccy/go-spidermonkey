package wpt

// httpcache_fixture.go: a port of fetch/http-cache/resources/http-cache.py.
//
// Every file in fetch/http-cache goes through this one handler, so without it
// the whole directory reports nothing — 24 files and 272 subtests, all of them
// about the HTTP cache in compat/web rather than about anything a browser does
// differently. It is the most leverage any single fixture in the suite has.
//
// The protocol is unusual and worth stating, because none of it is guessable
// from the request alone. The CLIENT sends the whole test plan on every request,
// base64-encoded in a Test-Requests header; the server keeps a per-uuid list of
// the requests it has actually seen, and answers request number len(seen) from
// that plan. So "was this response served from the cache?" is answered by
// comparing the client's request count with the server's Server-Request-Count:
// if the server never saw the request, the cache served it. Afterwards the test
// fetches ?dispatch=state to read what the server saw and check the validators.
//
// Two details in the plan are rewritten by the server rather than the client. A
// Location or Content-Location value is made absolute against the request URL
// ("magic locations"), and an INTEGER value for a date header is a delta in
// seconds from now ("magic dates") — which is how a test asks for a response
// that is already stale without knowing the clock.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The header sets the fixture treats specially, spelled as the original does.
var (
	notedHeaders    = map[string]bool{"content-type": true, "access-control-allow-origin": true, "last-modified": true, "etag": true}
	locationHeaders = map[string]bool{"location": true, "content-location": true}
	dateHeaders     = map[string]bool{"date": true, "expires": true, "last-modified": true}
	noBodyStatus    = map[int]bool{204: true, 304: true}
)

// cacheRequestConfig is one entry of the plan the client sends. Fields the
// server does not read are left out; the plan is JSON the client also reads
// back, so an unknown field must survive, which it does — the plan is never
// re-serialized.
type cacheRequestConfig struct {
	ResponseHeaders [][]any `json:"response_headers"`
	ResponseStatus  []any   `json:"response_status"`
	ResponseBody    *string `json:"response_body"`
	ExpectedType    string  `json:"expected_type"`
}

// cacheServerState is what the server records about one request it saw. The
// client reads every field.
type cacheServerState struct {
	Now             float64           `json:"now"`
	RequestMethod   string            `json:"request_method"`
	RequestHeaders  map[string]string `json:"request_headers"`
	ResponseHeaders map[string]string `json:"response_headers"`
}

// httpCacheHandler answers resources/http-cache.py.
func httpCacheHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	dispatch := r.URL.Query().Get("dispatch")
	uuid := r.URL.Query().Get("uuid")
	w.Header().Set("Access-Control-Allow-Credentials", "true")

	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", headerOr(r, "Origin", "*"))
		w.Header().Set("Access-Control-Allow-Methods", "GET")
		w.Header().Set("Access-Control-Allow-Headers", headerOr(r, "Access-Control-Request-Headers", "*"))
		w.Header().Set("Access-Control-Max-Age", "86400")
		_, _ = w.Write([]byte("Preflight request"))
		return true
	}
	if uuid == "" {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("UUID not found"))
		return true
	}
	switch dispatch {
	case "test":
		return httpCacheTest(st, uuid, w, r)
	case "state":
		// Reading the state also CLEARS it, as the original's stash.take does.
		w.Header().Set("Content-Type", "text/plain")
		raw, ok := st.take(uuid)
		if !ok {
			raw = "null"
		}
		_, _ = w.Write([]byte(raw))
		return true
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("Fallthrough"))
	return true
}

func httpCacheTest(st *stash, uuid string, w http.ResponseWriter, r *http.Request) bool {
	var seen []cacheServerState
	if raw, ok := st.take(uuid); ok {
		_ = json.Unmarshal([]byte(raw), &seen)
	}
	plan, err := decodeTestRequests(r.Header.Get("Test-Requests"))
	if err != nil || len(plan) <= len(seen) {
		w.Header().Set("Content-Type", "text/plain")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("No or bad Test-Requests request header"))
		} else {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("Config not found"))
		}
		// The state is put back: a malformed request must not lose what the server
		// had already recorded, or every later assertion in the file is meaningless.
		putServerState(st, uuid, seen)
		return true
	}
	config := plan[len(seen)]

	now := time.Now()
	noted := map[string]string{}
	for _, header := range config.ResponseHeaders {
		if len(header) < 2 {
			continue
		}
		name, _ := header[0].(string)
		value := header[1]
		lower := strings.ToLower(name)
		if locationHeaders[lower] {
			// Magic location: the value names a query argument on THIS handler, so
			// it is made absolute against the request URL rather than resolved as a
			// path.
			if s, ok := value.(string); ok && s != "" {
				value = requestURL(r) + "&target=" + s
			} else {
				value = requestURL(r)
			}
		}
		if dateHeaders[lower] {
			// Magic date: a NUMBER is a delta in seconds from now. JSON gives it as a
			// float64, and only a whole number means a delta — the original tests
			// isinstance(int), and a string date is passed through untouched.
			if f, ok := value.(float64); ok {
				value = httpDate(now.Add(time.Duration(f) * time.Second))
			}
		}
		text := fmt.Sprint(value)
		w.Header().Set(name, text)
		if notedHeaders[lower] {
			noted[lower] = text
		}
	}

	state := cacheServerState{
		// Seconds since the epoch as a float, which is what time.time() returns.
		Now:             float64(now.UnixNano()) / float64(time.Second),
		RequestMethod:   r.Method,
		RequestHeaders:  lowercasedHeaders(r),
		ResponseHeaders: noted,
	}
	seen = append(seen, state)
	putServerState(st, uuid, seen)

	if _, ok := noted["access-control-allow-origin"]; !ok {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
	if _, ok := noted["content-type"]; !ok {
		w.Header().Set("Content-Type", "text/plain")
	}
	w.Header().Set("Server-Request-Count", fmt.Sprint(len(seen)))

	code := 200
	if len(config.ResponseStatus) > 0 {
		if f, ok := config.ResponseStatus[0].(float64); ok {
			code = int(f)
		}
	}
	if strings.HasSuffix(config.ExpectedType, "validated") {
		// The test expects this request to be a REVALIDATION, so the answer is 304
		// only if the validator matches what the first response advertised. When it
		// does not, the status is 999 — a deliberate nonsense code, so the failure
		// says "the client did not revalidate" rather than passing quietly.
		code = 999
		if len(seen) > 0 {
			ref := seen[0].ResponseHeaders
			if lm := ref["last-modified"]; lm != "" && r.Header.Get("If-Modified-Since") == lm {
				code = 304
			}
			if etag := ref["etag"]; etag != "" && r.Header.Get("If-None-Match") == etag {
				code = 304
			}
		}
	}
	w.WriteHeader(code)
	if noBodyStatus[code] {
		return true
	}
	body := uuid
	if config.ResponseBody != nil {
		body = *config.ResponseBody
	}
	_, _ = w.Write([]byte(body))
	return true
}

func decodeTestRequests(encoded string) ([]cacheRequestConfig, error) {
	if encoded == "" {
		return nil, fmt.Errorf("no Test-Requests header")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	var plan []cacheRequestConfig
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, err
	}
	return plan, nil
}

func putServerState(st *stash, uuid string, seen []cacheServerState) {
	b, err := json.Marshal(seen)
	if err != nil {
		return
	}
	st.put(uuid, string(b))
}

// lowercasedHeaders is the request's headers keyed as the fixture reports them.
// A repeated header is joined with ", ", which is how wptserve's own mapping
// presents one.
func lowercasedHeaders(r *http.Request) map[string]string {
	out := map[string]string{}
	for name, values := range r.Header {
		out[strings.ToLower(name)] = strings.Join(values, ", ")
	}
	return out
}

// requestURL rebuilds the absolute URL of this request, which the magic-location
// rewrite appends to. The Host header is the authority the CLIENT used, and it
// has to be, or a Location pointing back here would cross an origin.
func requestURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + r.URL.RequestURI()
}

// httpDate formats an HTTP-date. time.RFC1123 is nearly it but spells the zone
// from the time's own location; the format is defined to say GMT.
func httpDate(t time.Time) string {
	return t.UTC().Format("Mon, 02 Jan 2006 15:04:05") + " GMT"
}

// staleScriptHandler ports fetch/stale-while-revalidate/resources/stale-script.py.
//
// The resource answers with `Cache-Control: private, max-age=0,
// stale-while-revalidate=60` and a fresh Unique-Id every time, and counts how
// often it was ACTUALLY requested per token; the `?query` form reports that
// count. The test then proves stale-while-revalidate end to end: the second
// fetch must return the FIRST response's Unique-Id (served stale from the
// cache) while the count still reaches 2 (the revalidation happened, in the
// background).
func staleScriptHandler(st *stash, w http.ResponseWriter, r *http.Request) bool {
	token := r.URL.Query().Get("token")
	_, isQuery := r.URL.Query()["query"]
	count := 0
	if raw, ok := st.take("stale-script/" + token); ok {
		count, _ = strconv.Atoi(raw)
	}
	if isQuery {
		// The original puts the value back only while count < 2: reading a
		// completed count consumes it. Copied as-is — the fixture's contract is
		// the original's behaviour, oddities included.
		if count < 2 {
			st.put("stale-script/"+token, strconv.Itoa(count))
		}
		w.Header().Set("Count", strconv.Itoa(count))
		w.WriteHeader(http.StatusOK)
		return true
	}
	count++
	st.put("stale-script/"+token, strconv.Itoa(count))
	// The id must differ between responses — it is how the test detects which
	// response a fetch got — and carries no other meaning.
	id := fmt.Sprintf("%020d", time.Now().UnixNano())
	w.Header().Set("Content-Type", "text/javascript")
	w.Header().Set("Cache-Control", "private, max-age=0, stale-while-revalidate=60")
	w.Header().Set("Unique-Id", id)
	_, _ = w.Write([]byte("report('" + id + "')"))
	return true
}
