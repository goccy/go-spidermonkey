# The DOM module: compat/web/dom

## Why a module, and what it buys

`document is not defined` is the single largest failure bucket in the WPT run:
251 test files stop there today. Beyond them sits the real prize: the runner
currently executes only the script forms (`.any.js`, `.worker.js`,
`.window.js`), and the materialized directories alone contain **15,743
testharness-based `.html` files** — almost four times the 4,022 cases run
today — every one of which needs a document to load into.

The DOM lands as an **opt-in module** (`compat/web/dom`), exactly like
`compat/web/canvas` and for the same reason: weight. The HTML parser and the
selector engine are real dependencies an embedding that never touches a
document should not link. `web.InstallWith(js, web.Options{Modules:
[]web.Module{dom.Module()}})` is the opt-in; a build without the import
carries none of it.

## Two stages

**Stage 1 — the module.** A living DOM tree good enough for the
document-touching `.window.js` tests that do not need a frame tree: of the
251 blocked files, 145 revolve around `<iframe>`/`contentWindow` (they need
browsing contexts, which stay TODO), leaving ~106 unblocked by this stage,
plus every partially-failing file whose remaining subtests only need
`document.createElement` and friends.

**Stage 2 — running `.html` tests.** With a parser and a tree, the WPT runner
can load a `.html` testharness file the way a browser does: parse it, execute
its `<script>` elements in order (inline and `src`), fire
`DOMContentLoaded`/`load`, and collect results through the harness hook it
already has. This is where the whole-suite coverage curve bends; it is a
runner change, not an API change, and it lands separately on top of Stage 1.

## What is Go and what is JS

Following the repository rule — the algorithm in Go, the object surface in
JS:

- **HTML parsing is `golang.org/x/net/html`** — the whole HTML5 parsing
  algorithm, tokenizer states, foster parenting, foreign content and all,
  already correct in Go. The host op `dom_parse_html(text, contextTag)`
  returns the parse as a structural tree (nested arrays); `innerHTML =`,
  `DOMParser.parseFromString(text, "text/html")` and Stage 2's document
  loading are all the same op with different contexts.
- **Selector parsing is Go.** The Selectors grammar is a specification-defined
  text format — exactly the legitimate kind of parsing — and the parser lives
  host-side (`dom_parse_selector(text)`), returning an AST. **Matching** the
  AST is tree-walking against live JS objects, so the matcher is JS.
- **The tree itself is JS.** Nodes hold live references — expando properties,
  event listeners, element identity across mutations — which is precisely
  what the module rule says belongs in the guest. Mutation algorithms
  (insert/remove/adopt), attribute maps, token lists, and event propagation
  along the ancestor chain are all guest code operating on guest state.
- **Serialization (innerHTML's getter) is JS**: the fragment serialization
  algorithm is a tree walk with a five-entry escape table and the void-element
  list; round-tripping the live tree through Go would cost more than the
  algorithm.

## Surface

One feature, `"dom"`, owning the tree interfaces:

Node, Document, DocumentFragment, DocumentType, CharacterData, Text, Comment,
Element, HTMLElement, HTMLUnknownElement (plus concrete subclasses only where
behaviour demands them — HTMLAnchorElement carries the HyperlinkElementUtils
URL accessors backed by the URL parser we already have), Attr, NamedNodeMap,
HTMLCollection, NodeList, DOMTokenList, DOMImplementation, and the `document`
instance. A second feature, `"dom-parsing"`, owns DOMParser.

Everything here is `[Exposed=Window]`: scope.js strips the lot from worker
scopes, the same way it strips Web Storage.

Node extends the EventTarget the platform already has; the module teaches
dispatchEvent propagation — capture, target, bubble — by building the path
from the parent chain, using the Event internals builtins.js already
maintains.

`document` is the environment's document: its URL tracks `location`, its
initial tree is the `about:blank` shape (`<html><head></head><body></body>`),
and `defaultView` is the global.

## Deliberately TODO (not out of scope)

Per the project's direction — rendering-adjacent surface is deferred, never
declared unsupported:

- **Browsing contexts**: `iframe.contentWindow`, `window.open`, navigation,
  and everything that needs more than one window. This is the other 145
  files.
- **Layout**: offsetWidth and friends; there is no renderer.
- **CSSOM**: `element.style` ships as a small property bag that serializes to
  the `style` attribute; the full CSSStyleDeclaration surface and
  `getComputedStyle` wait for a CSS module.
- **document.open/write**: the reentrant parser dance is its own project.
