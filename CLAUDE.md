# Project rules

## Implement in Go first; JavaScript is the last resort

The compat layers (`compat/web`, `compat/nodejs`, `compat/cfworkers`) are a
thin JavaScript surface over host functions written in Go. When a feature
needs implementing, the order of preference is:

1. **The Go standard library.** `net/url`, `net/netip`, `mime`, `crypto/*`,
   `encoding/*`, `archive/*`, `time` — use them.
2. **`golang.org/x/...`.** These are quasi-standard: `x/net/idna`,
   `x/net/html`, `x/text/encoding`, `x/crypto/...`. Add them freely, and
   prefer them over writing the same algorithm again. A new `golang.org/x`
   requirement needs no deliberation.
3. **Other well-established Go libraries**, when the standard library and
   `golang.org/x` have no answer.
4. **Go written here**, for a specification with no library behind it.
5. **JavaScript**, only when the feature cannot live host-side at all.

JavaScript is a last resort because it is the layer where correctness is
hardest to test in isolation, where a spec algorithm ends up approximated
(see the string-matching rule in the global CLAUDE.md), and where the same
work has to be redone that a Go package already does correctly.

What legitimately belongs in JavaScript: the shape of the API surface
(classes, getters/setters, brand checks, argument coercion, iterators),
promise and microtask plumbing, and anything that must hold live JS object
references. The ALGORITHM behind it belongs in Go.

Concretely, before writing a JS implementation of anything non-trivial,
check for a Go package that already does it. "There is a `golang.org/x`
package for this" is a reason to add the dependency, not a reason to
hand-roll it in the guest.
