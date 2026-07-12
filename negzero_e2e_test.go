package spidermonkey_test

import (
	"strings"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// e2e proof that wasm2go v0.4.5's negative-zero fix reaches the Go layer
// through the canonical pipeline (make wasm + buf generate).
func TestNegativeZeroE2E(t *testing.T) {
	i := newInterp(t, spidermonkey.Config{})
	for src, want := range map[string]string{
		`1 / Number("-0")`:                "-Infinity",
		`1 / Math.atan2(-1, Infinity)`:    "-Infinity",
		`1 / Math.sumPrecise([-0])`:       "-Infinity",
		`Object.is(Number("-0"), -0)`:     "true",
	} {
		if r := eval(t, i, src); !r.Ok || r.Result != want {
			t.Errorf("%s = %+v, want %s", src, r, want)
		}
	}
}

// e2e proof of the NUL fix through the canonical pipeline: source containing
// a raw NUL byte must not be truncated at the bridge.
func TestNULSourceE2E(t *testing.T) {
	i := newInterp(t, spidermonkey.Config{})
	if r := eval(t, i, "'a\x00b'.length"); !r.Ok || r.Result != "3" {
		t.Errorf("NUL-embedded source = %+v, want 3", r)
	}
}

// e2e: registry-backed ES modules through the canonical pipeline.
func TestModuleE2E(t *testing.T) {
	i := newInterp(t, spidermonkey.Config{})
	if r, err := i.EvalModule("m.js", "globalThis.me2e = 'mod-ok';"); err != nil || !r.Ok {
		t.Fatalf("EvalModule: %+v %v", r, err)
	}
	if r := eval(t, i, "me2e"); r.Result != "mod-ok" {
		t.Errorf("module effect = %+v", r)
	}
}

// e2e: the with-Intl engine through the canonical pipeline. Everything the
// engine-level smoke proved must also hold through Go.
func TestIntlEngineE2E(t *testing.T) {
	i := newInterp(t, spidermonkey.Config{})
	if r := eval(t, i, "typeof Intl"); r.Result != "object" {
		t.Skipf("engine without Intl (result=%q); run against the intl bundle", r.Result)
	}
	for _, tc := range []struct{ src, want string }{
		{"new Intl.NumberFormat('ja-JP').format(1234567)", "1,234,567"},
		{"/\\p{Script=Hiragana}/u.test('あ') ? 'p-ok' : 'no'", "p-ok"},
		{"'\\u0041\\u030A'.normalize('NFC') === '\\u00C5' ? 'nfc-ok' : 'no'", "nfc-ok"},
		{"Temporal.PlainDate.from('2026-07-11').add({days: 30}).toString()", "2026-08-10"},
		{"Temporal.Instant.from('2026-07-11T00:00:00Z').toZonedDateTimeISO('Asia/Tokyo').hour", "9"},
		{"typeof SharedArrayBuffer", "function"},
		{"(() => { const ia = new Int32Array(new SharedArrayBuffer(8)); Atomics.add(ia, 0, 42); return Atomics.load(ia, 0) })()", "42"},
	} {
		if r := eval(t, i, tc.src); !r.Ok || r.Result != tc.want {
			t.Errorf("%s = %+v, want %q", tc.src, r, tc.want)
		}
	}
	// The recursion ceiling scales with the quota on the patched engine.
	deep := strings.Repeat("(function(){return ", 60) + "1" + strings.Repeat("})()", 60)
	big := newInterp(t, spidermonkey.Config{NativeStackQuotaBytes: 4 << 20})
	if r := eval(t, big, deep); !r.Ok || r.Result != "1" {
		t.Errorf("60-deep nest under 4MiB quota = %+v, want 1", r)
	}
}
