package web

// features.go: which parts of the web platform an installation exposes.
//
// This exists because compat/nodejs depends on SOME of this package. Node
// adopted a slice of the web platform — fetch, URL, TextEncoder, the streams,
// WebCrypto — and nothing beyond it, so sharing the implementation is right and
// inheriting the whole surface is not: a Node runtime that also answers to
// XMLHttpRequest is not Node, and a caller has no way to tell which layer put it
// there.
//
// So the surface is named at the granularity of a feature, and a feature is
// defined by the globals it installs. The table below is the contract, and
// TestEveryGlobalIsClassified holds it to it: a global this package installs
// and does not classify fails the build's tests rather than silently joining
// whatever the last caller asked for.

import (
	"context"
	"crypto/x509"
	"fmt"
	"strings"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// Feature is one selectable part of the web platform surface.
type Feature string

const (
	FeatureConsole         Feature = "console"
	FeatureEncoding        Feature = "encoding"
	FeatureURL             Feature = "url"
	FeatureURLPattern      Feature = "urlpattern"
	FeatureEvents          Feature = "events"
	FeatureStreams         Feature = "streams"
	FeatureCompression     Feature = "compression"
	FeatureFetch           Feature = "fetch"
	FeatureFileAPI         Feature = "fileapi"
	FeatureCrypto          Feature = "crypto"
	FeaturePerformance     Feature = "performance"
	FeatureTimers          Feature = "timers"
	FeatureStructuredClone Feature = "structured-clone"
	FeatureMessaging       Feature = "messaging"
	FeatureXMLHttpRequest  Feature = "xmlhttprequest"
	FeatureWebSocket       Feature = "websocket"
	FeatureEventSource     Feature = "eventsource"
	FeatureWebLocks        Feature = "web-locks"
	FeatureWebAssembly     Feature = "webassembly"
	FeatureCanvas          Feature = "canvas"
	FeatureObservable      Feature = "observable"
	FeatureCache           Feature = "cache"
	// The features below exist because a SPEC boundary runs through what used to
	// be one of the features above. Each is separately selectable because each
	// is separately implementable, and an embedding that wants one without the
	// other is asking a reasonable question: FormData is XHR's, not the File
	// API's; atob/btoa are HTML's, not Encoding's; AbortController is DOM's and
	// is what makes anything cancellable, fetch included.
	FeatureAbort            Feature = "abort"
	FeatureFormData         Feature = "formdata"
	FeatureBase64           Feature = "base64"
	FeatureEncodingStreams  Feature = "encoding-streams"
	FeatureBroadcastChannel Feature = "broadcast-channel"
	FeatureGeometry         Feature = "geometry"
	FeatureImageBitmap      Feature = "imagebitmap"
	// Font loading (CSS Font Loading Module) is its own specification: a
	// runtime can render canvas text without letting pages register fonts.
	FeatureFonts Feature = "fonts"
	FeatureWorker           Feature = "worker"
)

// featureGlobals maps each feature onto the globals it owns. Every global this
// package installs belongs to exactly one feature, except the ones in
// alwaysInstalled below.
var featureGlobals = map[Feature][]string{
	FeatureConsole:  {"console"},
	FeatureEncoding: {"TextEncoder", "TextDecoder"},
	// The stream half of Encoding is separate because it needs streams, and an
	// embedding without them can still have the encoder and the decoder.
	FeatureEncodingStreams: {"TextEncoderStream", "TextDecoderStream"},
	// atob/btoa are HTML's base64 utilities. They live next to Encoding in
	// everyone's mental model and in no specification.
	FeatureBase64:     {"atob", "btoa"},
	FeatureURL:        {"URL", "URLSearchParams"},
	FeatureURLPattern: {"URLPattern"},
	FeatureCanvas: {
		"OffscreenCanvas", "OffscreenCanvasRenderingContext2D", "CanvasGradient",
		"Path2D", "ImageData", "CanvasPattern", "CanvasFilter", "TextMetrics",
	},
	// An ImageBitmap is what a canvas DRAWS, not part of the canvas: it is
	// created from a blob or an ImageData and can be handed between agents.
	FeatureImageBitmap: {"ImageBitmap", "createImageBitmap"},
	// The geometry interfaces are their own specification, and a canvas is only
	// one of the things that speaks in them.
	FeatureGeometry: {"DOMPoint", "DOMPointReadOnly", "DOMMatrix", "DOMMatrixReadOnly"},
	FeatureFonts:    {"FontFace", "FontFaceSet", "fonts"},
	// Observable is its own feature rather than part of events, even though it
	// is an event stream: it is not in the Minimum Common API, and a profile
	// that offers the standard's surface must be able to leave it out.
	FeatureObservable: {"Observable", "Subscriber"},
	FeatureEvents: {
		"Event", "EventTarget", "CustomEvent", "ErrorEvent", "MessageEvent",
		"PromiseRejectionEvent",
		"addEventListener", "removeEventListener", "dispatchEvent", "reportError",
	},
	// Aborting is DOM's, and it is what makes anything cancellable — a fetch, a
	// stream pipe, a subscription. It is separable from events because plenty of
	// code wants a signal without ever dispatching one of its own.
	FeatureAbort: {"AbortController", "AbortSignal"},
	FeatureStreams: {
		"ReadableStream", "WritableStream", "TransformStream",
		"ReadableStreamBYOBReader", "ReadableStreamBYOBRequest",
		"ReadableByteStreamController", "ReadableStreamDefaultController",
		"ReadableStreamDefaultReader", "TransformStreamDefaultController",
		"WritableStreamDefaultController", "WritableStreamDefaultWriter",
		"ByteLengthQueuingStrategy", "CountQueuingStrategy",
	},
	FeatureCompression: {"CompressionStream", "DecompressionStream"},
	FeatureFetch:       {"fetch", "Headers", "Request", "Response"},
	// The Cache API speaks fetch's vocabulary but is not part of it: the Minimum
	// Common API does not require it, and the runtimes that have it have it as
	// an addition.
	FeatureCache:   {"caches", "Cache", "CacheStorage"},
	FeatureFileAPI: {"Blob", "File", "FileList", "FileReader", "FileReaderSync"},
	// FormData is XMLHttpRequest's, not the File API's. It is here on its own
	// because fetch needs it for a multipart body and XHR is not wanted.
	FeatureFormData: {"FormData"},
	FeatureCrypto:   {"crypto", "Crypto", "CryptoKey", "SubtleCrypto"},
	FeaturePerformance: {
		"performance", "Performance", "PerformanceEntry", "PerformanceMark",
		"PerformanceMeasure", "PerformanceObserver", "PerformanceObserverEntryList",
	},
	FeatureTimers:          {"setTimeout", "clearTimeout", "setInterval", "clearInterval", "queueMicrotask"},
	FeatureStructuredClone: {"structuredClone"},
	FeatureMessaging:       {"MessageChannel", "MessagePort"},
	// A BroadcastChannel is not a channel between two ports but a bus by name,
	// with its own reach and its own lifetime.
	FeatureBroadcastChannel: {"BroadcastChannel"},
	FeatureXMLHttpRequest:   {"XMLHttpRequest", "XMLHttpRequestEventTarget", "XMLHttpRequestUpload", "ProgressEvent"},
	FeatureWebSocket:        {"WebSocket", "CloseEvent", "WebSocketStream", "WebSocketError"},
	FeatureEventSource:      {"EventSource"},
	FeatureWebLocks:         {"Lock", "LockManager"},
	FeatureWebAssembly:      {"WebAssembly"},
	FeatureWorker:           {"Worker"},
}

// alwaysInstalled are the globals no feature owns because everything needs
// them: DOMException is how every other feature reports a failure, and the rest
// are properties of the global SCOPE rather than of any one API — ECMA-429
// requires them of a runtime, not of a feature selection, so removing a feature
// must never remove them.
var alwaysInstalled = []string{
	"DOMException", "QuotaExceededError", "self", "navigator", "isSecureContext",
	"onerror", "onunhandledrejection", "onrejectionhandled",
}

// scopeGlobals are the global-scope interfaces installed for each Scope. They
// belong to no feature — a worker does not have "the worker feature", it has a
// different global — but they are enumerated here so that every global this
// package installs is accounted for somewhere. See js/scope.js.
var scopeGlobals = map[Scope][]string{
	ScopeWindow:          {"Window", "Location", "Navigator"},
	ScopeDedicatedWorker: {"WorkerGlobalScope", "DedicatedWorkerGlobalScope", "WorkerLocation", "WorkerNavigator", "close", "postMessage", "onmessage", "onmessageerror", "onconnect", "onlanguagechange", "onoffline", "ononline"},
	ScopeSharedWorker:    {"WorkerGlobalScope", "SharedWorkerGlobalScope", "WorkerLocation", "WorkerNavigator", "close", "onmessage", "onmessageerror", "onconnect", "onlanguagechange", "onoffline", "ononline"},
	ScopeServiceWorker:   {"WorkerGlobalScope", "ServiceWorkerGlobalScope", "WorkerLocation", "WorkerNavigator", "close", "onmessage", "onmessageerror", "onconnect", "onlanguagechange", "onoffline", "ononline"},
}

// ScopeGlobals reports the global-scope interfaces a Scope installs, so the
// package's own tests can hold the enumeration to what the installation does.
func ScopeGlobals(s Scope) []string {
	if s == "" {
		s = ScopeWindow
	}
	return append([]string(nil), scopeGlobals[s]...)
}

// AllFeatures is the whole web platform surface this package implements.
func AllFeatures() []Feature {
	out := make([]Feature, 0, len(featureGlobals))
	for f := range featureGlobals {
		out = append(out, f)
	}
	return out
}

// Profile is a named level of the platform, for an embedding that wants "the
// surface Deno and Bun have" without having to know which features add up to
// it — and, just as importantly, without silently gaining the ones that do not.
//
// The gap is real and it grows: this package implements a good deal that no
// server-side runtime exposes (a canvas, Observable, XMLHttpRequest, a Cache),
// and an embedding that took everything would be claiming a surface its users
// cannot rely on being there anywhere else.
type Profile string

const (
	// ProfileMinimumCommon is ECMA-429, the Minimum Common API: exactly what a
	// non-browser runtime is REQUIRED to have, and nothing else. This is the
	// level to ask for when the goal is parity with Deno, Bun or Workers.
	ProfileMinimumCommon Profile = "minimum-common"
	// ProfileServerRuntime is the Minimum Common API plus what those runtimes
	// have all converged on beyond it and application code genuinely expects:
	// WebSocket, EventSource, Web Locks and the Cache API travel with fetch.
	ProfileServerRuntime Profile = "server-runtime"
	// ProfileFull is everything this package implements, including the parts of
	// the platform no server-side runtime exposes. It is the default, because an
	// embedding that names nothing is asking for what is here.
	ProfileFull Profile = "full"
)

// minimumCommonFeatures are the features ECMA-429 requires, named rather than
// derived: the standard is a list of interfaces, and which of this package's
// features carry them is a fact about the standard, not about what happens to
// be implemented here. compat/web/ecma429_test.go holds this to the interface
// list it is drawn from.
var minimumCommonFeatures = []Feature{
	FeatureConsole,
	FeatureEncoding,
	FeatureEncodingStreams,
	FeatureBase64,
	FeatureAbort,
	FeatureFormData,
	FeatureURL,
	FeatureURLPattern,
	FeatureEvents,
	FeatureStreams,
	FeatureCompression,
	FeatureFetch,
	FeatureFileAPI,
	FeatureCrypto,
	FeaturePerformance,
	FeatureTimers,
	FeatureStructuredClone,
	FeatureMessaging,
	FeatureWebAssembly,
}

// serverRuntimeExtras are what the runtimes add on top of the standard. Each is
// here because all of Deno, Bun and Workers have it, not because it is
// specified as required.
var serverRuntimeExtras = []Feature{
	FeatureWebSocket,
	FeatureEventSource,
	FeatureWebLocks,
	FeatureWorker,
	FeatureCache,
	FeatureBroadcastChannel,
	FeatureImageBitmap,
}

// MinimumCommonFeatures is the surface a non-browser runtime is expected to
// have — the "minimum common API" that Node, Deno, Bun and Cloudflare Workers
// converged on, and what compat/nodejs asks for.
func MinimumCommonFeatures() []Feature {
	return append([]Feature(nil), minimumCommonFeatures...)
}

// FeaturesFor resolves a profile into the features it names. An unknown profile
// resolves to the full surface, which is what naming nothing does.
func FeaturesFor(p Profile) []Feature {
	switch p {
	case ProfileMinimumCommon:
		return MinimumCommonFeatures()
	case ProfileServerRuntime:
		return append(MinimumCommonFeatures(), serverRuntimeExtras...)
	default:
		return AllFeatures()
	}
}

// Options configures an installation.
type Options struct {
	// Features selects the surface to expose. A nil slice defers to Profile;
	// naming features that depend on one another is the caller's responsibility
	// (fetch needs streams for a body, for instance).
	Features []Feature
	// Profile names a LEVEL of the platform when Features is nil — the surface a
	// server-side runtime is expected to have, rather than a list the caller has
	// to keep in step with this package. Empty means ProfileFull.
	Profile Profile
	// RootCAs, when set, are certificate authorities the guest's TLS connections
	// trust IN ADDITION to the system pool — for an origin behind a private CA, or
	// a test server that mints its own certificate. Nil means the system pool
	// alone, which is what a guest should normally get.
	RootCAs *x509.CertPool
	// Scope says what KIND of global this is, which decides the global-scope
	// interfaces installed: Window and Location, or WorkerGlobalScope with its
	// subtype, WorkerNavigator and WorkerLocation. It is not a feature selection
	// — a worker does not have "fewer features", it has a different global — so
	// it is a separate field. Empty means ScopeWindow.
	Scope Scope
	// Location, when set, is the URL `location` reports. A runtime has one only
	// because an embedding gave it one: there is no document here to supply it.
	// Empty leaves `location` undefined, which is what a bare embedding has.
	Location string
	// HardwareConcurrency is what navigator.hardwareConcurrency reports. Zero
	// means 1: one interpreter is one thread, and claiming more parallelism than
	// the embedding can deliver is worse than under-reporting it.
	HardwareConcurrency int
}

// Scope is the kind of global an installation creates.
type Scope string

const (
	ScopeWindow          Scope = "window"
	ScopeDedicatedWorker Scope = "dedicatedworker"
	ScopeSharedWorker    Scope = "sharedworker"
	ScopeServiceWorker   Scope = "serviceworker"
)

// featureSet turns a selection into a lookup, treating nil as everything.
func featureSet(features []Feature, profile Profile) map[Feature]bool {
	if features == nil {
		features = FeaturesFor(profile)
	}
	set := make(map[Feature]bool, len(features))
	for _, f := range features {
		set[f] = true
	}
	return set
}

// removeUnselected deletes the globals of every feature the caller did not ask
// for. Installing and then removing — rather than not installing — is what
// keeps the feature table the single statement of what belongs to what: the JS
// is one surface, and splitting it per feature would put the same list in two
// places and let them drift.
func removeUnselected(js *spidermonkey.JS, features []Feature, profile Profile) error {
	var drop []string
	set := featureSet(features, profile)
	for f, globals := range featureGlobals {
		if set[f] {
			continue
		}
		drop = append(drop, globals...)
	}
	if len(drop) == 0 {
		return nil
	}
	var b strings.Builder
	for _, name := range drop {
		fmt.Fprintf(&b, "delete globalThis[%s];\n", jsLiteral(name))
	}
	r, err := js.Eval(context.Background(), b.String())
	if err != nil {
		return fmt.Errorf("web: removing unselected features: %w", err)
	}
	if r.Error != nil {
		return fmt.Errorf("web: removing unselected features: %w", r.Error)
	}
	return nil
}

// FeatureGlobals reports the globals a feature owns. It is exported so the
// package's own tests — and a caller that wants to check a surface — can hold
// the table to what the installation does rather than to what it says.
func FeatureGlobals(f Feature) []string {
	return append([]string(nil), featureGlobals[f]...)
}

// AlwaysInstalledGlobals reports the globals no feature owns.
func AlwaysInstalledGlobals() []string {
	return append([]string(nil), alwaysInstalled...)
}
