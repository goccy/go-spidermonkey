package web

// httpcache.go: the HTTP cache behind fetch's `cache` option.
//
// fetch does not define caching of its own — it defers to HTTP's, and then adds
// six modes that say how much of it to obey. So this is RFC 9111's core
// (freshness, revalidation, Vary) with the modes layered on top, as a
// RoundTripper: that is where a cache belongs in Go, and it composes with the
// permissive transport in fetchraw.go without either knowing about the other.
//
// What it deliberately does not do: range requests and 206 responses go
// straight past it. A partial response is not the resource, and storing one as
// though it were is how a cache starts returning truncated bodies.
//
// The store is per-installation and in memory, bounded by total body bytes with
// the oldest entries evicted first. A cache with no ceiling is a memory leak
// with a hit rate.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cacheMode is fetch's `cache` option.
type cacheMode string

const (
	cacheDefault      cacheMode = "default"
	cacheNoStore      cacheMode = "no-store"
	cacheReload       cacheMode = "reload"
	cacheNoCache      cacheMode = "no-cache"
	cacheForceCache   cacheMode = "force-cache"
	cacheOnlyIfCached cacheMode = "only-if-cached"
)

// cacheModeKey carries the mode from the fetch op to the transport. It travels
// on the request context because the mode is a property of the fetch call, not
// of the connection or the client.
type cacheModeKey struct{}

func withCacheMode(ctx context.Context, mode string) context.Context {
	switch cacheMode(mode) {
	case cacheNoStore, cacheReload, cacheNoCache, cacheForceCache, cacheOnlyIfCached:
		return context.WithValue(ctx, cacheModeKey{}, cacheMode(mode))
	}
	return ctx
}

func cacheModeOf(req *http.Request) cacheMode {
	if m, ok := req.Context().Value(cacheModeKey{}).(cacheMode); ok {
		return m
	}
	return cacheDefault
}

// maxCacheBytes bounds the total stored body size.
const maxCacheBytes = 32 << 20

// cacheEntry is a stored response, byte for byte. The times are what freshness
// is computed from: HTTP's age arithmetic needs to know when the request went
// out and when the response came back, not just what the headers say.
type cacheEntry struct {
	status    int
	header    http.Header
	body      []byte
	requested time.Time
	received  time.Time
	// varyKey is the request-header values the response said it varies by. An
	// entry is only reusable for a request whose values match.
	varyKey string
	stored  time.Time
}

// responseCache stores, per primary key, one entry PER VARIANT: a response
// with Vary: Foo selects on the request's Foo value, and storing a second
// variant must not evict the first — a cache holds the variants side by side,
// or Vary would make it useless for exactly the resources that use it.
type responseCache struct {
	mu      sync.Mutex
	entries map[string][]*cacheEntry
	bytes   int
}

func newResponseCache() *responseCache {
	return &responseCache{entries: map[string][]*cacheEntry{}}
}

// key is the primary cache key: the method and the full URL. Vary is handled
// separately, as a check on the entry rather than as part of the key, so that a
// request can find the entry and then discover it does not apply.
func cacheKey(req *http.Request) string { return req.Method + " " + req.URL.String() }

// varyKeyFor builds the value an entry's Vary directive selects on. A Vary of
// "*" never matches, which is the standard's way of saying "do not reuse this".
func varyKeyFor(vary string, h http.Header) (string, bool) {
	if vary == "" {
		return "", true
	}
	var parts []string
	for _, name := range strings.Split(vary, ",") {
		name = strings.TrimSpace(name)
		if name == "*" {
			return "", false
		}
		parts = append(parts, name+"="+h.Get(name))
	}
	return strings.Join(parts, "\n"), true
}

func (c *responseCache) get(req *http.Request) *cacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries[cacheKey(req)] {
		// Each variant's OWN Vary decides what it selects on; the header can
		// differ between variants when the origin changed its mind.
		want, ok := varyKeyFor(e.header.Get("Vary"), req.Header)
		if ok && want == e.varyKey {
			return e
		}
	}
	return nil
}

func (c *responseCache) put(req *http.Request, e *cacheEntry) {
	key, ok := varyKeyFor(e.header.Get("Vary"), req.Header)
	if !ok {
		return
	}
	e.varyKey = key
	e.stored = time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	pk := cacheKey(req)
	variants := c.entries[pk]
	replaced := false
	for i, old := range variants {
		if old.varyKey == e.varyKey {
			c.bytes -= len(old.body)
			variants[i] = e
			replaced = true
			break
		}
	}
	if !replaced {
		variants = append(variants, e)
	}
	c.entries[pk] = variants
	c.bytes += len(e.body)
	// Evict oldest-first until under the ceiling.
	for c.bytes > maxCacheBytes {
		oldestKey, oldestIdx := "", -1
		oldest := time.Now().Add(time.Hour)
		for k, list := range c.entries {
			for i, v := range list {
				if v == e {
					continue // never evict what was just stored
				}
				if v.stored.Before(oldest) {
					oldestKey, oldestIdx, oldest = k, i, v.stored
				}
			}
		}
		if oldestIdx < 0 {
			return
		}
		list := c.entries[oldestKey]
		c.bytes -= len(list[oldestIdx].body)
		list = append(list[:oldestIdx], list[oldestIdx+1:]...)
		if len(list) == 0 {
			delete(c.entries, oldestKey)
		} else {
			c.entries[oldestKey] = list
		}
	}
}

func (c *responseCache) reset() {
	c.mu.Lock()
	c.entries = map[string][]*cacheEntry{}
	c.bytes = 0
	c.mu.Unlock()
}

// ---------------------------------------------------------- freshness

// directives parses a Cache-Control field into a map. A directive with no value
// maps to the empty string, which is enough to test for its presence.
func directives(h http.Header) map[string]string {
	out := map[string]string{}
	for _, v := range h.Values("Cache-Control") {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			name, value, _ := strings.Cut(part, "=")
			out[strings.ToLower(strings.TrimSpace(name))] = strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return out
}

// heuristicStatus lists the codes RFC 9111 calls heuristically cacheable: a
// response with one of these and no explicit lifetime may still be assigned
// one. Any other status gets a heuristic only when the response opts in with
// Cache-Control: public.
var heuristicStatus = map[int]bool{
	200: true, 203: true, 204: true, 300: true, 301: true, 308: true,
	404: true, 405: true, 410: true, 414: true, 501: true,
}

// hasExplicitLifetime reports whether the response states its own freshness.
// s-maxage deliberately does not count: it addresses shared caches, and this
// cache is a private one — it is fetch's own, serving a single user agent.
func hasExplicitLifetime(h http.Header) bool {
	if _, ok := directives(h)["max-age"]; ok {
		return true
	}
	return h.Get("Expires") != ""
}

// freshnessLifetime is how long the response may be reused without asking:
// max-age, then Expires, then — with nothing explicit — the heuristic the
// standard suggests, one tenth of the time since Last-Modified. The heuristic
// applies only where the standard allows one: a heuristically-cacheable status,
// or an explicit Cache-Control: public.
func (e *cacheEntry) freshnessLifetime() time.Duration {
	cc := directives(e.header)
	if v, ok := cc["max-age"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	base := e.received
	if d := e.header.Get("Date"); d != "" {
		if dt, derr := http.ParseTime(d); derr == nil {
			base = dt
		}
	}
	if exp := e.header.Get("Expires"); exp != "" {
		t, err := http.ParseTime(exp)
		if err != nil || t.Before(base) {
			// An unparseable or past Expires means already expired.
			return 0
		}
		return t.Sub(base)
	}
	_, public := cc["public"]
	if !heuristicStatus[e.status] && !public {
		return 0
	}
	if lm := e.header.Get("Last-Modified"); lm != "" {
		if t, err := http.ParseTime(lm); err == nil && base.After(t) {
			return base.Sub(t) / 10
		}
	}
	return 0
}

// freshened is this entry with the 304's headers layered over its own and its
// clock restarted — a new value, because the stored one may be being served
// concurrently and is treated as immutable once in the cache.
func (e *cacheEntry) freshened(h http.Header, requested time.Time) *cacheEntry {
	out := &cacheEntry{
		status:    e.status,
		header:    e.header.Clone(),
		body:      e.body,
		requested: requested,
		received:  time.Now(),
		varyKey:   e.varyKey,
	}
	for k, v := range h {
		out.header[k] = append([]string(nil), v...)
	}
	return out
}

// age is how old the stored response is now, including any Age the server
// reported when it arrived.
func (e *cacheEntry) age(now time.Time) time.Duration {
	var reported time.Duration
	if a := e.header.Get("Age"); a != "" {
		if n, err := strconv.Atoi(a); err == nil {
			reported = time.Duration(n) * time.Second
		}
	}
	return reported + now.Sub(e.received)
}

// storable reports whether a response may be kept at all. The status matters
// only when the response says nothing about its own lifetime: an explicit
// max-age or Expires makes any final status storable — the origin has said how
// long it is good for — while without one only the heuristically-cacheable
// statuses may be kept.
func storable(req *http.Request, resp *http.Response) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	// A partial response is not the resource. See the file comment.
	if resp.StatusCode == http.StatusPartialContent || req.Header.Get("Range") != "" {
		return false
	}
	if resp.StatusCode < 200 {
		return false
	}
	if !hasExplicitLifetime(resp.Header) && !heuristicStatus[resp.StatusCode] {
		// One more way in: a heuristic is allowed for any status that opts in
		// with Cache-Control: public (see freshnessLifetime).
		if _, public := directives(resp.Header)["public"]; !public {
			return false
		}
	}
	if _, ok := directives(resp.Header)["no-store"]; ok {
		return false
	}
	// Cache-Control: private is NOT a reason to refuse. It forbids SHARED
	// caches; this cache is a private one — it belongs to the single user agent
	// doing the fetching, which is exactly who private admits.
	return true
}

// ------------------------------------------------------- the transport

type cachingTransport struct {
	next  http.RoundTripper
	cache *responseCache

	// revalidating holds the cache keys with a background revalidation already
	// in flight, so a burst of requests inside the stale-while-revalidate window
	// costs one origin request rather than one each.
	revalidatingMu sync.Mutex
	revalidating   map[string]bool
}

// staleWhileRevalidate reads the response's RFC 5861 window, if it granted one.
func staleWhileRevalidate(h http.Header) (time.Duration, bool) {
	v, ok := directives(h)["stale-while-revalidate"]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

// revalidateInBackground refreshes a stale entry without making the caller
// wait. The request is rebuilt free of the caller's context — the caller may
// be gone before the origin answers, and this revalidation is the cache's
// business, not the caller's — but bounded, so an origin that never answers
// cannot accumulate goroutines.
func (t *cachingTransport) revalidateInBackground(req *http.Request, entry *cacheEntry) {
	key := cacheKey(req)
	t.revalidatingMu.Lock()
	if t.revalidating == nil {
		t.revalidating = map[string]bool{}
	}
	if t.revalidating[key] {
		t.revalidatingMu.Unlock()
		return
	}
	t.revalidating[key] = true
	t.revalidatingMu.Unlock()

	out := req.Clone(context.Background())
	out.Body = nil
	if etag := entry.header.Get("ETag"); etag != "" {
		out.Header.Set("If-None-Match", etag)
	} else if lm := entry.header.Get("Last-Modified"); lm != "" {
		out.Header.Set("If-Modified-Since", lm)
	}
	go func() {
		defer func() {
			t.revalidatingMu.Lock()
			delete(t.revalidating, key)
			t.revalidatingMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		requested := time.Now()
		resp, err := t.next.RoundTrip(out.WithContext(ctx))
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotModified {
			t.cache.put(req, entry.freshened(resp.Header, requested))
			return
		}
		if !storable(req, resp) {
			return
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxEntryBytes))
		if err != nil {
			return
		}
		t.cache.put(req, &cacheEntry{
			status: resp.StatusCode, header: resp.Header.Clone(), body: body,
			requested: requested, received: time.Now(),
		})
	}()
}

// serve turns a stored entry back into a response. The body is a fresh reader
// over the stored bytes each time, so two callers cannot consume one another's.
func (e *cacheEntry) serve(req *http.Request) *http.Response {
	h := make(http.Header, len(e.header))
	for k, v := range e.header {
		h[k] = append([]string(nil), v...)
	}
	return &http.Response{
		Status:        strconv.Itoa(e.status) + " " + http.StatusText(e.status),
		StatusCode:    e.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader(e.body)),
		ContentLength: int64(len(e.body)),
		Request:       req,
	}
}

// addModeHeaders applies the request headers the mode implies. These are the
// standard's own words: a reload says "no-cache" to everything in the path, and
// a no-cache says "max-age=0", which is the difference between "do not use a
// stored response" and "do not use one without checking".
func addModeHeaders(req *http.Request, mode cacheMode) {
	switch mode {
	case cacheNoStore, cacheReload:
		if req.Header.Get("Pragma") == "" {
			req.Header.Set("Pragma", "no-cache")
		}
		if req.Header.Get("Cache-Control") == "" {
			req.Header.Set("Cache-Control", "no-cache")
		}
	case cacheNoCache:
		if req.Header.Get("Cache-Control") == "" {
			req.Header.Set("Cache-Control", "max-age=0")
		}
	}
}

// conditionalHeaders are the request headers that make the request its own
// validation. A request carrying one is asking the ORIGIN a question about the
// resource's state, so the cache must not answer for it.
var conditionalHeaders = []string{
	"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range",
}

func isConditional(h http.Header) bool {
	for _, name := range conditionalHeaders {
		if h.Get(name) != "" {
			return true
		}
	}
	return false
}

// reqDirectives is the caller's own Cache-Control (RFC 9111 §5.2.1) — the
// header a page author writes on a fetch, distinct from fetch's cache MODE.
// Both express constraints on the cache; the header is the finer instrument.
type reqDirectives struct {
	noCache, noStore, onlyIfCached bool
	maxAge                         time.Duration
	hasMaxAge                      bool
	maxStale                       time.Duration
	hasMaxStale, maxStaleAny       bool
	minFresh                       time.Duration
	hasMinFresh                    bool
}

func parseReqDirectives(h http.Header) reqDirectives {
	var rd reqDirectives
	cc := directives(h)
	_, rd.noCache = cc["no-cache"]
	_, rd.noStore = cc["no-store"]
	_, rd.onlyIfCached = cc["only-if-cached"]
	if v, ok := cc["max-age"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			rd.maxAge, rd.hasMaxAge = time.Duration(n)*time.Second, true
		}
	}
	if v, ok := cc["max-stale"]; ok {
		rd.hasMaxStale = true
		if v == "" {
			// max-stale with no value accepts any staleness at all.
			rd.maxStaleAny = true
		} else if n, err := strconv.Atoi(v); err == nil {
			rd.maxStale = time.Duration(n) * time.Second
		}
	}
	if v, ok := cc["min-fresh"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			rd.minFresh, rd.hasMinFresh = time.Duration(n)*time.Second, true
		}
	}
	return rd
}

// usableWithoutValidation decides whether a stored entry may answer this
// request directly. The response's own constraints come first (its no-cache,
// its lifetime), then the request's: each directive can only make the cache
// LESS willing, except max-stale, which widens the window for an entry that is
// merely stale — never for one whose origin said must-revalidate.
func usableWithoutValidation(e *cacheEntry, rd reqDirectives, now time.Time) bool {
	cc := directives(e.header)
	if _, ok := cc["no-cache"]; ok {
		return false
	}
	age, lifetime := e.age(now), e.freshnessLifetime()
	usable := age < lifetime
	if rd.hasMaxAge && age > rd.maxAge {
		usable = false
	}
	if rd.hasMinFresh && lifetime-age < rd.minFresh {
		usable = false
	}
	if !usable && rd.hasMaxStale {
		if _, must := cc["must-revalidate"]; !must && (rd.maxStaleAny || age-lifetime <= rd.maxStale) {
			usable = true
		}
	}
	return usable
}

// gatewayTimeout is the answer RFC 9111 gives an only-if-cached request that
// nothing stored can satisfy: a synthesized 504, not a network attempt.
func gatewayTimeout(req *http.Request) *http.Response {
	return &http.Response{
		Status:        "504 Gateway Timeout",
		StatusCode:    http.StatusGatewayTimeout,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{},
		Body:          io.NopCloser(bytes.NewReader(nil)),
		ContentLength: 0,
		Request:       req,
	}
}

// invalidate drops the entries a successful unsafe request makes stale: the
// request's own URL, and any same-origin Location / Content-Location target the
// response names — those are the resources the origin just said it changed.
func (c *responseCache) invalidate(req *http.Request, resp *http.Response) {
	drop := func(u *url.URL) {
		if u == nil || u.Hostname() != req.URL.Hostname() {
			return
		}
		key := http.MethodGet + " " + u.String()
		c.mu.Lock()
		for _, old := range c.entries[key] {
			c.bytes -= len(old.body)
		}
		delete(c.entries, key)
		c.mu.Unlock()
	}
	drop(req.URL)
	for _, name := range []string{"Location", "Content-Location"} {
		if v := resp.Header.Get(name); v != "" {
			if u, err := req.URL.Parse(v); err == nil {
				drop(u)
			}
		}
	}
}

func (t *cachingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	mode := cacheModeOf(req)
	rd := parseReqDirectives(req.Header)
	consult := mode != cacheNoStore && mode != cacheReload && !rd.noStore
	store := mode != cacheNoStore && !rd.noStore
	if isConditional(req.Header) {
		// The caller wrote its own validator: it wants the origin's answer, and a
		// stored response is not that answer.
		consult, store = false, false
	}

	var entry *cacheEntry
	if consult && (req.Method == http.MethodGet || req.Method == http.MethodHead) {
		entry = t.cache.get(req)
	}
	if entry != nil {
		switch mode {
		case cacheForceCache, cacheOnlyIfCached:
			// Both take a stored response however stale it is; they differ only in
			// what happens when there is none.
			return entry.serve(req), nil
		case cacheDefault:
			// The request's own only-if-cached directive takes whatever is stored;
			// its no-cache forbids answering without revalidating, whatever the
			// entry's freshness.
			if rd.onlyIfCached {
				return entry.serve(req), nil
			}
			now := time.Now()
			if !rd.noCache && usableWithoutValidation(entry, rd, now) {
				return entry.serve(req), nil
			}
			// stale-while-revalidate (RFC 5861): within the window the response
			// granted, a stale entry is served IMMEDIATELY and the revalidation
			// happens behind the caller's back — that trade of staleness for
			// latency is the whole point of the directive. Only when the request
			// itself imposes no freshness demands of its own.
			if !rd.noCache && !rd.hasMaxAge && !rd.hasMinFresh {
				if swr, ok := staleWhileRevalidate(entry.header); ok &&
					entry.age(now)-entry.freshnessLifetime() <= swr {
					t.revalidateInBackground(req, entry)
					return entry.serve(req), nil
				}
			}
		}
	}
	if entry == nil && mode == cacheOnlyIfCached {
		// The standard makes this a network error rather than an empty response:
		// the caller asked for the cache and there is nothing there.
		return nil, errOnlyIfCached
	}
	if entry == nil && rd.onlyIfCached && mode != cacheNoStore && mode != cacheReload {
		// As a HEADER, only-if-cached answers differently than the fetch MODE
		// does: HTTP's own rule is a synthesized 504, not an error.
		return gatewayTimeout(req), nil
	}

	out := req.Clone(req.Context())
	addModeHeaders(out, mode)
	// A stale entry with a validator is revalidated rather than refetched, which
	// is the whole economy of an HTTP cache: the server answers 304 and no body
	// crosses the network.
	if entry != nil && mode != cacheReload {
		if etag := entry.header.Get("ETag"); etag != "" && out.Header.Get("If-None-Match") == "" {
			out.Header.Set("If-None-Match", etag)
		} else if lm := entry.header.Get("Last-Modified"); lm != "" && out.Header.Get("If-Modified-Since") == "" {
			out.Header.Set("If-Modified-Since", lm)
		}
	}

	requested := time.Now()
	resp, err := t.next.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	// A successful unsafe request invalidates what it changed: its own URL, and
	// the Location / Content-Location resources the response names.
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
	default:
		if resp.StatusCode < 400 {
			t.cache.invalidate(req, resp)
		}
	}
	if resp.StatusCode == http.StatusNotModified && entry != nil {
		// Freshen the stored headers and serve the stored body. The response's
		// own headers win where it sends them, since they are the newer word.
		// The freshening builds a NEW entry rather than mutating the stored one:
		// another request may be serving that entry's headers right now.
		resp.Body.Close()
		fresh := entry.freshened(resp.Header, requested)
		if store {
			t.cache.put(req, fresh)
		}
		return fresh.serve(req), nil
	}
	if !store || !storable(req, resp) {
		return resp, nil
	}
	// The body is stored as the caller reads it, not read ahead of them. Reading
	// it here first would turn every cacheable response into a buffered one, which
	// is exactly what a streaming proxy must not do — and a cache is no reason for
	// the first byte to wait for the last.
	stored := &cacheEntry{
		status:    resp.StatusCode,
		header:    resp.Header.Clone(),
		requested: requested,
		received:  time.Now(),
	}
	resp.Body = &cacheFiller{
		rc:    resp.Body,
		limit: maxEntryBytes,
		// The declared length is how completeness is recognised without an EOF: a
		// consumer that reads exactly Content-Length bytes and closes never sees
		// one, and that consumer is the common case here.
		expect: resp.ContentLength,
		done: func(body []byte) {
			stored.body = body
			t.cache.put(req, stored)
		},
	}
	return resp, nil
}

// maxEntryBytes caps one stored body. A response larger than this streams
// through unstored: holding it would evict everything else to cache one thing.
const maxEntryBytes = maxCacheBytes / 4

// cacheFiller copies what the caller reads into a buffer, and stores the entry
// once the body has been read to its end. A body that errors, or outgrows the
// cap, is simply not stored — the caller's read is unaffected either way.
type cacheFiller struct {
	rc     io.ReadCloser
	buf    bytes.Buffer
	limit  int
	expect int64 // the declared Content-Length, or -1 when unknown
	done   func([]byte)
	giveUp bool
	stored bool
}

// finish stores the body, once. A body that errored, outgrew the cap, or is
// short of its declared length is not stored: half a response is not a response.
func (f *cacheFiller) finish() {
	if f.giveUp || f.stored {
		return
	}
	f.stored = true
	f.done(append([]byte(nil), f.buf.Bytes()...))
}

func (f *cacheFiller) Read(p []byte) (int, error) {
	n, err := f.rc.Read(p)
	if n > 0 && !f.giveUp {
		if f.buf.Len()+n > f.limit {
			f.giveUp = true
			f.buf.Reset()
		} else {
			f.buf.Write(p[:n])
		}
	}
	if err == io.EOF {
		f.finish()
	} else if err != nil {
		f.giveUp = true
	} else if f.expect >= 0 && int64(f.buf.Len()) >= f.expect {
		// Every byte the response promised has arrived; a consumer that stops here
		// never calls Read again.
		f.finish()
	}
	return n, err
}

func (f *cacheFiller) Close() error {
	if f.expect >= 0 && int64(f.buf.Len()) >= f.expect {
		f.finish()
	}
	return f.rc.Close()
}
