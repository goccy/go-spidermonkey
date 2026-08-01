package web

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"github.com/andybalholm/brotli"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/internal/compress"
	"io"
	"sync"
)

// compression.go: the host half of CompressionStream/DecompressionStream.
//
// These are WinterTC web APIs, so they belong here. They used to exist only in
// compat/nodejs (because that is where the zlib host op lived), which left a
// web-only embedding without them and scored zero on the whole WPT
// `compression` directory. The codec itself is shared —
// compat/internal/compress — so node:zlib and this are the same implementation.

// opMIMEType normalizes a media type the way the platform does everywhere it
// appears: parsed and serialized back, or "" when it does not parse. Blob,
// File, Request and Response all report their type in that form, and the
// mimesniff tests check every one of them against the same table.
func (w *Web) opMIMEType(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.ValueOf(""), nil
	}
	m, ok := parseMIMEType(args[0].String())
	if !ok {
		return spidermonkey.ValueOf(""), nil
	}
	return spidermonkey.ValueOf(m.String()), nil
}

// opCompress runs one buffer through a named codec ("gzip", "gunzip",
// "deflate", "inflate", "deflateRaw", "inflateRaw"). A codec failure — corrupt
// input to DecompressionStream, most often — comes back as an error object the
// guest turns into a TypeError on the stream, not as a host error.
func (w *Web) opCompress(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("compress: (method, data) required")
	}
	data, err := argBytes(args[1])
	if err != nil {
		return nil, err
	}
	out, err := compress.Run(args[0].String(), data)
	if err != nil {
		return spidermonkey.ValueOf(map[string]any{"error": err.Error()}), nil
	}
	u8, err := w.js.NewBytes(out)
	if err != nil {
		return nil, err
	}
	return u8, nil
}

// ------------------------- CompressionStream and DecompressionStream as
// STREAMS.
//
// The whole-buffer codec behind opCompress cannot serve these: a
// DecompressionStream must hand its reader output as soon as the input allows
// it, and a consumer that writes one chunk and then reads — which is what
// almost every test in compression/ does, and what any real pipeline does —
// waits forever for a codec that only produces at close.
//
// So a codec here is a HANDLE with state: created, fed, and finished. The
// encoders are push-based and need no goroutine. The decoders PULL — a
// gzip.Reader reads from a source — so each runs on its own goroutine and takes
// its input through a rendezvous rather than a pipe: a push has to return
// everything its bytes produced, and only the decoder asking for more input
// establishes that. Both report bytes as they become available.
//
// That rendezvous only says what it means if the decoder never reads AHEAD of
// what it can already emit, which is why a gzip.Reader here is single-member:
// left in its default multistream mode it goes looking for another member's
// header before handing back the member it has already decoded, and every
// single-chunk decompression came back empty. A single member is also what the
// standard asks for — bytes after it are junk, not a second stream.

// maxCodecOutput bounds what one codec may produce in total. A compressed
// stream can expand enormously — that is what a decompression bomb is — and a
// guest supplying the input must not be able to exhaust the host through it.
const maxCodecOutput = 256 << 20

// codec is one in-flight compression or decompression.
type codec struct {
	mu sync.Mutex
	// For an encoder: the writer to feed, and the buffer it writes into.
	enc interface {
		io.WriteCloser
		Flush() error
	}
	buf *bytes.Buffer
	// For a decoder: the rendezvous with its goroutine, and the channel its
	// output arrives on.
	feed *feedReader
	out  chan []byte
	// pending records that the decoder has asked for input and is waiting to be
	// answered. Only the guest's thread touches it — every op runs there — so it
	// needs no lock.
	pending bool
	// ended records that the decoder's goroutine has finished — the output
	// channel is closed. Read from the guest's thread only, like pending.
	ended bool
	freed bool
	total int
	// failed is what stopped the decoder badly. Reaching the end of the stream is
	// NOT a failure: a deflate stream ends at its final block, which arrives while
	// the push that carried it is still being collected.
	failed error
}

// errStreamEnded is what a push finds once the decoder has consumed a complete
// stream. Bytes after the end are the guest's error, and the standard says so.
var errStreamEnded = errors.New("junk found after the end of the compressed stream")

func (c *codec) fail(err error) {
	c.mu.Lock()
	if c.failed == nil {
		c.failed = err
	}
	c.mu.Unlock()
}

// feedReader is the input side of a decoder goroutine.
//
// A decoder PULLS: it reads whenever it wants more input, and the host has to
// know when that happens. Every read this cannot answer from what it already
// holds asks on req and then waits on data, so the decoder is only ever found in
// one of two states — asking, or finished. That makes "this push has produced
// everything it can" an OBSERVATION. A pipe underneath could not say it: a write
// returning means the bytes were taken, not that they were decoded, so every
// push's result depended on how the two goroutines happened to interleave.
type feedReader struct {
	req  chan struct{}
	data chan []byte
	quit chan struct{}
	cur  []byte
	eof  bool
}

// Read and ReadByte together satisfy flate.Reader, which is what keeps the
// compress/* decoders from wrapping this in a bufio of their own. That wrapper
// would read AHEAD — up to 4 KiB past the stream — and the bytes it swallowed
// would be invisible here, so trailing junk could not be reported as the error
// the standard says it is.
func (f *feedReader) ReadByte() (byte, error) {
	if err := f.ensure(); err != nil {
		return 0, err
	}
	b := f.cur[0]
	f.cur = f.cur[1:]
	return b, nil
}

func (f *feedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := f.ensure(); err != nil {
		return 0, err
	}
	n := copy(p, f.cur)
	f.cur = f.cur[n:]
	return n, nil
}

// ensure blocks until there is input in hand, asking for more when there is not.
func (f *feedReader) ensure() error {
	for len(f.cur) == 0 {
		if f.eof {
			return io.EOF
		}
		select {
		case f.req <- struct{}{}:
		case <-f.quit:
			f.eof = true
			return io.EOF
		}
		select {
		case b := <-f.data:
			// An empty answer is the end of the input, which is how finish and
			// cancellation both retire the decoder.
			if len(b) == 0 {
				f.eof = true
				return io.EOF
			}
			f.cur = b
		case <-f.quit:
			f.eof = true
			return io.EOF
		}
	}
	return nil
}

func (c *codec) failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failed
}

type codecAPI struct {
	js *spidermonkey.JS
	mu sync.Mutex
	// codecs by handle. The guest holds the handle; nothing here outlives the
	// installation.
	codecs map[int64]*codec
	next   int64
}

func newCodecAPI(js *spidermonkey.JS) *codecAPI {
	return &codecAPI{js: js, codecs: map[int64]*codec{}}
}

func (a *codecAPI) ops() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"codec_new":    a.opNew,
		"codec_push":   a.opPush,
		"codec_finish": a.opFinish,
		"codec_free":   a.opFree,
	}
}

// opNew(format, decompress) -> handle.
func (a *codecAPI) opNew(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("codec_new: (format, decompress) required")
	}
	format := args[0].String()
	c := &codec{}
	if args[1].Bool() {
		c.feed = &feedReader{
			req:  make(chan struct{}),
			data: make(chan []byte),
			quit: make(chan struct{}),
		}
		src := io.Reader(c.feed)
		c.out = make(chan []byte, 64)
		switch format {
		case "gzip":
			// gzip.NewReader reads the header eagerly, and on this goroutine that
			// blocks until the first chunk arrives — which is what is wanted: the
			// header IS the first bytes.
			go func() {
				r, rerr := gzip.NewReader(src)
				if rerr != nil {
					c.fail(rerr)
					close(c.out)
					return
				}
				r.Multistream(false)
				pumpCodec(r, c)
			}()
		case "deflate":
			go func() {
				r, rerr := zlib.NewReader(src)
				if rerr != nil {
					c.fail(rerr)
					close(c.out)
					return
				}
				pumpCodec(r, c)
			}()
		case "deflate-raw":
			go pumpCodec(flate.NewReader(src), c)
		case "brotli":
			// Not in the standard's format list, but this runtime has offered it
			// since before that list existed and a caller depends on it.
			go pumpCodec(brotli.NewReader(src), c)
		default:
			return spidermonkey.ValueOf(map[string]any{"error": "unsupported format " + format}), nil
		}
		return a.register(c), nil
	}
	c.buf = &bytes.Buffer{}
	switch format {
	case "gzip":
		c.enc = gzip.NewWriter(c.buf)
	case "deflate":
		c.enc = zlib.NewWriter(c.buf)
	case "deflate-raw":
		w, err := flate.NewWriter(c.buf, flate.DefaultCompression)
		if err != nil {
			return spidermonkey.ValueOf(map[string]any{"error": err.Error()}), nil
		}
		c.enc = w
	case "brotli":
		c.enc = brotli.NewWriter(c.buf)
	default:
		return spidermonkey.ValueOf(map[string]any{"error": "unsupported format " + format}), nil
	}
	return a.register(c), nil
}

func (a *codecAPI) register(c *codec) spidermonkey.Value {
	a.mu.Lock()
	a.next++
	id := a.next
	a.codecs[id] = c
	a.mu.Unlock()
	return spidermonkey.ValueOf(float64(id))
}

func (a *codecAPI) get(v spidermonkey.Value) *codec {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.codecs[int64(v.Float())]
}

// pumpCodec drains a decoder into the codec's output channel until it ends,
// then closes that channel — which is how the ops learn the stream is over,
// whether it ended at its own last byte or at a defect in it.
func pumpCodec(r io.Reader, c *codec) {
	defer close(c.out)
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			c.mu.Lock()
			c.total += n
			over := c.total > maxCodecOutput
			c.mu.Unlock()
			if over {
				c.fail(fmt.Errorf("decompressed output exceeds %d bytes", maxCodecOutput))
				return
			}
			select {
			case c.out <- append([]byte(nil), buf[:n]...):
			case <-c.feed.quit:
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				c.fail(err)
			}
			return
		}
	}
}

// awaitReq waits until the decoder asks for input, appending whatever it emits
// on the way. It reports false when the decoder stopped instead of asking.
func awaitReq(c *codec, all *[]byte) bool {
	if c.pending {
		return true
	}
	for {
		select {
		case chunk, ok := <-c.out:
			if !ok {
				c.ended = true
				return false
			}
			*all = append(*all, chunk...)
		case <-c.feed.req:
			c.pending = true
			return true
		}
	}
}

// drainQueued takes what the decoder emitted before it asked for more. The
// output channel is buffered, so a chunk and the request that follows it are
// both ready at once and the select above may see the request first.
func drainQueued(c *codec, all *[]byte) bool {
	for {
		select {
		case chunk, ok := <-c.out:
			if !ok {
				c.ended = true
				return false
			}
			*all = append(*all, chunk...)
		default:
			return true
		}
	}
}

// opPush(handle, bytes) -> the output that became available, or an error.
func (a *codecAPI) opPush(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	c := a.get(args[0])
	if c == nil {
		return nil, fmt.Errorf("codec_push: unknown codec")
	}
	data, err := argBytes(args[1])
	if err != nil {
		return nil, err
	}
	if c.enc != nil {
		if _, werr := c.enc.Write(data); werr != nil {
			return a.result(nil, werr)
		}
		// Flush so a caller that writes and then reads sees something. Without it
		// a deflate encoder buffers a whole window and the reader waits.
		if ferr := c.enc.Flush(); ferr != nil {
			return a.result(nil, ferr)
		}
		return a.take(c)
	}
	// A decoder that has already stopped answers with why rather than being fed:
	// what it could not decode it will not decode later either.
	if err := c.failure(); err != nil {
		return a.result(nil, err)
	}
	if len(data) == 0 {
		// An empty chunk is legal and means nothing. It must not be handed over:
		// an empty answer is how the decoder is told the input has ended.
		return a.result(nil, nil)
	}
	return a.collect(c, data)
}

// opFinish(handle) -> the remaining output, or an error. The codec is done.
func (a *codecAPI) opFinish(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	c := a.get(args[0])
	if c == nil {
		return nil, fmt.Errorf("codec_finish: unknown codec")
	}
	if c.enc != nil {
		if cerr := c.enc.Close(); cerr != nil {
			return a.result(nil, cerr)
		}
		return a.take(c)
	}
	var all []byte
	if awaitReq(c, &all) {
		// Tell the decoder there is no more input. A format that was mid-stream
		// reports that as a defect, which is what an unterminated stream is.
		c.pending = false
		c.feed.data <- nil
	}
	for chunk := range c.out {
		all = append(all, chunk...)
	}
	c.ended = true
	return a.result(all, c.stopReason())
}

// collect hands one push's bytes to the decoder and gathers everything they
// produce. It returns when the decoder asks for input AGAIN — the point at which
// those bytes are spent — or when the decoder stops.
func (a *codecAPI) collect(c *codec, data []byte) (spidermonkey.Value, error) {
	var all []byte
	if !awaitReq(c, &all) {
		// The decoder stopped before it could take these bytes, so they follow a
		// stream that is already over.
		if err := c.failure(); err != nil {
			return a.result(all, err)
		}
		return a.result(all, errStreamEnded)
	}
	c.pending = false
	c.feed.data <- data
	if awaitReq(c, &all) {
		drainQueued(c, &all)
	}
	return a.result(all, c.stopReason())
}

// stopReason is what a decoder that has stopped should be reported as: the
// defect that stopped it, or — when it stopped because the stream was complete
// — whatever of the input it never reached. Nothing at all while it is running.
func (c *codec) stopReason() error {
	if err := c.failure(); err != nil {
		return err
	}
	if !c.ended {
		return nil
	}
	// The goroutine is gone, so its input buffer is this thread's to look at.
	if len(c.feed.cur) > 0 {
		return errStreamEnded
	}
	return nil
}

// take returns and clears an encoder's pending output.
func (a *codecAPI) take(c *codec) (spidermonkey.Value, error) {
	out := append([]byte(nil), c.buf.Bytes()...)
	c.buf.Reset()
	return a.result(out, nil)
}

// result is what every op answers with: the output, and what went wrong. BOTH
// can be present — a stream with junk after its end produced real bytes first,
// and the guest has to deliver those before it errors the stream — so this is
// one shape rather than "bytes or an error".
func (a *codecAPI) result(b []byte, err error) (spidermonkey.Value, error) {
	// Built member by member rather than from a Go map: the output is a
	// Uint8Array the guest already holds, and a map crosses as data.
	out, nerr := a.js.NewObject()
	if nerr != nil {
		return nil, nerr
	}
	if len(b) > 0 {
		u8, berr := a.js.NewBytes(b)
		if berr != nil {
			return nil, berr
		}
		serr := out.Set("bytes", u8)
		// The property keeps it alive guest-side; this handle would otherwise
		// stay rooted for the life of the instance, once per chunk.
		u8.Free()
		if serr != nil {
			return nil, serr
		}
	}
	if err != nil {
		if serr := out.Set("error", spidermonkey.ValueOf(err.Error())); serr != nil {
			return nil, serr
		}
	}
	return out, nil
}

// opFree(handle) releases a codec whose stream was cancelled or errored.
func (a *codecAPI) opFree(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	a.mu.Lock()
	id := int64(args[0].Float())
	c := a.codecs[id]
	delete(a.codecs, id)
	a.mu.Unlock()
	if c != nil {
		c.release()
	}
	return spidermonkey.Undefined(), nil
}

// release retires a decoder's goroutine wherever it is waiting. Without it a
// cancelled stream would leave one blocked on a request nothing will answer.
func (c *codec) release() {
	if c.feed == nil || c.freed {
		return
	}
	c.freed = true
	close(c.feed.quit)
}

// closeAll releases every codec still open.
func (a *codecAPI) closeAll() {
	a.mu.Lock()
	codecs := a.codecs
	a.codecs = map[int64]*codec{}
	a.mu.Unlock()
	for _, c := range codecs {
		c.release()
	}
}

// collectRaw is collect without the guest-value wrapping, for tests.
func (a *codecAPI) collectRaw(c *codec, data []byte) ([]byte, error) {
	var all []byte
	if !awaitReq(c, &all) {
		return all, errStreamEnded
	}
	c.pending = false
	c.feed.data <- data
	if !awaitReq(c, &all) {
		return all, c.failure()
	}
	drainQueued(c, &all)
	return all, c.failure()
}
