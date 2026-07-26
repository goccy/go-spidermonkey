package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// TestBufferFillAndRangeChecks pins the Buffer fixes: fill(string) fills
// with the string pattern (not zeros), and the 8-bit accessors throw RangeError
// out of range instead of silently returning undefined / no-op writing.
func TestBufferFillAndRangeChecks(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		r.fillStr = Buffer.alloc(4).fill("x").toString();
		r.fillPat = Buffer.alloc(5).fill("ab").toString();
		r.fillRange = (() => { const b = Buffer.alloc(4); b.fill("z", 1, 3); return b.toString("hex"); })();
		try { Buffer.from([1]).readUInt8(5); r.readOOB = "no-throw"; }
		catch (e) { r.readOOB = e.constructor.name; }
		try { Buffer.alloc(1).writeUInt8(9, 5); r.writeOOB = "no-throw"; }
		catch (e) { r.writeOOB = e.constructor.name; }
	`)
	for expr, want := range map[string]string{
		"r.fillStr":   "xxxx",
		"r.fillPat":   "ababa",
		"r.fillRange": "007a7a00",
		"r.readOOB":   "RangeError",
		"r.writeOOB":  "RangeError",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// TestBufferBinaryAccessors verifies the previously-missing Buffer read/write
// accessors exist and round-trip.
func TestBufferBinaryAccessors(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		let b = Buffer.alloc(8);
		b.writeFloatLE(1.5, 0);
		r.floatLE = b.readFloatLE(0);
		b = Buffer.alloc(8); b.writeInt32LE(-1000, 0); r.i32le = b.readInt32LE(0);
		b = Buffer.alloc(8); b.writeBigInt64LE(-123n, 0); r.big = String(b.readBigInt64LE(0));
		b = Buffer.alloc(8); b.writeInt16LE(-2, 0); r.i16 = b.readInt16LE(0);
		b = Buffer.from([1,2,3,4,5,6]); r.uintLE = b.readUIntLE(0, 3);
		b = Buffer.from([0x01,0x02,0x03,0x04]); b.swap32(); r.swapped = b.toString("hex");
		b = Buffer.from([0xaa,0xbb]); b.swap16(); r.sw16 = b.toString("hex");
	`)
	for expr, want := range map[string]string{
		"String(r.floatLE)": "1.5",
		"String(r.i32le)":   "-1000",
		"r.big":             "-123",
		"String(r.i16)":     "-2",
		"String(r.uintLE)":  "197121", // 1 + 2*256 + 3*65536
		"r.swapped":         "04030201",
		"r.sw16":            "bbaa",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// TestBufferLenientDecoding verifies hex/base64 decoding is lenient like Node:
// invalid hex stops (no zero-fill), invalid base64 is ignored (no throw).
func TestBufferLenientDecoding(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		r.hexBad = Buffer.from("41zz42", "hex").toString("hex");   // Node: "41"
		r.hexOdd = Buffer.from("4", "hex").length;                 // Node: 0
		try { r.b64 = Buffer.from("a*b*c", "base64").length >= 0 ? "ok" : "no"; }
		catch { r.b64 = "threw"; }
	`)
	if got := evalStr(t, js, "r.hexBad"); got != "41" {
		t.Errorf("invalid hex decode = %q, want 41", got)
	}
	if got := evalStr(t, js, "String(r.hexOdd)"); got != "0" {
		t.Errorf("odd hex nibble length = %q, want 0", got)
	}
	if got := evalStr(t, js, "r.b64"); got != "ok" {
		t.Errorf("lenient base64 = %q, want ok (no throw)", got)
	}
}

// TestBufferSwapRejectsBadLength verifies swap16/32/64 throw on a non-multiple
// length rather than silently corrupting.
func TestBufferSwapRejectsBadLength(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const chk = (name, fn) => { try { fn(); r[name] = "ok"; } catch { r[name] = "threw"; } };
		chk("s16bad", () => Buffer.from([1,2,3]).swap16());
		chk("s32bad", () => Buffer.from([1,2,3,4,5]).swap32());
		chk("s16ok", () => Buffer.from([1,2,3,4]).swap16());
	`)
	if got := evalStr(t, js, "r.s16bad"); got != "threw" {
		t.Errorf("swap16 on length 3 = %q, want threw", got)
	}
	if got := evalStr(t, js, "r.s32bad"); got != "threw" {
		t.Errorf("swap32 on length 5 = %q, want threw", got)
	}
	if got := evalStr(t, js, "r.s16ok"); got != "ok" {
		t.Errorf("swap16 on length 4 = %q, want ok", got)
	}
}

// TestBufferWriteAndSearchEncoding pins Buffer.write(str,offset,length),
// indexOf/lastIndexOf with encodings, from(TypedArray) truncation, isEncoding.
func TestBufferWriteAndSearchEncoding(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const b = Buffer.alloc(5, 0x2e);
		r.wrote = b.write("hello", 0, 3);       // "hel.."
		r.wroteStr = b.toString();
		r.idxEnc = Buffer.from("hello").indexOf("l", "utf8");   // 2
		r.last = Buffer.from("hellol").lastIndexOf("l");        // 5
		r.fromU16 = Buffer.from(new Uint16Array([0x1234, 0x5678])).toString("hex"); // 3478
		r.isEnc = Buffer.isEncoding("ucs2") && Buffer.isEncoding("utf16le");
	`)
	for expr, want := range map[string]string{
		"String(r.wrote)":  "3",
		"r.wroteStr":       "hel..",
		"String(r.idxEnc)": "2",
		"String(r.last)":   "5",
		"r.fromU16":        "3478",
		"String(r.isEnc)":  "true",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// TestBufferWriteRangeCheck verifies out-of-range integer writes throw
// (RangeError) rather than silently truncating.
func TestBufferWriteRangeCheck(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const chk = (name, fn) => { try { fn(); r[name] = "ok"; } catch { r[name] = "threw"; } };
		chk("u8", () => Buffer.alloc(1).writeUInt8(300, 0));
		chk("u16", () => Buffer.alloc(2).writeUInt16BE(0x10000, 0));
		chk("i8", () => Buffer.alloc(1).writeInt8(-200, 0));
		chk("ok8", () => Buffer.alloc(1).writeUInt8(255, 0));
		chk("okI", () => Buffer.alloc(4).writeInt32LE(-1000, 0));
	`)
	for expr, want := range map[string]string{
		"r.u8": "threw", "r.u16": "threw", "r.i8": "threw", "r.ok8": "ok", "r.okI": "ok",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// TestBufferBigIntRange verifies writeBigUInt64/writeBigInt64 range-check.
func TestBufferBigIntRange(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		const chk = (name, fn) => { try { fn(); r[name] = "ok"; } catch { r[name] = "threw"; } };
		chk("over", () => Buffer.alloc(8).writeBigUInt64BE(2n**64n + 5n));
		chk("neg", () => Buffer.alloc(8).writeBigUInt64BE(-1n));
		chk("ok", () => Buffer.alloc(8).writeBigUInt64BE(123n));
	`)
	if got := evalStr(t, js, "r.over"); got != "threw" {
		t.Errorf("writeBigUInt64 overflow = %q, want threw", got)
	}
	if got := evalStr(t, js, "r.neg"); got != "threw" {
		t.Errorf("writeBigUInt64 negative = %q, want threw", got)
	}
	if got := evalStr(t, js, "r.ok"); got != "ok" {
		t.Errorf("writeBigUInt64 valid = %q, want ok", got)
	}
}
