// Package canvas is the OffscreenCanvas slice of the web platform, as an
// opt-in module for compat/web.
//
// It is a separate package for one reason: weight. Canvas pulls in the image
// codecs (png, jpeg, gif, webp), the rasterizer, and ten embedded TrueType
// faces for text — megabytes of code and data an embedding that never draws
// should not carry. Importing this package and passing Module() to
// web.InstallWith is the opt-in; a build that does neither links none of it.
//
//	web.InstallWith(js, web.Options{
//		Modules: []web.Module{canvas.Module()},
//	})
//
// The module provides the features "canvas" (OffscreenCanvas and the 2d
// context), "imagebitmap" (ImageBitmap/createImageBitmap), "geometry"
// (DOMPoint, DOMMatrix) and "fonts" (FontFace, FontFaceSet) — selectable
// individually through web.Options.Features like every built-in feature.
package canvas

import (
	_ "embed"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

//go:embed canvas.js
var canvasJS string

// Module is what an embedding passes to web.Options.Modules to opt in.
func Module() web.Module {
	return web.Module{
		Features: map[web.Feature][]string{
			web.FeatureCanvas: {
				"OffscreenCanvas", "OffscreenCanvasRenderingContext2D", "CanvasGradient",
				"Path2D", "ImageData", "CanvasPattern", "CanvasFilter", "TextMetrics",
			},
			web.FeatureImageBitmap: {"ImageBitmap", "createImageBitmap"},
			web.FeatureGeometry:    {"DOMPoint", "DOMPointReadOnly", "DOMMatrix", "DOMMatrixReadOnly"},
			web.FeatureFonts:       {"FontFace", "FontFaceSet", "fonts"},
		},
		Script: canvasJS,
		Ops: func(js *spidermonkey.JS) (map[string]spidermonkey.Func, func(), error) {
			a := newCanvasAPI(js)
			return a.ops(), a.closeAll, nil
		},
	}
}
