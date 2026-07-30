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
	FeatureWorker          Feature = "worker"
)

// featureGlobals maps each feature onto the globals it owns. Every global this
// package installs belongs to exactly one feature, except the ones in
// alwaysInstalled below.
var featureGlobals = map[Feature][]string{
	FeatureConsole:    {"console"},
	FeatureEncoding:   {"TextEncoder", "TextDecoder", "TextEncoderStream", "TextDecoderStream", "atob", "btoa"},
	FeatureURL:        {"URL", "URLSearchParams"},
	FeatureURLPattern: {"URLPattern"},
	FeatureEvents: {
		"Event", "EventTarget", "CustomEvent", "ErrorEvent", "MessageEvent",
		"PromiseRejectionEvent", "AbortController", "AbortSignal",
		"addEventListener", "removeEventListener", "dispatchEvent", "reportError",
	},
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
	FeatureFileAPI:     {"Blob", "File", "FileList", "FileReader", "FileReaderSync", "FormData"},
	FeatureCrypto:      {"crypto", "Crypto", "CryptoKey", "SubtleCrypto"},
	FeaturePerformance: {
		"performance", "Performance", "PerformanceEntry", "PerformanceMark",
		"PerformanceMeasure", "PerformanceObserver", "PerformanceObserverEntryList",
	},
	FeatureTimers:          {"setTimeout", "clearTimeout", "setInterval", "clearInterval", "queueMicrotask"},
	FeatureStructuredClone: {"structuredClone"},
	FeatureMessaging:       {"MessageChannel", "MessagePort", "BroadcastChannel"},
	FeatureXMLHttpRequest:  {"XMLHttpRequest", "XMLHttpRequestEventTarget", "XMLHttpRequestUpload", "ProgressEvent"},
	FeatureWebSocket:       {"WebSocket", "CloseEvent"},
	FeatureEventSource:     {"EventSource"},
	FeatureWebLocks:        {"Lock", "LockManager"},
	FeatureWebAssembly:     {"WebAssembly"},
	FeatureWorker:          {"Worker"},
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

// MinimumCommonFeatures is the surface a non-browser runtime is expected to
// have — the "minimum common API" that Node, Deno, Bun and Cloudflare Workers
// converged on. It is every feature except the browser-only ones, and it is
// what compat/nodejs asks for.
func MinimumCommonFeatures() []Feature {
	var out []Feature
	for _, f := range AllFeatures() {
		if f == FeatureXMLHttpRequest {
			continue
		}
		out = append(out, f)
	}
	return out
}

// Options configures an installation.
type Options struct {
	// Features selects the surface to expose. A nil slice means the whole web
	// platform; naming features that depend on one another is the caller's
	// responsibility (fetch needs streams for a body, for instance).
	Features []Feature
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
func featureSet(features []Feature) map[Feature]bool {
	if features == nil {
		features = AllFeatures()
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
func removeUnselected(js *spidermonkey.JS, features []Feature) error {
	var drop []string
	set := featureSet(features)
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
