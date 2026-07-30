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

type responseCache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
	bytes   int
}

func newResponseCache() *responseCache {
	return &responseCache{entries: map[string]*cacheEntry{}}
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
	e := c.entries[cacheKey(req)]
	if e == nil {
		return nil
	}
	want, ok := varyKeyFor(e.header.Get("Vary"), req.Header)
	if !ok || want != e.varyKey {
		return nil
	}
	return e
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
	if old := c.entries[cacheKey(req)]; old != nil {
		c.bytes -= len(old.body)
	}
	c.entries[cacheKey(req)] = e
	c.bytes += len(e.body)
	// Evict oldest-first until under the ceiling.
	for c.bytes > maxCacheBytes && len(c.entries) > 1 {
		var oldestKey string
		oldest := time.Now().Add(time.Hour)
		for k, v := range c.entries {
			if v.stored.Before(oldest) {
				oldestKey, oldest = k, v.stored
			}
		}
		c.bytes -= len(c.entries[oldestKey].body)
		delete(c.entries, oldestKey)
	}
}

func (c *responseCache) reset() {
	c.mu.Lock()
	c.entries = map[string]*cacheEntry{}
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

// freshnessLifetime is how long the response may be reused without asking,
// from max-age if given and from Expires otherwise. There is no heuristic
// fallback: a response that says nothing about its lifetime is treated as
// already stale, so it is revalidated rather than guessed about.
func (e *cacheEntry) freshnessLifetime() time.Duration {
	cc := directives(e.header)
	if v, ok := cc["max-age"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	exp := e.header.Get("Expires")
	if exp == "" {
		return 0
	}
	t, err := http.ParseTime(exp)
	if err != nil {
		// An unparseable Expires means already expired, per the standard.
		return 0
	}
	base := e.received
	if d := e.header.Get("Date"); d != "" {
		if dt, derr := http.ParseTime(d); derr == nil {
			base = dt
		}
	}
	if t.Before(base) {
		return 0
	}
	return t.Sub(base)
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

func (e *cacheEntry) fresh(now time.Time) bool {
	cc := directives(e.header)
	if _, ok := cc["no-cache"]; ok {
		return false
	}
	if _, ok := cc["must-revalidate"]; ok && e.age(now) >= e.freshnessLifetime() {
		return false
	}
	return e.age(now) < e.freshnessLifetime()
}

// storable reports whether a response may be kept at all.
func storable(req *http.Request, resp *http.Response) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	// A partial response is not the resource. See the file comment.
	if resp.StatusCode == http.StatusPartialContent || req.Header.Get("Range") != "" {
		return false
	}
	switch resp.StatusCode {
	case 200, 203, 204, 300, 301, 304, 404, 405, 410, 414, 501:
	default:
		return false
	}
	cc := directives(resp.Header)
	if _, ok := cc["no-store"]; ok {
		return false
	}
	if _, ok := cc["private"]; ok {
		return false
	}
	return true
}

// ------------------------------------------------------- the transport

type cachingTransport struct {
	next  http.RoundTripper
	cache *responseCache
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

func (t *cachingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	mode := cacheModeOf(req)
	consult := mode != cacheNoStore && mode != cacheReload
	store := mode != cacheNoStore
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
			if entry.fresh(time.Now()) {
				return entry.serve(req), nil
			}
		}
	}
	if entry == nil && mode == cacheOnlyIfCached {
		// The standard makes this a network error rather than an empty response:
		// the caller asked for the cache and there is nothing there.
		return nil, errOnlyIfCached
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
	if resp.StatusCode == http.StatusNotModified && entry != nil {
		// Freshen the stored headers and serve the stored body. The response's
		// own headers win where it sends them, since they are the newer word.
		resp.Body.Close()
		for k, v := range resp.Header {
			entry.header[k] = append([]string(nil), v...)
		}
		entry.requested, entry.received = requested, time.Now()
		if store {
			t.cache.put(req, entry)
		}
		return entry.serve(req), nil
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
