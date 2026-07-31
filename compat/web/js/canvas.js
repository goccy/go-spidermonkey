// compat/web: OffscreenCanvas and its 2d context
// (https://html.spec.whatwg.org/multipage/canvas.html).
//
// The STATE is here and the PIXELS are in Go. That split is the whole design:
// the save/restore stack, the current transform, the styles and the path are
// what the specification describes and what a caller observes, so they live
// where they can be read back exactly; the drawing is an algorithm — flatten a
// curve, fill by a winding rule, composite with an alpha — and belongs in Go
// (see canvas.go).
//
// A path crosses the bridge already FLATTENED into device-space polygons. Two
// reasons: the transform that applies to a segment is the one in force when the
// segment was added, which only this side knows, and a flat point list is one
// crossing for a whole shape instead of one per command.
//
// A stroke crosses as a FILL. Every segment becomes a quad, every join a wedge
// and every cap its own shape, all wound the same way so the nonzero rule
// unions them — which is what a stroke is. Doing it here keeps the host to one
// operation (fill this polygon set) instead of two that must agree.
(() => {
	"use strict";

	const ops = globalThis.__web_ops;
	const canvasNew = ops.canvas_new;
	const canvasFree = ops.canvas_free;
	const canvasResize = ops.canvas_resize;
	const canvasFill = ops.canvas_fill;
	const canvasClear = ops.canvas_clear;
	const canvasClip = ops.canvas_clip;
	const canvasGetImageData = ops.canvas_get_image_data;
	const canvasPutImageData = ops.canvas_put_image_data;

	// ------------------------------------------------------------- colours

	const NAMED_COLORS = {
		transparent: [0, 0, 0, 0], black: [0, 0, 0, 1], silver: [192, 192, 192, 1],
		gray: [128, 128, 128, 1], grey: [128, 128, 128, 1], white: [255, 255, 255, 1],
		maroon: [128, 0, 0, 1], red: [255, 0, 0, 1], purple: [128, 0, 128, 1],
		fuchsia: [255, 0, 255, 1], magenta: [255, 0, 255, 1], green: [0, 128, 0, 1],
		lime: [0, 255, 0, 1], olive: [128, 128, 0, 1], yellow: [255, 255, 0, 1],
		navy: [0, 0, 128, 1], blue: [0, 0, 255, 1], teal: [0, 128, 128, 1],
		aqua: [0, 255, 255, 1], cyan: [0, 255, 255, 1], orange: [255, 165, 0, 1],
		pink: [255, 192, 203, 1], brown: [165, 42, 42, 1], gold: [255, 215, 0, 1],
		indigo: [75, 0, 130, 1], violet: [238, 130, 238, 1], tan: [210, 180, 140, 1],
		beige: [245, 245, 220, 1], ivory: [255, 255, 240, 1], khaki: [240, 230, 140, 1],
		salmon: [250, 128, 114, 1], coral: [255, 127, 80, 1], crimson: [220, 20, 60, 1],
		darkblue: [0, 0, 139, 1], darkgreen: [0, 100, 0, 1], darkred: [139, 0, 0, 1],
		lightblue: [173, 216, 230, 1], lightgreen: [144, 238, 144, 1],
		lightgray: [211, 211, 211, 1], lightgrey: [211, 211, 211, 1],
	};

	// parseColor answers [r,g,b,a] with r/g/b as bytes and a in 0..1, or null
	// when the value is not a colour at all — which the setters treat as "leave
	// the style alone", because an unparseable style is ignored rather than
	// throwing.
	function parseColor(input) {
		const s = String(input).trim().toLowerCase();
		if (Object.prototype.hasOwnProperty.call(NAMED_COLORS, s)) return NAMED_COLORS[s].slice();
		if (s.startsWith("#")) {
			const hex = s.slice(1);
			const expand = (c) => parseInt(c + c, 16);
			if (/^[0-9a-f]{3}$/.test(hex)) return [expand(hex[0]), expand(hex[1]), expand(hex[2]), 1];
			if (/^[0-9a-f]{4}$/.test(hex)) return [expand(hex[0]), expand(hex[1]), expand(hex[2]), expand(hex[3]) / 255];
			if (/^[0-9a-f]{6}$/.test(hex)) {
				return [parseInt(hex.slice(0, 2), 16), parseInt(hex.slice(2, 4), 16), parseInt(hex.slice(4, 6), 16), 1];
			}
			if (/^[0-9a-f]{8}$/.test(hex)) {
				return [parseInt(hex.slice(0, 2), 16), parseInt(hex.slice(2, 4), 16),
					parseInt(hex.slice(4, 6), 16), parseInt(hex.slice(6, 8), 16) / 255];
			}
			return null;
		}
		const fn = /^(rgba?|hsla?)\s*\(([^)]*)\)$/.exec(s);
		if (!fn) return null;
		const name = fn[1];
		const parts = fn[2].split(/[\s,/]+/).filter((p) => p !== "");
		if (parts.length < 3) return null;
		const num = (p, scale) => {
			if (p.endsWith("%")) return parseFloat(p) / 100 * scale;
			return parseFloat(p);
		};
		const alpha = parts.length > 3 ? Math.min(1, Math.max(0, num(parts[3], 1))) : 1;
		if (Number.isNaN(alpha)) return null;
		if (name === "rgb" || name === "rgba") {
			const rgb = [0, 1, 2].map((i) => Math.round(Math.min(255, Math.max(0, num(parts[i], 255)))));
			if (rgb.some(Number.isNaN)) return null;
			return [rgb[0], rgb[1], rgb[2], alpha];
		}
		const hue = ((parseFloat(parts[0]) % 360) + 360) % 360;
		const sat = Math.min(1, Math.max(0, parseFloat(parts[1]) / 100));
		const light = Math.min(1, Math.max(0, parseFloat(parts[2]) / 100));
		if ([hue, sat, light].some(Number.isNaN)) return null;
		const rgb = hslToRgb(hue, sat, light);
		return [rgb[0], rgb[1], rgb[2], alpha];
	}

	function hslToRgb(h, s, l) {
		const c = (1 - Math.abs(2 * l - 1)) * s;
		const x = c * (1 - Math.abs(((h / 60) % 2) - 1));
		const m = l - c / 2;
		let r = 0, g = 0, b = 0;
		if (h < 60) { r = c; g = x; } else if (h < 120) { r = x; g = c; } else if (h < 180) { g = c; b = x; } else if (h < 240) { g = x; b = c; } else if (h < 300) { r = x; b = c; } else { r = c; b = x; }
		return [Math.round((r + m) * 255), Math.round((g + m) * 255), Math.round((b + m) * 255)];
	}

	// serializeColor is what the fillStyle getter answers with: the shortest
	// hex form when the colour is opaque, and rgba() otherwise. That is the
	// serialization the specification defines and the tests read back.
	function serializeColor(c) {
		const [r, g, b, a] = c;
		if (a >= 1) {
			const hex = (n) => n.toString(16).padStart(2, "0");
			return `#${hex(r)}${hex(g)}${hex(b)}`;
		}
		// The alpha is serialized without a trailing zero run, as CSS does.
		const alpha = Number(a.toFixed(10)).toString();
		return `rgba(${r}, ${g}, ${b}, ${alpha})`;
	}

	// ------------------------------------------------------------ matrices
	// A transform is [a, b, c, d, e, f], the same order setTransform takes.

	const IDENTITY = [1, 0, 0, 1, 0, 0];
	const multiply = (m, n) => [
		m[0] * n[0] + m[2] * n[1],
		m[1] * n[0] + m[3] * n[1],
		m[0] * n[2] + m[2] * n[3],
		m[1] * n[2] + m[3] * n[3],
		m[0] * n[4] + m[2] * n[5] + m[4],
		m[1] * n[4] + m[3] * n[5] + m[5],
	];
	const applyMatrix = (m, x, y) => [m[0] * x + m[2] * y + m[4], m[1] * x + m[3] * y + m[5]];
	function invertMatrix(m) {
		const det = m[0] * m[3] - m[1] * m[2];
		if (det === 0 || !Number.isFinite(det)) return null;
		return [
			m[3] / det, -m[1] / det, -m[2] / det, m[0] / det,
			(m[2] * m[5] - m[3] * m[4]) / det,
			(m[1] * m[4] - m[0] * m[5]) / det,
		];
	}
	const finiteMatrix = (m) => m.every((v) => Number.isFinite(v));

	// ---------------------------------------------------------------- paths
	// A Path2D holds its commands in USER space; they are flattened into device
	// space when the path is drawn, with the transform in force then.

	class Path2D {
		constructor(source = undefined) {
			Object.defineProperty(this, "_cmds", { value: [], writable: true });
			if (source instanceof Path2D) this._cmds = source._cmds.map((c) => c.slice());
		}
		moveTo(x, y) { this._cmds.push(["M", +x, +y]); }
		lineTo(x, y) { this._cmds.push(["L", +x, +y]); }
		closePath() { this._cmds.push(["Z"]); }
		quadraticCurveTo(cx, cy, x, y) { this._cmds.push(["Q", +cx, +cy, +x, +y]); }
		bezierCurveTo(c1x, c1y, c2x, c2y, x, y) { this._cmds.push(["C", +c1x, +c1y, +c2x, +c2y, +x, +y]); }
		rect(x, y, w, h) { this._cmds.push(["R", +x, +y, +w, +h]); }
		arc(x, y, r, start, end, ccw = false) {
			if (r < 0) throw new DOMException("arc: the radius must not be negative", "IndexSizeError");
			this._cmds.push(["A", +x, +y, +r, +r, 0, +start, +end, Boolean(ccw)]);
		}
		ellipse(x, y, rx, ry, rotation, start, end, ccw = false) {
			if (rx < 0 || ry < 0) throw new DOMException("ellipse: the radii must not be negative", "IndexSizeError");
			this._cmds.push(["A", +x, +y, +rx, +ry, +rotation, +start, +end, Boolean(ccw)]);
		}
		arcTo(x1, y1, x2, y2, r) {
			if (r < 0) throw new DOMException("arcTo: the radius must not be negative", "IndexSizeError");
			this._cmds.push(["T", +x1, +y1, +x2, +y2, +r]);
		}
		roundRect(x, y, w, h, radii = 0) {
			const r = typeof radii === "number" ? radii : (Array.isArray(radii) ? Number(radii[0]) : 0);
			this._cmds.push(["RR", +x, +y, +w, +h, Math.abs(r)]);
		}
		addPath(path) {
			if (!(path instanceof Path2D)) throw new TypeError("addPath: a Path2D is required");
			for (const c of path._cmds) this._cmds.push(c.slice());
		}
	}
	Object.defineProperty(Path2D.prototype, Symbol.toStringTag, { value: "Path2D", configurable: true });

	// CURVE_STEPS is how finely a curve is flattened. It is fixed rather than
	// adaptive because the tests sample points well inside shapes, and a fixed
	// step keeps the point count — and so the cost of a crossing — predictable.
	const CURVE_STEPS = 48;

	// flatten turns a command list into device-space polygons.
	function flatten(cmds, matrix) {
		const subpaths = [];
		let current = null;
		let cx = 0, cy = 0;     // current point, USER space
		let startX = 0, startY = 0;
		const push = (x, y) => {
			const [dx, dy] = applyMatrix(matrix, x, y);
			if (!Number.isFinite(dx) || !Number.isFinite(dy)) return;
			if (current === null) current = [];
			current.push(dx, dy);
		};
		const begin = (x, y) => {
			if (current && current.length >= 4) subpaths.push(current);
			current = null;
			cx = startX = x;
			cy = startY = y;
			push(x, y);
		};
		for (const c of cmds) {
			switch (c[0]) {
				case "M":
					begin(c[1], c[2]);
					break;
				case "L":
					if (current === null) begin(c[1], c[2]);
					else { push(c[1], c[2]); cx = c[1]; cy = c[2]; }
					break;
				case "Z":
					if (current && current.length >= 4) {
						push(startX, startY);
						subpaths.push(current);
					}
					current = null;
					cx = startX; cy = startY;
					// A closed subpath leaves the current point at its start, and the
					// next segment continues from there rather than starting a new one.
					push(startX, startY);
					break;
				case "Q": {
					if (current === null) begin(cx, cy);
					const [qx, qy, ex, ey] = [c[1], c[2], c[3], c[4]];
					for (let i = 1; i <= CURVE_STEPS; i++) {
						const t = i / CURVE_STEPS, u = 1 - t;
						push(u * u * cx + 2 * u * t * qx + t * t * ex, u * u * cy + 2 * u * t * qy + t * t * ey);
					}
					cx = ex; cy = ey;
					break;
				}
				case "C": {
					if (current === null) begin(cx, cy);
					const [c1x, c1y, c2x, c2y, ex, ey] = c.slice(1);
					for (let i = 1; i <= CURVE_STEPS; i++) {
						const t = i / CURVE_STEPS, u = 1 - t;
						push(
							u * u * u * cx + 3 * u * u * t * c1x + 3 * u * t * t * c2x + t * t * t * ex,
							u * u * u * cy + 3 * u * u * t * c1y + 3 * u * t * t * c2y + t * t * t * ey,
						);
					}
					cx = ex; cy = ey;
					break;
				}
				case "R": {
					const [x, y, w, h] = c.slice(1);
					if (current && current.length >= 4) subpaths.push(current);
					current = null;
					push(x, y); push(x + w, y); push(x + w, y + h); push(x, y + h); push(x, y);
					if (current) subpaths.push(current);
					current = null;
					cx = startX = x; cy = startY = y;
					break;
				}
				case "RR": {
					const [x, y, w, h, r0] = c.slice(1);
					const r = Math.min(Math.abs(r0), Math.abs(w) / 2, Math.abs(h) / 2);
					if (current && current.length >= 4) subpaths.push(current);
					current = null;
					const corner = (ax, ay, from) => {
						for (let i = 0; i <= 16; i++) {
							const t = from + (Math.PI / 2) * (i / 16);
							push(ax + r * Math.cos(t), ay + r * Math.sin(t));
						}
					};
					push(x + r, y);
					push(x + w - r, y);
					corner(x + w - r, y + r, -Math.PI / 2);
					push(x + w, y + h - r);
					corner(x + w - r, y + h - r, 0);
					push(x + r, y + h);
					corner(x + r, y + h - r, Math.PI / 2);
					push(x, y + r);
					corner(x + r, y + r, Math.PI);
					if (current) subpaths.push(current);
					current = null;
					cx = startX = x + r; cy = startY = y;
					break;
				}
				case "A": {
					const [ax, ay, rx, ry, rot, start, end, ccw] = c.slice(1);
					let a0 = start, a1 = end;
					const TWO_PI = Math.PI * 2;
					if (!ccw && a1 - a0 >= TWO_PI) a1 = a0 + TWO_PI;
					else if (ccw && a0 - a1 >= TWO_PI) a1 = a0 - TWO_PI;
					else if (!ccw && a1 < a0) a1 += TWO_PI * Math.ceil((a0 - a1) / TWO_PI);
					else if (ccw && a1 > a0) a1 -= TWO_PI * Math.ceil((a1 - a0) / TWO_PI);
					const steps = Math.max(8, Math.ceil(Math.abs(a1 - a0) / (Math.PI / 32)));
					const cosR = Math.cos(rot), sinR = Math.sin(rot);
					const at = (t) => {
						const px = rx * Math.cos(t), py = ry * Math.sin(t);
						return [ax + px * cosR - py * sinR, ay + px * sinR + py * cosR];
					};
					if (current === null) {
						const [sx, sy] = at(a0);
						begin(sx, sy);
					} else {
						const [sx, sy] = at(a0);
						push(sx, sy);
					}
					for (let i = 1; i <= steps; i++) {
						const [px, py] = at(a0 + (a1 - a0) * (i / steps));
						push(px, py);
					}
					const [ex, ey] = at(a1);
					cx = ex; cy = ey;
					break;
				}
				case "T": {
					// arcTo: the arc tangent to both lines, then a line to its start.
					const [x1, y1, x2, y2, r] = c.slice(1);
					if (current === null) begin(cx, cy);
					const v1 = [cx - x1, cy - y1];
					const v2 = [x2 - x1, y2 - y1];
					const l1 = Math.hypot(v1[0], v1[1]), l2 = Math.hypot(v2[0], v2[1]);
					if (l1 === 0 || l2 === 0 || r === 0) { push(x1, y1); cx = x1; cy = y1; break; }
					const u1 = [v1[0] / l1, v1[1] / l1], u2 = [v2[0] / l2, v2[1] / l2];
					const cross = u1[0] * u2[1] - u1[1] * u2[0];
					if (Math.abs(cross) < 1e-12) { push(x1, y1); cx = x1; cy = y1; break; }
					const angle = Math.acos(Math.min(1, Math.max(-1, u1[0] * u2[0] + u1[1] * u2[1])));
					const tanDist = r / Math.tan(angle / 2);
					const t1 = [x1 + u1[0] * tanDist, y1 + u1[1] * tanDist];
					const t2 = [x1 + u2[0] * tanDist, y1 + u2[1] * tanDist];
					push(t1[0], t1[1]);
					// The centre is off the bisector by r/sin(angle/2).
					const bis = [u1[0] + u2[0], u1[1] + u2[1]];
					const bl = Math.hypot(bis[0], bis[1]);
					const centre = [x1 + bis[0] / bl * (r / Math.sin(angle / 2)), y1 + bis[1] / bl * (r / Math.sin(angle / 2))];
					let s0 = Math.atan2(t1[1] - centre[1], t1[0] - centre[0]);
					let s1 = Math.atan2(t2[1] - centre[1], t2[0] - centre[0]);
					let sweep = s1 - s0;
					while (sweep > Math.PI) sweep -= Math.PI * 2;
					while (sweep < -Math.PI) sweep += Math.PI * 2;
					const steps = Math.max(4, Math.ceil(Math.abs(sweep) / (Math.PI / 32)));
					for (let i = 1; i <= steps; i++) {
						const t = s0 + sweep * (i / steps);
						push(centre[0] + r * Math.cos(t), centre[1] + r * Math.sin(t));
					}
					cx = t2[0]; cy = t2[1];
					break;
				}
			}
		}
		if (current && current.length >= 4) subpaths.push(current);
		return subpaths;
	}

	// signedArea decides a polygon's winding. Every piece of a stroke is emitted
	// with the SAME winding so the nonzero rule unions them instead of letting
	// two overlapping pieces cancel.
	function signedArea(poly) {
		let a = 0;
		for (let i = 0; i < poly.length; i += 2) {
			const j = (i + 2) % poly.length;
			a += poly[i] * poly[j + 1] - poly[j] * poly[i + 1];
		}
		return a / 2;
	}
	function ccw(poly) {
		if (signedArea(poly) < 0) {
			const out = [];
			for (let i = poly.length - 2; i >= 0; i -= 2) out.push(poly[i], poly[i + 1]);
			return out;
		}
		return poly;
	}

	// strokeOutline turns device-space polylines into the polygons that, filled
	// by the nonzero rule, ARE the stroke.
	function strokeOutline(subpaths, width, cap, join) {
		const hw = width / 2;
		const out = [];
		const circle = (x, y) => {
			const poly = [];
			for (let i = 0; i < 24; i++) {
				const t = (i / 24) * Math.PI * 2;
				poly.push(x + hw * Math.cos(t), y + hw * Math.sin(t));
			}
			return ccw(poly);
		};
		for (const sp of subpaths) {
			const pts = [];
			for (let i = 0; i < sp.length; i += 2) {
				const x = sp[i], y = sp[i + 1];
				if (pts.length >= 2 && Math.abs(pts[pts.length - 2] - x) < 1e-12
					&& Math.abs(pts[pts.length - 1] - y) < 1e-12) continue;
				pts.push(x, y);
			}
			if (pts.length < 4) {
				// A subpath with no length draws a round cap's dot and nothing else.
				if (pts.length === 2 && cap === "round") out.push(circle(pts[0], pts[1]));
				continue;
			}
			for (let i = 0; i + 3 < pts.length; i += 2) {
				const x0 = pts[i], y0 = pts[i + 1], x1 = pts[i + 2], y1 = pts[i + 3];
				const dx = x1 - x0, dy = y1 - y0;
				const len = Math.hypot(dx, dy);
				if (len === 0) continue;
				const nx = -dy / len * hw, ny = dx / len * hw;
				let ex0 = x0, ey0 = y0, ex1 = x1, ey1 = y1;
				const first = i === 0, last = i + 4 >= pts.length;
				if (cap === "square") {
					// A square cap extends the segment by half the width at each end
					// that is not joined to another segment.
					if (first) { ex0 -= dx / len * hw; ey0 -= dy / len * hw; }
					if (last) { ex1 += dx / len * hw; ey1 += dy / len * hw; }
				}
				out.push(ccw([ex0 + nx, ey0 + ny, ex1 + nx, ey1 + ny, ex1 - nx, ey1 - ny, ex0 - nx, ey0 - ny]));
				// The join at the far end of every segment but the last, and the caps
				// at both ends, are filled with a disc for "round" and with a triangle
				// for the others — which is what bevel is, and what a miter degrades
				// to beyond its limit.
				if (!last || join === "round") out.push(circle(x1, y1));
				if (i > 0 && join !== "round") {
					const px = pts[i - 2], py = pts[i - 1];
					const pdx = x0 - px, pdy = y0 - py, plen = Math.hypot(pdx, pdy);
					if (plen > 0) {
						const pnx = -pdy / plen * hw, pny = pdx / plen * hw;
						out.push(ccw([x0, y0, x0 + pnx, y0 + pny, x0 + nx, y0 + ny]));
						out.push(ccw([x0, y0, x0 - pnx, y0 - pny, x0 - nx, y0 - ny]));
					}
				}
				if (cap === "round" && (first || last)) {
					if (first) out.push(circle(x0, y0));
					if (last) out.push(circle(x1, y1));
				}
			}
		}
		return out;
	}

	// packSubpaths turns polygons into the two typed arrays the host reads: all
	// the coordinates end to end, and the point count of each subpath.
	function packSubpaths(polys) {
		let total = 0;
		for (const p of polys) total += p.length;
		const coords = new Float64Array(total);
		const lengths = new Int32Array(polys.length);
		let at = 0;
		for (let i = 0; i < polys.length; i++) {
			coords.set(polys[i], at);
			at += polys[i].length;
			lengths[i] = polys[i].length / 2;
		}
		return { subpaths: coords, lengths };
	}

	// --------------------------------------------------------- gradients

	class CanvasGradient {
		constructor(internal, radial, coords) {
			if (internal !== GRADIENT_INTERNAL) throw new TypeError("Illegal constructor");
			Object.defineProperties(this, {
				_radial: { value: radial },
				_coords: { value: coords },
				_stops: { value: [] },
			});
		}
		addColorStop(offset, color) {
			if (arguments.length < 2) throw new TypeError("addColorStop requires 2 arguments");
			const o = Number(offset);
			if (!Number.isFinite(o) || o < 0 || o > 1) {
				throw new DOMException("addColorStop: the offset must be between 0 and 1", "IndexSizeError");
			}
			const c = parseColor(color);
			if (c === null) throw new DOMException(`addColorStop: ${String(color)} is not a colour`, "SyntaxError");
			this._stops.push({ offset: o, color: c });
			this._stops.sort((a, b) => a.offset - b.offset);
		}
	}
	Object.defineProperty(CanvasGradient.prototype, Symbol.toStringTag, {
		value: "CanvasGradient", configurable: true,
	});
	const GRADIENT_INTERNAL = Symbol("CanvasGradient.internal");

	// ---------------------------------------------------------- ImageData

	class ImageData {
		constructor(a, b, c) {
			let data, width, height;
			if (a instanceof Uint8ClampedArray) {
				data = a;
				width = Number(b);
				if (!Number.isInteger(width) || width <= 0) {
					throw new DOMException("ImageData: the width must be a positive integer", "IndexSizeError");
				}
				if (data.length % 4 !== 0 || (data.length / 4) % width !== 0) {
					throw new DOMException("ImageData: the data does not fill whole rows", "InvalidStateError");
				}
				height = data.length / 4 / width;
				if (c !== undefined && Number(c) !== height) {
					throw new DOMException("ImageData: the height does not match the data", "IndexSizeError");
				}
			} else {
				width = Number(a);
				height = Number(b);
				if (!Number.isInteger(width) || !Number.isInteger(height) || width <= 0 || height <= 0) {
					throw new DOMException("ImageData: the dimensions must be positive integers", "IndexSizeError");
				}
				data = new Uint8ClampedArray(width * height * 4);
			}
			Object.defineProperties(this, {
				_data: { value: data },
				_width: { value: width },
				_height: { value: height },
			});
		}
		get data() { return this._data; }
		get width() { return this._width; }
		get height() { return this._height; }
		get colorSpace() { return "srgb"; }
	}
	Object.defineProperty(ImageData.prototype, Symbol.toStringTag, {
		value: "ImageData", configurable: true,
	});

	// ------------------------------------------------------- the context

	const DEFAULT_STATE = () => ({
		transform: IDENTITY.slice(),
		fill: [0, 0, 0, 1], fillGradient: null,
		stroke: [0, 0, 0, 1], strokeGradient: null,
		globalAlpha: 1,
		composite: "source-over",
		lineWidth: 1, lineCap: "butt", lineJoin: "miter", miterLimit: 10,
		lineDash: [], lineDashOffset: 0,
		clip: null, // device-space polygons, or null for none
		font: "10px sans-serif", textAlign: "start", textBaseline: "alphabetic",
		shadowColor: [0, 0, 0, 0], shadowBlur: 0, shadowOffsetX: 0, shadowOffsetY: 0,
		imageSmoothingEnabled: true, imageSmoothingQuality: "low",
		filter: "none", direction: "inherit", letterSpacing: "0px", wordSpacing: "0px",
		fontKerning: "auto", fontStretch: "normal", fontVariantCaps: "normal",
		textRendering: "auto",
	});

	class OffscreenCanvasRenderingContext2D {
		constructor(internal, canvas) {
			if (internal !== CONTEXT_INTERNAL) throw new TypeError("Illegal constructor");
			Object.defineProperties(this, {
				_canvas: { value: canvas },
				_state: { value: DEFAULT_STATE(), writable: true },
				_stack: { value: [] },
				_path: { value: new Path2D(), writable: true },
			});
		}

		get canvas() { return this._canvas; }

		// ----------------------------------------------------------- state
		save() {
			this._stack.push(JSON.parse(JSON.stringify({ ...this._state, clip: null })));
			// The clip is a device-space mask and is not JSON; it is carried by
			// reference, which is safe because a clip is never mutated in place.
			this._stack[this._stack.length - 1].clip = this._state.clip;
			this._stack[this._stack.length - 1].fillGradient = this._state.fillGradient;
			this._stack[this._stack.length - 1].strokeGradient = this._state.strokeGradient;
		}
		restore() {
			const s = this._stack.pop();
			if (!s) return;
			const hadClip = this._state.clip;
			this._state = s;
			if (hadClip !== s.clip) this._applyClip();
		}
		reset() {
			this._state = DEFAULT_STATE();
			this._stack.length = 0;
			this._path = new Path2D();
			canvasResize(this._canvas._handle, this._canvas.width, this._canvas.height);
		}
		isContextLost() { return false; }

		// ------------------------------------------------------- transforms
		scale(x, y) { this._mul([+x, 0, 0, +y, 0, 0]); }
		rotate(angle) {
			const c = Math.cos(+angle), s = Math.sin(+angle);
			this._mul([c, s, -s, c, 0, 0]);
		}
		translate(x, y) { this._mul([1, 0, 0, 1, +x, +y]); }
		transform(a, b, c, d, e, f) { this._mul([+a, +b, +c, +d, +e, +f]); }
		setTransform(a, b, c, d, e, f) {
			if (a === undefined) { this._state.transform = IDENTITY.slice(); return; }
			if (typeof a === "object") {
				const m = a;
				const next = [Number(m.a), Number(m.b), Number(m.c), Number(m.d), Number(m.e), Number(m.f)];
				if (finiteMatrix(next)) this._state.transform = next;
				return;
			}
			const next = [+a, +b, +c, +d, +e, +f];
			if (finiteMatrix(next)) this._state.transform = next;
		}
		resetTransform() { this._state.transform = IDENTITY.slice(); }
		getTransform() {
			const m = this._state.transform;
			return new DOMMatrixLike(m);
		}
		_mul(m) {
			if (!finiteMatrix(m)) return;
			this._state.transform = multiply(this._state.transform, m);
		}

		// ---------------------------------------------------------- styles
		get fillStyle() {
			return this._state.fillGradient ?? serializeColor(this._state.fill);
		}
		set fillStyle(v) {
			if (v instanceof CanvasGradient) { this._state.fillGradient = v; return; }
			const c = parseColor(v);
			if (c === null) return;
			this._state.fill = c;
			this._state.fillGradient = null;
		}
		get strokeStyle() {
			return this._state.strokeGradient ?? serializeColor(this._state.stroke);
		}
		set strokeStyle(v) {
			if (v instanceof CanvasGradient) { this._state.strokeGradient = v; return; }
			const c = parseColor(v);
			if (c === null) return;
			this._state.stroke = c;
			this._state.strokeGradient = null;
		}
		get globalAlpha() { return this._state.globalAlpha; }
		set globalAlpha(v) {
			const n = Number(v);
			// Out of range is IGNORED, not clamped: the value was not an alpha.
			if (Number.isFinite(n) && n >= 0 && n <= 1) this._state.globalAlpha = n;
		}
		get globalCompositeOperation() { return this._state.composite; }
		set globalCompositeOperation(v) {
			const name = String(v);
			if (COMPOSITE_NAMES.has(name)) this._state.composite = name;
		}
		get lineWidth() { return this._state.lineWidth; }
		set lineWidth(v) {
			const n = Number(v);
			if (Number.isFinite(n) && n > 0) this._state.lineWidth = n;
		}
		get lineCap() { return this._state.lineCap; }
		set lineCap(v) { if (["butt", "round", "square"].includes(String(v))) this._state.lineCap = String(v); }
		get lineJoin() { return this._state.lineJoin; }
		set lineJoin(v) { if (["round", "bevel", "miter"].includes(String(v))) this._state.lineJoin = String(v); }
		get miterLimit() { return this._state.miterLimit; }
		set miterLimit(v) {
			const n = Number(v);
			if (Number.isFinite(n) && n > 0) this._state.miterLimit = n;
		}
		get lineDashOffset() { return this._state.lineDashOffset; }
		set lineDashOffset(v) { const n = Number(v); if (Number.isFinite(n)) this._state.lineDashOffset = n; }
		setLineDash(segments) {
			const list = Array.from(segments ?? [], Number);
			if (list.some((n) => !Number.isFinite(n) || n < 0)) return;
			this._state.lineDash = list.length % 2 === 1 ? list.concat(list) : list;
		}
		getLineDash() { return this._state.lineDash.slice(); }
		get shadowColor() { return serializeColor(this._state.shadowColor); }
		set shadowColor(v) { const c = parseColor(v); if (c !== null) this._state.shadowColor = c; }
		get shadowBlur() { return this._state.shadowBlur; }
		set shadowBlur(v) { const n = Number(v); if (Number.isFinite(n) && n >= 0) this._state.shadowBlur = n; }
		get shadowOffsetX() { return this._state.shadowOffsetX; }
		set shadowOffsetX(v) { const n = Number(v); if (Number.isFinite(n)) this._state.shadowOffsetX = n; }
		get shadowOffsetY() { return this._state.shadowOffsetY; }
		set shadowOffsetY(v) { const n = Number(v); if (Number.isFinite(n)) this._state.shadowOffsetY = n; }
		get imageSmoothingEnabled() { return this._state.imageSmoothingEnabled; }
		set imageSmoothingEnabled(v) { this._state.imageSmoothingEnabled = Boolean(v); }
		get imageSmoothingQuality() { return this._state.imageSmoothingQuality; }
		set imageSmoothingQuality(v) {
			if (["low", "medium", "high"].includes(String(v))) this._state.imageSmoothingQuality = String(v);
		}
		get filter() { return this._state.filter; }
		set filter(v) { this._state.filter = String(v); }
		get font() { return this._state.font; }
		set font(v) { this._state.font = String(v); }
		get textAlign() { return this._state.textAlign; }
		set textAlign(v) {
			if (["start", "end", "left", "right", "center"].includes(String(v))) this._state.textAlign = String(v);
		}
		get textBaseline() { return this._state.textBaseline; }
		set textBaseline(v) {
			if (["top", "hanging", "middle", "alphabetic", "ideographic", "bottom"].includes(String(v))) {
				this._state.textBaseline = String(v);
			}
		}
		get direction() { return this._state.direction; }
		set direction(v) { if (["ltr", "rtl", "inherit"].includes(String(v))) this._state.direction = String(v); }
		get letterSpacing() { return this._state.letterSpacing; }
		set letterSpacing(v) { this._state.letterSpacing = String(v); }
		get wordSpacing() { return this._state.wordSpacing; }
		set wordSpacing(v) { this._state.wordSpacing = String(v); }
		get fontKerning() { return this._state.fontKerning; }
		set fontKerning(v) { if (["auto", "normal", "none"].includes(String(v))) this._state.fontKerning = String(v); }
		get fontStretch() { return this._state.fontStretch; }
		set fontStretch(v) { this._state.fontStretch = String(v); }
		get fontVariantCaps() { return this._state.fontVariantCaps; }
		set fontVariantCaps(v) { this._state.fontVariantCaps = String(v); }
		get textRendering() { return this._state.textRendering; }
		set textRendering(v) { this._state.textRendering = String(v); }

		// --------------------------------------------------------- drawing
		createLinearGradient(x0, y0, x1, y1) {
			if (arguments.length < 4) throw new TypeError("createLinearGradient requires 4 arguments");
			const coords = [+x0, +y0, 0, +x1, +y1, 0];
			if (!coords.every(Number.isFinite)) {
				throw new TypeError("createLinearGradient: the coordinates must be finite");
			}
			return new CanvasGradient(GRADIENT_INTERNAL, false, coords);
		}
		createRadialGradient(x0, y0, r0, x1, y1, r1) {
			if (arguments.length < 6) throw new TypeError("createRadialGradient requires 6 arguments");
			const coords = [+x0, +y0, +r0, +x1, +y1, +r1];
			if (!coords.every(Number.isFinite)) {
				throw new TypeError("createRadialGradient: the coordinates must be finite");
			}
			if (coords[2] < 0 || coords[5] < 0) {
				throw new DOMException("createRadialGradient: a radius must not be negative", "IndexSizeError");
			}
			return new CanvasGradient(GRADIENT_INTERNAL, true, coords);
		}

		beginPath() { this._path = new Path2D(); }
		moveTo(x, y) { this._path.moveTo(x, y); }
		lineTo(x, y) { this._path.lineTo(x, y); }
		closePath() { this._path.closePath(); }
		quadraticCurveTo(a, b, c, d) { this._path.quadraticCurveTo(a, b, c, d); }
		bezierCurveTo(a, b, c, d, e, f) { this._path.bezierCurveTo(a, b, c, d, e, f); }
		rect(x, y, w, h) { this._path.rect(x, y, w, h); }
		roundRect(x, y, w, h, r) { this._path.roundRect(x, y, w, h, r); }
		arc(x, y, r, s, e, ccwFlag) { this._path.arc(x, y, r, s, e, ccwFlag); }
		ellipse(x, y, rx, ry, rot, s, e, ccwFlag) { this._path.ellipse(x, y, rx, ry, rot, s, e, ccwFlag); }
		arcTo(x1, y1, x2, y2, r) { this._path.arcTo(x1, y1, x2, y2, r); }

		fillRect(x, y, w, h) {
			if (!allFinite(arguments)) return;
			if (+w === 0 || +h === 0) return;
			this._paint(this._rectPolys(+x, +y, +w, +h), false);
		}
		strokeRect(x, y, w, h) {
			if (!allFinite(arguments)) return;
			const polys = (+w === 0 && +h === 0) ? [] : this._rectPolys(+x, +y, +w, +h);
			this._paintStroke(polys.length ? polys : []);
		}
		clearRect(x, y, w, h) {
			if (!allFinite(arguments)) return;
			if (+w === 0 || +h === 0) return;
			const { subpaths, lengths } = packSubpaths(this._rectPolys(+x, +y, +w, +h));
			canvasClear(this._canvas._handle, { subpaths, lengths });
		}
		fill(a, b) {
			const { path, rule } = fillArgs(a, b);
			this._paint(flatten((path ?? this._path)._cmds, this._state.transform), rule === "evenodd");
		}
		stroke(a) {
			const path = a instanceof Path2D ? a : this._path;
			this._paintStroke(flatten(path._cmds, this._state.transform));
		}
		clip(a, b) {
			const { path, rule } = fillArgs(a, b);
			const polys = flatten((path ?? this._path)._cmds, this._state.transform);
			const { subpaths, lengths } = packSubpaths(polys);
			this._state.clip = { subpaths, lengths, evenOdd: rule === "evenodd", prev: this._state.clip };
			canvasClip(this._canvas._handle, { subpaths, lengths, evenOdd: rule === "evenodd", replace: false });
		}
		isPointInPath(a, b, c, d) {
			// Answered from the flattened path with a crossing count, which is the
			// same question the rasterizer answers per pixel.
			let path = this._path, x = a, y = b, rule = c;
			if (a instanceof Path2D) { path = a; x = b; y = c; rule = d; }
			return pointInPolys(flatten(path._cmds, this._state.transform), +x, +y, rule === "evenodd");
		}
		isPointInStroke(a, b, c) {
			let path = this._path, x = a, y = b;
			if (a instanceof Path2D) { path = a; x = b; y = c; }
			const polys = strokeOutline(flatten(path._cmds, this._state.transform),
				this._deviceLineWidth(), this._state.lineCap, this._state.lineJoin);
			return pointInPolys(polys, +x, +y, false);
		}

		// --------------------------------------------------------- pixels
		createImageData(a, b) {
			if (a instanceof ImageData) return new ImageData(a.width, a.height);
			return new ImageData(Number(a), Number(b));
		}
		getImageData(x, y, w, h) {
			if (arguments.length < 4) throw new TypeError("getImageData requires 4 arguments");
			const sw = Math.trunc(Number(w)), sh = Math.trunc(Number(h));
			if (sw === 0 || sh === 0) {
				throw new DOMException("getImageData: the size must not be zero", "IndexSizeError");
			}
			const ax = sw < 0 ? Math.trunc(Number(x)) + sw : Math.trunc(Number(x));
			const ay = sh < 0 ? Math.trunc(Number(y)) + sh : Math.trunc(Number(y));
			const bytes = canvasGetImageData(this._canvas._handle, ax, ay, Math.abs(sw), Math.abs(sh));
			return new ImageData(new Uint8ClampedArray(bytes), Math.abs(sw), Math.abs(sh));
		}
		putImageData(data, dx, dy, sx = 0, sy = 0, sw = undefined, sh = undefined) {
			if (!(data instanceof ImageData)) throw new TypeError("putImageData: an ImageData is required");
			const useW = sw === undefined ? data.width : Math.trunc(Number(sw));
			const useH = sh === undefined ? data.height : Math.trunc(Number(sh));
			canvasPutImageData(this._canvas._handle, new Uint8Array(data.data.buffer, data.data.byteOffset, data.data.byteLength),
				data.width, data.height, Math.trunc(Number(dx)), Math.trunc(Number(dy)),
				Math.trunc(Number(sx)), Math.trunc(Number(sy)), useW, useH);
		}

		// -------------------------------------------------------- internals
		_rectPolys(x, y, w, h) {
			const m = this._state.transform;
			const corners = [[x, y], [x + w, y], [x + w, y + h], [x, y + h], [x, y]];
			const poly = [];
			for (const [px, py] of corners) {
				const [dx, dy] = applyMatrix(m, px, py);
				if (!Number.isFinite(dx) || !Number.isFinite(dy)) return [];
				poly.push(dx, dy);
			}
			return [poly];
		}
		_deviceLineWidth() {
			// The stroke width is in USER space; the device width is it scaled by
			// the transform. A non-uniform scale has no single answer, so the
			// geometric mean of the two axes is used, which is what a uniform scale
			// reduces to.
			const m = this._state.transform;
			const sx = Math.hypot(m[0], m[1]), sy = Math.hypot(m[2], m[3]);
			return this._state.lineWidth * Math.sqrt(Math.abs(sx * sy) || 1);
		}
		_paint(polys, evenOdd) {
			if (!polys.length) return;
			const { subpaths, lengths } = packSubpaths(polys);
			canvasFill(this._canvas._handle, {
				subpaths, lengths, evenOdd,
				...this._paintSpec(this._state.fill, this._state.fillGradient),
			});
		}
		_paintStroke(polys) {
			if (!polys.length) return;
			const outline = strokeOutline(polys, this._deviceLineWidth(),
				this._state.lineCap, this._state.lineJoin);
			if (!outline.length) return;
			const { subpaths, lengths } = packSubpaths(outline);
			canvasFill(this._canvas._handle, {
				subpaths, lengths, evenOdd: false,
				...this._paintSpec(this._state.stroke, this._state.strokeGradient),
			});
		}
		_paintSpec(color, grad) {
			const spec = {
				alpha: this._state.globalAlpha,
				composite: this._state.composite,
			};
			if (grad) {
				const inverse = invertMatrix(this._state.transform);
				spec.gradient = {
					radial: grad._radial,
					x0: grad._coords[0], y0: grad._coords[1], r0: grad._coords[2],
					x1: grad._coords[3], y1: grad._coords[4], r1: grad._coords[5],
					degenerate: grad._stops.length === 0,
					inverse: new Float64Array(inverse ?? IDENTITY),
					stops: new Float64Array(grad._stops.flatMap((s) => [
						s.offset, s.color[0] / 255, s.color[1] / 255, s.color[2] / 255, s.color[3],
					])),
				};
			} else {
				spec.color = new Float64Array([color[0] / 255, color[1] / 255, color[2] / 255, color[3]]);
			}
			return spec;
		}
		_applyClip() {
			const clip = this._state.clip;
			if (!clip) { canvasClip(this._canvas._handle, null); return; }
			canvasClip(this._canvas._handle, {
				subpaths: clip.subpaths, lengths: clip.lengths, evenOdd: clip.evenOdd, replace: true,
			});
		}
	}
	Object.defineProperty(OffscreenCanvasRenderingContext2D.prototype, Symbol.toStringTag, {
		value: "OffscreenCanvasRenderingContext2D", configurable: true,
	});
	const CONTEXT_INTERNAL = Symbol("context.internal");

	const COMPOSITE_NAMES = new Set([
		"source-over", "source-in", "source-out", "source-atop",
		"destination-over", "destination-in", "destination-out", "destination-atop",
		"lighter", "copy", "xor",
	]);

	function allFinite(args) {
		for (const a of args) if (!Number.isFinite(Number(a))) return false;
		return true;
	}

	function fillArgs(a, b) {
		if (a instanceof Path2D) return { path: a, rule: b === undefined ? "nonzero" : String(b) };
		return { path: null, rule: a === undefined ? "nonzero" : String(a) };
	}

	// pointInPolys is the crossing count isPointInPath answers with.
	function pointInPolys(polys, x, y, evenOdd) {
		let winding = 0, crossings = 0;
		for (const poly of polys) {
			for (let i = 0; i + 1 < poly.length / 2; i++) {
				const x0 = poly[i * 2], y0 = poly[i * 2 + 1];
				const x1 = poly[i * 2 + 2], y1 = poly[i * 2 + 3];
				if ((y0 <= y) === (y1 <= y)) continue;
				const t = (y - y0) / (y1 - y0);
				if (x0 + t * (x1 - x0) <= x) continue;
				crossings++;
				winding += y1 > y0 ? 1 : -1;
			}
		}
		return evenOdd ? crossings % 2 === 1 : winding !== 0;
	}

	// DOMMatrixLike is what getTransform answers with. The full DOMMatrix is a
	// geometry interface of its own; what a caller does with this one is read its
	// six components back, and that is what it offers.
	class DOMMatrixLike {
		constructor(m) {
			const [a, b, c, d, e, f] = m;
			Object.assign(this, {
				a, b, c, d, e, f,
				m11: a, m12: b, m21: c, m22: d, m41: e, m42: f,
				m13: 0, m14: 0, m23: 0, m24: 0, m31: 0, m32: 0, m33: 1, m34: 0, m43: 0, m44: 1,
				is2D: true, isIdentity: a === 1 && b === 0 && c === 0 && d === 1 && e === 0 && f === 0,
			});
		}
	}

	// --------------------------------------------------------- the canvas

	class OffscreenCanvas {
		constructor(width, height) {
			if (arguments.length < 2) throw new TypeError("OffscreenCanvas requires 2 arguments");
			const w = toUnsignedLong(width), h = toUnsignedLong(height);
			Object.defineProperties(this, {
				_width: { value: w, writable: true },
				_height: { value: h, writable: true },
				_handle: { value: canvasNew(w, h), writable: true },
				_context: { value: null, writable: true },
			});
		}
		get width() { return this._width; }
		set width(v) {
			const w = toUnsignedLong(v);
			this._width = w;
			canvasResize(this._handle, w, this._height);
			// Resizing RESETS the context, which is what assigning to width means.
			if (this._context) this._context._state = DEFAULT_STATE();
		}
		get height() { return this._height; }
		set height(v) {
			const h = toUnsignedLong(v);
			this._height = h;
			canvasResize(this._handle, this._width, h);
			if (this._context) this._context._state = DEFAULT_STATE();
		}
		getContext(type, options = undefined) {
			if (arguments.length < 1) throw new TypeError("getContext requires 1 argument");
			if (String(type) !== "2d") return null;
			if (!this._context) {
				this._context = new OffscreenCanvasRenderingContext2D(CONTEXT_INTERNAL, this);
			}
			return this._context;
		}
	}
	Object.defineProperty(OffscreenCanvas.prototype, Symbol.toStringTag, {
		value: "OffscreenCanvas", configurable: true,
	});

	function toUnsignedLong(v) {
		const n = Number(v);
		if (!Number.isFinite(n) || n < 0) return 0;
		return Math.floor(n) % 4294967296;
	}

	globalThis.OffscreenCanvas = OffscreenCanvas;
	globalThis.OffscreenCanvasRenderingContext2D = OffscreenCanvasRenderingContext2D;
	globalThis.CanvasGradient = CanvasGradient;
	globalThis.Path2D = Path2D;
	globalThis.ImageData = ImageData;
})();
