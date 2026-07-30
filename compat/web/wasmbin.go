package web

// wasmbin.go: the minimum of the WebAssembly binary format this embedding has
// to read and write itself, alongside wazero.
//
// Reading: the JS API reports things wazero's public API does not — import
// kinds and their type details (a global's mutability, a table's element
// type), custom sections by name — so the relevant sections are parsed here.
// The format is a foreign binary format with a public specification; parsing
// it is the legitimate kind of byte work.
//
// Writing: two small jobs. WebAssembly.Memory and WebAssembly.Global stand
// alone in the JS API, but in wazero every entity lives in a module — so a
// constructor emits a one-entity module and instantiates it. And instantiating
// a module with JS-provided imports needs each import redirected to the module
// that actually backs it (a host module of trampolines, a Memory's backing
// module), which is a rewrite of the import section's name strings.

import (
	"encoding/binary"
	"fmt"
	"math"
)

// wasm section ids and import kinds, as the specification numbers them.
const (
	secCustom = 0
	secType   = 1
	secImport = 2
	secMemory = 5
	secGlobal = 6
	secExport = 7

	kindFunc   = 0
	kindTable  = 1
	kindMemory = 2
	kindGlobal = 3
)

var wasmMagic = []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

// wasmImport is one entry of the import section, with the type details the JS
// API's Module.imports() and "read the imports" need.
type wasmImport struct {
	Module, Name string
	Kind         int
	// Global imports: value type byte and mutability.
	ValType byte
	Mutable bool
	// Function imports: type index (resolved through the type section by the
	// caller when needed).
	TypeIndex uint32
}

// wasmExport is one entry of the export section.
type wasmExport struct {
	Name string
	Kind int
	// Index is the entity's index in its own index space. For a function that
	// index IS the exported function's `name` in the JS API — an exported wasm
	// function is named by its position, not by its export string.
	Index uint32
}

type wasmMeta struct {
	Imports []wasmImport
	Exports []wasmExport
	// Customs maps a custom section name to its payloads, in order; the same
	// name may appear more than once.
	Customs map[string][][]byte
}

type wasmReader struct {
	b   []byte
	off int
}

func (r *wasmReader) u8() (byte, error) {
	if r.off >= len(r.b) {
		return 0, fmt.Errorf("truncated")
	}
	v := r.b[r.off]
	r.off++
	return v, nil
}

func (r *wasmReader) uleb() (uint64, error) {
	var v uint64
	var shift uint
	for {
		b, err := r.u8()
		if err != nil {
			return 0, err
		}
		v |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return v, nil
		}
		shift += 7
		if shift > 63 {
			return 0, fmt.Errorf("leb128 too long")
		}
	}
}

func (r *wasmReader) bytes(n uint64) ([]byte, error) {
	if uint64(r.off)+n > uint64(len(r.b)) {
		return nil, fmt.Errorf("truncated")
	}
	v := r.b[r.off : r.off+int(n)]
	r.off += int(n)
	return v, nil
}

func (r *wasmReader) name() (string, error) {
	n, err := r.uleb()
	if err != nil {
		return "", err
	}
	b, err := r.bytes(n)
	return string(b), err
}

// skipLimits reads a limits structure (shared flag included).
func (r *wasmReader) skipLimits() error {
	flags, err := r.u8()
	if err != nil {
		return err
	}
	if _, err := r.uleb(); err != nil {
		return err
	}
	if flags&0x01 != 0 {
		if _, err := r.uleb(); err != nil {
			return err
		}
	}
	return nil
}

// parseWasm reads the sections the JS API needs. It assumes the module has
// already been VALIDATED by wazero — this is metadata extraction, not a
// validator, and malformed input past the checks below just yields an error.
func parseWasm(b []byte) (*wasmMeta, error) {
	if len(b) < 8 || string(b[:4]) != "\x00asm" {
		return nil, fmt.Errorf("not a wasm module")
	}
	m := &wasmMeta{Customs: map[string][][]byte{}}
	r := &wasmReader{b: b, off: 8}
	for r.off < len(b) {
		id, err := r.u8()
		if err != nil {
			return nil, err
		}
		size, err := r.uleb()
		if err != nil {
			return nil, err
		}
		body, err := r.bytes(size)
		if err != nil {
			return nil, err
		}
		s := &wasmReader{b: body}
		switch id {
		case secCustom:
			name, err := s.name()
			if err != nil {
				return nil, err
			}
			m.Customs[name] = append(m.Customs[name], body[s.off:])
		case secImport:
			n, err := s.uleb()
			if err != nil {
				return nil, err
			}
			for i := uint64(0); i < n; i++ {
				var imp wasmImport
				if imp.Module, err = s.name(); err != nil {
					return nil, err
				}
				if imp.Name, err = s.name(); err != nil {
					return nil, err
				}
				kind, err := s.u8()
				if err != nil {
					return nil, err
				}
				imp.Kind = int(kind)
				switch kind {
				case kindFunc:
					t, err := s.uleb()
					if err != nil {
						return nil, err
					}
					imp.TypeIndex = uint32(t)
				case kindTable:
					if _, err := s.u8(); err != nil { // elemtype
						return nil, err
					}
					if err := s.skipLimits(); err != nil {
						return nil, err
					}
				case kindMemory:
					if err := s.skipLimits(); err != nil {
						return nil, err
					}
				case kindGlobal:
					vt, err := s.u8()
					if err != nil {
						return nil, err
					}
					mut, err := s.u8()
					if err != nil {
						return nil, err
					}
					imp.ValType, imp.Mutable = vt, mut == 1
				default:
					return nil, fmt.Errorf("unknown import kind %d", kind)
				}
				m.Imports = append(m.Imports, imp)
			}
		case secExport:
			n, err := s.uleb()
			if err != nil {
				return nil, err
			}
			for i := uint64(0); i < n; i++ {
				var exp wasmExport
				if exp.Name, err = s.name(); err != nil {
					return nil, err
				}
				kind, err := s.u8()
				if err != nil {
					return nil, err
				}
				exp.Kind = int(kind)
				idx, err := s.uleb()
				if err != nil {
					return nil, err
				}
				exp.Index = uint32(idx)
				m.Exports = append(m.Exports, exp)
			}
		}
	}
	return m, nil
}

// ------------------------------------------------------------ emitting

func uleb(v uint64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if v == 0 {
			return out
		}
	}
}

// sleb encodes a signed LEB128, which const-expression immediates use.
func sleb(v int64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		done := (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0)
		if !done {
			b |= 0x80
		}
		out = append(out, b)
		if done {
			return out
		}
	}
}

func wasmName(s string) []byte {
	return append(uleb(uint64(len(s))), s...)
}

func section(id byte, body []byte) []byte {
	out := []byte{id}
	out = append(out, uleb(uint64(len(body)))...)
	return append(out, body...)
}

// emitMemoryModule builds a module that defines one memory and exports it as
// "m" — the backing a standalone WebAssembly.Memory needs, since wazero has no
// module-less memories. max < 0 means no maximum.
func emitMemoryModule(min uint32, max int64, shared bool) []byte {
	var limits []byte
	switch {
	case shared:
		// A shared memory requires a maximum, which the JS constructor enforced.
		limits = append([]byte{0x03}, uleb(uint64(min))...)
		limits = append(limits, uleb(uint64(max))...)
	case max >= 0:
		limits = append([]byte{0x01}, uleb(uint64(min))...)
		limits = append(limits, uleb(uint64(max))...)
	default:
		limits = append([]byte{0x00}, uleb(uint64(min))...)
	}
	memSec := append([]byte{0x01}, limits...)
	expBody := append([]byte{0x01}, wasmName("m")...)
	expBody = append(expBody, byte(kindMemory), 0x00)
	out := append([]byte(nil), wasmMagic...)
	out = append(out, section(secMemory, memSec)...)
	out = append(out, section(secExport, expBody)...)
	return out
}

// emitGlobalModule builds a module that defines one global with the given
// value and exports it as "g". The value rides in the init expression, which
// is how a wasm global is born with a value.
func emitGlobalModule(valType byte, mutable bool, bits uint64) []byte {
	var init []byte
	switch valType {
	case 0x7f: // i32
		init = append([]byte{0x41}, sleb(int64(int32(uint32(bits))))...)
	case 0x7e: // i64
		init = append([]byte{0x42}, sleb(int64(bits))...)
	case 0x7d: // f32
		init = []byte{0x43}
		init = binary.LittleEndian.AppendUint32(init, uint32(bits))
	case 0x7c: // f64
		init = []byte{0x44}
		init = binary.LittleEndian.AppendUint64(init, bits)
	case 0x6f, 0x70: // externref / funcref: born null
		init = []byte{0xd0, valType}
	}
	init = append(init, 0x0b) // end
	mut := byte(0)
	if mutable {
		mut = 1
	}
	glbBody := append([]byte{0x01, valType, mut}, init...)
	expBody := append([]byte{0x01}, wasmName("g")...)
	expBody = append(expBody, byte(kindGlobal), 0x00)
	out := append([]byte(nil), wasmMagic...)
	out = append(out, section(secGlobal, glbBody)...)
	out = append(out, section(secExport, expBody)...)
	return out
}

// rewriteImports returns the module bytes with each import's (module, name)
// replaced according to redirect, leaving everything else — including the
// import's type — byte-for-byte intact. This is how a JS import object meets
// wazero's name-based linking: each JS-provided entity lives in a module of
// its own with a unique name, and the importer is rewritten to point there.
func rewriteImports(b []byte, redirect func(module, name string, index int) (string, string)) ([]byte, error) {
	if len(b) < 8 {
		return nil, fmt.Errorf("not a wasm module")
	}
	r := &wasmReader{b: b, off: 8}
	for r.off < len(b) {
		secStart := r.off
		id, err := r.u8()
		if err != nil {
			return nil, err
		}
		size, err := r.uleb()
		if err != nil {
			return nil, err
		}
		bodyStart := r.off
		body, err := r.bytes(size)
		if err != nil {
			return nil, err
		}
		if id != secImport {
			continue
		}
		s := &wasmReader{b: body}
		n, err := s.uleb()
		if err != nil {
			return nil, err
		}
		newBody := append([]byte(nil), uleb(n)...)
		for i := uint64(0); i < n; i++ {
			module, err := s.name()
			if err != nil {
				return nil, err
			}
			name, err := s.name()
			if err != nil {
				return nil, err
			}
			descStart := s.off
			kind, err := s.u8()
			if err != nil {
				return nil, err
			}
			switch kind {
			case kindFunc:
				if _, err := s.uleb(); err != nil {
					return nil, err
				}
			case kindTable:
				if _, err := s.u8(); err != nil {
					return nil, err
				}
				if err := s.skipLimits(); err != nil {
					return nil, err
				}
			case kindMemory:
				if err := s.skipLimits(); err != nil {
					return nil, err
				}
			case kindGlobal:
				if _, err := s.u8(); err != nil {
					return nil, err
				}
				if _, err := s.u8(); err != nil {
					return nil, err
				}
			default:
				return nil, fmt.Errorf("unknown import kind %d", kind)
			}
			newModule, newName := redirect(module, name, int(i))
			newBody = append(newBody, wasmName(newModule)...)
			newBody = append(newBody, wasmName(newName)...)
			newBody = append(newBody, body[descStart:s.off]...)
		}
		out := append([]byte(nil), b[:secStart]...)
		out = append(out, section(secImport, newBody)...)
		out = append(out, b[bodyStart+int(size):]...)
		return out, nil
	}
	// No import section: nothing to rewrite.
	return b, nil
}

// f32bits/f64bits are spelled out so the call sites read as conversions, not
// as bit tricks.
func f64bits(f float64) uint64     { return math.Float64bits(f) }
func f64FromBits(u uint64) float64 { return math.Float64frombits(u) }
