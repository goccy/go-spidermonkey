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
	const canvasFromBytes = ops.canvas_from_bytes;
	const canvasDecodeImage = ops.canvas_decode_image;
	const canvasDrawImage = ops.canvas_draw_image;
	const canvasLayerBegin = ops.canvas_layer_begin;
	const canvasLayerEnd = ops.canvas_layer_end;
	const canvasEncode = ops.canvas_encode;

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

	// CSS system colours (css-color-4 §6.1), resolved for a light theme. The
	// deprecated names alias the modern ones the specification maps them to.
	{
		const sys = {
			accentcolor: [0, 117, 255, 1], accentcolortext: [255, 255, 255, 1],
			activetext: [255, 0, 0, 1], buttonborder: [118, 118, 118, 1],
			buttonface: [239, 239, 239, 1], buttontext: [0, 0, 0, 1],
			canvas: [255, 255, 255, 1], canvastext: [0, 0, 0, 1],
			field: [255, 255, 255, 1], fieldtext: [0, 0, 0, 1],
			graytext: [128, 128, 128, 1], highlight: [181, 213, 255, 1],
			highlighttext: [0, 0, 0, 1], linktext: [0, 0, 238, 1],
			mark: [255, 255, 0, 1], marktext: [0, 0, 0, 1],
			selecteditem: [0, 117, 255, 1], selecteditemtext: [255, 255, 255, 1],
			visitedtext: [85, 26, 139, 1],
		};
		const aliases = {
			activeborder: "buttonborder", activecaption: "canvas", appworkspace: "canvas",
			background: "canvas", buttonhighlight: "buttonface", buttonshadow: "buttonborder",
			captiontext: "canvastext", inactiveborder: "buttonborder", inactivecaption: "canvas",
			inactivecaptiontext: "graytext", infobackground: "canvas", infotext: "canvastext",
			menu: "canvas", menutext: "canvastext", scrollbar: "canvas",
			threeddarkshadow: "buttonborder", threedface: "buttonface",
			threedhighlight: "buttonborder", threedlightshadow: "buttonborder",
			threedshadow: "buttonborder", window: "canvas", windowframe: "buttonborder",
			windowtext: "canvastext",
		};
		Object.assign(NAMED_COLORS, sys);
		for (const [from, to] of Object.entries(aliases)) NAMED_COLORS[from] = sys[to];
	}

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
		// An unclosed function is legal — the CSS tokenizer closes every open
		// block at the end of input — but nothing may follow the ')'.
		const fn = /^(rgba?|hsla?)\(\s*([^()]*?)\s*\)?$/.exec(s);
		if (!fn) return null;
		return parseColorFunction(fn[1], fn[2]);
	}

	const ANGLE_UNITS = { deg: 1, grad: 0.9, rad: 180 / Math.PI, turn: 360 };

	// tokenizeColorArgs splits a colour function's arguments into value,
	// comma and slash tokens. A CSS number never ends with a bare dot, which
	// is what makes "100." (and so "1. 0") a parse error rather than 100.
	function tokenizeColorArgs(body) {
		const re = /\s*(?:([+-]?(?:\d+(?:\.\d+)?|\.\d+)(?:e[+-]?\d+)?)(%|[a-z]+)?|(,)|(\/)|none)\s*/y;
		const tokens = [];
		while (re.lastIndex < body.length) {
			const at = re.lastIndex;
			const m = re.exec(body);
			if (!m || m.index !== at || m[0].length === 0) return null;
			if (m[1] !== undefined) tokens.push({ v: parseFloat(m[1]), unit: m[2] ?? "" });
			else if (m[3]) tokens.push(",");
			else if (m[4]) tokens.push("/");
			else tokens.push({ none: true });
		}
		return tokens;
	}

	// parseColorFunction is the rgb()/hsl() grammar of CSS Color 4. The two
	// syntaxes never mix: a comma anywhere selects the LEGACY form (values
	// separated by commas, an optional fourth value as the alpha, and — for
	// rgb — all three channels the same kind, number or percentage, with the
	// hsl saturation and lightness percentages only); no comma selects the
	// MODERN form (space-separated, "/" before the alpha, "none" allowed,
	// kinds free to mix).
	function parseColorFunction(name, body) {
		const tokens = tokenizeColorArgs(body);
		if (!tokens || tokens.length === 0) return null;
		const legacy = tokens.includes(",");
		let vals, alphaTok;
		if (legacy) {
			const seq = [];
			let expectValue = true;
			for (const t of tokens) {
				if (expectValue) {
					if (t === "," || t === "/" || t.none) return null;
					seq.push(t);
					expectValue = false;
				} else {
					if (t !== ",") return null;
					expectValue = true;
				}
			}
			if (expectValue) return null; // a trailing comma names a missing value
			if (seq.length !== 3 && seq.length !== 4) return null;
			vals = seq.slice(0, 3);
			alphaTok = seq[3];
		} else {
			const slash = tokens.indexOf("/");
			const head = slash === -1 ? tokens : tokens.slice(0, slash);
			if (head.length !== 3) return null;
			vals = head;
			if (slash !== -1) {
				const tail = tokens.slice(slash + 1);
				if (tail.length !== 1 || tail[0] === "/" ) return null;
				alphaTok = tail[0];
			}
		}
		let alpha = 1;
		if (alphaTok !== undefined) {
			if (alphaTok.none) alpha = 0;
			else if (alphaTok.unit === "%") alpha = alphaTok.v / 100;
			else if (alphaTok.unit === "") alpha = alphaTok.v;
			else return null;
			alpha = Math.min(1, Math.max(0, alpha));
		}
		if (name === "rgb" || name === "rgba") {
			if (legacy) {
				const units = vals.map((t) => t.unit);
				if (!(units.every((u) => u === "") || units.every((u) => u === "%"))) return null;
			}
			const rgb = [];
			for (const t of vals) {
				if (t.none) rgb.push(0);
				else if (t.unit === "") rgb.push(t.v);
				else if (t.unit === "%") rgb.push(t.v / 100 * 255);
				else return null;
			}
			return [...rgb.map((v) => Math.round(Math.min(255, Math.max(0, v)))), alpha];
		}
		const [h, sTok, lTok] = vals;
		let hue;
		if (h.none) hue = 0;
		else if (h.unit === "") hue = h.v;
		else if (ANGLE_UNITS[h.unit] !== undefined) hue = h.v * ANGLE_UNITS[h.unit];
		else return null;
		const component = (t) => {
			if (t.none) return 0;
			if (t.unit === "%") return t.v;
			if (t.unit === "" && !legacy) return t.v;
			return null;
		};
		const sat = component(sTok), light = component(lTok);
		if (sat === null || light === null || !Number.isFinite(hue)) return null;
		const rgb = hslToRgb(((hue % 360) + 360) % 360,
			Math.min(1, Math.max(0, sat / 100)), Math.min(1, Math.max(0, light / 100)));
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

	// fixup2D is the DOMMatrix2DInit validate-and-fixup algorithm: each 2D
	// component may be spelled by its legacy alias (a..f) or its matrix name
	// (m11..m42), and naming BOTH with different values is a TypeError.
	function fixup2D(init) {
		const fields = [["a", "m11", 1], ["b", "m12", 0], ["c", "m21", 0],
			["d", "m22", 1], ["e", "m41", 0], ["f", "m42", 0]];
		const out = [];
		for (const [alias, canon, def] of fields) {
			const va = init[alias] === undefined ? undefined : Number(init[alias]);
			const vc = init[canon] === undefined ? undefined : Number(init[canon]);
			if (va !== undefined && vc !== undefined && va !== vc
				&& !(Number.isNaN(va) && Number.isNaN(vc))) {
				throw new TypeError(`the ${alias} and ${canon} members must agree`);
			}
			out.push(vc !== undefined ? vc : va !== undefined ? va : def);
		}
		return out;
	}

	// ---------------------------------------------------------------- paths
	// A Path2D holds its commands in USER space; they are flattened into device
	// space when the path is drawn, with the transform in force then. The
	// context's default path is the other way round: the specification
	// transforms points AS THEY ARE ADDED, so the context bakes the matrix in
	// force into each point and its command list is device-space already
	// (bézier control points transform exactly — an affine map of a bézier is
	// the bézier of the mapped control points — which is why arcs are baked as
	// béziers rather than sampled).

	const TAU = Math.PI * 2;
	// Transformed coordinates are clamped rather than dropped: a rectangle
	// scaled by 1e308 must still CONTAIN the origin, and a clamped huge value
	// preserves which side of every edge a queried point is on.
	const COORD_LIMIT = 1e154;
	const clampCoord = (v) => (v > COORD_LIMIT ? COORD_LIMIT : v < -COORD_LIMIT ? -COORD_LIMIT : v);

	// arcSweep resolves start/end/direction into a signed sweep. A difference
	// of a whole number of turns is a FULL circle when the endpoints differ
	// and an empty arc when they are the same value: arc(…, 2π, 0) draws the
	// circle, arc(…, 0, 0) draws nothing.
	function arcSweep(a0, a1, ccwFlag) {
		if (!ccwFlag && a1 - a0 >= TAU) return TAU;
		if (ccwFlag && a0 - a1 >= TAU) return -TAU;
		if (a0 === a1) return 0;
		let d = (a1 - a0) % TAU;
		if (!ccwFlag && d <= 0) d += TAU;
		else if (ccwFlag && d >= 0) d -= TAU;
		return d;
	}

	// arcBeziers approximates an elliptical arc with one cubic per quarter
	// turn, the standard tangent-length construction. The worst radial error
	// of a quarter is 2.7e-4 of the radius, which survives any later affine
	// transform because the béziers do.
	function arcBeziers(x, y, rx, ry, rot, a0, sweep) {
		const cosR = Math.cos(rot), sinR = Math.sin(rot);
		const pt = (t) => {
			const px = rx * Math.cos(t), py = ry * Math.sin(t);
			return [x + px * cosR - py * sinR, y + px * sinR + py * cosR];
		};
		const deriv = (t) => {
			const dx = -rx * Math.sin(t), dy = ry * Math.cos(t);
			return [dx * cosR - dy * sinR, dx * sinR + dy * cosR];
		};
		const [sx, sy] = pt(a0);
		const curves = [];
		const n = Math.max(1, Math.ceil(Math.abs(sweep) / (Math.PI / 2)));
		for (let i = 0; i < n; i++) {
			const t0 = a0 + sweep * (i / n), t1 = a0 + sweep * ((i + 1) / n);
			const k = (4 / 3) * Math.tan((t1 - t0) / 4);
			const [p0x, p0y] = pt(t0), [p3x, p3y] = pt(t1);
			const [d0x, d0y] = deriv(t0), [d1x, d1y] = deriv(t1);
			curves.push([p0x + k * d0x, p0y + k * d0y, p3x - k * d1x, p3y - k * d1y, p3x, p3y]);
		}
		return { sx, sy, curves };
	}

	// arcToCommands is the arcTo geometry: from the current point (x0, y0),
	// the arc of radius r tangent to both lines through (x1, y1), reached by a
	// straight line. Degenerate inputs collapse to a line to (x1, y1), as the
	// specification says.
	function arcToCommands(x0, y0, x1, y1, x2, y2, r) {
		const v1x = x0 - x1, v1y = y0 - y1, v2x = x2 - x1, v2y = y2 - y1;
		const l1 = Math.hypot(v1x, v1y), l2 = Math.hypot(v2x, v2y);
		if (l1 === 0 || l2 === 0 || r === 0) return [["L", x1, y1]];
		const u1x = v1x / l1, u1y = v1y / l1, u2x = v2x / l2, u2y = v2y / l2;
		if (Math.abs(u1x * u2y - u1y * u2x) < 1e-12) return [["L", x1, y1]];
		const angle = Math.acos(Math.min(1, Math.max(-1, u1x * u2x + u1y * u2y)));
		const tanDist = r / Math.tan(angle / 2);
		const t1x = x1 + u1x * tanDist, t1y = y1 + u1y * tanDist;
		const t2x = x1 + u2x * tanDist, t2y = y1 + u2y * tanDist;
		// The centre is off the angle bisector by r/sin(angle/2).
		const bx = u1x + u2x, by = u1y + u2y;
		const bl = Math.hypot(bx, by);
		const cx = x1 + (bx / bl) * (r / Math.sin(angle / 2));
		const cy = y1 + (by / bl) * (r / Math.sin(angle / 2));
		const s0 = Math.atan2(t1y - cy, t1x - cx);
		const s1 = Math.atan2(t2y - cy, t2x - cx);
		let sweep = s1 - s0;
		while (sweep > Math.PI) sweep -= TAU;
		while (sweep < -Math.PI) sweep += TAU;
		const arc = arcBeziers(cx, cy, r, r, 0, s0, sweep);
		return [["L", t1x, t1y], ...arc.curves.map((b) => ["C", ...b])];
	}

	// rectCommandList: four connected lines, the subpath marked closed, and a
	// NEW subpath at (x, y) — which is where the specification leaves the
	// current point, so a following lineTo draws from the rectangle's corner.
	function rectCommandList(x, y, w, h) {
		return [
			["M", x, y], ["L", x + w, y], ["L", x + w, y + h], ["L", x, y + h], ["Z"],
			["M", x, y],
		];
	}

	// KAPPA is the tangent length that makes one cubic a quarter circle.
	const KAPPA = 0.5522847498307936;

	// normRoundRect converts and validates roundRect's arguments: each radius
	// is a number or a DOMPointInit (an elliptical corner), one to four of
	// them fill in like a CSS shorthand, a negative component is a RangeError,
	// a non-finite one silently ignores the whole call, negative width or
	// height flips the corner assignment, and radii too large for an edge are
	// scaled down together. Returns null when the call is to be ignored.
	function normRoundRect(x, y, w, h, radii) {
		x = +x; y = +y; w = +w; h = +h;
		if (radii === undefined) radii = 0;
		const list = (radii !== null && typeof radii === "object" && Symbol.iterator in radii)
			? [...radii] : [radii];
		if (list.length < 1 || list.length > 4) {
			throw new RangeError("roundRect: between one and four radii are required");
		}
		let ignore = false;
		const norm = list.map((v) => {
			let rx, ry;
			if (v !== null && v !== undefined && typeof v === "object") {
				rx = +(v.x ?? 0); ry = +(v.y ?? 0);
			} else if (v === null || v === undefined) {
				rx = 0; ry = 0;
			} else {
				rx = ry = +v;
			}
			if (!Number.isFinite(rx) || !Number.isFinite(ry)) { ignore = true; return { x: 0, y: 0 }; }
			if (rx < 0 || ry < 0) throw new RangeError("roundRect: a radius must not be negative");
			return { x: rx, y: ry };
		});
		if (ignore || ![x, y, w, h].every(Number.isFinite)) return null;
		const c = (r) => ({ x: r.x, y: r.y });
		let [ul, ur, lr, ll] =
			norm.length === 1 ? [c(norm[0]), c(norm[0]), c(norm[0]), c(norm[0])]
				: norm.length === 2 ? [c(norm[0]), c(norm[1]), c(norm[0]), c(norm[1])]
					: norm.length === 3 ? [c(norm[0]), c(norm[1]), c(norm[2]), c(norm[1])]
						: [c(norm[0]), c(norm[1]), c(norm[2]), c(norm[3])];
		// Exactly one negative dimension reverses the traversal direction, so
		// a flipped roundRect cancels an unflipped one under the nonzero rule
		// — the same winding rect() gets for free from its signed arithmetic.
		const reversed = (w < 0) !== (h < 0);
		if (w < 0) { x += w; w = -w; [ul, ur] = [ur, ul]; [ll, lr] = [lr, ll]; }
		if (h < 0) { y += h; h = -h; [ul, ll] = [ll, ul]; [ur, lr] = [lr, ur]; }
		let scale = 1;
		for (const [sum, edge] of [[ul.x + ur.x, w], [ll.x + lr.x, w], [ul.y + ll.y, h], [ur.y + lr.y, h]]) {
			if (sum > edge) scale = Math.min(scale, edge / sum);
		}
		if (scale < 1) {
			for (const r of [ul, ur, lr, ll]) { r.x *= scale; r.y *= scale; }
		}
		return { x, y, w, h, ul, ur, lr, ll, reversed };
	}

	function roundRectCommandList(v) {
		const { x, y, w, h, ul, ur, lr, ll, reversed } = v;
		if (reversed) {
			return [
				["M", x + ul.x, y],
				["C", x + ul.x - KAPPA * ul.x, y, x, y + ul.y - KAPPA * ul.y, x, y + ul.y],
				["L", x, y + h - ll.y],
				["C", x, y + h - ll.y + KAPPA * ll.y, x + ll.x - KAPPA * ll.x, y + h, x + ll.x, y + h],
				["L", x + w - lr.x, y + h],
				["C", x + w - lr.x + KAPPA * lr.x, y + h, x + w, y + h - lr.y + KAPPA * lr.y, x + w, y + h - lr.y],
				["L", x + w, y + ur.y],
				["C", x + w, y + ur.y - KAPPA * ur.y, x + w - ur.x + KAPPA * ur.x, y, x + w - ur.x, y],
				["Z"],
				["M", x, y],
			];
		}
		return [
			["M", x + ul.x, y],
			["L", x + w - ur.x, y],
			["C", x + w - ur.x + KAPPA * ur.x, y, x + w, y + ur.y - KAPPA * ur.y, x + w, y + ur.y],
			["L", x + w, y + h - lr.y],
			["C", x + w, y + h - lr.y + KAPPA * lr.y, x + w - lr.x + KAPPA * lr.x, y + h, x + w - lr.x, y + h],
			["L", x + ll.x, y + h],
			["C", x + ll.x - KAPPA * ll.x, y + h, x, y + h - ll.y + KAPPA * ll.y, x, y + h - ll.y],
			["L", x, y + ul.y],
			["C", x, y + ul.y - KAPPA * ul.y, x + ul.x - KAPPA * ul.x, y, x + ul.x, y],
			["Z"],
			["M", x, y],
		];
	}

	class Path2D {
		constructor(source = undefined) {
			Object.defineProperty(this, "_cmds", { value: [], writable: true });
			if (source instanceof Path2D) this._cmds = source._cmds.map((c) => c.slice());
		}
		// Every method converts its arguments FIRST (the conversion is what
		// throws for a BigInt), then a non-finite value silently ignores the
		// call — the whole call, not just one point — and only then a negative
		// radius throws. That order is the specification's.
		moveTo(x, y) {
			x = +x; y = +y;
			if (Number.isFinite(x) && Number.isFinite(y)) this._cmds.push(["M", x, y]);
		}
		lineTo(x, y) {
			x = +x; y = +y;
			if (Number.isFinite(x) && Number.isFinite(y)) this._cmds.push(["L", x, y]);
		}
		closePath() { this._cmds.push(["Z"]); }
		quadraticCurveTo(cpx, cpy, x, y) {
			const a = [+cpx, +cpy, +x, +y];
			if (a.every(Number.isFinite)) this._cmds.push(["Q", ...a]);
		}
		bezierCurveTo(c1x, c1y, c2x, c2y, x, y) {
			const a = [+c1x, +c1y, +c2x, +c2y, +x, +y];
			if (a.every(Number.isFinite)) this._cmds.push(["C", ...a]);
		}
		rect(x, y, w, h) {
			const a = [+x, +y, +w, +h];
			if (a.every(Number.isFinite)) this._cmds.push(["R", ...a]);
		}
		roundRect(x, y, w, h, radii) {
			const v = normRoundRect(x, y, w, h, radii);
			if (v) this._cmds.push(["RR", v]);
		}
		arc(x, y, r, start, end, ccwFlag = false) {
			const a = [+x, +y, +r, +start, +end];
			ccwFlag = Boolean(ccwFlag);
			if (!a.every(Number.isFinite)) return;
			if (a[2] < 0) throw new DOMException("arc: the radius must not be negative", "IndexSizeError");
			this._cmds.push(["A", a[0], a[1], a[2], a[2], 0, a[3], a[4], ccwFlag]);
		}
		ellipse(x, y, rx, ry, rotation, start, end, ccwFlag = false) {
			const a = [+x, +y, +rx, +ry, +rotation, +start, +end];
			ccwFlag = Boolean(ccwFlag);
			if (!a.every(Number.isFinite)) return;
			if (a[2] < 0 || a[3] < 0) {
				throw new DOMException("ellipse: the radii must not be negative", "IndexSizeError");
			}
			this._cmds.push(["A", ...a, ccwFlag]);
		}
		arcTo(x1, y1, x2, y2, r) {
			const a = [+x1, +y1, +x2, +y2, +r];
			if (!a.every(Number.isFinite)) return;
			if (a[4] < 0) throw new DOMException("arcTo: the radius must not be negative", "IndexSizeError");
			this._cmds.push(["T", ...a]);
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

	// flatten turns a command list into polygons in the matrix's target space.
	// A subpath that a closePath (or a rect) sealed carries `closed: true`,
	// which is what tells the stroker to join its ends instead of capping them.
	function flatten(cmds, matrix) {
		const subpaths = [];
		let current = null;
		let cx = 0, cy = 0;     // current point in the commands' own space
		let startX = 0, startY = 0;
		const push = (x, y) => {
			const [dx, dy] = applyMatrix(matrix, x, y);
			if (Number.isNaN(dx) || Number.isNaN(dy)) return;
			if (current === null) current = [];
			current.push(clampCoord(dx), clampCoord(dy));
		};
		const begin = (x, y) => {
			if (current && current.length >= 4) subpaths.push(current);
			current = null;
			cx = startX = x;
			cy = startY = y;
			push(x, y);
		};
		const step = (c) => {
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
						current.closed = true;
						subpaths.push(current);
					}
					current = null;
					cx = startX; cy = startY;
					// A closed subpath leaves the current point at its start, and the
					// next segment continues from there rather than starting a new one.
					push(startX, startY);
					break;
				case "Q": {
					// With no subpath yet, the curve's first control point is where
					// the subpath begins — "ensure there is a subpath", per spec.
					if (current === null) begin(c[1], c[2]);
					const [qx, qy, ex, ey] = [c[1], c[2], c[3], c[4]];
					const x0 = cx, y0 = cy;
					for (let i = 1; i <= CURVE_STEPS; i++) {
						const t = i / CURVE_STEPS, u = 1 - t;
						push(u * u * x0 + 2 * u * t * qx + t * t * ex, u * u * y0 + 2 * u * t * qy + t * t * ey);
					}
					cx = ex; cy = ey;
					break;
				}
				case "C": {
					if (current === null) begin(c[1], c[2]);
					const [c1x, c1y, c2x, c2y, ex, ey] = c.slice(1);
					const x0 = cx, y0 = cy;
					for (let i = 1; i <= CURVE_STEPS; i++) {
						const t = i / CURVE_STEPS, u = 1 - t;
						push(
							u * u * u * x0 + 3 * u * u * t * c1x + 3 * u * t * t * c2x + t * t * t * ex,
							u * u * u * y0 + 3 * u * u * t * c1y + 3 * u * t * t * c2y + t * t * t * ey,
						);
					}
					cx = ex; cy = ey;
					break;
				}
				case "R":
					for (const s of rectCommandList(c[1], c[2], c[3], c[4])) step(s);
					break;
				case "RR":
					for (const s of roundRectCommandList(c[1])) step(s);
					break;
				case "A": {
					const [ax, ay, rx, ry, rot, a0, a1, ccwFlag] = c.slice(1);
					const arc = arcBeziers(ax, ay, rx, ry, rot, a0, arcSweep(a0, a1, ccwFlag));
					step([current === null ? "M" : "L", arc.sx, arc.sy]);
					for (const b of arc.curves) step(["C", ...b]);
					break;
				}
				case "T": {
					// With no subpath, arcTo only ensures one at (x1, y1): the arc
					// from a point to itself is zero-length and pruned.
					if (current === null) { begin(c[1], c[2]); break; }
					for (const s of arcToCommands(cx, cy, c[1], c[2], c[3], c[4], c[5])) step(s);
					break;
				}
			}
		};
		for (const c of cmds) step(c);
		if (current && current.length >= 4) subpaths.push(current);
		return subpaths;
	}

	// dashPolys cuts polylines into their dashed pieces, measured along the
	// path in the space the polylines are in (which is USER space when a
	// stroke is being built — dash lengths scale with the transform because
	// the whole outline does).
	function dashPolys(subpaths, pattern, offset) {
		if (!pattern.length) return subpaths;
		let total = 0;
		for (const d of pattern) total += d;
		if (!(total > 0) || !Number.isFinite(total) || !Number.isFinite(offset)) return subpaths;
		const out = [];
		for (const sp of subpaths) {
			let phase = offset % total;
			if (phase < 0) phase += total;
			let idx = 0;
			while (phase >= pattern[idx]) { phase -= pattern[idx]; idx = (idx + 1) % pattern.length; }
			let on = idx % 2 === 0;
			let run = null;
			for (let i = 0; i + 3 < sp.length; i += 2) {
				const x0 = sp[i], y0 = sp[i + 1], x1 = sp[i + 2], y1 = sp[i + 3];
				const segLen = Math.hypot(x1 - x0, y1 - y0);
				if (segLen === 0) continue;
				const ux = (x1 - x0) / segLen, uy = (y1 - y0) / segLen;
				let pos = 0;
				while (pos < segLen) {
					const remain = pattern[idx] - phase;
					const take = Math.min(remain, segLen - pos);
					if (on) {
						if (run === null) run = [x0 + ux * pos, y0 + uy * pos];
						run.push(x0 + ux * (pos + take), y0 + uy * (pos + take));
					}
					pos += take;
					phase += take;
					if (pattern[idx] - phase <= 1e-9) {
						if (run && run.length >= 4) out.push(run);
						run = null;
						phase = 0;
						idx = (idx + 1) % pattern.length;
						on = !on;
					}
				}
			}
			if (run && run.length >= 4) out.push(run);
			run = null;
		}
		return out;
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

	// strokeOutline turns polylines into the polygons that, filled by the
	// nonzero rule, ARE the stroke: a quad per segment, a join per interior
	// vertex (every vertex, when the subpath is closed), and caps on open
	// ends. A subpath with no length is pruned entirely — it draws neither
	// caps nor joins, which is the specification's "prune" step.
	function strokeOutline(subpaths, width, cap, join, miterLimit) {
		const hw = width / 2;
		const out = [];
		if (!(hw > 0)) return out;
		const disc = (x, y) => {
			const poly = [];
			for (let i = 0; i < 24; i++) {
				const t = (i / 24) * TAU;
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
			let closed = sp.closed === true;
			// A closed polyline repeats its first point; drop the repeat so the
			// wrap-around segment and join don't see a zero-length edge.
			if (closed && pts.length >= 4
				&& Math.abs(pts[0] - pts[pts.length - 2]) < 1e-12
				&& Math.abs(pts[1] - pts[pts.length - 1]) < 1e-12) {
				pts.length -= 2;
			}
			const n = pts.length / 2;
			if (n < 2) continue;
			// A closed 2-point subpath (a zero-height rect) is a hairpin: the
			// segment out and back, with a JOIN at each end — which is what
			// puts round-join discs on the ends of a degenerate rectangle.
			const segCount = closed ? n : n - 1;
			for (let s = 0; s < segCount; s++) {
				const i1 = (s + 1) % n;
				const x0 = pts[s * 2], y0 = pts[s * 2 + 1], x1 = pts[i1 * 2], y1 = pts[i1 * 2 + 1];
				const dx = x1 - x0, dy = y1 - y0;
				const len = Math.hypot(dx, dy);
				if (len === 0) continue;
				const ux = dx / len, uy = dy / len;
				let ex0 = x0, ey0 = y0, ex1 = x1, ey1 = y1;
				const first = !closed && s === 0, last = !closed && s === segCount - 1;
				if (cap === "square") {
					// A square cap extends the segment by half the width at each end
					// that is not joined to another segment.
					if (first) { ex0 -= ux * hw; ey0 -= uy * hw; }
					if (last) { ex1 += ux * hw; ey1 += uy * hw; }
				}
				const nx = -uy * hw, ny = ux * hw;
				out.push(ccw([ex0 + nx, ey0 + ny, ex1 + nx, ey1 + ny, ex1 - nx, ey1 - ny, ex0 - nx, ey0 - ny]));
				if (cap === "round") {
					if (first) out.push(disc(x0, y0));
					if (last) out.push(disc(x1, y1));
				}
			}
			for (let k = closed ? 0 : 1; k < (closed ? n : n - 1); k++) {
				const vx = pts[k * 2], vy = pts[k * 2 + 1];
				const pk = (k - 1 + n) % n, nk = (k + 1) % n;
				const inX = vx - pts[pk * 2], inY = vy - pts[pk * 2 + 1];
				const outX = pts[nk * 2] - vx, outY = pts[nk * 2 + 1] - vy;
				const inLen = Math.hypot(inX, inY), outLen = Math.hypot(outX, outY);
				if (inLen === 0 || outLen === 0) continue;
				const u1x = inX / inLen, u1y = inY / inLen, u2x = outX / outLen, u2y = outY / outLen;
				if (join === "round") { out.push(disc(vx, vy)); continue; }
				const crossZ = u1x * u2y - u1y * u2x;
				if (Math.abs(crossZ) < 1e-12) continue;
				// The join fills the wedge on the OUTER side of the turn — the
				// side the path turns away from. The inner side needs nothing:
				// there the two quads overlap, and a triangle there would paint
				// area a butt cap just beyond the corner excludes.
				const sgn = crossZ > 0 ? 1 : -1;
				const o1x = sgn * u1y * hw, o1y = -sgn * u1x * hw;
				const o2x = sgn * u2y * hw, o2y = -sgn * u2x * hw;
				out.push(ccw([vx, vy, vx + o1x, vy + o1y, vx + o2x, vy + o2y]));
				if (join === "miter") {
					const cosPhi = u1x * u2x + u1y * u2y;
					// The miter ratio is 1/sin(θ/2) for interior angle θ, and
					// sin(θ/2) = √((1+cos φ)/2) for turn angle φ.
					const sinHalf = Math.sqrt(Math.max(0, (1 + cosPhi) / 2));
					if (sinHalf > 0 && 1 / sinHalf <= miterLimit) {
						const bx = o1x + o2x, by = o1y + o2y;
						const bl = Math.hypot(bx, by);
						if (bl > 0) {
							const apexX = vx + (bx / bl) * (hw / sinHalf);
							const apexY = vy + (by / bl) * (hw / sinHalf);
							out.push(ccw([vx, vy, vx + o1x, vy + o1y, apexX, apexY, vx + o2x, vy + o2y]));
						}
					}
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
		constructor(internal, kind, coords) {
			if (internal !== GRADIENT_INTERNAL) throw new TypeError("Illegal constructor");
			Object.defineProperties(this, {
				_kind: { value: kind },
				_coords: { value: coords },
				_stops: { value: [] },
			});
		}
		addColorStop(offset, color) {
			if (arguments.length < 2) throw new TypeError("addColorStop requires 2 arguments");
			// A non-finite offset is a TypeError (the Web IDL double conversion
			// rejects it); a finite one outside [0, 1] is an IndexSizeError.
			const o = +offset;
			if (!Number.isFinite(o)) throw new TypeError("addColorStop: the offset must be finite");
			if (o < 0 || o > 1) {
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

	// A pattern is an image and how it tiles. It carries its own transform,
	// which setTransform on the pattern replaces — the drawing transform is
	// applied on top of it.
	class CanvasPattern {
		constructor(internal, info, repetition) {
			if (internal !== PATTERN_INTERNAL) throw new TypeError("Illegal constructor");
			Object.defineProperties(this, {
				_info: { value: info },
				_repetition: { value: repetition },
				_transform: { value: IDENTITY.slice(), writable: true },
			});
		}
		setTransform(matrix = undefined) {
			if (matrix === undefined || matrix === null) { this._transform = IDENTITY.slice(); return; }
			const next = fixup2D(matrix);
			if (finiteMatrix(next)) this._transform = next;
		}
	}
	Object.defineProperty(CanvasPattern.prototype, Symbol.toStringTag, {
		value: "CanvasPattern", configurable: true,
	});
	const PATTERN_INTERNAL = Symbol("CanvasPattern.internal");

	// ---------------------------------------------------------- ImageData

	// enforcedLong is Web IDL's [EnforceRange] long conversion, which is what
	// every ImageData dimension and pixel coordinate goes through: a value
	// that is not finite, or that truncates outside the type's range, is a
	// TypeError rather than a wrap-around.
	function enforcedLong(v, what, unsigned = false) {
		const n = Math.trunc(+v);
		const lo = unsigned ? 0 : -2147483648;
		const hi = unsigned ? 4294967295 : 2147483647;
		if (!Number.isFinite(n) || n < lo || n > hi) {
			throw new TypeError(`${what} is out of range`);
		}
		// Math.trunc(-0.5) is -0; normalize so later comparisons see plain 0.
		return n === 0 ? 0 : n;
	}

	function imageDataSettings(settings, what) {
		let pixelFormat = "rgba-unorm8", colorSpace = "srgb";
		if (settings !== undefined && settings !== null) {
			if (settings.pixelFormat !== undefined) {
				pixelFormat = String(settings.pixelFormat);
				if (pixelFormat !== "rgba-unorm8" && pixelFormat !== "rgba-float16") {
					throw new TypeError(`${what}: ${pixelFormat} is not a pixel format`);
				}
			}
			if (settings.colorSpace !== undefined) {
				colorSpace = String(settings.colorSpace);
				if (colorSpace !== "srgb" && colorSpace !== "display-p3") {
					throw new TypeError(`${what}: ${colorSpace} is not a color space`);
				}
			}
		}
		return { pixelFormat, colorSpace };
	}

	// imageDataBytes is an ImageData's pixels as the RGBA bytes the host
	// stores: a float16 buffer holds 0..1 values and is quantized here.
	function imageDataBytes(data) {
		if (data.pixelFormat !== "rgba-float16") {
			return new Uint8Array(data.data.buffer, data.data.byteOffset, data.data.byteLength);
		}
		const out = new Uint8Array(data.data.length);
		for (let i = 0; i < data.data.length; i++) {
			const v = data.data[i];
			out[i] = v <= 0 ? 0 : v >= 1 ? 255 : Math.round(v * 255);
		}
		return out;
	}

	class ImageData {
		constructor(a, b, c, d) {
			let data, width, height, settings;
			if (a instanceof Uint8ClampedArray || (typeof Float16Array !== "undefined" && a instanceof Float16Array)) {
				data = a;
				settings = imageDataSettings(c !== undefined && typeof c === "object" ? c : d, "ImageData");
				if (a instanceof Uint8ClampedArray && settings.pixelFormat === "rgba-float16") {
					throw new TypeError("ImageData: rgba-float16 needs a Float16Array");
				}
				if (!(a instanceof Uint8ClampedArray) && settings.pixelFormat === "rgba-unorm8") {
					settings = { ...settings, pixelFormat: "rgba-float16" };
				}
				width = enforcedLong(b, "ImageData: the width", true);
				if (width === 0) {
					throw new DOMException("ImageData: the width must be a positive integer", "IndexSizeError");
				}
				if (data.length % 4 !== 0 || (data.length / 4) % width !== 0) {
					throw new DOMException("ImageData: the data does not fill whole rows", "InvalidStateError");
				}
				height = data.length / 4 / width;
				if (c !== undefined && typeof c !== "object" && enforcedLong(c, "ImageData: the height", true) !== height) {
					throw new DOMException("ImageData: the height does not match the data", "IndexSizeError");
				}
			} else {
				width = enforcedLong(a, "ImageData: the width", true);
				height = enforcedLong(b, "ImageData: the height", true);
				settings = imageDataSettings(c, "ImageData");
				if (width === 0 || height === 0) {
					throw new DOMException("ImageData: the dimensions must be positive integers", "IndexSizeError");
				}
				data = settings.pixelFormat === "rgba-float16"
					? new Float16Array(width * height * 4)
					: new Uint8ClampedArray(width * height * 4);
			}
			Object.defineProperties(this, {
				_data: { value: data },
				_width: { value: width },
				_height: { value: height },
				_pixelFormat: { value: settings.pixelFormat },
				_colorSpace: { value: settings.colorSpace },
			});
		}
		get data() { return this._data; }
		get width() { return this._width; }
		get height() { return this._height; }
		get colorSpace() { return this._colorSpace; }
		get pixelFormat() { return this._pixelFormat; }
	}
	Object.defineProperty(ImageData.prototype, Symbol.toStringTag, {
		value: "ImageData", configurable: true,
	});

	// ------------------------------------------------------- ImageBitmap
	// A bitmap is a surface and its size. It is a distinct type from a canvas
	// because it is IMMUTABLE and because closing it releases the pixels — which
	// is the whole reason the interface exists.

	class ImageBitmap {
		constructor(internal, handle, width, height) {
			if (internal !== BITMAP_INTERNAL) throw new TypeError("Illegal constructor");
			Object.defineProperties(this, {
				_handle: { value: handle, writable: true },
				_width: { value: width, writable: true },
				_height: { value: height, writable: true },
			});
		}
		get width() { return this._width; }
		get height() { return this._height; }
		close() {
			if (this._handle === null) return;
			canvasFree(this._handle);
			this._handle = null;
			this._width = 0;
			this._height = 0;
		}
	}
	Object.defineProperty(ImageBitmap.prototype, Symbol.toStringTag, {
		value: "ImageBitmap", configurable: true,
	});
	const BITMAP_INTERNAL = Symbol("ImageBitmap.internal");

	// sourceSurface answers the handle and size of anything that can be drawn:
	// a bitmap, a canvas, or an ImageData. Anything else is not an image source,
	// and saying so is better than drawing nothing.
	function sourceSurface(source) {
		if (source instanceof ImageBitmap) {
			if (source._handle === null) {
				throw new DOMException("the ImageBitmap has been closed", "InvalidStateError");
			}
			return { handle: source._handle, width: source._width, height: source._height };
		}
		if (source instanceof OffscreenCanvas) {
			// A canvas with unclosed layers has no well-defined pixels to read.
			if (source._context && source._context._layers > 0) {
				throw new DOMException("the source canvas has open layers", "InvalidStateError");
			}
			return { handle: source._handle, width: source.width, height: source.height };
		}
		if (source instanceof ImageData) {
			const handle = canvasFromBytes(source.width, source.height, imageDataBytes(source));
			return { handle, width: source.width, height: source.height, temporary: true };
		}
		return null;
	}

	globalThis.createImageBitmap = function createImageBitmap(source, ...rest) {
		if (arguments.length < 1) {
			return Promise.reject(new TypeError("createImageBitmap requires 1 argument"));
		}
		// A Blob has to be DECODED, which is the one source that is not already
		// pixels; everything else is a surface that exists.
		if (typeof Blob === "function" && source instanceof Blob) {
			return source.bytes().then((bytes) => {
				const r = canvasDecodeImage(bytes);
				if (r.error !== undefined) {
					throw new DOMException(`createImageBitmap: ${r.error}`, "InvalidStateError");
				}
				return new ImageBitmap(BITMAP_INTERNAL, r.handle, r.width, r.height);
			});
		}
		let info;
		try {
			info = sourceSurface(source);
		} catch (e) {
			return Promise.reject(e);
		}
		if (!info) {
			return Promise.reject(new TypeError("createImageBitmap: the source is not an image"));
		}
		if (info.width === 0 || info.height === 0) {
			return Promise.reject(new DOMException("createImageBitmap: the source is empty", "InvalidStateError"));
		}
		// A bitmap is a COPY: it must not change when its source does.
		const copy = canvasFromBytes(info.width, info.height,
			new Uint8Array(canvasGetImageData(info.handle, 0, 0, info.width, info.height)));
		if (info.temporary) canvasFree(info.handle);
		return Promise.resolve(new ImageBitmap(BITMAP_INTERNAL, copy, info.width, info.height));
	};

	// ------------------------------------------------------- the context

	const DEFAULT_STATE = () => ({
		transform: IDENTITY.slice(),
		fill: [0, 0, 0, 1], fillGradient: null, fillPattern: null,
		stroke: [0, 0, 0, 1], strokeGradient: null, strokePattern: null,
		globalAlpha: 1,
		composite: "source-over",
		lineWidth: 1, lineCap: "butt", lineJoin: "miter", miterLimit: 10,
		lineDash: [], lineDashOffset: 0,
		clip: null, // device-space polygons, or null for none
		font: "10px sans-serif", fontParsed: null, lang: "inherit",
		textAlign: "start", textBaseline: "alphabetic",
		shadowColor: [0, 0, 0, 0], shadowBlur: 0, shadowOffsetX: 0, shadowOffsetY: 0,
		imageSmoothingEnabled: true, imageSmoothingQuality: "low",
		filter: "none", filterObject: null, direction: "inherit", letterSpacing: "0px", wordSpacing: "0px",
		fontKerning: "auto", fontStretch: "normal", fontVariantCaps: "normal",
		textRendering: "auto",
	});

	// CanvasFilter wraps a validated filter-primitive list; assigning one to
	// ctx.filter (or passing the same structure to beginLayer) is the
	// object-based spelling of the filter attribute.
	class CanvasFilter {
		constructor(init = undefined) {
			validateLayerFilter(init);
			Object.defineProperty(this, "_prims", { value: init });
		}
	}
	Object.defineProperty(CanvasFilter.prototype, Symbol.toStringTag, {
		value: "CanvasFilter", configurable: true,
	});

	// validCSSFilter decides whether a string is a <filter-value-list>: a
	// whitespace-separated sequence of filter functions. Invalid strings —
	// including the CSS-wide keywords and the empty string — leave the
	// attribute untouched, so validation must be exact, not approximate.
	const CSS_LENGTH = /^[+-]?(?:\d+(?:\.\d+)?|\.\d+)(?:e[+-]?\d+)?(?:px|em|rem|ex|ch|vw|vh|vmin|vmax|cm|mm|in|pt|pc|q)$/i;
	const CSS_ANGLE = /^[+-]?(?:\d+(?:\.\d+)?|\.\d+)(?:e[+-]?\d+)?(?:deg|grad|rad|turn)$/i;
	const CSS_NUMBER_OR_PCT = /^[+-]?(?:\d+(?:\.\d+)?|\.\d+)(?:e[+-]?\d+)?%?$/;
	const isZero = (t) => /^[+-]?0+(?:\.0+)?$/.test(t);
	function validFilterFunction(name, args) {
		const body = args.trim();
		switch (name) {
			case "blur":
				return body === "" || CSS_LENGTH.test(body) || isZero(body);
			case "hue-rotate":
				return body === "" || CSS_ANGLE.test(body) || isZero(body);
			case "brightness":
			case "contrast":
			case "grayscale":
			case "invert":
			case "opacity":
			case "saturate":
			case "sepia":
				return body === "" || (CSS_NUMBER_OR_PCT.test(body) && parseFloat(body) >= 0);
			case "drop-shadow": {
				// color? && <length>{2,3} — the colour may itself be a function.
				const parts = [];
				let depth = 0, cur = "";
				for (const ch of body) {
					if (ch === "(") depth++;
					if (ch === ")") depth--;
					if (/\s/.test(ch) && depth === 0) {
						if (cur) { parts.push(cur); cur = ""; }
					} else {
						cur += ch;
					}
				}
				if (cur) parts.push(cur);
				let lengths = 0, colors = 0;
				for (const part of parts) {
					if (CSS_LENGTH.test(part) || isZero(part)) lengths++;
					else if (parseColor(part) !== null) colors++;
					else return false;
				}
				return lengths >= 2 && lengths <= 3 && colors <= 1;
			}
			case "url":
				return true;
			default:
				return false;
		}
	}
	function validCSSFilter(str) {
		const s2 = str.trim();
		if (s2 === "none") return true;
		if (s2 === "") return false;
		let at = 0;
		while (at < s2.length) {
			while (at < s2.length && /\s/.test(s2[at])) at++;
			if (at >= s2.length) break;
			const m = /^([a-z-]+)\(/i.exec(s2.slice(at));
			if (!m) return false;
			let depth = 1;
			let end = at + m[0].length;
			while (end < s2.length && depth > 0) {
				if (s2[end] === "(") depth++;
				else if (s2[end] === ")") depth--;
				end++;
			}
			if (depth !== 0) return false;
			const args = s2.slice(at + m[0].length, end - 1);
			if (!validFilterFunction(m[1].toLowerCase(), args)) return false;
			at = end;
		}
		return true;
	}

	// -------------------------------------------------------- the font value
	// parseFontShorthand is the CSS font shorthand: optional style, variant
	// (small-caps only), weight and stretch in any order, then a size with an
	// optional /line-height (parsed and dropped), then a font family list.
	// Relative sizes resolve against the canvas default of 10px. The parsed
	// form is kept alongside its canonical serialization, which is what the
	// attribute answers with.

	const FONT_STRETCH_KEYWORDS = [
		"ultra-condensed", "extra-condensed", "condensed", "semi-condensed",
		"semi-expanded", "expanded", "extra-expanded", "ultra-expanded",
	];
	const GENERIC_FAMILIES = new Set([
		"serif", "sans-serif", "monospace", "cursive", "fantasy",
		"system-ui", "math", "ui-serif", "ui-sans-serif", "ui-monospace", "ui-rounded",
	]);
	const CSS_WIDE_KEYWORDS = new Set(["inherit", "initial", "unset", "revert", "revert-layer", "default"]);
	// Pixels per unit, for the units a font size accepts. Relative units
	// resolve against the default font (10px) or the root default (16px).
	const FONT_UNITS = {
		px: 1, pt: 4 / 3, pc: 16, in: 96, cm: 96 / 2.54, mm: 96 / 25.4, q: 96 / 101.6,
		em: 10, rem: 16, ex: 5, ch: 5, "%": 0.1,
	};
	const FONT_SIZE_KEYWORDS = {
		"xx-small": 16 * 3 / 5, "x-small": 16 * 3 / 4, small: 16 * 8 / 9, medium: 16,
		large: 16 * 6 / 5, "x-large": 16 * 3 / 2, "xx-large": 16 * 2, "xxx-large": 16 * 3,
		larger: 12, smaller: 10 * 5 / 6,
	};

	function parseFontLength(tok) {
		const m = /^([+-]?(?:\d+(?:\.\d+)?|\.\d+)(?:e[+-]?\d+)?)(px|pt|pc|in|cm|mm|q|em|rem|ex|ch|%)$/i.exec(tok);
		if (!m) return null;
		const n = parseFloat(m[1]) * FONT_UNITS[m[2].toLowerCase()];
		return Number.isFinite(n) && n >= 0 ? n : null;
	}

	function parseFontFamilies(raw) {
		const families = [];
		let at = 0;
		const ws = () => { while (at < raw.length && /\s/.test(raw[at])) at++; };
		for (;;) {
			ws();
			if (at >= raw.length) return null;
			const ch = raw[at];
			if (ch === '"' || ch === "'") {
				at++;
				let val = "";
				for (;;) {
					if (at >= raw.length) return null; // unterminated
					const c = raw[at];
					if (c === ch) { at++; break; }
					if (c === "\\") {
						at++;
						if (at >= raw.length) return null;
						val += raw[at]; at++;
						continue;
					}
					val += c; at++;
				}
				families.push({ quoted: true, name: val });
			} else {
				const words = [];
				for (;;) {
					ws();
					if (at >= raw.length || raw[at] === ",") break;
					const m = /^-?[A-Za-z_\u0080-\uffff][A-Za-z0-9_\u0080-\uffff-]*/.exec(raw.slice(at));
					if (!m) return null;
					words.push(m[0]);
					at += m[0].length;
				}
				if (words.length === 0) return null;
				if (words.length === 1 && CSS_WIDE_KEYWORDS.has(words[0].toLowerCase())) return null;
				const name = words.join(" ");
				const generic = words.length === 1 && GENERIC_FAMILIES.has(words[0].toLowerCase());
				families.push({ quoted: false, name: generic ? name.toLowerCase() : name, generic });
			}
			ws();
			if (at >= raw.length) break;
			if (raw[at] !== ",") return null;
			at++;
		}
		return families.length ? families : null;
	}

	function serializeFontSize(px) {
		// The shortest decimal that round-trips, the way CSS serializes numbers.
		return String(Math.round(px * 1000) / 1000);
	}

	function serializeFontFamily(f) {
		if (f.quoted) return '"' + f.name.replace(/[\\"]/g, (c) => "\\" + c) + '"';
		return f.name;
	}

	// parseSpacing is letterSpacing/wordSpacing's value: a CSS <length>,
	// serialized with its number and lowercased unit.
	function parseSpacing(v) {
		const m = /^\s*([+-]?(?:\d+(?:\.\d+)?|\.\d+)(?:e[+-]?\d+)?)(px|pt|pc|in|cm|mm|q|em|rem|ex|ch)\s*$/i.exec(String(v));
		if (!m) return null;
		const n = parseFloat(m[1]);
		if (!Number.isFinite(n)) return null;
		return String(Math.round(n * 1000) / 1000) + m[2].toLowerCase();
	}

	function parseFontShorthand(input) {
		const str = String(input).trim();
		if (str === "" || CSS_WIDE_KEYWORDS.has(str.toLowerCase())) return null;
		let style = "normal", variant = "normal", weight = "normal", stretch = "normal";
		const seen = { style: false, variant: false, weight: false, stretch: false };
		const re = /\S+/g;
		let sizePx = null, familyStart = -1;
		for (;;) {
			const m = re.exec(str);
			if (!m) return null;
			const tok = m[0], low = tok.toLowerCase();
			if (low === "normal") continue; // normal is valid for every slot
			if (!seen.style && (low === "italic" || low === "oblique")) {
				style = low; seen.style = true; continue;
			}
			if (!seen.variant && low === "small-caps") {
				variant = low; seen.variant = true; continue;
			}
			if (!seen.weight && (low === "bold" || low === "bolder" || low === "lighter")) {
				weight = low; seen.weight = true; continue;
			}
			if (!seen.weight && /^[1-9]00$/.test(low)) {
				weight = low; seen.weight = true; continue;
			}
			if (!seen.stretch && FONT_STRETCH_KEYWORDS.includes(low)) {
				stretch = low; seen.stretch = true; continue;
			}
			// This token must be the size (with an optional /line-height glued
			// on); everything after it is the family list.
			let sizeTok = tok, rest = str.slice(re.lastIndex);
			const slash = sizeTok.indexOf("/");
			let lh = null;
			if (slash !== -1) {
				lh = sizeTok.slice(slash + 1);
				sizeTok = sizeTok.slice(0, slash);
			} else if (/^\s*\//.test(rest)) {
				rest = rest.replace(/^\s*\//, "");
				lh = "";
			}
			if (lh !== null && lh === "") {
				const lm = /^\s*(\S+)/.exec(rest);
				if (!lm) return null;
				lh = lm[1];
				rest = rest.slice(rest.indexOf(lm[1]) + lm[1].length);
			}
			if (lh !== null && lh !== "normal" && parseFontLength(lh) === null
				&& !/^(?:\d+(?:\.\d+)?|\.\d+)$/.test(lh)) {
				return null;
			}
			if (FONT_SIZE_KEYWORDS[sizeTok.toLowerCase()] !== undefined) {
				sizePx = FONT_SIZE_KEYWORDS[sizeTok.toLowerCase()];
			} else {
				sizePx = parseFontLength(sizeTok);
			}
			if (sizePx === null) return null;
			familyStart = str.length - rest.length;
			break;
		}
		const families = parseFontFamilies(str.slice(familyStart));
		if (!families) return null;
		const parts = [];
		if (style !== "normal") parts.push(style);
		if (variant !== "normal") parts.push(variant);
		if (weight !== "normal" && weight !== "400") parts.push(weight);
		if (stretch !== "normal") parts.push(stretch);
		parts.push(serializeFontSize(sizePx) + "px");
		return {
			style, variant, weight, stretch, sizePx, families,
			serialized: parts.join(" ") + " " + families.map(serializeFontFamily).join(", "),
		};
	}

	// ------------------------------------------------- layer filter checking
	// validateLayerFilter is the parameter CONTRACT of beginLayer's filter,
	// checked up front so a throwing beginLayer opens nothing. Each attribute
	// converts the way the canvas-filters proposal says: a "number" is a
	// restricted double (NaN and infinity are TypeErrors, so null is 0 and
	// 'test' throws), a "number or list" accepts one level of iterable, and
	// an enum accepts exactly its named strings.

	function filterNum(v, what) {
		const n = +v;
		if (!Number.isFinite(n)) throw new TypeError(`beginLayer: ${what} must be a number`);
		return n;
	}
	function filterNumList(v, what, maxLen) {
		const out = (v !== null && typeof v === "object" && Symbol.iterator in v)
			? [...v].map((e) => filterNum(e, what))
			: [filterNum(v, what)];
		if (out.length > maxLen) {
			throw new TypeError(`beginLayer: ${what} takes at most ${maxLen} numbers`);
		}
		return out;
	}
	function filterSequence(v, what) {
		if (v === null || typeof v !== "object" || !(Symbol.iterator in v)) {
			throw new TypeError(`beginLayer: ${what} must be a list`);
		}
		return [...v];
	}
	// colorMatrixValues resolves a colorMatrix primitive to its 20 numbers:
	// the saturate/hueRotate/luminanceToAlpha shorthands are the fixed
	// matrices SVG defines for feColorMatrix.
	function colorMatrixValues(prim) {
		const type = prim.type === undefined ? "matrix" : String(prim.type);
		if (type === "matrix") return [...prim.values].map((v) => +v);
		const v = "values" in prim ? +prim.values : 0;
		if (type === "saturate") {
			return [
				0.213 + 0.787 * v, 0.715 - 0.715 * v, 0.072 - 0.072 * v, 0, 0,
				0.213 - 0.213 * v, 0.715 + 0.285 * v, 0.072 - 0.072 * v, 0, 0,
				0.213 - 0.213 * v, 0.715 - 0.715 * v, 0.072 + 0.928 * v, 0, 0,
				0, 0, 0, 1, 0,
			];
		}
		if (type === "hueRotate") {
			const c = Math.cos(v * Math.PI / 180), n = Math.sin(v * Math.PI / 180);
			return [
				0.213 + 0.787 * c - 0.213 * n, 0.715 - 0.715 * c - 0.715 * n, 0.072 - 0.072 * c + 0.928 * n, 0, 0,
				0.213 - 0.213 * c + 0.143 * n, 0.715 + 0.285 * c + 0.140 * n, 0.072 - 0.072 * c - 0.283 * n, 0, 0,
				0.213 - 0.213 * c - 0.787 * n, 0.715 - 0.715 * c + 0.715 * n, 0.072 + 0.928 * c + 0.072 * n, 0, 0,
				0, 0, 0, 1, 0,
			];
		}
		if (type === "luminanceToAlpha") {
			return [
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0.2125, 0.7154, 0.0721, 0, 0,
			];
		}
		return null;
	}

	function validateLayerFilter(filter) {
		if (filter === null || filter === undefined || typeof filter !== "object") return;
		const list = Symbol.iterator in filter ? [...filter] : [filter];
		for (const prim of list) {
			if (prim === null || typeof prim !== "object") {
				throw new TypeError("beginLayer: a filter primitive must be a dictionary");
			}
			switch (String(prim.name)) {
				case "gaussianBlur":
					if (!("stdDeviation" in prim)) {
						throw new TypeError("beginLayer: gaussianBlur needs a stdDeviation");
					}
					filterNumList(prim.stdDeviation, "stdDeviation", 2);
					break;
				case "colorMatrix": {
					const type = prim.type === undefined ? "matrix" : String(prim.type);
					if (type === "matrix") {
						const vals = filterSequence(prim.values, "values").map((e) => filterNum(e, "values"));
						if (vals.length !== 20) {
							throw new TypeError("beginLayer: colorMatrix takes 20 values");
						}
					} else if ((type === "saturate" || type === "hueRotate") && "values" in prim) {
						filterNum(prim.values, "values");
					}
					break;
				}
				case "convolveMatrix": {
					const rows = filterSequence(prim.kernelMatrix, "kernelMatrix")
						.map((row) => filterSequence(row, "kernelMatrix").map((e) => filterNum(e, "kernelMatrix")));
					if (rows.length === 0 || !rows.every((r) => r.length === rows[0].length)
						|| (rows.length > 1 && rows[0].length === 0)) {
						throw new TypeError("beginLayer: kernelMatrix must be a rectangular matrix");
					}
					break;
				}
				case "dropShadow":
					if ("dx" in prim) filterNum(prim.dx, "dx");
					if ("dy" in prim) filterNum(prim.dy, "dy");
					if ("floodOpacity" in prim) filterNum(prim.floodOpacity, "floodOpacity");
					if ("stdDeviation" in prim) filterNumList(prim.stdDeviation, "stdDeviation", 2);
					if ("floodColor" in prim && parseColor(String(prim.floodColor)) === null) {
						throw new TypeError("beginLayer: floodColor must be a colour");
					}
					break;
				case "turbulence": {
					if ("baseFrequency" in prim) {
						const bf = filterNumList(prim.baseFrequency, "baseFrequency", 2);
						if (bf.some((n) => n < 0)) {
							throw new TypeError("beginLayer: baseFrequency must not be negative");
						}
					}
					if ("numOctaves" in prim && filterNum(prim.numOctaves, "numOctaves") < 0) {
						throw new TypeError("beginLayer: numOctaves must not be negative");
					}
					if ("seed" in prim) filterNum(prim.seed, "seed");
					if ("stitchTiles" in prim && !["stitch", "noStitch"].includes(prim.stitchTiles)) {
						throw new TypeError("beginLayer: stitchTiles must be stitch or noStitch");
					}
					if ("type" in prim && !["fractalNoise", "turbulence"].includes(prim.type)) {
						throw new TypeError("beginLayer: type must be fractalNoise or turbulence");
					}
					break;
				}
			}
		}
	}

	class OffscreenCanvasRenderingContext2D {
		constructor(internal, canvas) {
			if (internal !== CONTEXT_INTERNAL) throw new TypeError("Illegal constructor");
			Object.defineProperties(this, {
				_canvas: { value: canvas },
				_state: { value: DEFAULT_STATE(), writable: true },
				_stack: { value: [] },
				// The default path holds DEVICE-space commands: each point was
				// transformed by the matrix in force when it was added. _dcur is
				// the device-space current point (null while the path is empty),
				// which arcTo maps back through the inverse transform to find its
				// user-space start.
				_path: { value: new Path2D(), writable: true },
				_dcur: { value: null, writable: true },
				_dstart: { value: null, writable: true },
				_layers: { value: 0, writable: true },
			});
		}

		get canvas() { return this._canvas; }

		// ----------------------------------------------------------- state
		// _snapshot clones the state onto the stack; kind records whether
		// save() or beginLayer() pushed it, because restore() may not cross a
		// layer boundary and endLayer() may not cross a save().
		_snapshot(kind) {
			const entry = JSON.parse(JSON.stringify({ ...this._state, clip: null }));
			// The clip is a device-space mask and is not JSON; it is carried by
			// reference, which is safe because a clip is never mutated in place.
			entry.clip = this._state.clip;
			entry.fillGradient = this._state.fillGradient;
			entry.strokeGradient = this._state.strokeGradient;
			entry.fillPattern = this._state.fillPattern;
			entry.strokePattern = this._state.strokePattern;
			entry._kind = kind;
			this._stack.push(entry);
		}
		save() {
			this._snapshot("save");
		}
		restore() {
			if (!this._stack.length) return;
			if (this._stack[this._stack.length - 1]._kind === "layer") {
				throw new DOMException("restore: the top of the stack is a layer", "InvalidStateError");
			}
			const s = this._stack.pop();
			const hadClip = this._state.clip;
			this._state = s;
			if (hadClip !== s.clip) this._applyClip();
		}
		// beginLayer opens a group that renders onto its own bitmap; endLayer
		// composites that bitmap as ONE image with the state saved here — the
		// alpha, the operator and the shadow apply to the group, not to each
		// draw inside it, which is what the layer-rendering attributes reset
		// to their defaults inside the layer.
		beginLayer(options = undefined) {
			// A dictionary accepts undefined and null (both mean "empty") and
			// any object; a primitive is a TypeError.
			if (options !== undefined && options !== null && typeof options !== "object") {
				throw new TypeError("beginLayer: the options must be a dictionary");
			}
			// The filter is validated NOW: a beginLayer that throws must
			// leave no layer open.
			if (options !== undefined && options !== null) validateLayerFilter(options.filter);
			this._snapshot("layer");
			const entry = this._stack[this._stack.length - 1];
			entry._layerFilter = options !== undefined && options !== null ? options.filter : undefined;
			canvasLayerBegin(this._canvas._handle);
			this._layers++;
			const st = this._state;
			st.globalAlpha = 1;
			st.composite = "source-over";
			st.shadowColor = [0, 0, 0, 0];
			st.shadowBlur = 0;
			st.shadowOffsetX = 0;
			st.shadowOffsetY = 0;
			st.filter = "none";
		}
		endLayer() {
			if (!this._stack.length || this._stack[this._stack.length - 1]._kind !== "layer") {
				throw new DOMException("endLayer: no layer is open", "InvalidStateError");
			}
			const s = this._stack.pop();
			const hadClip = this._state.clip;
			this._state = s;
			this._layers--;
			// The group composites with the RESTORED state: its alpha, its
			// operator and its shadow act on the layer as a whole.
			const spec = { alpha: s.globalAlpha, composite: s.composite };
			// A colorMatrix layer filter is rendered by the host; the other
			// primitives are validated but not yet rendered.
			if (s._layerFilter !== null && typeof s._layerFilter === "object") {
				const prims = Symbol.iterator in s._layerFilter ? [...s._layerFilter] : [s._layerFilter];
				if (prims.length === 1 && prims[0] && prims[0].name === "colorMatrix") {
					const m = colorMatrixValues(prims[0]);
					if (m) spec.colorMatrix = new Float64Array(m);
				}
			}
			if (s.shadowColor[3] > 0 && (s.shadowOffsetX !== 0 || s.shadowOffsetY !== 0 || s.shadowBlur > 0)) {
				spec.shadow = {
					dx: s.shadowOffsetX, dy: s.shadowOffsetY, blur: s.shadowBlur,
					color: new Float64Array([
						s.shadowColor[0] / 255, s.shadowColor[1] / 255,
						s.shadowColor[2] / 255, s.shadowColor[3],
					]),
				};
			}
			canvasLayerEnd(this._canvas._handle, spec);
			if (hadClip !== s.clip) this._applyClip();
		}
		reset() {
			this._state = DEFAULT_STATE();
			this._stack.length = 0;
			this._path = new Path2D();
			this._dcur = null;
			this._dstart = null;
			this._layers = 0;
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
			if (typeof a === "object" && a !== null) {
				const next = fixup2D(a);
				if (finiteMatrix(next)) this._state.transform = next;
				return;
			}
			const next = [+a, +b, +c, +d, +e, +f];
			if (finiteMatrix(next)) this._state.transform = next;
		}
		resetTransform() { this._state.transform = IDENTITY.slice(); }
		getTransform() {
			const m = this._state.transform;
			return new DOMMatrix(m);
		}
		_mul(m) {
			if (!finiteMatrix(m)) return;
			this._state.transform = multiply(this._state.transform, m);
		}

		// ---------------------------------------------------------- styles
		get fillStyle() {
			return this._state.fillGradient ?? this._state.fillPattern ?? serializeColor(this._state.fill);
		}
		set fillStyle(v) {
			if (v instanceof CanvasGradient) { this._state.fillGradient = v; this._state.fillPattern = null; return; }
			if (v instanceof CanvasPattern) { this._state.fillPattern = v; this._state.fillGradient = null; return; }
			const c = parseColor(v);
			if (c === null) return;
			this._state.fill = c;
			this._state.fillGradient = null;
			this._state.fillPattern = null;
		}
		get strokeStyle() {
			return this._state.strokeGradient ?? this._state.strokePattern ?? serializeColor(this._state.stroke);
		}
		set strokeStyle(v) {
			if (v instanceof CanvasGradient) { this._state.strokeGradient = v; this._state.strokePattern = null; return; }
			if (v instanceof CanvasPattern) { this._state.strokePattern = v; this._state.strokeGradient = null; return; }
			const c = parseColor(v);
			if (c === null) return;
			this._state.stroke = c;
			this._state.strokeGradient = null;
			this._state.strokePattern = null;
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
		get filter() { return this._state.filterObject ?? this._state.filter; }
		set filter(v) {
			if (v instanceof CanvasFilter) {
				this._state.filterObject = v;
				this._state.filter = "none";
				return;
			}
			// A string is kept VERBATIM when it is a valid CSS filter value
			// list, and ignored entirely when it is not — including the CSS-wide
			// keywords, which are not values here.
			const str = String(v);
			if (validCSSFilter(str)) {
				this._state.filter = str;
				this._state.filterObject = null;
			}
		}
		get font() { return this._state.font; }
		set font(v) {
			const f = parseFontShorthand(v);
			if (f === null) return;
			this._state.font = f.serialized;
			this._state.fontParsed = f;
		}
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
		set letterSpacing(v) {
			const t = parseSpacing(v);
			if (t !== null) this._state.letterSpacing = t;
		}
		get wordSpacing() { return this._state.wordSpacing; }
		set wordSpacing(v) {
			const t = parseSpacing(v);
			if (t !== null) this._state.wordSpacing = t;
		}
		get fontKerning() { return this._state.fontKerning; }
		set fontKerning(v) { if (["auto", "normal", "none"].includes(String(v))) this._state.fontKerning = String(v); }
		get fontStretch() { return this._state.fontStretch; }
		set fontStretch(v) {
			// The enums below are CASE-SENSITIVE: an attribute set to a value
			// that is not one of the names keeps its old value.
			if (["normal", ...FONT_STRETCH_KEYWORDS].includes(v)) this._state.fontStretch = v;
		}
		get fontVariantCaps() { return this._state.fontVariantCaps; }
		set fontVariantCaps(v) {
			if (["normal", "small-caps", "all-small-caps", "petite-caps",
				"all-petite-caps", "unicase", "titling-caps"].includes(v)) {
				this._state.fontVariantCaps = v;
			}
		}
		get textRendering() { return this._state.textRendering; }
		set textRendering(v) {
			if (["auto", "optimizeSpeed", "optimizeLegibility", "geometricPrecision"].includes(v)) {
				this._state.textRendering = v;
			}
		}
		get lang() { return this._state.lang; }
		set lang(v) { this._state.lang = String(v); }

		// --------------------------------------------------------- drawing
		createLinearGradient(x0, y0, x1, y1) {
			if (arguments.length < 4) throw new TypeError("createLinearGradient requires 4 arguments");
			const coords = [+x0, +y0, 0, +x1, +y1, 0];
			if (!coords.every(Number.isFinite)) {
				throw new TypeError("createLinearGradient: the coordinates must be finite");
			}
			return new CanvasGradient(GRADIENT_INTERNAL, "linear", coords);
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
			return new CanvasGradient(GRADIENT_INTERNAL, "radial", coords);
		}
		createConicGradient(startAngle, x, y) {
			if (arguments.length < 3) throw new TypeError("createConicGradient requires 3 arguments");
			const coords = [+startAngle, +x, +y];
			if (!coords.every(Number.isFinite)) {
				throw new TypeError("createConicGradient: the arguments must be finite");
			}
			return new CanvasGradient(GRADIENT_INTERNAL, "conic", coords);
		}

		beginPath() { this._path = new Path2D(); this._dcur = null; this._dstart = null; }
		// _appendUser bakes user-space M/L/Q/C/Z commands into the default
		// path through the current transform — the specification transforms
		// points when they are ADDED, not when the path is drawn. Only those
		// five commands ever reach it: arcs arrive as béziers, which an affine
		// matrix maps exactly.
		_appendUser(cmds) {
			const m = this._state.transform;
			for (const c of cmds) {
				if (c[0] === "Z") {
					this._path._cmds.push(["Z"]);
					if (this._dstart) this._dcur = this._dstart.slice();
					continue;
				}
				const out = [c[0]];
				for (let i = 1; i + 1 < c.length; i += 2) {
					const [dx, dy] = applyMatrix(m, c[i], c[i + 1]);
					out.push(clampCoord(dx), clampCoord(dy));
				}
				this._path._cmds.push(out);
				this._dcur = [out[out.length - 2], out[out.length - 1]];
				if (c[0] === "M") this._dstart = this._dcur.slice();
			}
		}
		moveTo(x, y) {
			x = +x; y = +y;
			if (!Number.isFinite(x) || !Number.isFinite(y)) return;
			this._appendUser([["M", x, y]]);
		}
		lineTo(x, y) {
			x = +x; y = +y;
			if (!Number.isFinite(x) || !Number.isFinite(y)) return;
			this._appendUser([[this._dcur === null ? "M" : "L", x, y]]);
		}
		closePath() {
			if (this._dcur !== null) this._appendUser([["Z"]]);
		}
		quadraticCurveTo(cpx, cpy, x, y) {
			const a = [+cpx, +cpy, +x, +y];
			if (!a.every(Number.isFinite)) return;
			const cmds = this._dcur === null ? [["M", a[0], a[1]]] : [];
			cmds.push(["Q", ...a]);
			this._appendUser(cmds);
		}
		bezierCurveTo(c1x, c1y, c2x, c2y, x, y) {
			const a = [+c1x, +c1y, +c2x, +c2y, +x, +y];
			if (!a.every(Number.isFinite)) return;
			const cmds = this._dcur === null ? [["M", a[0], a[1]]] : [];
			cmds.push(["C", ...a]);
			this._appendUser(cmds);
		}
		rect(x, y, w, h) {
			const a = [+x, +y, +w, +h];
			if (!a.every(Number.isFinite)) return;
			this._appendUser(rectCommandList(...a));
		}
		roundRect(x, y, w, h, r) {
			const v = normRoundRect(x, y, w, h, r);
			if (v) this._appendUser(roundRectCommandList(v));
		}
		arc(x, y, r, s, e, ccwFlag = false) {
			const a = [+x, +y, +r, +s, +e];
			ccwFlag = Boolean(ccwFlag);
			if (!a.every(Number.isFinite)) return;
			if (a[2] < 0) throw new DOMException("arc: the radius must not be negative", "IndexSizeError");
			this._appendArc(a[0], a[1], a[2], a[2], 0, a[3], a[4], ccwFlag);
		}
		ellipse(x, y, rx, ry, rot, s, e, ccwFlag = false) {
			const a = [+x, +y, +rx, +ry, +rot, +s, +e];
			ccwFlag = Boolean(ccwFlag);
			if (!a.every(Number.isFinite)) return;
			if (a[2] < 0 || a[3] < 0) {
				throw new DOMException("ellipse: the radii must not be negative", "IndexSizeError");
			}
			this._appendArc(...a, ccwFlag);
		}
		_appendArc(x, y, rx, ry, rot, s, e, ccwFlag) {
			const arc = arcBeziers(x, y, rx, ry, rot, s, arcSweep(s, e, ccwFlag));
			const cmds = [[this._dcur === null ? "M" : "L", arc.sx, arc.sy]];
			for (const b of arc.curves) cmds.push(["C", ...b]);
			this._appendUser(cmds);
		}
		arcTo(x1, y1, x2, y2, r) {
			const a = [+x1, +y1, +x2, +y2, +r];
			if (!a.every(Number.isFinite)) return;
			if (a[4] < 0) throw new DOMException("arcTo: the radius must not be negative", "IndexSizeError");
			if (this._dcur === null) { this._appendUser([["M", a[0], a[1]]]); return; }
			// The tangent geometry works from the user-space current point,
			// which is the device-space one pulled back through the transform.
			const inv = invertMatrix(this._state.transform);
			const [x0, y0] = inv ? applyMatrix(inv, this._dcur[0], this._dcur[1]) : [a[0], a[1]];
			this._appendUser(arcToCommands(x0, y0, a[0], a[1], a[2], a[3], a[4]));
		}

		fillRect(x, y, w, h) {
			if (!allFinite(arguments)) return;
			if (+w === 0 || +h === 0) return;
			this._paint(this._rectPolys(+x, +y, +w, +h), false);
		}
		strokeRect(x, y, w, h) {
			if (!allFinite(arguments)) return;
			x = +x; y = +y; w = +w; h = +h;
			// A zero-by-zero rectangle draws nothing; a rectangle that is zero
			// in ONE dimension strokes as a line (the degenerate closed path).
			if (w === 0 && h === 0) return;
			const poly = [x, y, x + w, y, x + w, y + h, x, y + h, x, y];
			poly.closed = true;
			this._paintStroke(this._strokePolys([poly]));
		}
		clearRect(x, y, w, h) {
			if (!allFinite(arguments)) return;
			if (+w === 0 || +h === 0) return;
			const { subpaths, lengths } = packSubpaths(this._rectPolys(+x, +y, +w, +h));
			canvasClear(this._canvas._handle, { subpaths, lengths });
		}
		// _fillPolys is the intended path as device-space polygons. The default
		// path is device-space already; a Path2D argument is user-space and is
		// transformed by the matrix in force NOW — the two halves of "the
		// transformation applies when points are added to the default path".
		_fillPolys(path) {
			if (path) return flatten(path._cmds, this._state.transform);
			return flatten(this._path._cmds, IDENTITY);
		}
		// _userPolys is the intended path as USER-space polylines, which is
		// the space a stroke's geometry lives in: the line width, the dashes
		// and the joins are laid out there and the finished outline is then
		// transformed, so a scale in force at stroke() time scales the width
		// too — even for a default path whose points were baked earlier.
		_userPolys(path) {
			if (path) return flatten(path._cmds, IDENTITY);
			const inv = invertMatrix(this._state.transform);
			return inv ? flatten(this._path._cmds, inv) : [];
		}
		_strokePolys(userPolys) {
			const st = this._state;
			const dashed = dashPolys(userPolys, st.lineDash, st.lineDashOffset);
			const outline = strokeOutline(dashed, st.lineWidth, st.lineCap, st.lineJoin, st.miterLimit);
			const m = st.transform;
			const out = [];
			for (const poly of outline) {
				const mapped = [];
				for (let i = 0; i < poly.length; i += 2) {
					const [dx, dy] = applyMatrix(m, poly[i], poly[i + 1]);
					if (Number.isNaN(dx) || Number.isNaN(dy)) { mapped.length = 0; break; }
					mapped.push(clampCoord(dx), clampCoord(dy));
				}
				if (mapped.length >= 6) out.push(mapped);
			}
			return out;
		}
		fill(a, b) {
			const { path, rule } = fillArgs(a, b);
			this._paint(this._fillPolys(path), rule === "evenodd");
		}
		stroke(a) {
			if (a !== undefined && !(a instanceof Path2D)) {
				throw new TypeError("stroke: a Path2D is required");
			}
			this._paintStroke(this._strokePolys(this._userPolys(a instanceof Path2D ? a : null)));
		}
		clip(a, b) {
			const { path, rule } = fillArgs(a, b);
			const polys = this._fillPolys(path);
			const { subpaths, lengths } = packSubpaths(polys);
			this._state.clip = { subpaths, lengths, evenOdd: rule === "evenodd", prev: this._state.clip };
			canvasClip(this._canvas._handle, { subpaths, lengths, evenOdd: rule === "evenodd", replace: false });
		}
		isPointInPath(a, b, c, d) {
			// Answered from the flattened path with a crossing count, which is the
			// same question the rasterizer answers per pixel. The point is in the
			// canvas coordinate space, unaffected by the current transformation.
			let path = null, x = a, y = b, rule = c;
			if (arguments.length >= 4) {
				if (!(a instanceof Path2D)) throw new TypeError("isPointInPath: a Path2D is required");
				path = a; x = b; y = c; rule = d;
			} else if (a instanceof Path2D) {
				path = a; x = b; y = c; rule = undefined;
			}
			rule = parseFillRule(rule);
			return pointInPolys(this._fillPolys(path), +x, +y, rule === "evenodd");
		}
		isPointInStroke(a, b, c) {
			let path = null, x = a, y = b;
			if (a instanceof Path2D) { path = a; x = b; y = c; }
			const polys = this._strokePolys(this._userPolys(path));
			return pointInPolys(polys, +x, +y, false);
		}

		drawImage(source, ...rest) {
			if (arguments.length < 3) throw new TypeError("drawImage requires at least 3 arguments");
			// A zero-sized CANVAS has no bitmap to draw — that is an error; a
			// zero-sized bitmap of any other kind simply draws nothing.
			if (source instanceof OffscreenCanvas && (source.width === 0 || source.height === 0)) {
				throw new DOMException("drawImage: the source canvas has no pixels", "InvalidStateError");
			}
			const info = sourceSurface(source);
			if (!info) throw new TypeError("drawImage: the source is not an image");
			if (info.width === 0 || info.height === 0) return;
			let sx = 0, sy = 0, sw = info.width, sh = info.height, dx, dy, dw, dh;
			if (rest.length === 2) {
				[dx, dy] = rest.map(Number);
				dw = sw; dh = sh;
			} else if (rest.length === 4) {
				[dx, dy, dw, dh] = rest.map(Number);
			} else if (rest.length === 8) {
				[sx, sy, sw, sh, dx, dy, dw, dh] = rest.map(Number);
			} else {
				throw new TypeError("drawImage: 2, 4 or 8 coordinates are required");
			}
			if (![sx, sy, sw, sh, dx, dy, dw, dh].every(Number.isFinite)) return;
			if (sw === 0 || sh === 0 || dw === 0 || dh === 0) return;
			// Negative dimensions name the same rectangle from its other side
			// and do NOT mirror the image.
			if (sw < 0) { sx += sw; sw = -sw; }
			if (sh < 0) { sy += sh; sh = -sh; }
			if (dw < 0) { dx += dw; dw = -dw; }
			if (dh < 0) { dy += dh; dh = -dh; }
			// The source rectangle must lie inside the image; one that does not is
			// an IndexSizeError rather than a clamp, because the caller named a
			// region that is not there.
			if (rest.length === 8 && (sx < 0 || sy < 0
				|| sx + sw > info.width || sy + sh > info.height)) {
				if (info.temporary) canvasFree(info.handle);
				throw new DOMException("drawImage: the source rectangle is outside the image", "IndexSizeError");
			}
			// forward maps the UNIT SQUARE onto the destination rectangle, through
			// the current transform; the host walks the destination and inverts it.
			const forward = multiply(this._state.transform, [dw, 0, 0, dh, dx, dy]);
			const inverse = invertMatrix(forward);
			if (!inverse) {
				if (info.temporary) canvasFree(info.handle);
				return;
			}
			canvasDrawImage(this._canvas._handle, info.handle, {
				sx, sy, sw, sh,
				forward: new Float64Array(forward),
				inverse: new Float64Array(inverse),
				alpha: this._state.globalAlpha,
				composite: this._state.composite,
				smooth: this._state.imageSmoothingEnabled,
				// An image casts a shadow from its own ALPHA, so a transparent part
				// of it casts none.
				...(this._paintSpec(this._state.fill, null, null).shadow
					? { shadow: this._paintSpec(this._state.fill, null, null).shadow } : {}),
			});
			if (info.temporary) canvasFree(info.handle);
		}

		createPattern(source, repetition) {
			if (arguments.length < 2) throw new TypeError("createPattern requires 2 arguments");
			const rep = repetition === null || repetition === "" ? "repeat" : String(repetition);
			if (!["repeat", "repeat-x", "repeat-y", "no-repeat"].includes(rep)) {
				throw new DOMException(`createPattern: ${rep} is not a repetition`, "SyntaxError");
			}
			const info = sourceSurface(source);
			if (!info) throw new TypeError("createPattern: the source is not an image");
			if (info.width === 0 || info.height === 0) {
				if (info.temporary) canvasFree(info.handle);
				throw new DOMException("createPattern: the source has no pixels", "InvalidStateError");
			}
			// A pattern is a SNAPSHOT: later drawing into the source canvas must
			// not show through it.
			const copy = canvasFromBytes(info.width, info.height,
				new Uint8Array(canvasGetImageData(info.handle, 0, 0, info.width, info.height)));
			if (info.temporary) canvasFree(info.handle);
			return new CanvasPattern(PATTERN_INTERNAL,
				{ handle: copy, width: info.width, height: info.height }, rep);
		}

		// --------------------------------------------------------- pixels
		createImageData(a, b, c) {
			if (arguments.length < 1) throw new TypeError("createImageData requires 1 argument");
			if (arguments.length === 1 || a instanceof ImageData) {
				if (!(a instanceof ImageData)) {
					throw new TypeError("createImageData: an ImageData is required");
				}
				return new ImageData(a.width, a.height,
					{ pixelFormat: a.pixelFormat, colorSpace: a.colorSpace });
			}
			const sw = enforcedLong(a, "createImageData: the width");
			const sh = enforcedLong(b, "createImageData: the height");
			const settings = imageDataSettings(c, "createImageData");
			if (sw === 0 || sh === 0) {
				throw new DOMException("createImageData: the size must not be zero", "IndexSizeError");
			}
			return new ImageData(Math.abs(sw), Math.abs(sh), settings);
		}
		getImageData(x, y, w, h, settings = undefined) {
			if (arguments.length < 4) throw new TypeError("getImageData requires 4 arguments");
			if (this._layers > 0) {
				throw new DOMException("getImageData: the canvas has open layers", "InvalidStateError");
			}
			const sw = enforcedLong(w, "getImageData: the width");
			const sh = enforcedLong(h, "getImageData: the height");
			const sx = enforcedLong(x, "getImageData: x");
			const sy = enforcedLong(y, "getImageData: y");
			const st = imageDataSettings(settings, "getImageData");
			if (sw === 0 || sh === 0) {
				throw new DOMException("getImageData: the size must not be zero", "IndexSizeError");
			}
			const ax = sw < 0 ? sx + sw : sx;
			const ay = sh < 0 ? sy + sh : sy;
			const bytes = canvasGetImageData(this._canvas._handle, ax, ay, Math.abs(sw), Math.abs(sh));
			if (st.pixelFormat === "rgba-float16") {
				const out = new ImageData(Math.abs(sw), Math.abs(sh), st);
				const u8 = new Uint8Array(bytes);
				for (let i = 0; i < u8.length; i++) out.data[i] = u8[i] / 255;
				return out;
			}
			return new ImageData(new Uint8ClampedArray(bytes), Math.abs(sw), Math.abs(sh),
				{ colorSpace: st.colorSpace });
		}
		putImageData(data, dx, dy, sx = undefined, sy = undefined, sw = undefined, sh = undefined) {
			if (!(data instanceof ImageData)) throw new TypeError("putImageData: an ImageData is required");
			if (this._layers > 0) {
				throw new DOMException("putImageData: the canvas has open layers", "InvalidStateError");
			}
			dx = enforcedLong(dx, "putImageData: dx");
			dy = enforcedLong(dy, "putImageData: dy");
			let dirtyX = sx === undefined ? 0 : enforcedLong(sx, "putImageData: dirtyX");
			let dirtyY = sy === undefined ? 0 : enforcedLong(sy, "putImageData: dirtyY");
			let dirtyW = sw === undefined ? data.width : enforcedLong(sw, "putImageData: dirtyWidth");
			let dirtyH = sh === undefined ? data.height : enforcedLong(sh, "putImageData: dirtyHeight");
			// A negative dirty dimension names the rectangle from its other side.
			if (dirtyW < 0) { dirtyX += dirtyW; dirtyW = -dirtyW; }
			if (dirtyH < 0) { dirtyY += dirtyH; dirtyH = -dirtyH; }
			canvasPutImageData(this._canvas._handle, imageDataBytes(data),
				data.width, data.height, dx, dy, dirtyX, dirtyY, dirtyW, dirtyH);
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
		_paint(polys, evenOdd) {
			if (!polys.length) return;
			const { subpaths, lengths } = packSubpaths(polys);
			canvasFill(this._canvas._handle, {
				subpaths, lengths, evenOdd,
				...this._paintSpec(this._state.fill, this._state.fillGradient, this._state.fillPattern),
			});
		}
		_paintStroke(polys) {
			if (!polys.length) return;
			const { subpaths, lengths } = packSubpaths(polys);
			canvasFill(this._canvas._handle, {
				subpaths, lengths, evenOdd: false,
				...this._paintSpec(this._state.stroke, this._state.strokeGradient, this._state.strokePattern),
			});
		}
		_paintSpec(color, grad, pat) {
			const spec = {
				alpha: this._state.globalAlpha,
				composite: this._state.composite,
			};
			// A colorMatrix filter set through a CanvasFilter object is applied
			// by the host to the source colour of each drawn pixel.
			const fo = this._state.filterObject;
			if (fo !== null && fo !== undefined && typeof fo === "object" && fo._prims !== undefined) {
				const prims = (fo._prims !== null && typeof fo._prims === "object" && Symbol.iterator in fo._prims)
					? [...fo._prims] : [fo._prims];
				if (prims.length === 1 && prims[0] && prims[0].name === "colorMatrix") {
					const m = colorMatrixValues(prims[0]);
					if (m) spec.colorMatrix = new Float64Array(m);
				}
			}
			// A shadow needs a colour with some alpha and something to displace it:
			// with no offset and no blur it would land exactly under the shape and
			// never be seen.
			const st = this._state;
			if (st.shadowColor[3] > 0 && (st.shadowOffsetX !== 0 || st.shadowOffsetY !== 0 || st.shadowBlur > 0)) {
				// The offset is in the OUTPUT bitmap's space and the transform does
				// not touch it: a shadow is cast by the light on the page, not by
				// anything in the drawing's own coordinate system.
				spec.shadow = {
					dx: st.shadowOffsetX,
					dy: st.shadowOffsetY,
					blur: st.shadowBlur,
					color: new Float64Array([
						st.shadowColor[0] / 255, st.shadowColor[1] / 255,
						st.shadowColor[2] / 255, st.shadowColor[3],
					]),
				};
			}
			if (pat) {
				// The pattern's own transform sits UNDER the drawing transform: the
				// image is placed by the first and then moved by the second.
				const inverse = invertMatrix(multiply(this._state.transform, pat._transform));
				spec.pattern = {
					handle: pat._info.handle,
					repeatX: pat._repetition === "repeat" || pat._repetition === "repeat-x",
					repeatY: pat._repetition === "repeat" || pat._repetition === "repeat-y",
					inverse: new Float64Array(inverse ?? IDENTITY),
				};
				return spec;
			}
			if (grad) {
				const inverse = invertMatrix(this._state.transform);
				spec.gradient = {
					radial: grad._kind === "radial",
					conic: grad._kind === "conic",
					// A conic gradient's coords are [startAngle, x, y]; the others
					// carry the declaration-order coordinates.
					...(grad._kind === "conic"
						? { angle: grad._coords[0], x0: grad._coords[1], y0: grad._coords[2] }
						: {
							x0: grad._coords[0], y0: grad._coords[1], r0: grad._coords[2],
							x1: grad._coords[3], y1: grad._coords[4], r1: grad._coords[5],
						}),
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
		"lighter", "copy", "xor", "clear",
	]);

	function allFinite(args) {
		for (const a of args) if (!Number.isFinite(Number(a))) return false;
		return true;
	}

	// parseFillRule is the CanvasFillRule enumeration: an argument that is not
	// one of its values is a TypeError, as Web IDL says for operation
	// arguments (an attribute would silently keep its old value instead).
	function parseFillRule(v) {
		if (v === undefined) return "nonzero";
		const rule = String(v);
		if (rule !== "nonzero" && rule !== "evenodd") {
			throw new TypeError(`${rule} is not a fill rule`);
		}
		return rule;
	}

	function fillArgs(a, b) {
		if (a instanceof Path2D) return { path: a, rule: parseFillRule(b) };
		return { path: null, rule: parseFillRule(a) };
	}

	// pointInPolys is the crossing count isPointInPath answers with, over the
	// CLOSED ring of each polygon (a fill closes an open subpath implicitly).
	// A point exactly ON a non-degenerate edge is inside under either rule —
	// the boundary belongs to the path — which a crossing count alone cannot
	// decide consistently. A degenerate polygon (every point the same, which
	// is what a singular transform produces) contains nothing.
	function pointInPolys(polys, x, y, evenOdd) {
		let winding = 0, crossings = 0;
		for (const poly of polys) {
			const n = poly.length / 2;
			for (let i = 0; i < n; i++) {
				const j = (i + 1) % n;
				const x0 = poly[i * 2], y0 = poly[i * 2 + 1];
				const x1 = poly[j * 2], y1 = poly[j * 2 + 1];
				const dx = x1 - x0, dy = y1 - y0;
				const lenSq = dx * dx + dy * dy;
				if (lenSq > 0) {
					const t = Math.max(0, Math.min(1, ((x - x0) * dx + (y - y0) * dy) / lenSq));
					const ex = x - (x0 + t * dx), ey = y - (y0 + t * dy);
					if (ex * ex + ey * ey < 1e-18) return true;
				}
				if ((y0 <= y) === (y1 <= y)) continue;
				const s = (y - y0) / (y1 - y0);
				if (x0 + s * (x1 - x0) <= x) continue;
				crossings++;
				winding += y1 > y0 ? 1 : -1;
			}
		}
		return evenOdd ? crossings % 2 === 1 : winding !== 0;
	}

	// DOMMatrixReadOnly/DOMMatrix (geometry) — the 4x4 matrix getTransform
	// answers with and setTransform accepts. Stored as the 16 column-major
	// components; a 2D matrix is one whose 3D components were never set.
	const M2D = [0, 1, 4, 5, 12, 13]; // where a..f live among m11..m44
	const IDENTITY16 = [1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1];
	class DOMMatrixReadOnly {
		constructor(init = undefined) {
			let m = IDENTITY16.slice(), is2D = true;
			if (init !== undefined) {
				const list = [...init].map(Number);
				if (list.length === 6) {
					for (let i = 0; i < 6; i++) m[M2D[i]] = list[i];
				} else if (list.length === 16) {
					m = list;
					is2D = false;
				} else {
					throw new TypeError("DOMMatrix: 6 or 16 numbers are required");
				}
			}
			Object.defineProperties(this, {
				_m: { value: m, writable: true },
				_is2D: { value: is2D, writable: true },
			});
		}
		get is2D() { return this._is2D; }
		get isIdentity() { return this._m.every((v, i) => v === IDENTITY16[i]); }
		toJSON() {
			const out = {};
			for (const k of ["a", "b", "c", "d", "e", "f", "is2D", "isIdentity"]) out[k] = this[k];
			for (let r = 1; r <= 4; r++) for (let c = 1; c <= 4; c++) out[`m${r}${c}`] = this[`m${r}${c}`];
			return out;
		}
		static fromMatrix(init = {}) {
			return new this(fixup2D(init));
		}
	}
	class DOMMatrix extends DOMMatrixReadOnly {}
	{
		const alias = { a: "m11", b: "m12", c: "m21", d: "m22", e: "m41", f: "m42" };
		const names = [];
		for (let r = 1; r <= 4; r++) for (let c = 1; c <= 4; c++) names.push(`m${r}${c}`);
		names.forEach((name, i) => {
			Object.defineProperty(DOMMatrixReadOnly.prototype, name, {
				get() { return this._m[i]; }, configurable: true,
			});
			Object.defineProperty(DOMMatrix.prototype, name, {
				get() { return this._m[i]; },
				set(v) {
					this._m[i] = Number(v);
					if (IDENTITY16[i] !== this._m[i] && !M2D.includes(i)) this._is2D = false;
				},
				configurable: true,
			});
		});
		for (const [short, long] of Object.entries(alias)) {
			const i = names.indexOf(long);
			Object.defineProperty(DOMMatrixReadOnly.prototype, short, {
				get() { return this._m[i]; }, configurable: true,
			});
			Object.defineProperty(DOMMatrix.prototype, short, {
				get() { return this._m[i]; },
				set(v) { this._m[i] = Number(v); },
				configurable: true,
			});
		}
		for (const cls of [DOMMatrixReadOnly, DOMMatrix]) {
			Object.defineProperty(cls.prototype, Symbol.toStringTag, { value: cls.name, configurable: true });
		}
	}

	// --------------------------------------------------------- the canvas

	class OffscreenCanvas {
		constructor(width, height) {
			if (arguments.length < 2) throw new TypeError("OffscreenCanvas requires 2 arguments");
			const w = enforcedSize(width, "OffscreenCanvas: the width");
			const h = enforcedSize(height, "OffscreenCanvas: the height");
			Object.defineProperties(this, {
				_width: { value: w, writable: true },
				_height: { value: h, writable: true },
				_handle: { value: canvasNew(w, h), writable: true },
				_context: { value: null, writable: true },
			});
		}
		get width() { return this._width; }
		set width(v) {
			const w = enforcedSize(v, "width");
			this._width = w;
			// Resizing RESETS the context — state, stack and path — which is
			// what assigning to width means, even when the size is unchanged.
			if (this._context) this._context.reset();
			else canvasResize(this._handle, w, this._height);
		}
		get height() { return this._height; }
		set height(v) {
			const h = enforcedSize(v, "height");
			this._height = h;
			if (this._context) this._context.reset();
			else canvasResize(this._handle, this._width, h);
		}
		getContext(type, options = undefined) {
			if (arguments.length < 1) throw new TypeError("getContext requires 1 argument");
			const id = String(type);
			// OffscreenRenderingContextId is an ENUM: a name not in it is a
			// TypeError, where a supported name we cannot serve returns null.
			if (!["2d", "bitmaprenderer", "webgl", "webgl2"].includes(id)) {
				throw new TypeError(`getContext: ${id} is not a context id`);
			}
			if (id !== "2d") return null;
			if (!this._context) {
				// The settings dictionary is converted ONCE, at creation, its
				// members read in alphabetical order as Web IDL does.
				if (options !== null && typeof options === "object") {
					// Each member is read exactly ONCE, in alphabetical order.
					void Boolean(options.alpha ?? true);
					const csRaw = options.colorSpace;
					const cs = csRaw === undefined ? "srgb" : String(csRaw);
					if (cs !== "srgb" && cs !== "display-p3") {
						throw new TypeError(`getContext: ${cs} is not a color space`);
					}
					void Boolean(options.desynchronized ?? false);
					void Boolean(options.willReadFrequently ?? false);
				}
				this._context = new OffscreenCanvasRenderingContext2D(CONTEXT_INTERNAL, this);
			}
			return this._context;
		}
		// transferToImageBitmap MOVES the bitmap out: the canvas is left with a
		// fresh transparent one of the same size.
		transferToImageBitmap() {
			if (!this._context) {
				throw new DOMException("transferToImageBitmap: the canvas has no context", "InvalidStateError");
			}
			if (this._context._layers > 0) {
				throw new DOMException("transferToImageBitmap: the canvas has open layers", "InvalidStateError");
			}
			const w = this._width, h = this._height;
			const handle = w > 0 && h > 0
				? canvasFromBytes(w, h, new Uint8Array(canvasGetImageData(this._handle, 0, 0, w, h)))
				: canvasNew(w, h);
			canvasResize(this._handle, w, h);
			return new ImageBitmap(BITMAP_INTERNAL, handle, w, h);
		}
		convertToBlob(options = undefined) {
			try {
				if (this._context && this._context._layers > 0) {
					throw new DOMException("convertToBlob: the canvas has open layers", "InvalidStateError");
				}
				if (this._width === 0 || this._height === 0) {
					throw new DOMException("convertToBlob: the canvas has no pixels", "IndexSizeError");
				}
				let type = "image/png", quality = -1;
				if (options !== null && typeof options === "object") {
					if (options.type !== undefined) type = String(options.type);
					if (options.quality !== undefined) {
						const q = +options.quality;
						if (Number.isFinite(q) && q >= 0 && q <= 1) quality = q;
					}
				}
				const r = canvasEncode(this._handle, type, quality);
				return Promise.resolve(new Blob([r.bytes], { type: r.type }));
			} catch (e) {
				return Promise.reject(e);
			}
		}
	}
	Object.defineProperty(OffscreenCanvas.prototype, Symbol.toStringTag, {
		value: "OffscreenCanvas", configurable: true,
	});

	// enforcedSize is the [EnforceRange] unsigned long long conversion the
	// canvas dimensions go through: junk is a TypeError, not a zero.
	function enforcedSize(v, what) {
		const n = Math.trunc(+v);
		if (!Number.isFinite(n) || n < 0 || n > Number.MAX_SAFE_INTEGER) {
			throw new TypeError(`${what} is out of range`);
		}
		return n === 0 ? 0 : n;
	}

	// DOMPointReadOnly/DOMPoint are the geometry interface roundRect's radii and
	// getTransform's result are described in terms of. Only the 2d part is
	// meaningful here, and z/w are the values the specification defaults them to.
	class DOMPointReadOnly {
		constructor(x = 0, y = 0, z = 0, w = 1) {
			Object.defineProperties(this, {
				_x: { value: Number(x) }, _y: { value: Number(y) },
				_z: { value: Number(z) }, _w: { value: Number(w) },
			});
		}
		get x() { return this._x; }
		get y() { return this._y; }
		get z() { return this._z; }
		get w() { return this._w; }
		toJSON() { return { x: this._x, y: this._y, z: this._z, w: this._w }; }
		static fromPoint(init = {}) {
			return new this(init.x ?? 0, init.y ?? 0, init.z ?? 0, init.w ?? 1);
		}
	}
	class DOMPoint extends DOMPointReadOnly {
		set x(v) { Object.defineProperty(this, "_x", { value: Number(v), configurable: true }); }
		get x() { return this._x; }
		set y(v) { Object.defineProperty(this, "_y", { value: Number(v), configurable: true }); }
		get y() { return this._y; }
		set z(v) { Object.defineProperty(this, "_z", { value: Number(v), configurable: true }); }
		get z() { return this._z; }
		set w(v) { Object.defineProperty(this, "_w", { value: Number(v), configurable: true }); }
		get w() { return this._w; }
	}
	for (const cls of [DOMPointReadOnly, DOMPoint]) {
		Object.defineProperty(cls.prototype, Symbol.toStringTag, { value: cls.name, configurable: true });
		globalThis[cls.name] ??= cls;
	}
	globalThis.DOMMatrixReadOnly ??= DOMMatrixReadOnly;
	globalThis.DOMMatrix ??= DOMMatrix;

	globalThis.CanvasFilter = CanvasFilter;
	globalThis.ImageBitmap = ImageBitmap;
	globalThis.CanvasPattern = CanvasPattern;
	globalThis.OffscreenCanvas = OffscreenCanvas;
	globalThis.OffscreenCanvasRenderingContext2D = OffscreenCanvasRenderingContext2D;
	globalThis.CanvasGradient = CanvasGradient;
	globalThis.Path2D = Path2D;
	globalThis.ImageData = ImageData;
})();
