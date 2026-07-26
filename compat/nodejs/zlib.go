package nodejs

// zlib.go: the compression primitives behind node:zlib and the web
// Compression/DecompressionStream. One-shot buffer transforms over Go's
// compress/* plus a pure-Go brotli; the JS side wraps them as sync functions,
// callback functions, and Transform streams.

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/andybalholm/brotli"
	spidermonkey "github.com/goccy/go-spidermonkey"
)

func (rt *Runtime) zlibOps() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"zlib_transform":    rt.opZlibTransform,
		"zlib_stream_new":   rt.opZlibStreamNew,
		"zlib_stream_write": rt.opZlibStreamWrite,
		"zlib_stream_flush": rt.opZlibStreamFlush,
		"zlib_stream_end":   rt.opZlibStreamEnd,
		"zlib_stream_free":  rt.opZlibStreamFree,
	}
}

// opZlibTransform(method, data) -> Uint8Array | {code,message}. method is
// "gzip"/"gunzip"/"deflate"/"inflate"/"deflateRaw"/"inflateRaw"/
// "brotliCompress"/"brotliDecompress".
func (rt *Runtime) opZlibTransform(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("zlib_transform: (method, data) required")
	}
	method := args[0].String()
	data, err := valueBytes(args[1])
	if err != nil {
		return nil, err
	}
	out, err := zlibRun(method, data)
	if err != nil {
		return spidermonkey.ValueOf(map[string]any{"code": "Z_DATA_ERROR", "message": err.Error()}), nil
	}
	u8, err := rt.js.NewBytes(out)
	if err != nil {
		return nil, err
	}
	return rt.trackReturn(u8), nil
}

// maxZlibOutput caps decompressed output so a small "zip bomb" can't expand to
// gigabytes on the host heap before it reaches the (capped) guest memory.
const maxZlibOutput = 256 << 20 // 256 MiB

// readCapped reads r but errors past maxZlibOutput instead of allocating without
// bound.
func readCapped(r io.Reader) ([]byte, error) {
	out, err := io.ReadAll(io.LimitReader(r, maxZlibOutput+1))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > maxZlibOutput {
		return nil, fmt.Errorf("decompressed output exceeds %d bytes", maxZlibOutput)
	}
	return out, nil
}

func zlibRun(method string, data []byte) ([]byte, error) {
	switch method {
	case "gzip":
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case "gunzip":
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return readCapped(r)
	case "unzip":
		// Node's unzip auto-detects the wrapper: gzip magic (0x1f 0x8b) -> gzip,
		// otherwise a zlib (deflate) stream. A plain deflate response body must
		// decompress here too, not fail as it did when unzip only did gzip.
		if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
			r, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				return nil, err
			}
			return readCapped(r)
		}
		r, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return readCapped(r)
	case "deflate":
		var buf bytes.Buffer
		w := zlib.NewWriter(&buf)
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case "inflate":
		r, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return readCapped(r)
	case "deflateRaw":
		var buf bytes.Buffer
		w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case "inflateRaw":
		r := flate.NewReader(bytes.NewReader(data))
		defer r.Close()
		return readCapped(r)
	case "brotliCompress":
		var buf bytes.Buffer
		w := brotli.NewWriter(&buf)
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case "brotliDecompress":
		return readCapped(brotli.NewReader(bytes.NewReader(data)))
	}
	return nil, fmt.Errorf("unsupported zlib method %q", method)
}

// ------------------------------- incremental (streaming) zlib

// flushWriter is the shared surface of gzip.Writer / zlib.Writer /
// flate.Writer / brotli.Writer: Flush() gives Z_SYNC_FLUSH semantics (all
// input consumed so far becomes decodable output), Close() finalizes.
type flushWriter interface {
	io.Writer
	Flush() error
	Close() error
}

// zlibStream is one live createGzip()/createGunzip()/... Transform, host side.
// Compression is truly incremental (a persistent Go writer). Decompression is
// incremental too: a persistent decoder goroutine (zlibDecomp) consumes each
// chunk as it arrives, so N chunks cost O(N) total instead of the O(N²) a
// re-decode-the-whole-prefix scheme costs.
type zlibStream struct {
	method string // "gzip"/"deflate"/"deflateRaw"/"brotliCompress" or the decompress forms
	// compressor state
	comp flushWriter
	cbuf *bytes.Buffer // compressed output not yet handed to the guest
	// decompressor state
	dec   *zlibDecomp
	sniff []byte // "unzip" only: input held until the wrapper can be sniffed
}

// zlibDecomp is a persistent streaming decompressor: a goroutine runs the Go
// decoder (gzip/zlib/flate/brotli Reader) over input handed in via feed(),
// appending decoded bytes to out. feed() returns only once the decoder has
// consumed everything fed so far and is idle again (or has terminated), so
// after each write the accumulated output deterministically contains every
// byte decodable from the input so far — the same observable semantics the
// old re-decode scheme had, at linear instead of quadratic cost.
type zlibDecomp struct {
	mu       sync.Mutex
	cond     *sync.Cond
	buf      []byte // input fed but not yet consumed by the decoder
	closed   bool   // end(): decoder sees EOF once buf drains
	abortErr error  // destroy/teardown: decoder aborts immediately
	drained  bool   // decoder is blocked waiting for more input
	finished bool   // decoder goroutine has exited
	out      []byte // decoded output not yet handed to the guest
	err      error  // terminal decode error (set when the goroutine exits)
	done     chan struct{}
}

// errZlibStreamDestroyed aborts the decoder goroutine on destroy()/teardown.
var errZlibStreamDestroyed = errors.New("zlib stream destroyed")

func newZlibDecomp(method string) *zlibDecomp {
	d := &zlibDecomp{done: make(chan struct{})}
	d.cond = sync.NewCond(&d.mu)
	go d.run(method)
	return d
}

// Read is the decoder's input source (io.Reader over the fed chunks). It
// blocks until input arrives, end() signals EOF, or abort() cancels; while
// blocked it marks the stream drained, which is what releases feed().
func (d *zlibDecomp) Read(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		if d.abortErr != nil {
			return 0, d.abortErr
		}
		if len(d.buf) > 0 {
			n := copy(p, d.buf)
			d.buf = d.buf[n:]
			return n, nil
		}
		if d.closed {
			return 0, io.EOF
		}
		d.drained = true
		d.cond.Broadcast()
		d.cond.Wait()
	}
}

// run is the decoder goroutine: it builds the codec reader over d (the input
// side) and copies decoded output into d.out until EOF, a decode error, the
// output cap, or an abort. It always terminates: every read from d returns
// data, io.EOF (after end()) or abortErr (after abort()).
func (d *zlibDecomp) run(method string) {
	var r io.Reader
	var terminal error
	switch method {
	case "gunzip":
		gr, err := gzip.NewReader(d)
		if err != nil {
			terminal = err
		} else {
			r = gr
		}
	case "inflate":
		zr, err := zlib.NewReader(d)
		if err != nil {
			terminal = err
		} else {
			r = zr
		}
	case "inflateRaw":
		fr := flate.NewReader(d)
		defer fr.Close()
		r = fr
	case "brotliDecompress":
		r = brotli.NewReader(d)
	default:
		terminal = fmt.Errorf("unsupported zlib method %q", method)
	}
	if terminal == nil {
		buf := make([]byte, 32<<10)
		var total int64
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				total += int64(n)
				if total > maxZlibOutput {
					terminal = fmt.Errorf("decompressed output exceeds %d bytes", maxZlibOutput)
					break
				}
				d.mu.Lock()
				d.out = append(d.out, buf[:n]...)
				d.mu.Unlock()
			}
			if rerr != nil {
				if rerr != io.EOF {
					terminal = rerr
				}
				break
			}
		}
	}
	d.mu.Lock()
	d.finished = true
	// An abort error is the caller's own cancellation, not a decode failure.
	if terminal != nil && !errors.Is(terminal, errZlibStreamDestroyed) {
		d.err = terminal
	}
	d.cond.Broadcast()
	d.mu.Unlock()
	close(d.done)
}

// feed hands p to the decoder and waits until it has consumed everything and
// gone idle (or terminated), so the caller can then collect all output this
// chunk made decodable. Input arriving after the compressed stream already
// ended cleanly is ignored (matching the previous behavior); a decode error
// is returned.
func (d *zlibDecomp) feed(p []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return d.err
	}
	if d.finished || d.closed || d.abortErr != nil {
		return nil
	}
	d.buf = append(d.buf, p...)
	d.drained = false
	d.cond.Broadcast()
	for !d.drained && !d.finished && d.abortErr == nil {
		d.cond.Wait()
	}
	return d.err
}

// take returns (and clears) the decoded output accumulated so far.
func (d *zlibDecomp) take() []byte {
	d.mu.Lock()
	out := d.out
	d.out = nil
	d.mu.Unlock()
	return out
}

// end signals EOF to the decoder, waits for it to finish (bounded: the decoder
// always terminates on EOF or error), and returns the remaining output or the
// terminal error.
func (d *zlibDecomp) end() ([]byte, error) {
	d.mu.Lock()
	d.closed = true
	d.cond.Broadcast()
	d.mu.Unlock()
	<-d.done
	d.mu.Lock()
	err := d.err
	out := d.out
	d.out = nil
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// abort cancels the decoder (destroy()/Runtime teardown) and reaps its
// goroutine. Safe to call more than once and after end().
func (d *zlibDecomp) abort() {
	d.mu.Lock()
	if d.abortErr == nil {
		d.abortErr = errZlibStreamDestroyed
	}
	d.cond.Broadcast()
	d.mu.Unlock()
	<-d.done
}

func newZlibCompressor(method string, buf *bytes.Buffer) (flushWriter, bool) {
	switch method {
	case "gzip":
		return gzip.NewWriter(buf), true
	case "deflate":
		return zlib.NewWriter(buf), true
	case "deflateRaw":
		w, _ := flate.NewWriter(buf, flate.DefaultCompression)
		return w, true
	case "brotliCompress":
		return brotli.NewWriter(buf), true
	}
	return nil, false
}

func isZlibDecompressMethod(method string) bool {
	switch method {
	case "gunzip", "inflate", "inflateRaw", "unzip", "brotliDecompress":
		return true
	}
	return false
}

// decompressor returns the stream's live decoder, starting it lazily for
// "unzip" once the wrapper can be sniffed (gzip magic vs. a zlib stream). It
// returns nil while an unzip stream is still waiting for its first 2 bytes.
// data is the chunk being written right now (already appended to z.sniff by
// the caller when sniffing); the returned pending bytes must be fed first.
func (z *zlibStream) decompressor() (*zlibDecomp, []byte) {
	if z.dec != nil {
		return z.dec, nil
	}
	// method == "unzip": sniff the wrapper.
	if len(z.sniff) < 2 {
		return nil, nil
	}
	method := "inflate"
	if z.sniff[0] == 0x1f && z.sniff[1] == 0x8b {
		method = "gunzip"
	}
	z.dec = newZlibDecomp(method)
	pending := z.sniff
	z.sniff = nil
	return z.dec, pending
}

func zlibErrValue(code string, err error) spidermonkey.Value {
	return spidermonkey.ValueOf(map[string]any{"code": code, "message": err.Error()})
}

func (rt *Runtime) zlibStreamByID(v spidermonkey.Value) (*zlibStream, int64) {
	id := int64(v.Float())
	rt.mu.Lock()
	z := rt.zstreams[id]
	rt.mu.Unlock()
	return z, id
}

// opZlibStreamNew(method) -> id | {code,message}.
func (rt *Runtime) opZlibStreamNew(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("zlib_stream_new: method required")
	}
	method := args[0].String()
	z := &zlibStream{method: method}
	if isZlibDecompressMethod(method) {
		// "unzip" must sniff the wrapper from the first 2 bytes before the
		// codec can be chosen; every other method starts its decoder now.
		if method != "unzip" {
			z.dec = newZlibDecomp(method)
		}
	} else {
		z.cbuf = &bytes.Buffer{}
		w, ok := newZlibCompressor(method, z.cbuf)
		if !ok {
			return zlibErrValue("Z_DATA_ERROR", fmt.Errorf("unsupported zlib method %q", method)), nil
		}
		z.comp = w
	}
	rt.mu.Lock()
	if rt.zstreams == nil {
		rt.zstreams = map[int64]*zlibStream{}
	}
	rt.nextZStream++
	id := rt.nextZStream
	rt.zstreams[id] = z
	rt.mu.Unlock()
	return spidermonkey.ValueOf(id), nil
}

// opZlibStreamWrite(id, data) -> Uint8Array (output available now) | err.
func (rt *Runtime) opZlibStreamWrite(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("zlib_stream_write: (id, data) required")
	}
	z, _ := rt.zlibStreamByID(args[0])
	if z == nil {
		return zlibErrValue("Z_STREAM_ERROR", errors.New("zlib stream is closed")), nil
	}
	data, err := valueBytes(args[1])
	if err != nil {
		return nil, err
	}
	if z.comp != nil {
		if _, err := z.comp.Write(data); err != nil {
			return zlibErrValue("Z_DATA_ERROR", err), nil
		}
		out := append([]byte(nil), z.cbuf.Bytes()...)
		z.cbuf.Reset()
		return rt.bytesReturn(out)
	}
	if z.dec == nil {
		z.sniff = append(z.sniff, data...)
		data = nil
	}
	dec, pending := z.decompressor()
	if dec == nil {
		return rt.bytesReturn(nil) // unzip still sniffing (<2 bytes so far)
	}
	if len(pending) > 0 {
		data = pending
	}
	if err := dec.feed(data); err != nil {
		return zlibErrValue("Z_DATA_ERROR", err), nil
	}
	return rt.bytesReturn(dec.take())
}

// opZlibStreamFlush(id) -> Uint8Array. Z_SYNC_FLUSH: everything written so far
// becomes decodable output. Decompressors already emit incrementally, so their
// flush just reports anything newly decodable (usually empty).
func (rt *Runtime) opZlibStreamFlush(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("zlib_stream_flush: id required")
	}
	z, _ := rt.zlibStreamByID(args[0])
	if z == nil {
		return zlibErrValue("Z_STREAM_ERROR", errors.New("zlib stream is closed")), nil
	}
	if z.comp != nil {
		if err := z.comp.Flush(); err != nil {
			return zlibErrValue("Z_DATA_ERROR", err), nil
		}
		out := append([]byte(nil), z.cbuf.Bytes()...)
		z.cbuf.Reset()
		return rt.bytesReturn(out)
	}
	// Decompressors emit as input arrives; flush just hands over anything not
	// yet collected.
	if z.dec == nil {
		return rt.bytesReturn(nil)
	}
	return rt.bytesReturn(z.dec.take())
}

// opZlibStreamEnd(id) -> Uint8Array (final output). Frees the stream.
func (rt *Runtime) opZlibStreamEnd(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("zlib_stream_end: id required")
	}
	z, id := rt.zlibStreamByID(args[0])
	if z == nil {
		return zlibErrValue("Z_STREAM_ERROR", errors.New("zlib stream is closed")), nil
	}
	rt.mu.Lock()
	delete(rt.zstreams, id)
	rt.mu.Unlock()
	if z.comp != nil {
		if err := z.comp.Close(); err != nil {
			return zlibErrValue("Z_DATA_ERROR", err), nil
		}
		return rt.bytesReturn(z.cbuf.Bytes())
	}
	if z.dec == nil {
		// unzip that never saw 2 bytes: a truncated (empty) stream.
		return zlibErrValue("Z_BUF_ERROR", errors.New("unexpected end of file")), nil
	}
	out, err := z.dec.end()
	if err != nil {
		code := "Z_DATA_ERROR"
		// A stream cut off mid-way (or ended before the header) is Node's
		// Z_BUF_ERROR "unexpected end of file".
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			code = "Z_BUF_ERROR"
			err = errors.New("unexpected end of file")
		}
		return zlibErrValue(code, err), nil
	}
	return rt.bytesReturn(out)
}

// opZlibStreamFree(id): drop a stream without finalizing (destroy path).
// Aborts and reaps a decompressor's decoder goroutine.
func (rt *Runtime) opZlibStreamFree(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("zlib_stream_free: id required")
	}
	id := int64(args[0].Float())
	rt.mu.Lock()
	z := rt.zstreams[id]
	delete(rt.zstreams, id)
	rt.mu.Unlock()
	if z != nil && z.dec != nil {
		z.dec.abort()
	}
	return nil, nil
}

// closeZlib frees every live zlib stream at Runtime teardown, aborting and
// reaping each decompressor's decoder goroutine — a stream abandoned without
// end()/destroy() must not leak its goroutine for the process lifetime.
func (rt *Runtime) closeZlib() {
	rt.mu.Lock()
	streams := make([]*zlibStream, 0, len(rt.zstreams))
	for _, z := range rt.zstreams {
		streams = append(streams, z)
	}
	rt.zstreams = nil
	rt.mu.Unlock()
	for _, z := range streams {
		if z.dec != nil {
			z.dec.abort()
		}
	}
}
