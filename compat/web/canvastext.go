package web

// canvastext.go: text shaping for the 2d context.
//
// fillText and strokeText are PATH operations here: the host turns a string
// into glyph outlines (moveTo/lineTo/quadTo/cubeTo commands on the alphabetic
// baseline, x advancing rightward) and the guest feeds them through the same
// fill and stroke pipeline every other shape uses — which is what makes the
// transform, gradients, patterns, shadows and compositing apply to text
// without a second code path. measureText is the same call, keeping the
// numbers and dropping the outline.
//
// Font-wide metrics are computed from the font's own DESIGN UNITS (head,
// hhea and BASE, read straight from the sfnt tables) rather than from
// pre-scaled fixed-point values: a face with an ascender of 600/800 em must
// measure exactly 30 at 40px, not 30 rounded through 26.6 fixed point. The
// BASE table is where a font states its hanging and ideographic baselines; a
// font without one gets the usual fallbacks.
//
// The Go fonts stand in for the generic families: they are real, complete
// TrueType faces that ship with golang.org/x/image, so metrics and shaping
// are honest rather than approximated. A FontFace registers its bytes under
// its family name and takes precedence.

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"sync"
	"unicode"
	"unicode/utf16"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gobolditalic"
	"golang.org/x/image/font/gofont/goitalic"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/gofont/gomonobolditalic"
	"golang.org/x/image/font/gofont/gomonoitalic"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/gofont/gosmallcaps"
	"golang.org/x/image/font/gofont/gosmallcapsitalic"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// fontFace is a parsed face plus the design-unit metrics shaping needs.
type fontFace struct {
	font *sfnt.Font
	meta fontMeta
}

type fontMeta struct {
	upem float64
	// ascent/descent are the font bounding box: hhea's values, unless the
	// face sets USE_TYPO_METRICS, in which case OS/2's typographic ones.
	ascent  float64
	descent float64
	// emAscent/emDescent divide the em square: OS/2's typographic values
	// when present, hhea's otherwise. The two pairs genuinely differ — the
	// WPT faces are built to catch an implementation conflating them.
	emAscent   float64
	emDescent  float64
	hang, ideo float64 // BASE table baselines, in units, positive up
	hasHang    bool
	hasIdeo    bool
}

// parseFontMeta reads head.unitsPerEm, hhea.ascender/descender and the BASE
// table's hang/ideo baselines from a raw sfnt file. These few fields are all
// that is needed, and x/image/font/sfnt does not expose them unscaled.
func parseFontMeta(data []byte) fontMeta {
	m := fontMeta{upem: 1000, ascent: 800, descent: 200}
	u16 := func(off int) uint16 {
		if off < 0 || off+2 > len(data) {
			return 0
		}
		return binary.BigEndian.Uint16(data[off:])
	}
	u32 := func(off int) uint32 {
		if off < 0 || off+4 > len(data) {
			return 0
		}
		return binary.BigEndian.Uint32(data[off:])
	}
	tables := map[string]int{}
	numTables := int(u16(4))
	for i := 0; i < numTables; i++ {
		rec := 12 + i*16
		if rec+16 > len(data) {
			break
		}
		tables[string(data[rec:rec+4])] = int(u32(rec + 8))
	}
	if off, ok := tables["head"]; ok {
		if v := u16(off + 18); v > 0 {
			m.upem = float64(v)
		}
	}
	if off, ok := tables["hhea"]; ok {
		m.ascent = float64(int16(u16(off + 4)))
		m.descent = -float64(int16(u16(off + 6)))
	}
	m.emAscent, m.emDescent = m.ascent, m.descent
	if off, ok := tables["OS/2"]; ok {
		m.emAscent = float64(int16(u16(off + 68)))
		m.emDescent = -float64(int16(u16(off + 70)))
		// USE_TYPO_METRICS says the typographic values are also the truth
		// about the font bounding box.
		if u16(off+62)&0x80 != 0 {
			m.ascent, m.descent = m.emAscent, m.emDescent
		}
	}
	if base, ok := tables["BASE"]; ok {
		if horizOff := int(u16(base + 4)); horizOff != 0 {
			horiz := base + horizOff
			tagList := horiz + int(u16(horiz))
			scriptList := horiz + int(u16(horiz+2))
			tagCount := int(u16(tagList))
			var tags []string
			for i := 0; i < tagCount; i++ {
				off := tagList + 2 + i*4
				if off+4 > len(data) {
					break
				}
				tags = append(tags, string(data[off:off+4]))
			}
			if int(u16(scriptList)) > 0 {
				// The first script record's default BaseValues carries one
				// coordinate per tag in the tag list.
				script := scriptList + int(u16(scriptList+2+4))
				values := script + int(u16(script))
				coordCount := int(u16(values + 2))
				for i := 0; i < coordCount && i < len(tags); i++ {
					coordOff := values + int(u16(values+4+i*2))
					coord := float64(int16(u16(coordOff + 2)))
					switch tags[i] {
					case "hang":
						m.hang, m.hasHang = coord, true
					case "ideo":
						m.ideo, m.hasIdeo = coord, true
					}
				}
			}
		}
	}
	return m
}

var builtinFonts struct {
	once  sync.Once
	faces map[string]*fontFace
}

func loadBuiltinFonts() map[string]*fontFace {
	builtinFonts.once.Do(func() {
		parse := func(b []byte) *fontFace {
			f, err := sfnt.Parse(b)
			if err != nil {
				panic(fmt.Sprintf("embedded font failed to parse: %v", err))
			}
			return &fontFace{font: f, meta: parseFontMeta(b)}
		}
		builtinFonts.faces = map[string]*fontFace{
			"regular":           parse(goregular.TTF),
			"bold":              parse(gobold.TTF),
			"italic":            parse(goitalic.TTF),
			"bold-italic":       parse(gobolditalic.TTF),
			"mono":              parse(gomono.TTF),
			"mono-bold":         parse(gomonobold.TTF),
			"mono-italic":       parse(gomonoitalic.TTF),
			"mono-bold-italic":  parse(gomonobolditalic.TTF),
			"small-caps":        parse(gosmallcaps.TTF),
			"small-caps-italic": parse(gosmallcapsitalic.TTF),
		}
	})
	return builtinFonts.faces
}

// pickFont resolves a family list to a face. A registered FontFace wins on
// its name; the generic families map onto the Go fonts by weight and slant.
func (a *canvasAPI) pickFont(families []string, bold, italic, smallCaps bool) *fontFace {
	faces := loadBuiltinFonts()
	variant := func(mono bool) *fontFace {
		key := "regular"
		switch {
		case smallCaps && italic:
			key = "small-caps-italic"
		case smallCaps:
			key = "small-caps"
		case mono && bold && italic:
			key = "mono-bold-italic"
		case mono && bold:
			key = "mono-bold"
		case mono && italic:
			key = "mono-italic"
		case mono:
			key = "mono"
		case bold && italic:
			key = "bold-italic"
		case bold:
			key = "bold"
		case italic:
			key = "italic"
		}
		return faces[key]
	}
	for _, fam := range families {
		name := strings.ToLower(strings.TrimSpace(fam))
		if f := a.fonts[name]; f != nil {
			return f
		}
		switch name {
		case "monospace", "ui-monospace":
			return variant(true)
		case "serif", "sans-serif", "cursive", "fantasy", "system-ui",
			"math", "ui-serif", "ui-sans-serif", "ui-rounded":
			return variant(false)
		}
	}
	return variant(false)
}

// opFontRegister(family, bytes) parses an sfnt face and registers it under
// the family name, which is how a FontFace becomes reachable from font.
func (a *canvasAPI) opFontRegister(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("canvas_font_register: (family, bytes) required")
	}
	family := strings.ToLower(strings.TrimSpace(args[0].String()))
	data, err := argBytes(args[1])
	if err != nil {
		return nil, err
	}
	f, perr := sfnt.Parse(data)
	if perr != nil {
		return spidermonkey.ValueOf(map[string]any{"error": perr.Error()}), nil
	}
	a.fonts[family] = &fontFace{font: f, meta: parseFontMeta(data)}
	return spidermonkey.ValueOf(map[string]any{}), nil
}

// opTextPath(text, spec) -> {path, starts, inks, width, ...metrics}.
//
//   - path: a flat Float64Array of [op, coords...] runs — 0 moveTo(2),
//     1 lineTo(2), 2 quadTo(4), 3 cubeTo(6), 4 closePath(0) — glyph outlines
//     on the alphabetic baseline, y down.
//   - starts: x at every UTF-16 code unit boundary (length+1 values), which
//     is what caret and cluster queries are answered from.
//   - inks: per code unit [minX, maxX, minY, maxY], NaN where a unit has no
//     ink of its own (spaces, trailing surrogates).
func (a *canvasAPI) opTextPath(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("canvas_text_path: (text, spec) required")
	}
	text := args[0].String()
	spec := args[1].Object()
	if spec == nil {
		return nil, fmt.Errorf("canvas_text_path: a spec is required")
	}
	defer spec.Free()

	size := objFloat(spec, "size", 10)
	letterSpacing := objFloat(spec, "letterSpacing", 0)
	wordSpacing := objFloat(spec, "wordSpacing", 0)
	kerning := objBool(spec, "kerning")
	// families crosses as one NUL-joined string: a family name can contain
	// almost anything, but never a NUL.
	var families []string
	if fs := objString(spec, "families", ""); fs != "" {
		families = strings.Split(fs, "\x00")
	}
	face := a.pickFont(families, objBool(spec, "bold"), objBool(spec, "italic"), objBool(spec, "smallCaps"))
	f := face.font
	scale := size / face.meta.upem

	var b sfnt.Buffer
	ppem := fixed.Int26_6(size * 64)
	if ppem <= 0 {
		ppem = 1
	}

	runes := []rune(text)
	codeUnits := len(utf16.Encode(runes))
	starts := make([]float64, codeUnits+1)
	inks := make([]float64, codeUnits*4)
	for i := range inks {
		inks[i] = math.NaN()
	}

	var path []float64
	x := 0.0
	minX, maxX := 0.0, 0.0
	minY, maxY := 0.0, 0.0
	hasInk := false
	prev := sfnt.GlyphIndex(0)
	hasPrev := false
	unit := 0
	for _, r := range runes {
		units := 1
		if r > 0xFFFF {
			units = 2
		}
		for u := 0; u < units; u++ {
			starts[unit+u] = x
		}
		if unicode.Is(unicode.Cc, r) {
			unit += units
			continue
		}
		g, gerr := f.GlyphIndex(&b, r)
		if gerr != nil {
			unit += units
			continue
		}
		if hasPrev && kerning {
			if k, kerr := f.Kern(&b, prev, g, ppem, font.HintingNone); kerr == nil {
				x += float64(k) / 64
				for u := 0; u < units; u++ {
					starts[unit+u] = x
				}
			}
		}
		gMinX, gMaxX := math.NaN(), math.NaN()
		gMinY, gMaxY := math.NaN(), math.NaN()
		// A whitespace character advances but never inks — even when the
		// font has no glyph for it and lookup lands on the .notdef box.
		segs, serr := f.LoadGlyph(&b, g, ppem, nil)
		if unicode.IsSpace(r) {
			segs, serr = nil, nil
		}
		if serr == nil {
			open := false
			grow := func(gx, gy float64) {
				if math.IsNaN(gMinX) {
					gMinX, gMaxX, gMinY, gMaxY = gx, gx, gy, gy
				} else {
					gMinX = math.Min(gMinX, gx)
					gMaxX = math.Max(gMaxX, gx)
					gMinY = math.Min(gMinY, gy)
					gMaxY = math.Max(gMaxY, gy)
				}
				if !hasInk {
					minX, maxX, minY, maxY = gx, gx, gy, gy
					hasInk = true
					return
				}
				minX = math.Min(minX, gx)
				maxX = math.Max(maxX, gx)
				minY = math.Min(minY, gy)
				maxY = math.Max(maxY, gy)
			}
			for _, seg := range segs {
				px := func(i int) float64 { return x + float64(seg.Args[i].X)/64 }
				py := func(i int) float64 { return float64(seg.Args[i].Y) / 64 }
				switch seg.Op {
				case sfnt.SegmentOpMoveTo:
					if open {
						path = append(path, 4)
					}
					path = append(path, 0, px(0), py(0))
					grow(px(0), py(0))
					open = true
				case sfnt.SegmentOpLineTo:
					path = append(path, 1, px(0), py(0))
					grow(px(0), py(0))
				case sfnt.SegmentOpQuadTo:
					path = append(path, 2, px(0), py(0), px(1), py(1))
					grow(px(0), py(0))
					grow(px(1), py(1))
				case sfnt.SegmentOpCubeTo:
					path = append(path, 3, px(0), py(0), px(1), py(1), px(2), py(2))
					grow(px(0), py(0))
					grow(px(1), py(1))
					grow(px(2), py(2))
				}
			}
			if open {
				path = append(path, 4)
			}
		}
		inks[unit*4] = gMinX
		inks[unit*4+1] = gMaxX
		inks[unit*4+2] = gMinY
		inks[unit*4+3] = gMaxY
		if adv, aerr := f.GlyphAdvance(&b, g, ppem, font.HintingNone); aerr == nil {
			x += float64(adv) / 64
		}
		x += letterSpacing
		if r == ' ' {
			x += wordSpacing
		}
		prev, hasPrev = g, true
		unit += units
	}
	starts[codeUnits] = x

	obj, err := a.js.NewObject()
	if err != nil {
		return nil, err
	}
	setBytes := func(name string, vals []float64) error {
		raw := make([]byte, len(vals)*8)
		for i, v := range vals {
			binary.LittleEndian.PutUint64(raw[i*8:], math.Float64bits(v))
		}
		u8, berr := a.js.NewBytes(raw)
		if berr != nil {
			return berr
		}
		serr := obj.Set(name, u8)
		u8.Free()
		return serr
	}
	if err := setBytes("path", path); err != nil {
		return nil, err
	}
	if err := setBytes("starts", starts); err != nil {
		return nil, err
	}
	if err := setBytes("inks", inks); err != nil {
		return nil, err
	}
	meta := face.meta
	hang := meta.ascent * 0.8
	if meta.hasHang {
		hang = meta.hang
	}
	ideo := -meta.descent
	if meta.hasIdeo {
		ideo = meta.ideo
	}
	fields := map[string]float64{
		"width":     x,
		"ascent":    meta.ascent * scale,
		"descent":   meta.descent * scale,
		"emAscent":  meta.emAscent * scale,
		"emDescent": meta.emDescent * scale,
		"hanging":   hang * scale,
		"ideo":      ideo * scale,
		"inkLeft":   minX,
		"inkRight":  maxX,
		"inkTop":    minY,
		"inkBottom": maxY,
	}
	for name, v := range fields {
		if err := obj.Set(name, spidermonkey.ValueOf(v)); err != nil {
			return nil, err
		}
	}
	return obj, nil
}
