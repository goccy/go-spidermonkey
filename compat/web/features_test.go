package web_test

// The feature table in features.go is a contract: it says which part of the web
// platform each global belongs to, and compat/nodejs relies on it to take the
// minimum common API without the browser-only surface. A table that merely
// documents the split would drift the first time a global was added, so these
// tests hold it to what the installation actually does.

import (
	"context"
	"strings"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
	"github.com/goccy/go-spidermonkey/compat/web/canvas"
)

// globalNames returns every own property of globalThis, which is what a caller
// can reach — including the non-enumerable ones, where most of the surface is.
func globalNames(t *testing.T, js *spidermonkey.JS) map[string]bool {
	t.Helper()
	r, err := js.Eval(context.Background(), `Object.getOwnPropertyNames(globalThis).join(" ")`)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("threw: %v", r.Error)
	}
	out := map[string]bool{}
	for _, n := range strings.Fields(r.Value.String()) {
		out[n] = true
	}
	return out
}

// installedGlobals is what an installation adds on top of a bare engine.
func installedGlobals(t *testing.T, opts web.Options) map[string]bool {
	t.Helper()
	bare, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer bare.Close()
	before := globalNames(t, bare)

	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	opts.Modules = []web.Module{canvas.Module()}
	w, err := web.InstallWith(js, opts)
	if err != nil {
		t.Fatalf("InstallWith: %v", err)
	}
	defer w.Close()

	added := map[string]bool{}
	for n := range globalNames(t, js) {
		// The "__"-prefixed names are plumbing between the guest and the host, not
		// API, and are deleted or hidden rather than exposed.
		if !before[n] && !strings.HasPrefix(n, "__") {
			added[n] = true
		}
	}
	return added
}

func TestEveryGlobalIsClassified(t *testing.T) {
	classified := map[string]web.Feature{}
	for _, f := range web.AllFeatures() {
		for _, g := range web.FeatureGlobals(f) {
			if other, dup := classified[g]; dup {
				t.Errorf("%s is claimed by both %s and %s", g, other, f)
			}
			classified[g] = f
		}
	}
	// Module-provided features classify through the module's own table —
	// that is where their globals are declared.
	for f, globals := range canvas.Module().Features {
		for _, g := range globals {
			if other, dup := classified[g]; dup {
				t.Errorf("%s is claimed by both %s and %s", g, other, f)
			}
			classified[g] = f
		}
	}
	for _, g := range web.AlwaysInstalledGlobals() {
		classified[g] = "(always)"
	}
	// The global-scope interfaces belong to the Scope, not to any feature, but
	// they are still enumerated — see ScopeGlobals.
	for _, g := range web.ScopeGlobals(web.ScopeWindow) {
		classified[g] = "(scope)"
	}

	for g := range installedGlobals(t, web.Options{}) {
		if _, ok := classified[g]; !ok {
			t.Errorf("the installation defines %q, which no feature claims: add it to "+
				"featureGlobals, or to alwaysInstalled if every feature needs it", g)
		}
	}
}

// TestScopeGlobalsAreInstalled holds the scope enumeration to the installation,
// in both directions: everything a Scope claims must appear, and a worker scope
// must NOT leave the window-only interfaces behind.
func TestScopeGlobalsAreInstalled(t *testing.T) {
	for _, scope := range []web.Scope{web.ScopeWindow, web.ScopeDedicatedWorker,
		web.ScopeSharedWorker, web.ScopeServiceWorker} {
		t.Run(string(scope), func(t *testing.T) {
			got := installedGlobals(t, web.Options{Scope: scope})
			for _, g := range web.ScopeGlobals(scope) {
				if !got[g] {
					t.Errorf("scope %s claims %q and the installation does not define it", scope, g)
				}
			}
			if scope == web.ScopeWindow {
				return
			}
			for _, g := range []string{"Window", "Location", "Navigator"} {
				if got[g] {
					t.Errorf("scope %s defines %q, which belongs to a window", scope, g)
				}
			}
		})
	}
}

// TestFeatureSelectionRemovesTheRest is the property compat/nodejs depends on:
// asking for a subset gets a subset.
func TestFeatureSelectionRemovesTheRest(t *testing.T) {
	got := installedGlobals(t, web.Options{Features: web.MinimumCommonFeatures()})
	for _, g := range web.FeatureGlobals(web.FeatureXMLHttpRequest) {
		if got[g] {
			t.Errorf("%q is present although XMLHttpRequest was not selected", g)
		}
	}
	// The minimum common API is still there in full: this is a subset, not a
	// different surface.
	for _, g := range web.FeatureGlobals(web.FeatureFetch) {
		if !got[g] {
			t.Errorf("%q is missing although fetch was selected", g)
		}
	}
}

// TestFeaturesAreSeparablyAskedFor is the property the fine-grained features
// exist for: where a specification boundary runs through what looks like one
// API, an embedding can take one side without the other.
//
// Each pair below is a boundary that was inside a single feature until it was
// split, and each is checked in the direction that used to be impossible.
func TestFeaturesAreSeparablyAskedFor(t *testing.T) {
	for _, c := range []struct {
		name    string
		take    []web.Feature
		present web.Feature
		absent  web.Feature
	}{
		{"the File API without FormData", []web.Feature{web.FeatureFileAPI}, web.FeatureFileAPI, web.FeatureFormData},
		{"encoding without its streams", []web.Feature{web.FeatureEncoding}, web.FeatureEncoding, web.FeatureEncodingStreams},
		{"encoding without base64", []web.Feature{web.FeatureEncoding}, web.FeatureEncoding, web.FeatureBase64},
		{"aborting without events", []web.Feature{web.FeatureAbort}, web.FeatureAbort, web.FeatureEvents},
		{"messaging without a broadcast bus", []web.Feature{web.FeatureMessaging}, web.FeatureMessaging, web.FeatureBroadcastChannel},
		{"a canvas without ImageBitmap", []web.Feature{web.FeatureCanvas}, web.FeatureCanvas, web.FeatureImageBitmap},
		{"a canvas without the geometry types", []web.Feature{web.FeatureCanvas}, web.FeatureCanvas, web.FeatureGeometry},
	} {
		t.Run(c.name, func(t *testing.T) {
			// A worker scope, because some feature globals are [Exposed=Worker] —
			// FileReaderSync among them — and this is a test about FEATURES, not
			// about which scope exposes what.
			got := installedGlobals(t, web.Options{Features: c.take, Scope: web.ScopeDedicatedWorker})
			for _, g := range web.FeatureGlobals(c.present) {
				if !got[g] {
					t.Errorf("%q is missing although %s was selected", g, c.present)
				}
			}
			for _, g := range web.FeatureGlobals(c.absent) {
				if got[g] {
					t.Errorf("%q is present although %s was not selected", g, c.absent)
				}
			}
		})
	}
}
