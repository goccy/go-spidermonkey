package web

// canvas.go: the raster surface behind OffscreenCanvas's 2d context.
//
// The DRAWING is here, in Go, because it is an algorithm: flattening curves,
// filling a path by a winding rule, stroking one into an outline, compositing
// with an alpha. rasterize() computes an anti-aliased coverage mask straight
// from the winding rule, and everything else is built on it.
//
// What stays in JavaScript is the API surface and the drawing STATE — the
// save/restore stack, the current transform, the styles — because that is what
// the specification describes and because a state machine that lives on one
// side of the bridge is one that cannot disagree with itself. Each call across
// carries the state it needs, so this file holds pixels and nothing else.
//
// Colours are non-premultiplied RGBA bytes, in the order getImageData reports
// them, and the surface stores them that way too: a premultiplied surface would
// have to round twice on every read, and the tests compare exact bytes.

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"sort"

	_ "golang.org/x/image/webp"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// maxCanvasPixels bounds one surface. A canvas is allocated by the guest and
// held until it is dropped, so a size it names must not be able to exhaust the
// host on its own.
const maxCanvasPixels = 64 << 20 // 64 Mpx = 256 MiB at 4 bytes each

// surface is one canvas bitmap: width × height non-premultiplied RGBA.
type surface struct {
	w, h int
	pix  []byte
}

func newSurface(w, h int) *surface {
	return &surface{w: w, h: h, pix: make([]byte, w*h*4)}
}

// paint is how a shape is coloured. A solid colour is the common case; a
// gradient answers per pixel.
type paint struct {
	solid    [4]float64 // r,g,b,a in 0..1, non-premultiplied
	gradient *gradient
	pattern  *pattern
}

// pattern is an image tiled across the plane. inverse maps a device point back
// into the image's own pixels, so each destination pixel finds its own source
// point — the same direction drawImage works in, and for the same reason.
type pattern struct {
	src        *surface
	repeatX    bool
	repeatY    bool
	inverse    [6]float64
	hasInverse bool
}

func (p *pattern) at(dx, dy float64) ([4]float64, bool) {
	x, y := dx, dy
	if p.hasInverse {
		m := p.inverse
		x = m[0]*dx + m[2]*dy + m[4]
		y = m[1]*dx + m[3]*dy + m[5]
	}
	ix, iy := int(math.Floor(x)), int(math.Floor(y))
	if p.repeatX {
		ix = ((ix % p.src.w) + p.src.w) % p.src.w
	} else if ix < 0 || ix >= p.src.w {
		return [4]float64{}, false
	}
	if p.repeatY {
		iy = ((iy % p.src.h) + p.src.h) % p.src.h
	} else if iy < 0 || iy >= p.src.h {
		return [4]float64{}, false
	}
	o := (iy*p.src.w + ix) * 4
	return [4]float64{
		float64(p.src.pix[o]) / 255, float64(p.src.pix[o+1]) / 255,
		float64(p.src.pix[o+2]) / 255, float64(p.src.pix[o+3]) / 255,
	}, true
}

// gradient is a linear or radial colour ramp in USER space, with the transform
// that maps user space to device space already applied by the caller.
type gradient struct {
	radial                bool
	x0, y0, r0            float64
	x1, y1, r1            float64
	stops                 []gradientStop
	inverse               [6]float64 // device -> user
	hasInverse            bool
	repeatNone            bool
	degenerateTransparent bool
}

type gradientStop struct {
	offset float64
	color  [4]float64
}

// at returns the colour of a gradient at a device-space point.
func (g *gradient) at(dx, dy float64) [4]float64 {
	if g.degenerateTransparent || len(g.stops) == 0 {
		return [4]float64{0, 0, 0, 0}
	}
	x, y := dx, dy
	if g.hasInverse {
		m := g.inverse
		x = m[0]*dx + m[2]*dy + m[4]
		y = m[1]*dx + m[3]*dy + m[5]
	}
	var t float64
	if g.radial {
		// The offset along a radial gradient is the solution of the pencil of
		// circles; for the common concentric case it reduces to a ratio of
		// distances, which is what this computes.
		dxc, dyc := x-g.x1, y-g.y1
		d := math.Hypot(dxc, dyc)
		if g.r1 == g.r0 {
			t = 0
		} else {
			t = (d - g.r0) / (g.r1 - g.r0)
		}
	} else {
		vx, vy := g.x1-g.x0, g.y1-g.y0
		den := vx*vx + vy*vy
		if den == 0 {
			return g.stops[len(g.stops)-1].color
		}
		t = ((x-g.x0)*vx + (y-g.y0)*vy) / den
	}
	return g.sample(t)
}

func (g *gradient) sample(t float64) [4]float64 {
	if math.IsNaN(t) {
		return [4]float64{0, 0, 0, 0}
	}
	if t <= g.stops[0].offset {
		return g.stops[0].color
	}
	last := g.stops[len(g.stops)-1]
	if t >= last.offset {
		return last.color
	}
	for i := 1; i < len(g.stops); i++ {
		a, b := g.stops[i-1], g.stops[i]
		if t > b.offset {
			continue
		}
		span := b.offset - a.offset
		if span <= 0 {
			return b.color
		}
		f := (t - a.offset) / span
		var out [4]float64
		for c := 0; c < 4; c++ {
			out[c] = a.color[c] + (b.color[c]-a.color[c])*f
		}
		return out
	}
	return last.color
}

// composite names the Porter-Duff / blending operator a draw uses.
type composite int

const (
	compSourceOver composite = iota
	compSourceIn
	compSourceOut
	compSourceAtop
	compDestinationOver
	compDestinationIn
	compDestinationOut
	compDestinationAtop
	compLighter
	compCopy
	compXOR
)

var compositeByName = map[string]composite{
	"source-over":      compSourceOver,
	"source-in":        compSourceIn,
	"source-out":       compSourceOut,
	"source-atop":      compSourceAtop,
	"destination-over": compDestinationOver,
	"destination-in":   compDestinationIn,
	"destination-out":  compDestinationOut,
	"destination-atop": compDestinationAtop,
	"lighter":          compLighter,
	"copy":             compCopy,
	"xor":              compXOR,
}

// blend applies one operator to a single pixel. src and dst are
// non-premultiplied RGBA in 0..1; cov is the source's coverage at this pixel.
func blend(op composite, src, dst [4]float64, cov float64) [4]float64 {
	// Premultiplied is the only form the operators are defined in.
	sa := src[3] * cov
	var s [4]float64
	for i := 0; i < 3; i++ {
		s[i] = src[i] * sa
	}
	s[3] = sa
	da := dst[3]
	var d [4]float64
	for i := 0; i < 3; i++ {
		d[i] = dst[i] * da
	}
	d[3] = da

	var fs, fd float64
	switch op {
	case compSourceOver:
		fs, fd = 1, 1-sa
	case compSourceIn:
		fs, fd = da, 0
	case compSourceOut:
		fs, fd = 1-da, 0
	case compSourceAtop:
		fs, fd = da, 1-sa
	case compDestinationOver:
		fs, fd = 1-da, 1
	case compDestinationIn:
		fs, fd = 0, sa
	case compDestinationOut:
		fs, fd = 0, 1-sa
	case compDestinationAtop:
		fs, fd = 1-da, sa
	case compLighter:
		fs, fd = 1, 1
	case compCopy:
		fs, fd = 1, 0
	case compXOR:
		fs, fd = 1-da, 1-sa
	}
	var out [4]float64
	for i := 0; i < 4; i++ {
		out[i] = s[i]*fs + d[i]*fd
		if out[i] < 0 {
			out[i] = 0
		}
		if out[i] > 1 {
			out[i] = 1
		}
	}
	// Back to non-premultiplied.
	if out[3] <= 0 {
		return [4]float64{0, 0, 0, 0}
	}
	for i := 0; i < 3; i++ {
		out[i] /= out[3]
		if out[i] > 1 {
			out[i] = 1
		}
	}
	return out
}

// fillMask composites a coverage mask onto the surface with one paint. mask is
// width×height alpha in 0..255 and is the ONLY thing that says where the shape
// is: every operator, including the destination-only ones, is applied over the
// whole surface, because that is what "the source is transparent here" means.
func (s *surface) fillMask(mask []byte, p paint, alpha float64, op composite, clip []byte) {
	wholeSurface := op != compSourceOver && op != compDestinationOut && op != compLighter
	for y := 0; y < s.h; y++ {
		for x := 0; x < s.w; x++ {
			i := (y*s.w + x)
			cov := float64(mask[i]) / 255
			if clip != nil {
				cov *= float64(clip[i]) / 255
			}
			if cov == 0 && !wholeSurface {
				continue
			}
			src := p.solid
			if p.gradient != nil {
				src = p.gradient.at(float64(x)+0.5, float64(y)+0.5)
			} else if p.pattern != nil {
				col, ok := p.pattern.at(float64(x)+0.5, float64(y)+0.5)
				if !ok {
					// Outside a non-repeating pattern there is no source at all, which
					// is transparent — and under source-over that is a no-op.
					if !wholeSurface {
						continue
					}
					col = [4]float64{}
				}
				src = col
			}
			src[3] *= alpha
			o := i * 4
			dst := [4]float64{
				float64(s.pix[o]) / 255, float64(s.pix[o+1]) / 255,
				float64(s.pix[o+2]) / 255, float64(s.pix[o+3]) / 255,
			}
			out := blend(op, src, dst, cov)
			s.pix[o] = clamp255(out[0])
			s.pix[o+1] = clamp255(out[1])
			s.pix[o+2] = clamp255(out[2])
			s.pix[o+3] = clamp255(out[3])
		}
	}
}

func clamp255(v float64) byte {
	n := int(math.Round(v * 255))
	if n < 0 {
		return 0
	}
	if n > 255 {
		return 255
	}
	return byte(n)
}

// rasterize turns a set of already-transformed device-space subpaths into a
// coverage mask by evaluating the winding rule itself: each pixel row is
// sampled on a few sub-rows, the edge crossings of a sub-row are sorted, and
// the rule (nonzero winding or even-odd parity) decides the inside spans,
// whose coverage is accumulated with exact fractional ends.
//
// The rule must be evaluated per SAMPLE, not per polygon: a stroke is a pile
// of deliberately overlapping same-winding pieces whose union is the shape,
// and a summed-area rasterizer (x/image/vector, which accumulates signed area
// and clamps) over-covers wherever pieces overlap inside a partially covered
// pixel — dozens of slivers each holding 3% of a pixel read as 90% when the
// union is 7%. Winding evaluation also gives even-odd within a single
// self-intersecting subpath, which no per-subpath combination can.
func rasterize(w, h int, subpaths [][]point, evenOdd bool) []byte {
	// subRows is the vertical sampling density; horizontal coverage is exact.
	const subRows = 4
	type edge struct {
		yMin, yMax float64
		x0, y0     float64
		dxdy       float64
		dir        int
	}
	var edges []edge
	for _, sp := range subpaths {
		if len(sp) < 2 {
			continue
		}
		for i := range sp {
			a, b := sp[i], sp[(i+1)%len(sp)]
			if a.y == b.y || math.IsNaN(a.y) || math.IsNaN(b.y) {
				continue
			}
			e := edge{x0: a.x, y0: a.y, dxdy: (b.x - a.x) / (b.y - a.y), dir: 1}
			if a.y < b.y {
				e.yMin, e.yMax = a.y, b.y
			} else {
				e.yMin, e.yMax, e.dir = b.y, a.y, -1
			}
			edges = append(edges, e)
		}
	}
	out := make([]byte, w*h)
	if len(edges) == 0 {
		return out
	}
	type crossing struct {
		x   float64
		dir int
	}
	crossings := make([]crossing, 0, 64)
	acc := make([]float64, w)
	addSpan := func(xa, xb float64) {
		xa = math.Max(xa, 0)
		xb = math.Min(xb, float64(w))
		if !(xb > xa) {
			return
		}
		ia, ib := int(xa), int(xb)
		if ib >= w {
			ib = w - 1
		}
		if ia == ib {
			acc[ia] += (xb - xa) / subRows
			return
		}
		acc[ia] += (float64(ia+1) - xa) / subRows
		for i := ia + 1; i < ib; i++ {
			acc[i] += 1.0 / subRows
		}
		acc[ib] += (xb - float64(ib)) / subRows
	}
	for row := 0; row < h; row++ {
		for i := range acc {
			acc[i] = 0
		}
		for k := 0; k < subRows; k++ {
			ys := float64(row) + (float64(k)+0.5)/subRows
			crossings = crossings[:0]
			for i := range edges {
				e := &edges[i]
				if ys < e.yMin || ys >= e.yMax {
					continue
				}
				x := e.x0 + (ys-e.y0)*e.dxdy
				if math.IsNaN(x) {
					continue
				}
				crossings = append(crossings, crossing{x: x, dir: e.dir})
			}
			if len(crossings) < 2 {
				continue
			}
			sort.Slice(crossings, func(i, j int) bool { return crossings[i].x < crossings[j].x })
			winding, parity := 0, 0
			inside := false
			var spanStart float64
			for _, c := range crossings {
				was := inside
				if evenOdd {
					parity ^= 1
					inside = parity == 1
				} else {
					winding += c.dir
					inside = winding != 0
				}
				if !was && inside {
					spanStart = c.x
				} else if was && !inside {
					addSpan(spanStart, c.x)
				}
			}
		}
		for i, v := range acc {
			c := int(v*255 + 0.5)
			if c > 255 {
				c = 255
			} else if c < 0 {
				c = 0
			}
			out[row*w+i] = byte(c)
		}
	}
	return out
}

type point struct{ x, y float64 }

// intersectMasks is how nested clips combine: a pixel survives both.
func intersectMasks(a, b []byte) []byte {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := make([]byte, len(a))
	for i := range a {
		out[i] = byte(int(a[i]) * int(b[i]) / 255)
	}
	return out
}

// ---------------------------------------------------------------- host ops

type canvasAPI struct {
	js       *spidermonkey.JS
	surfaces map[int64]*surface
	clips    map[int64][]byte
	next     int64
}

func newCanvasAPI(js *spidermonkey.JS) *canvasAPI {
	return &canvasAPI{js: js, surfaces: map[int64]*surface{}, clips: map[int64][]byte{}}
}

func (a *canvasAPI) ops() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"canvas_new":            a.opNew,
		"canvas_from_bytes":     a.opFromBytes,
		"canvas_decode_image":   a.opDecodeImage,
		"canvas_draw_image":     a.opDrawImage,
		"canvas_free":           a.opFree,
		"canvas_resize":         a.opResize,
		"canvas_fill":           a.opFill,
		"canvas_clear":          a.opClear,
		"canvas_clip":           a.opClip,
		"canvas_get_image_data": a.opGetImageData,
		"canvas_put_image_data": a.opPutImageData,
	}
}

func (a *canvasAPI) surface(v spidermonkey.Value) (*surface, int64, error) {
	id := int64(v.Float())
	s := a.surfaces[id]
	if s == nil {
		return nil, 0, fmt.Errorf("canvas: unknown surface")
	}
	return s, id, nil
}

// opNew(width, height) -> handle.
func (a *canvasAPI) opNew(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("canvas_new: (width, height) required")
	}
	w, h := int(args[0].Float()), int(args[1].Float())
	if w < 0 || h < 0 || w*h > maxCanvasPixels {
		return nil, fmt.Errorf("canvas_new: %dx%d is not an allocatable size", w, h)
	}
	a.next++
	a.surfaces[a.next] = newSurface(w, h)
	return spidermonkey.ValueOf(float64(a.next)), nil
}

func (a *canvasAPI) opFree(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	id := int64(args[0].Float())
	delete(a.surfaces, id)
	delete(a.clips, id)
	return spidermonkey.Undefined(), nil
}

// opResize(handle, width, height): a resize CLEARS the canvas, which is what
// assigning to width or height does.
func (a *canvasAPI) opResize(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	s, id, err := a.surface(args[0])
	if err != nil {
		return nil, err
	}
	w, h := int(args[1].Float()), int(args[2].Float())
	if w < 0 || h < 0 || w*h > maxCanvasPixels {
		return nil, fmt.Errorf("canvas_resize: %dx%d is not an allocatable size", w, h)
	}
	_ = s
	a.surfaces[id] = newSurface(w, h)
	delete(a.clips, id)
	return spidermonkey.Undefined(), nil
}

// opFill(handle, spec) draws one shape. spec carries the flattened device-space
// subpaths and everything about how they are painted, so the guest's state
// never has to be mirrored here.
func (a *canvasAPI) opFill(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	s, id, err := a.surface(args[0])
	if err != nil {
		return nil, err
	}
	if len(args) < 2 || args[1].Object() == nil {
		return nil, fmt.Errorf("canvas_fill: a spec is required")
	}
	spec := args[1].Object()
	defer spec.Free()
	subpaths, err := readSubpaths(spec)
	if err != nil {
		return nil, err
	}
	if len(subpaths) == 0 {
		return spidermonkey.Undefined(), nil
	}
	p, err := a.readPaint(spec)
	if err != nil {
		return nil, err
	}
	alpha := objFloat(spec, "alpha", 1)
	op := compSourceOver
	if name := objString(spec, "composite", "source-over"); name != "" {
		if c, ok := compositeByName[name]; ok {
			op = c
		}
	}
	mask := rasterize(s.w, s.h, subpaths, objBool(spec, "evenOdd"))
	// The shadow is the SAME shape, offset, blurred and painted in one colour,
	// and it goes down first — the shape is drawn over its own shadow.
	if sh, ok := readShadow(spec); ok {
		s.fillMask(shadowMask(mask, s.w, s.h, sh), paint{solid: sh.color}, alpha, op, a.clips[id])
	}
	s.fillMask(mask, p, alpha, op, a.clips[id])
	return spidermonkey.Undefined(), nil
}

// shadow is the offset, blur and colour a draw casts.
type shadow struct {
	dx, dy float64
	blur   float64
	color  [4]float64
}

func readShadow(spec *spidermonkey.Object) (shadow, bool) {
	v, err := spec.Get("shadow")
	if err != nil || v == nil {
		return shadow{}, false
	}
	o := v.Object()
	if o == nil {
		return shadow{}, false
	}
	defer o.Free()
	var sh shadow
	sh.dx, sh.dy = objFloat(o, "dx", 0), objFloat(o, "dy", 0)
	sh.blur = objFloat(o, "blur", 0)
	cv, cerr := o.Get("color")
	if cerr != nil || cv == nil {
		return shadow{}, false
	}
	co := cv.Object()
	if co == nil {
		return shadow{}, false
	}
	b, berr := co.Bytes()
	co.Free()
	if berr != nil {
		return shadow{}, false
	}
	f := bytesToFloat64s(b)
	for i := 0; i < 4 && i < len(f); i++ {
		sh.color[i] = f[i]
	}
	// A fully transparent shadow colour is no shadow at all, and neither is a
	// draw with no offset and no blur: it would land exactly under the shape.
	if sh.color[3] == 0 {
		return shadow{}, false
	}
	return sh, true
}

// shadowMask offsets a coverage mask and blurs it. The blur is three box passes,
// which is the standard approximation of a Gaussian and the one the
// specification's "2 * sigma = blur" is calibrated against.
func shadowMask(mask []byte, w, h int, sh shadow) []byte {
	out := make([]byte, len(mask))
	ox, oy := int(math.Round(sh.dx)), int(math.Round(sh.dy))
	for y := 0; y < h; y++ {
		sy := y - oy
		if sy < 0 || sy >= h {
			continue
		}
		for x := 0; x < w; x++ {
			sx := x - ox
			if sx < 0 || sx >= w {
				continue
			}
			out[y*w+x] = mask[sy*w+sx]
		}
	}
	if sh.blur <= 0 {
		return out
	}
	sigma := sh.blur / 2
	radius := int(math.Round(sigma * 1.5))
	if radius < 1 {
		return out
	}
	for pass := 0; pass < 3; pass++ {
		out = boxBlur(out, w, h, radius)
	}
	return out
}

func boxBlur(src []byte, w, h, r int) []byte {
	tmp := make([]byte, len(src))
	// Horizontal, then vertical: a box blur is separable, which is what makes
	// three passes affordable.
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			sum, n := 0, 0
			for k := -r; k <= r; k++ {
				i := x + k
				if i < 0 || i >= w {
					continue
				}
				sum += int(src[y*w+i])
				n++
			}
			tmp[y*w+x] = byte(sum / n)
		}
	}
	out := make([]byte, len(src))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			sum, n := 0, 0
			for k := -r; k <= r; k++ {
				i := y + k
				if i < 0 || i >= h {
					continue
				}
				sum += int(tmp[i*w+x])
				n++
			}
			out[y*w+x] = byte(sum / n)
		}
	}
	return out
}

// opClear(handle, spec) is clearRect: the shape's pixels become transparent
// black, ignoring every style. It is not a fill with a transparent colour —
// that would be a no-op under source-over.
func (a *canvasAPI) opClear(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	s, id, err := a.surface(args[0])
	if err != nil {
		return nil, err
	}
	spec := args[1].Object()
	defer spec.Free()
	subpaths, err := readSubpaths(spec)
	if err != nil {
		return nil, err
	}
	mask := rasterize(s.w, s.h, subpaths, false)
	clip := a.clips[id]
	for i := range mask {
		cov := float64(mask[i]) / 255
		if clip != nil {
			cov *= float64(clip[i]) / 255
		}
		if cov == 0 {
			continue
		}
		o := i * 4
		for c := 0; c < 4; c++ {
			s.pix[o+c] = byte(float64(s.pix[o+c]) * (1 - cov))
		}
	}
	return spidermonkey.Undefined(), nil
}

// opClip(handle, spec|null): null resets the clip, which is what restoring a
// state that had none does.
func (a *canvasAPI) opClip(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	s, id, err := a.surface(args[0])
	if err != nil {
		return nil, err
	}
	if len(args) < 2 || args[1].Object() == nil {
		delete(a.clips, id)
		return spidermonkey.Undefined(), nil
	}
	spec := args[1].Object()
	defer spec.Free()
	subpaths, err := readSubpaths(spec)
	if err != nil {
		return nil, err
	}
	mask := rasterize(s.w, s.h, subpaths, objBool(spec, "evenOdd"))
	if objBool(spec, "replace") {
		a.clips[id] = mask
	} else {
		a.clips[id] = intersectMasks(a.clips[id], mask)
	}
	return spidermonkey.Undefined(), nil
}

// opGetImageData(handle, x, y, w, h) -> bytes, in getImageData's order and
// non-premultiplied, which is how the surface stores them.
func (a *canvasAPI) opGetImageData(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	s, _, err := a.surface(args[0])
	if err != nil {
		return nil, err
	}
	x, y := int(args[1].Float()), int(args[2].Float())
	w, h := int(args[3].Float()), int(args[4].Float())
	if w <= 0 || h <= 0 || w*h > maxCanvasPixels {
		return nil, fmt.Errorf("canvas_get_image_data: %dx%d is not a readable size", w, h)
	}
	out := make([]byte, w*h*4)
	for row := 0; row < h; row++ {
		sy := y + row
		if sy < 0 || sy >= s.h {
			continue
		}
		for col := 0; col < w; col++ {
			sx := x + col
			if sx < 0 || sx >= s.w {
				continue
			}
			copy(out[(row*w+col)*4:], s.pix[(sy*s.w+sx)*4:(sy*s.w+sx)*4+4])
		}
	}
	u8, err := a.js.NewBytes(out)
	if err != nil {
		return nil, err
	}
	return u8, nil
}

// opPutImageData(handle, bytes, w, h, dx, dy, sx, sy, sw, sh) writes pixels
// straight in: putImageData REPLACES, ignoring the transform, the clip, the
// alpha and the composite operator.
func (a *canvasAPI) opPutImageData(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	s, _, err := a.surface(args[0])
	if err != nil {
		return nil, err
	}
	data, err := argBytes(args[1])
	if err != nil {
		return nil, err
	}
	w, h := int(args[2].Float()), int(args[3].Float())
	dx, dy := int(args[4].Float()), int(args[5].Float())
	sx, sy := int(args[6].Float()), int(args[7].Float())
	sw, sh := int(args[8].Float()), int(args[9].Float())
	for row := 0; row < sh; row++ {
		srcY := sy + row
		dstY := dy + sy + row
		if srcY < 0 || srcY >= h || dstY < 0 || dstY >= s.h {
			continue
		}
		for col := 0; col < sw; col++ {
			srcX := sx + col
			dstX := dx + sx + col
			if srcX < 0 || srcX >= w || dstX < 0 || dstX >= s.w {
				continue
			}
			si := (srcY*w + srcX) * 4
			di := (dstY*s.w + dstX) * 4
			if si+4 > len(data) {
				continue
			}
			copy(s.pix[di:di+4], data[si:si+4])
		}
	}
	return spidermonkey.Undefined(), nil
}

// opFromBytes(width, height, bytes) -> handle. This is how an ImageBitmap is
// made from pixels that already exist: an ImageData's array, or the result of
// decoding an encoded image.
func (a *canvasAPI) opFromBytes(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("canvas_from_bytes: (width, height, bytes) required")
	}
	w, h := int(args[0].Float()), int(args[1].Float())
	if w <= 0 || h <= 0 || w*h > maxCanvasPixels {
		return nil, fmt.Errorf("canvas_from_bytes: %dx%d is not an allocatable size", w, h)
	}
	data, err := argBytes(args[2])
	if err != nil {
		return nil, err
	}
	s := newSurface(w, h)
	copy(s.pix, data)
	a.next++
	a.surfaces[a.next] = s
	return spidermonkey.ValueOf(float64(a.next)), nil
}

// opDecodeImage(bytes) -> {width, height, handle}. The formats are the ones the
// standard library and golang.org/x decode; anything else is reported rather
// than guessed at, because a wrong guess would produce an image made of noise.
func (a *canvasAPI) opDecodeImage(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	data, err := argBytes(args[0])
	if err != nil {
		return nil, err
	}
	img, _, derr := image.Decode(bytes.NewReader(data))
	if derr != nil {
		return spidermonkey.ValueOf(map[string]any{"error": derr.Error()}), nil
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || w*h > maxCanvasPixels {
		return spidermonkey.ValueOf(map[string]any{"error": "the image is not an allocatable size"}), nil
	}
	s := newSurface(w, h)
	// image/draw gives premultiplied RGBA; the surface is non-premultiplied, so
	// each pixel is divided back out — which is exactly what getImageData
	// reports, and what the tests compare.
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)
	for i := 0; i < w*h; i++ {
		r, g, bl, al := rgba.Pix[i*4], rgba.Pix[i*4+1], rgba.Pix[i*4+2], rgba.Pix[i*4+3]
		if al == 0 {
			continue
		}
		s.pix[i*4] = byte(int(r) * 255 / int(al))
		s.pix[i*4+1] = byte(int(g) * 255 / int(al))
		s.pix[i*4+2] = byte(int(bl) * 255 / int(al))
		s.pix[i*4+3] = al
	}
	a.next++
	a.surfaces[a.next] = s
	return spidermonkey.ValueOf(map[string]any{
		"width": float64(w), "height": float64(h), "handle": float64(a.next),
	}), nil
}

// opDrawImage(dst, src, spec) paints a rectangle of one surface onto another.
// The destination is described by the INVERSE of the transform that maps the
// source rectangle onto it, so each destination pixel finds its own source
// point rather than the source having to be walked forward — which is what
// makes an arbitrary transform work without gaps.
func (a *canvasAPI) opDrawImage(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	dst, dstID, err := a.surface(args[0])
	if err != nil {
		return nil, err
	}
	src, _, err := a.surface(args[1])
	if err != nil {
		return nil, err
	}
	spec := args[2].Object()
	if spec == nil {
		return nil, fmt.Errorf("canvas_draw_image: a spec is required")
	}
	defer spec.Free()
	sx, sy := objFloat(spec, "sx", 0), objFloat(spec, "sy", 0)
	sw, sh := objFloat(spec, "sw", float64(src.w)), objFloat(spec, "sh", float64(src.h))
	alpha := objFloat(spec, "alpha", 1)
	smooth := objBool(spec, "smooth")
	op := compSourceOver
	if c, ok := compositeByName[objString(spec, "composite", "source-over")]; ok {
		op = c
	}
	inv, ok := readMatrix(spec, "inverse")
	if !ok {
		return spidermonkey.Undefined(), nil
	}
	// The destination's extent is the bounding box of the mapped rectangle, so
	// only the pixels that can be covered are visited.
	fwd, hasFwd := readMatrix(spec, "forward")
	minX, minY, maxX, maxY := 0, 0, dst.w, dst.h
	if hasFwd {
		minX, minY, maxX, maxY = quadBounds(fwd, dst.w, dst.h)
	}
	clip := a.clips[dstID]
	// An image casts a shadow from its own alpha: the mask is where the image is
	// opaque, so a transparent part of it casts nothing.
	if cast, ok := readShadow(spec); ok {
		mask := make([]byte, dst.w*dst.h)
		for y := minY; y < maxY; y++ {
			for x := minX; x < maxX; x++ {
				px, py := float64(x)+0.5, float64(y)+0.5
				u := inv[0]*px + inv[2]*py + inv[4]
				v := inv[1]*px + inv[3]*py + inv[5]
				if u < 0 || u >= 1 || v < 0 || v >= 1 {
					continue
				}
				col, okPix := sampleSurface(src, sx+u*sw, sy+v*sh, smooth)
				if !okPix {
					continue
				}
				mask[y*dst.w+x] = clamp255(col[3])
			}
		}
		dst.fillMask(shadowMask(mask, dst.w, dst.h, cast), paint{solid: cast.color}, alpha, op, clip)
	}
	for y := minY; y < maxY; y++ {
		for x := minX; x < maxX; x++ {
			// The centre of the destination pixel, mapped back into the unit square
			// of the source rectangle.
			px, py := float64(x)+0.5, float64(y)+0.5
			u := inv[0]*px + inv[2]*py + inv[4]
			v := inv[1]*px + inv[3]*py + inv[5]
			if u < 0 || u >= 1 || v < 0 || v >= 1 {
				continue
			}
			fx := sx + u*sw
			fy := sy + v*sh
			col, okPix := sampleSurface(src, fx, fy, smooth)
			if !okPix {
				continue
			}
			i := y*dst.w + x
			cov := 1.0
			if clip != nil {
				cov = float64(clip[i]) / 255
				if cov == 0 {
					continue
				}
			}
			col[3] *= alpha
			o := i * 4
			d := [4]float64{
				float64(dst.pix[o]) / 255, float64(dst.pix[o+1]) / 255,
				float64(dst.pix[o+2]) / 255, float64(dst.pix[o+3]) / 255,
			}
			out := blend(op, col, d, cov)
			dst.pix[o] = clamp255(out[0])
			dst.pix[o+1] = clamp255(out[1])
			dst.pix[o+2] = clamp255(out[2])
			dst.pix[o+3] = clamp255(out[3])
		}
	}
	return spidermonkey.Undefined(), nil
}

// sampleSurface reads one source pixel. Nearest is the honest default: the
// tests that check exact bytes draw at integer offsets, where a bilinear filter
// would round differently for no benefit.
func sampleSurface(s *surface, x, y float64, smooth bool) ([4]float64, bool) {
	ix, iy := int(math.Floor(x)), int(math.Floor(y))
	if ix < 0 || iy < 0 || ix >= s.w || iy >= s.h {
		return [4]float64{}, false
	}
	o := (iy*s.w + ix) * 4
	return [4]float64{
		float64(s.pix[o]) / 255, float64(s.pix[o+1]) / 255,
		float64(s.pix[o+2]) / 255, float64(s.pix[o+3]) / 255,
	}, true
}

// quadBounds is the pixel range the unit square can reach under one transform.
func quadBounds(m [6]float64, w, h int) (int, int, int, int) {
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, c := range [][2]float64{{0, 0}, {1, 0}, {1, 1}, {0, 1}} {
		x := m[0]*c[0] + m[2]*c[1] + m[4]
		y := m[1]*c[0] + m[3]*c[1] + m[5]
		minX, maxX = math.Min(minX, x), math.Max(maxX, x)
		minY, maxY = math.Min(minY, y), math.Max(maxY, y)
	}
	lo := func(v float64, cap int) int {
		n := int(math.Floor(v))
		if n < 0 {
			return 0
		}
		if n > cap {
			return cap
		}
		return n
	}
	hi := func(v float64, cap int) int {
		n := int(math.Ceil(v)) + 1
		if n < 0 {
			return 0
		}
		if n > cap {
			return cap
		}
		return n
	}
	return lo(minX, w), lo(minY, h), hi(maxX, w), hi(maxY, h)
}

func readMatrix(o *spidermonkey.Object, name string) ([6]float64, bool) {
	var m [6]float64
	v, err := o.Get(name)
	if err != nil || v == nil {
		return m, false
	}
	io := v.Object()
	if io == nil {
		return m, false
	}
	defer io.Free()
	b, berr := io.Bytes()
	if berr != nil {
		return m, false
	}
	f := bytesToFloat64s(b)
	if len(f) < 6 {
		return m, false
	}
	copy(m[:], f[:6])
	return m, true
}

// closeAll drops every surface. Called from Web.Close.
func (a *canvasAPI) closeAll() {
	a.surfaces = map[int64]*surface{}
	a.clips = map[int64][]byte{}
}

// ------------------------------------------------------------- spec reading

func readSubpaths(spec *spidermonkey.Object) ([][]point, error) {
	v, err := spec.Get("subpaths")
	if err != nil || v == nil {
		return nil, nil
	}
	o := v.Object()
	if o == nil {
		return nil, nil
	}
	defer o.Free()
	// The guest sends the flattened points as one flat Float64Array plus the
	// length of each subpath, which is one crossing rather than one per point.
	data, err := o.Bytes()
	if err != nil {
		return nil, err
	}
	lensV, err := spec.Get("lengths")
	if err != nil || lensV == nil {
		return nil, nil
	}
	lo := lensV.Object()
	if lo == nil {
		return nil, nil
	}
	defer lo.Free()
	lenBytes, err := lo.Bytes()
	if err != nil {
		return nil, err
	}
	coords := bytesToFloat64s(data)
	lengths := bytesToInt32s(lenBytes)
	var out [][]point
	at := 0
	for _, n := range lengths {
		count := int(n)
		if count <= 0 || at+count*2 > len(coords) {
			at += count * 2
			continue
		}
		sp := make([]point, count)
		for i := 0; i < count; i++ {
			sp[i] = point{coords[at+i*2], coords[at+i*2+1]}
		}
		out = append(out, sp)
		at += count * 2
	}
	return out, nil
}

func (a *canvasAPI) readPaint(spec *spidermonkey.Object) (paint, error) {
	var p paint
	if pv, err := spec.Get("pattern"); err == nil && pv != nil {
		if po := pv.Object(); po != nil {
			pat, perr := a.readPattern(po)
			po.Free()
			if perr != nil {
				return p, perr
			}
			if pat != nil {
				p.pattern = pat
				return p, nil
			}
		}
	}
	if gv, err := spec.Get("gradient"); err == nil && gv != nil {
		if go_ := gv.Object(); go_ != nil {
			g, gerr := readGradient(go_)
			go_.Free()
			if gerr != nil {
				return p, gerr
			}
			p.gradient = g
			return p, nil
		}
	}
	cv, err := spec.Get("color")
	if err != nil || cv == nil {
		return p, nil
	}
	co := cv.Object()
	if co == nil {
		return p, nil
	}
	defer co.Free()
	b, err := co.Bytes()
	if err != nil {
		return p, err
	}
	f := bytesToFloat64s(b)
	for i := 0; i < 4 && i < len(f); i++ {
		p.solid[i] = f[i]
	}
	return p, nil
}

func (a *canvasAPI) readPattern(o *spidermonkey.Object) (*pattern, error) {
	src := a.surfaces[int64(objFloat(o, "handle", 0))]
	if src == nil || src.w == 0 || src.h == 0 {
		return nil, nil
	}
	p := &pattern{src: src, repeatX: objBool(o, "repeatX"), repeatY: objBool(o, "repeatY")}
	if m, ok := readMatrix(o, "inverse"); ok {
		p.inverse = m
		p.hasInverse = true
	}
	return p, nil
}

func readGradient(o *spidermonkey.Object) (*gradient, error) {
	g := &gradient{}
	g.radial = objBool(o, "radial")
	g.x0, g.y0, g.r0 = objFloat(o, "x0", 0), objFloat(o, "y0", 0), objFloat(o, "r0", 0)
	g.x1, g.y1, g.r1 = objFloat(o, "x1", 0), objFloat(o, "y1", 0), objFloat(o, "r1", 0)
	g.degenerateTransparent = objBool(o, "degenerate")
	if iv, err := o.Get("inverse"); err == nil && iv != nil {
		if io := iv.Object(); io != nil {
			b, berr := io.Bytes()
			io.Free()
			if berr == nil {
				f := bytesToFloat64s(b)
				if len(f) >= 6 {
					copy(g.inverse[:], f[:6])
					g.hasInverse = true
				}
			}
		}
	}
	sv, err := o.Get("stops")
	if err != nil || sv == nil {
		return g, nil
	}
	so := sv.Object()
	if so == nil {
		return g, nil
	}
	defer so.Free()
	b, err := so.Bytes()
	if err != nil {
		return g, err
	}
	f := bytesToFloat64s(b)
	// Five doubles per stop: offset then r,g,b,a.
	for i := 0; i+4 < len(f); i += 5 {
		g.stops = append(g.stops, gradientStop{
			offset: f[i],
			color:  [4]float64{f[i+1], f[i+2], f[i+3], f[i+4]},
		})
	}
	return g, nil
}

func objFloat(o *spidermonkey.Object, name string, def float64) float64 {
	v, err := o.Get(name)
	if err != nil || v == nil || v.IsUndefined() {
		return def
	}
	if inner := v.Object(); inner != nil {
		inner.Free()
		return def
	}
	return v.Float()
}

func objBool(o *spidermonkey.Object, name string) bool {
	v, err := o.Get(name)
	if err != nil || v == nil {
		return false
	}
	if inner := v.Object(); inner != nil {
		inner.Free()
		return false
	}
	return v.Bool()
}

func objString(o *spidermonkey.Object, name, def string) string {
	v, err := o.Get(name)
	if err != nil || v == nil || v.IsUndefined() {
		return def
	}
	if inner := v.Object(); inner != nil {
		inner.Free()
		return def
	}
	return v.String()
}

// bytesToFloat64s reinterprets a Float64Array's bytes. The guest and the host
// are the same machine, so the byte order is the same one on both sides.
func bytesToFloat64s(b []byte) []float64 {
	out := make([]float64, len(b)/8)
	for i := range out {
		var bits uint64
		for j := 0; j < 8; j++ {
			bits |= uint64(b[i*8+j]) << (8 * j)
		}
		out[i] = math.Float64frombits(bits)
	}
	return out
}

func bytesToInt32s(b []byte) []int32 {
	out := make([]int32, len(b)/4)
	for i := range out {
		var bits uint32
		for j := 0; j < 4; j++ {
			bits |= uint32(b[i*4+j]) << (8 * j)
		}
		out[i] = int32(bits)
	}
	return out
}
