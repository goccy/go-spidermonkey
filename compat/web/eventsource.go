package web

// eventsource.go: the Go half of EventSource
// (https://html.spec.whatwg.org/multipage/server-sent-events.html).
//
// The split is the same as WebSocket's: the guest owns the API shape
// (js/eventsource.js — constructor validation, readyState, the event handler
// attributes), and this side owns everything that runs or blocks: the request,
// the stream parser, and the reconnection schedule. The parsing ALGORITHM —
// the event stream format's line and field rules — is the part that must not
// be approximated, and it lives here as a small state machine over the bytes.
//
// The connection uses the same permission-checked dialer as fetch and
// WebSocket, so an EventSource cannot reach an origin fetch could not.

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/internal/eventloop"
)

type esAPI struct {
	js       *spidermonkey.JS
	loop     *eventloop.Loop
	dispatch *spidermonkey.Object // __es_dispatch(id, kind, a, b, c)

	roots      *x509.CertPool
	clientOnce sync.Once
	client     *http.Client

	mu    sync.Mutex
	conns map[int64]*esConn
	next  int64
}

type esConn struct {
	api    *esAPI
	id     int64
	ctx    context.Context
	cancel context.CancelFunc
	url    string

	mu       sync.Mutex
	sentDone bool
}

func installEventSource(js *spidermonkey.JS, loop *eventloop.Loop, roots *x509.CertPool) (*esAPI, error) {
	a := &esAPI{js: js, loop: loop, roots: roots, conns: map[int64]*esConn{}}
	v, err := js.Global().Get("__es_dispatch")
	if err != nil {
		return nil, err
	}
	o := v.Object()
	if o == nil || !o.IsFunction() {
		return nil, fmt.Errorf("web: __es_dispatch is not a function")
	}
	a.dispatch = o
	for name, fn := range map[string]spidermonkey.Func{
		"__es_connect": a.opConnect,
		"__es_close":   a.opClose,
	} {
		if err := js.Global().DefineFunc(name, fn); err != nil {
			return nil, err
		}
	}
	return a, nil
}

func (a *esAPI) closeAll() {
	a.mu.Lock()
	conns := make([]*esConn, 0, len(a.conns))
	for _, c := range a.conns {
		conns = append(conns, c)
	}
	a.conns = map[int64]*esConn{}
	a.mu.Unlock()
	for _, c := range conns {
		c.cancel()
	}
}

// __es_connect(url) starts the connection and returns the handle. The URL has
// already been parsed and serialized by the guest constructor — that is where
// the specification raises its SyntaxError.
func (a *esAPI) opConnect(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	a.clientOnce.Do(func() {
		// EventSource shares fetch's caching-transport semantics deliberately NOT:
		// an event stream must never be answered from a cache, and the request
		// carries Cache-Control: no-cache to say so. A plain transport suffices.
		a.client = &http.Client{Transport: &http.Transport{
			DialContext: permissionDial(cfg), TLSClientConfig: tlsConfig(a.roots),
		}}
	})
	if len(args) < 1 {
		return nil, fmt.Errorf("__es_connect: a URL is required")
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.next++
	c := &esConn{api: a, id: a.next, ctx: ctx, cancel: cancel, url: args[0].String()}
	a.conns[c.id] = c
	a.mu.Unlock()
	// An open EventSource is a live handle: it keeps the loop running the same
	// way an open socket does, released when the connection is failed or closed.
	a.loop.AddPending("eventsource")
	go c.run()
	return spidermonkey.ValueOf(float64(c.id)), nil
}

// __es_close(id) is the guest's close(): the connection is simply abandoned.
func (a *esAPI) opClose(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	a.mu.Lock()
	c := a.conns[int64(args[0].Float())]
	delete(a.conns, int64(args[0].Float()))
	a.mu.Unlock()
	if c != nil {
		c.cancel()
		c.release()
	}
	return spidermonkey.Undefined(), nil
}

// release gives back the loop-pending exactly once, whichever of close, fatal
// failure or teardown gets there first.
func (c *esConn) release() {
	c.mu.Lock()
	done := c.sentDone
	c.sentDone = true
	c.mu.Unlock()
	if !done {
		c.api.loop.DonePending("eventsource")
	}
}

// run drives the connection for the EventSource's whole life: connect, stream,
// and — when the stream ends or the network fails — reconnect after the
// current retry delay, carrying the last seen event id.
func (c *esConn) run() {
	// The reconnection time. A retry: field changes it for every later attempt.
	retry := 3 * time.Second
	lastEventID := ""
	for {
		p := &esParser{
			conn:     c,
			id:       lastEventID,
			idBuffer: lastEventID,
			retry:    &retry,
		}
		outcome := c.once(p)
		lastEventID = p.id
		switch outcome {
		case esClosed:
			c.release()
			return
		case esFailed:
			// "Fail the connection": one error event, readyState CLOSED, and no
			// reconnection — this is what a wrong status or MIME type earns.
			c.post("error", spidermonkey.ValueOf(true))
			c.release()
			return
		case esReconnect:
			// "Reestablish the connection": an error event with readyState back
			// to CONNECTING, then a delay, then a fresh request.
			c.post("error", spidermonkey.ValueOf(false))
			select {
			case <-time.After(retry):
			case <-c.ctx.Done():
				c.release()
				return
			}
		}
	}
}

type esOutcome int

const (
	esReconnect esOutcome = iota
	esFailed
	esClosed
)

// once makes one request and consumes its stream to the end.
func (c *esConn) once(p *esParser) esOutcome {
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return esFailed
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if p.id != "" {
		req.Header.Set("Last-Event-ID", p.id)
	}
	resp, err := c.api.client.Do(req)
	if err != nil {
		if c.ctx.Err() != nil {
			return esClosed
		}
		// A network error reestablishes; only a wrong ANSWER fails.
		return esReconnect
	}
	defer resp.Body.Close()
	// The specification's test is exact: status 200 and a Content-Type whose
	// essence is text/event-stream. Anything else fails the connection —
	// including 204, which used to mean "close politely" in older drafts.
	essence, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK || essence != "text/event-stream" {
		return esFailed
	}
	c.post("open")
	buf := make([]byte, 8<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			p.feed(buf[:n])
		}
		if err != nil {
			if c.ctx.Err() != nil {
				return esClosed
			}
			if err == io.EOF {
				return esReconnect
			}
			return esReconnect
		}
	}
}

// post calls the guest dispatcher on the loop goroutine. A connection the
// guest has closed stays silent: its events are already history.
func (c *esConn) post(kind string, args ...spidermonkey.Value) {
	c.api.loop.Post(func() error {
		c.mu.Lock()
		done := c.sentDone
		c.mu.Unlock()
		if done && kind != "error" {
			return nil
		}
		all := append([]spidermonkey.Value{spidermonkey.ValueOf(float64(c.id)), spidermonkey.ValueOf(kind)}, args...)
		_, err := c.api.dispatch.Call(all...)
		return err
	})
}

// esParser is the event stream format, incrementally: bytes arrive in whatever
// chunks the network delivers, and events fire as their blank line arrives —
// not at end of stream, which for a live stream never comes.
type esParser struct {
	conn *esConn

	line    []byte // the partial line a chunk boundary split
	sawCR   bool   // the last byte processed was a CR (a following LF is part of it)
	first   bool   // BOM not yet considered
	started bool

	data      strings.Builder
	hasData   bool
	eventType string
	// idBuffer holds an id: line that has not been COMMITTED yet. The
	// specification commits the buffer to the source's last event ID at
	// dispatch time — the blank line — not when the id line is parsed, so an
	// id in a block the stream never finished is never sent back on
	// reconnection and never appears on an event.
	idBuffer string
	id       string // the committed last event ID, live across reconnections
	retry    *time.Duration
}

func (p *esParser) feed(chunk []byte) {
	if !p.started {
		p.started = true
		// One leading BOM is not content.
		if len(chunk) >= 3 && chunk[0] == 0xEF && chunk[1] == 0xBB && chunk[2] == 0xBF {
			chunk = chunk[3:]
		}
	}
	for _, b := range chunk {
		switch b {
		case '\r':
			p.endLine()
			p.sawCR = true
		case '\n':
			if p.sawCR {
				// The LF of a CRLF; its line already ended at the CR.
				p.sawCR = false
				continue
			}
			p.endLine()
		default:
			p.sawCR = false
			p.line = append(p.line, b)
		}
	}
}

func (p *esParser) endLine() {
	p.sawCR = false
	line := string(p.line)
	p.line = p.line[:0]
	if line == "" {
		p.dispatch()
		return
	}
	if line[0] == ':' {
		return
	}
	field, value, hadColon := strings.Cut(line, ":")
	if !hadColon {
		field, value = line, ""
	}
	// Exactly one leading space belongs to the syntax; a second one is data.
	value = strings.TrimPrefix(value, " ")
	// Field names are CASE-SENSITIVE: "Data" is an unknown field, ignored.
	switch field {
	case "data":
		p.data.WriteString(value)
		p.data.WriteString("\n")
		p.hasData = true
	case "event":
		p.eventType = value
	case "id":
		// An id holding a NULL is ignored wholesale, not sanitized.
		if !strings.ContainsRune(value, 0) {
			p.idBuffer = value
		}
	case "retry":
		if value != "" && strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
			var ms int64
			for _, r := range value {
				ms = ms*10 + int64(r-'0')
			}
			*p.retry = time.Duration(ms) * time.Millisecond
		}
	}
}

// dispatch fires the buffered event, if there is one to fire. The id buffer
// commits FIRST: a blank line after a lone id: line updates the last event ID
// even though no event goes out.
func (p *esParser) dispatch() {
	p.id = p.idBuffer
	if !p.hasData {
		// No data means no event — but the type buffer still resets.
		p.eventType = ""
		return
	}
	data := strings.TrimSuffix(p.data.String(), "\n")
	eventType := p.eventType
	p.data.Reset()
	p.hasData = false
	p.eventType = ""
	p.conn.post("message",
		spidermonkey.ValueOf(eventType), spidermonkey.ValueOf(data), spidermonkey.ValueOf(p.id))
}
