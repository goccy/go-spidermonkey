package internal

// SourceIsModule: "is this an ES module?" answered by the engine's parser.
//
// The negatives are the point. A source-text match for import/export is
// line-anchored (so a minified one-line bundle reads as CommonJS) and blind to
// comments and string literals (so the word `export` inside either reads as a
// module). Compiling as a module AND as CommonJS isolates exactly the
// constructs that can appear in only one of them.

import "testing"

func TestSourceIsModule(t *testing.T) {
	js, _ := newJS(t)

	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		{"import declaration", `import x from "y"; x()`, true},
		{"export declaration", `export const a = 1`, true},
		{"default export", `export default 1`, true},
		{"import.meta", `const u = import.meta.url`, true},
		{"top-level await", `const r = await Promise.resolve(1)`, true},
		{"re-export", `export { a } from "./b.js"`, true},
		{"minified one-liner", `;export default 1;`, true},
		{"shebang then export", "#!/usr/bin/env node\nexport default 1", true},

		{"plain script", `const a = 1; a + 1`, false},
		{"commonjs exports", `module.exports = { a: 1 }`, false},
		{"commonjs require", `const fs = require("fs"); exports.fs = fs`, false},
		{"top-level return", `if (x) return; module.exports = 1`, false},
		{"dynamic import", `const p = import("./x.js")`, false},
		{"export in a comment", "// export default 1\nmodule.exports = 1", false},
		{"export in a string", `const s = "export default 1"`, false},
		{"shebang then commonjs", "#!/usr/bin/env node\nmodule.exports = 1", false},
		{"empty", ``, false},
		{"unparsable", `this ( is ) not = javascript ][`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := js.SourceIsModule(tc.src)
			if err != nil {
				t.Fatalf("SourceIsModule: %v", err)
			}
			if got != tc.want {
				t.Fatalf("SourceIsModule(%q) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

// Classifying must not disturb the runtime: nothing is registered, evaluated
// or left pending, not even for a source that fails to compile both ways.
func TestSourceIsModuleDoesNotEvaluate(t *testing.T) {
	js, _ := newJS(t)

	if _, err := js.Eval(`globalThis.marker = 0`); err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if _, err := js.SourceIsModule(`globalThis.marker = 1; export {}`); err != nil {
		t.Fatalf("SourceIsModule: %v", err)
	}
	if _, err := js.SourceIsModule(`this ( is ) not = javascript ][`); err != nil {
		t.Fatalf("SourceIsModule on broken source: %v", err)
	}
	raw, err := js.Eval(`globalThis.marker`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	r := decodeEnvelope(t, raw)
	if !r.Ok {
		t.Fatalf("runtime unusable after classification: %s", r.Error)
	}
	if !contains(r.Result, `"v":0`) {
		t.Fatalf("classification evaluated the source: marker = %s", r.Result)
	}
}
