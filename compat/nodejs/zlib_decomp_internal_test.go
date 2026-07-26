package nodejs

// White-box tests for the persistent streaming decompressor: the decoder
// goroutine must always terminate — on clean EOF, on decode error, on
// truncation, and on abort (destroy/teardown) — and never wedge waiting for
// input that will never come.

import (
	"bytes"
	"compress/gzip"
	"testing"
	"time"
)

func waitReaped(t *testing.T, d *zlibDecomp, what string) {
	t.Helper()
	select {
	case <-d.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: decoder goroutine was not reaped", what)
	}
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestZlibDecompFeedEndRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte("streaming decode "), 4096)
	gz := gzipped(t, payload)

	d := newZlibDecomp("gunzip")
	var got []byte
	for off := 0; off < len(gz); off += 1024 {
		end := off + 1024
		if end > len(gz) {
			end = len(gz)
		}
		if err := d.feed(gz[off:end]); err != nil {
			t.Fatalf("feed: %v", err)
		}
		got = append(got, d.take()...)
	}
	rest, err := d.end()
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	got = append(got, rest...)
	if !bytes.Equal(got, payload) {
		t.Fatalf("round trip mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	waitReaped(t, d, "clean EOF")
}

func TestZlibDecompAbortMidStreamReapsGoroutine(t *testing.T) {
	gz := gzipped(t, bytes.Repeat([]byte("x"), 100000))
	d := newZlibDecomp("gunzip")
	if err := d.feed(gz[:64]); err != nil {
		t.Fatalf("feed: %v", err)
	}
	d.abort()
	waitReaped(t, d, "abort mid-stream")
	// abort must be idempotent (destroy op + Runtime teardown can both run).
	d.abort()
}

func TestZlibDecompAbortBeforeAnyInputReapsGoroutine(t *testing.T) {
	// The decoder is blocked inside gzip.NewReader waiting for the header.
	d := newZlibDecomp("gunzip")
	d.abort()
	waitReaped(t, d, "abort before input")
}

func TestZlibDecompTruncatedEndTerminatesWithError(t *testing.T) {
	gz := gzipped(t, bytes.Repeat([]byte("y"), 100000))
	d := newZlibDecomp("gunzip")
	if err := d.feed(gz[:len(gz)/2]); err != nil {
		t.Fatalf("feed: %v", err)
	}
	if _, err := d.end(); err == nil {
		t.Fatal("end() on a truncated stream returned no error")
	}
	waitReaped(t, d, "truncated end")
}

func TestZlibDecompCorruptInputSurfacesError(t *testing.T) {
	gz := gzipped(t, bytes.Repeat([]byte("z"), 100000))
	for i := 20; i < len(gz)-8; i++ {
		gz[i] ^= 0x5a
	}
	d := newZlibDecomp("gunzip")
	var feedErr error
	for off := 0; off < len(gz) && feedErr == nil; off += 512 {
		end := off + 512
		if end > len(gz) {
			end = len(gz)
		}
		feedErr = d.feed(gz[off:end])
	}
	if feedErr == nil {
		if _, endErr := d.end(); endErr == nil {
			t.Fatal("corrupt input produced no error from feed or end")
		}
	}
	waitReaped(t, d, "corrupt input")
}
