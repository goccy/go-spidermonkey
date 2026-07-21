package spidermonkey

// Raw bytes cross the guest/host boundary DIRECTLY: the bridge's js_bytes_new /
// js_bytes_read carry the payload over the length-delimited (8-bit clean)
// protobuf channel, so there is no base64/JSON encoding step in either
// direction — NUL bytes and non-UTF-8 sequences cross intact.

// NewBytes copies data into a fresh guest Uint8Array and returns its handle.
// This is the []byte -> JS direction of the binary bridge; the reverse is
// Object.Bytes. The returned Object is a Value, so it can be passed to Call,
// Set, or returned from a host Func like any other value. The caller owns the
// handle (Free releases the host's pin; the guest keeps the array alive as
// long as it references it).
func (js *JS) NewBytes(data []byte) (*Object, error) {
	h, err := js.raw.BytesNew(data)
	if err != nil {
		return nil, err
	}
	return &Object{js: js, handle: h}, nil
}

// Bytes copies the object's binary contents into a fresh Go []byte. It accepts
// a Uint8Array, any other ArrayBuffer view (Int32Array, DataView, ...) — read
// as its raw byte window — an ArrayBuffer, or a SharedArrayBuffer. Any other
// object is an error. This is the JS -> []byte direction of the binary bridge;
// the reverse is JS.NewBytes.
func (o *Object) Bytes() ([]byte, error) {
	return o.js.raw.BytesRead(o.handle)
}
