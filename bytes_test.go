package spidermonkey_test

// Coverage of the binary bridge: JS.NewBytes carries Go []byte into the guest
// as a real Uint8Array (identity-preserving handle), and Object.Bytes reads
// any guest binary value (Uint8Array, other views, DataView, ArrayBuffer)
// back into Go — byte-exact in both directions, including NUL and non-UTF-8
// byte sequences that could never survive the string channel.

import (
	"bytes"
	"math/rand"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func TestNewBytesIsGuestUint8Array(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	data := []byte{0x00, 0x01, 0x7f, 0x80, 0xfe, 0xff}
	u8, err := js.NewBytes(data)
	if err != nil {
		t.Fatalf("NewBytes: %v", err)
	}
	defer u8.Free()

	// The guest must see a genuine Uint8Array with the exact contents —
	// verify via the handle (identity), not a copy.
	if err := js.Global().Set("hostBytes", u8); err != nil {
		t.Fatalf("Set(hostBytes): %v", err)
	}
	v := evalValue(t, js, `hostBytes instanceof Uint8Array && hostBytes.join(",") === "0,1,127,128,254,255"`)
	if !v.Bool() {
		t.Errorf("guest does not see the expected Uint8Array: %s", evalValue(t, js, `hostBytes.join(",")`).String())
	}

	// Guest-side mutation must be visible through the same handle (identity).
	evalValue(t, js, `hostBytes[0] = 42`)
	got, err := u8.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if got[0] != 42 {
		t.Errorf("handle does not alias the guest array: got[0] = %d, want 42", got[0])
	}
}

func TestBytesReadsGuestBinaryValues(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	cases := []struct {
		name string
		src  string
		want []byte
	}{
		{"uint8array", `new Uint8Array([1, 0, 255])`, []byte{1, 0, 255}},
		{"empty", `new Uint8Array(0)`, []byte{}},
		{"arraybuffer", `new Uint8Array([9, 8, 7]).buffer`, []byte{9, 8, 7}},
		{"int32array-le", `new Int32Array([0x04030201])`, []byte{1, 2, 3, 4}},
		{"dataview-window", `new DataView(new Uint8Array([1, 2, 3, 4, 5]).buffer, 1, 3)`, []byte{2, 3, 4}},
		{"subarray-offset", `new Uint8Array([1, 2, 3, 4, 5]).subarray(2, 4)`, []byte{3, 4}},
	}
	for _, tc := range cases {
		obj := evalValue(t, js, tc.src).Object()
		if obj == nil {
			t.Fatalf("%s: not an object", tc.name)
		}
		got, err := obj.Bytes()
		if err != nil {
			t.Fatalf("%s: Bytes: %v", tc.name, err)
		}
		if !bytes.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		obj.Free()
	}

	// A non-binary object is a guest TypeError, not garbage bytes.
	plain := evalValue(t, js, `({})`).Object()
	defer plain.Free()
	if _, err := plain.Bytes(); err == nil {
		t.Errorf("Bytes on a plain object: want TypeError, got nil error")
	}
}

func TestBytesRoundTripLarge(t *testing.T) {
	js, _ := spidermonkey.New(spidermonkey.Config{})
	defer js.Close()

	data := make([]byte, 1<<20) // 1 MiB spanning every byte value
	rand.New(rand.NewSource(1)).Read(data)

	u8, err := js.NewBytes(data)
	if err != nil {
		t.Fatalf("NewBytes: %v", err)
	}
	defer u8.Free()
	got, err := u8.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("1 MiB round trip is not byte-exact")
	}
}
