package spidermonkey_test

// Throughput of the binary bridge in both directions, across payload sizes.
// The bytes cross raw (length-delimited protobuf), so per-byte cost is memcpy
// plus the fixed per-call bridge overhead visible at the small sizes.
//
//	go test -bench BenchmarkBytes -benchmem .

import (
	"encoding/base64"
	"fmt"
	"math/rand"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func benchSizes() []int { return []int{1 << 10, 64 << 10, 1 << 20} }

func BenchmarkBytesNew(b *testing.B) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		b.Fatal(err)
	}
	defer js.Close()
	for _, size := range benchSizes() {
		data := make([]byte, size)
		rand.New(rand.NewSource(1)).Read(data)
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			b.SetBytes(int64(size))
			for i := 0; i < b.N; i++ {
				u8, err := js.NewBytes(data)
				if err != nil {
					b.Fatal(err)
				}
				u8.Free()
			}
		})
	}
}

// BenchmarkBytesBase64RoundTrip measures what the binary bridge REPLACED: the
// same []byte -> Uint8Array -> []byte round trip over the string channel via
// base64 (Go encoding/base64 + guest Uint8Array.fromBase64/toBase64). Compare
// with BenchmarkBytesNew + BenchmarkBytesRead to see the direct path's win.
func BenchmarkBytesBase64RoundTrip(b *testing.B) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		b.Fatal(err)
	}
	defer js.Close()
	decode := evalValue(b, js, `(b64 => Uint8Array.fromBase64(b64))`).Object()
	encode := evalValue(b, js, `(u8 => u8.toBase64())`).Object()
	for _, size := range benchSizes() {
		data := make([]byte, size)
		rand.New(rand.NewSource(1)).Read(data)
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			b.SetBytes(int64(size) * 2) // one crossing each way per iteration
			for i := 0; i < b.N; i++ {
				v, err := decode.Call(spidermonkey.ValueOf(base64.StdEncoding.EncodeToString(data)))
				if err != nil {
					b.Fatal(err)
				}
				u8 := v.Object()
				s, err := encode.Call(u8)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := base64.StdEncoding.DecodeString(s.String()); err != nil {
					b.Fatal(err)
				}
				u8.Free()
			}
		})
	}
}

func BenchmarkBytesRead(b *testing.B) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		b.Fatal(err)
	}
	defer js.Close()
	for _, size := range benchSizes() {
		data := make([]byte, size)
		rand.New(rand.NewSource(1)).Read(data)
		u8, err := js.NewBytes(data)
		if err != nil {
			b.Fatal(err)
		}
		defer u8.Free()
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			b.SetBytes(int64(size))
			for i := 0; i < b.N; i++ {
				if _, err := u8.Bytes(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
