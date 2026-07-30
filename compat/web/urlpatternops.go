package web

// urlpatternops.go: the parts of URLPattern that surround the pattern syntax —
// canonicalizing the literal text of each component, splitting a constructor
// string into components, and the ops the guest calls.
//
// Canonicalization reuses the URL parser's state overrides (see urlparse.go),
// which is what the standard says to do: a hostname in a pattern is put through
// the host parser, a port through the port parser, and so on, so that
// "EXAMPLE.com" and "example.com" are the same pattern and ":80" disappears
// from an http one. Doing it any other way means two notions of what a hostname
// is.

import (
	"encoding/json"
	"fmt"
	"strings"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// patternComponents is every component of a URLPattern, in the order the
// standard compares them.
var patternComponents = []string{
	"protocol", "username", "password", "hostname", "port", "pathname", "search", "hash",
}

// canonicalize runs one component's literal text through the URL parser, using
// the state the standard names for it. An empty string is always itself.
func canonicalize(component, value, protocol string, special bool) (string, error) {
	if value == "" {
		return "", nil
	}
	switch component {
	case "protocol":
		// A full parse of "<value>://dummy.test", not a state override: the scheme
		// state's override refuses to move a URL between the special and
		// non-special worlds, and canonicalizing "http" from nothing has to.
		u, err := parseURL(value+"://dummy.test", nil, nil, 0, false)
		if err != nil {
			return "", err
		}
		return u.scheme, nil
	case "username":
		return pctEncode(value, userinfoSet), nil
	case "password":
		return pctEncode(value, userinfoSet), nil
	case "hostname":
		// A hostname is a DOMAIN, so the dummy URL is given a special scheme: that
		// is what puts the text through IDNA, turning "caf\u00e9.com" into
		// "xn--caf-dma.com" rather than percent-escaping it as an opaque host.
		u, err := parseURL(value, nil, &urlRecord{scheme: "https"}, stHostname, true)
		if err != nil {
			return "", err
		}
		return u.host, nil
	case "ipv6hostname":
		// An IPv6 hostname pattern is only lower-cased and checked, never parsed:
		// the address it describes is not complete until the groups in it have
		// matched, so there is nothing to parse yet.
		var b strings.Builder
		for _, r := range value {
			if !isASCIIHexDigit(r) && r != '[' && r != ']' && r != ':' {
				return "", fmt.Errorf("%q is not valid in an IPv6 hostname", r)
			}
			b.WriteRune(toLowerASCII(r))
		}
		return b.String(), nil
	case "port":
		// No scheme on the dummy URL: with one, the port state would drop a port
		// that happens to be that scheme's default, and a pattern of "80" for
		// protocol "http" means the port 80 and must say so.
		u, err := parseURL(value, nil, &urlRecord{}, stPort, true)
		if err != nil {
			return "", err
		}
		return u.port, nil
	case "pathname":
		// Only a special scheme has a path of segments; anything else has an
		// opaque path, which escapes far less — "var x = 1;" keeps its spaces
		// there and would become "var%20x%20=%201;" as a segment path. Whether the
		// protocol is special is decided by the caller, because a protocol PATTERN
		// counts as special when it matches any special scheme: "(https|javascript)"
		// does and "(data|javascript)" does not.
		if !special {
			u, err := parseURL(value, nil, &urlRecord{scheme: protocol, opaque: true}, stOpaquePath, true)
			if err != nil {
				return "", err
			}
			return u.serializePath(), nil
		}
		// A pathname is canonicalized in the context of a URL, so a value that
		// does not start with "/" is given one and has it removed again — the
		// alternative is running the path state on something it cannot represent.
		leading := strings.HasPrefix(value, "/")
		in := value
		if !leading {
			in = "/-" + value
		}
		rec := &urlRecord{scheme: protocol, host: "dummy", hasHost: true}
		if rec.scheme == "" {
			rec.scheme = "https"
		}
		u, err := parseURL(in, nil, rec, stPathStart, true)
		if err != nil {
			return "", err
		}
		out := u.serializePath()
		if !leading {
			out = strings.TrimPrefix(out, "/-")
		}
		return out, nil
	case "search":
		u, err := parseURL(strings.TrimPrefix(value, "?"), nil, &urlRecord{scheme: protocol}, stQuery, true)
		if err != nil {
			return "", err
		}
		if u.query == nil {
			return "", nil
		}
		return *u.query, nil
	case "hash":
		u, err := parseURL(strings.TrimPrefix(value, "#"), nil, &urlRecord{scheme: protocol}, stFragment, true)
		if err != nil {
			return "", err
		}
		if u.fragment == nil {
			return "", nil
		}
		return *u.fragment, nil
	}
	return value, nil
}

// encoderFor returns the callback the pattern parser applies to literal text.
// A component whose pattern contains a regular expression group is NOT
// canonicalized: the text around it may be a fragment of something that is only
// valid once the group has matched.
func encoderFor(component, protocol string, special bool) func(string) (string, error) {
	return func(s string) (string, error) {
		out, err := canonicalize(component, s, protocol, special)
		if err != nil {
			return "", fmt.Errorf("%s: %w", component, err)
		}
		return out, nil
	}
}

// stripInitDelimiter removes the delimiter a component's value may carry from
// having been written as part of a URL. It happens before the pattern is parsed:
// the ":" of "http{s}?:" is not part of the pattern, and leaving it there makes
// the tokenizer read a nameless ":".
func stripInitDelimiter(component, value string) string {
	switch component {
	case "protocol":
		return strings.TrimSuffix(value, ":")
	case "search":
		return strings.TrimPrefix(value, "?")
	case "hash":
		return strings.TrimPrefix(value, "#")
	}
	return value
}

func isASCIIHexDigit(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

// ------------------------------------------------ constructor string

// ctorState is the state machine that splits a constructor string such as
// "https://example.com:8080/foo?q#h" into components. It is a second state
// machine because a pattern may contain the very characters that delimit
// components — "{" groups and "(" regular expressions have to be skipped over,
// which is why this cannot be a matter of searching for ":" and "/".
type ctorState int

const (
	csInit ctorState = iota
	csProtocol
	csAuthority
	csUsername
	csPassword
	csHostname
	csPort
	csPathname
	csSearch
	csHash
	csDone
)

type ctorParser struct {
	src        []rune
	tokens     []token
	i          int // token index
	state      ctorState
	groupDepth int
	hostIPv6   int // bracket depth inside a hostname
	start      int // token index where the current component began
	out        map[string]string
	protocol   string
}

// parseConstructorString runs the standard's constructor-string parser. Only the
// components it actually sees are set; the rest are left absent so the caller
// can fill them in from a base URL or leave them as wildcards.
func parseConstructorString(input string) (map[string]string, error) {
	toks, err := tokenize(input, false)
	if err != nil {
		return nil, err
	}
	p := &ctorParser{src: []rune(input), tokens: toks, out: map[string]string{}}
	for ; p.i < len(p.tokens); p.i++ {
		t := p.tokens[p.i]
		if t.kind == tokOpen {
			p.groupDepth++
			continue
		}
		if p.groupDepth > 0 {
			if t.kind == tokClose {
				p.groupDepth--
			}
			continue
		}
		switch p.state {
		case csInit:
			// Init only looks for the ":" that ends a protocol, one token at a
			// time. Finding it rewinds to re-read the protocol; reaching the end
			// without it means the whole string is a relative pattern, which is
			// re-read as a pathname.
			if p.isNonSpecialPatternChar(p.i, ":") {
				p.rewindAndSet(csProtocol)
				continue
			}
			if t.kind == tokEnd {
				p.rewindAndSet(csPathname)
				continue
			}
		case csProtocol:
			if p.isNonSpecialPatternChar(p.i, ":") {
				p.set("protocol")
				if p.nextIsAuthoritySlashes() {
					p.i += 2
					p.state, p.start = csAuthority, p.i+1
				} else {
					p.state, p.start = csPathname, p.i+1
				}
			}
		case csAuthority:
			if p.isNonSpecialPatternChar(p.i, "@") {
				p.rewindAndSet(csUsername)
				continue
			}
			if p.isNonSpecialPatternChar(p.i, "/") || p.isSearchPrefix(p.i) ||
				p.isNonSpecialPatternChar(p.i, "#") || t.kind == tokEnd {
				p.rewindAndSet(csHostname)
				continue
			}
		case csUsername:
			if p.isNonSpecialPatternChar(p.i, ":") {
				p.set("username")
				p.state, p.start = csPassword, p.i+1
				continue
			}
			if p.isNonSpecialPatternChar(p.i, "@") {
				p.set("username")
				p.state, p.start = csHostname, p.i+1
			}
		case csPassword:
			if p.isNonSpecialPatternChar(p.i, "@") {
				p.set("password")
				p.state, p.start = csHostname, p.i+1
			}
		case csHostname:
			switch {
			case p.isNonSpecialPatternChar(p.i, "["):
				p.hostIPv6++
			case p.isNonSpecialPatternChar(p.i, "]"):
				p.hostIPv6--
			case p.hostIPv6 == 0 && p.isNonSpecialPatternChar(p.i, ":"):
				p.set("hostname")
				p.state, p.start = csPort, p.i+1
			case p.hostIPv6 == 0 && (p.isNonSpecialPatternChar(p.i, "/") ||
				p.isSearchPrefix(p.i) || p.isNonSpecialPatternChar(p.i, "#") || t.kind == tokEnd):
				p.set("hostname")
				p.startComponentAt(p.i)
			}
		case csPort:
			if p.isNonSpecialPatternChar(p.i, "/") || p.isSearchPrefix(p.i) ||
				p.isNonSpecialPatternChar(p.i, "#") || t.kind == tokEnd {
				p.set("port")
				p.startComponentAt(p.i)
			}
		case csPathname:
			if p.isSearchPrefix(p.i) || p.isNonSpecialPatternChar(p.i, "#") || t.kind == tokEnd {
				p.set("pathname")
				p.startComponentAt(p.i)
			}
		case csSearch:
			if p.isNonSpecialPatternChar(p.i, "#") || t.kind == tokEnd {
				p.set("search")
				p.startComponentAt(p.i)
			}
		case csHash:
			if t.kind == tokEnd {
				p.set("hash")
				p.state = csDone
			}
		}
		if p.state == csDone {
			break
		}
	}
	// A RELATIVE constructor string that names no pathname must leave it unset,
	// so the base URL's path is inherited. The parser reaches the pathname state
	// by falling through rather than by seeing a delimiter, so "#baz" produces an
	// empty pathname the caller never wrote — and an empty one that IS written
	// gets resolved against the base's directory, which turns "/foo" into "/".
	// There is no way to write an empty pathname in a relative string, so an
	// empty one here always means "absent".
	if _, hasProtocol := p.out["protocol"]; !hasProtocol {
		if v, ok := p.out["pathname"]; ok && v == "" {
			delete(p.out, "pathname")
		}
	}
	return p.out, nil
}


// isNonSpecialPatternChar reports whether token i is the given literal
// character, not escaped and not part of a group or regular expression.
func (p *ctorParser) isNonSpecialPatternChar(i int, want string) bool {
	t := p.tokens[i]
	if t.value != want {
		return false
	}
	// An escaped character is literal text, never a delimiter: "\\:" in a pattern
	// is a colon to match, not the end of a component.
	return t.kind == tokChar || t.kind == tokInvalidChar
}

// isSearchPrefix decides whether the "?" at i starts the query. The tokenizer
// classes every "?" as a modifier, because that is what it is almost everywhere
// — so the question is answered by looking BACK: a "?" that follows a name, a
// regular expression, a closing brace or another modifier belongs to that group,
// and anything else means the query begins here.
func (p *ctorParser) isSearchPrefix(i int) bool {
	if p.tokens[i].value != "?" {
		return false
	}
	if p.tokens[i].kind == tokChar || p.tokens[i].kind == tokInvalidChar {
		return true
	}
	if i == 0 {
		return true
	}
	switch p.tokens[i-1].kind {
	case tokName, tokRegexp, tokClose, tokOtherModifier, tokAsterisk:
		return false
	}
	return true
}

func (p *ctorParser) nextIsAuthoritySlashes() bool {
	return p.i+2 < len(p.tokens) &&
		p.tokens[p.i+1].kind == tokChar && p.tokens[p.i+1].value == "/" &&
		p.tokens[p.i+2].kind == tokChar && p.tokens[p.i+2].value == "/"
}

// set records the component that ends just before the current token.
func (p *ctorParser) set(component string) {
	from, to := p.tokens[p.start].index, p.tokens[p.i].index
	if p.start >= len(p.tokens) || from > to {
		p.out[component] = ""
		return
	}
	p.out[component] = string(p.src[from:to])
	if component == "protocol" {
		p.protocol = p.out[component]
	}
}

// startComponentAt moves to whichever component the delimiter at i introduces.
// Leaving the authority for the path or beyond fixes the port at empty: a URL
// string that names a host and no port is saying there is no port, where an
// unmentioned component would mean any port at all.
func (p *ctorParser) startComponentAt(i int) {
	if _, ok := p.out["port"]; !ok && p.state == csHostname {
		p.out["port"] = ""
	}
	switch p.tokens[i].value {
	case "?":
		p.state, p.start = csSearch, i+1
	case "#":
		p.state, p.start = csHash, i+1
	case "/":
		p.state, p.start = csPathname, i
	default:
		p.state = csDone
	}
	if p.tokens[i].kind == tokEnd {
		p.state = csDone
	}
}

// rewindAndSet restarts scanning the current component in a new state.
func (p *ctorParser) rewindAndSet(state ctorState) {
	p.i = p.start - 1
	p.state = state
}

// -------------------------------------------------------------------- ops

// opPatternCompile(component, pattern, protocol, protocolIsSpecial) -> the compiled component: its
// canonical pattern string, the regular-expression source the guest compiles,
// the group names in order, and whether the pattern contains a regular
// expression at all (which the interface reports as hasRegExpGroups).
func (w *Web) opPatternCompile(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("pattern compile: (component, pattern, protocol) required")
	}
	component, pattern, protocol := args[0].String(), strArg(args[1]), args[2].String()
	special := len(args) > 3 && args[3].Bool()
	pattern = stripInitDelimiter(component, pattern)
	opts := componentOptions(component)
	// An IPv6 hostname is canonicalized by a different rule from a domain, and
	// the bracket is what says which.
	encComponent := component
	if component == "hostname" && strings.HasPrefix(pattern, "[") {
		encComponent = "ipv6hostname"
	}
	parts, err := parsePattern(pattern, opts, encoderFor(encComponent, protocol, special))
	if err != nil {
		return spidermonkey.ValueOf(map[string]any{
			"__patternError": true, "message": err.Error(),
		}), nil
	}
	source, names := partsToRegexp(parts, opts)
	hasRegexp := false
	for _, p := range parts {
		if p.kind == partRegexp {
			hasRegexp = true
		}
	}
	nameList := make([]any, len(names))
	for i, n := range names {
		nameList[i] = n
	}
	return spidermonkey.ValueOf(map[string]any{
		"pattern":   partsToPatternString(parts, opts),
		"regexp":    source,
		"names":     nameList,
		"hasRegexp": hasRegexp,
	}), nil
}

// opPatternFromString(input) -> the components a constructor string names. A
// component the string does not mention is absent from the result.
func (w *Web) opPatternFromString(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("pattern from string: (input) required")
	}
	out, err := parseConstructorString(strArg(args[0]))
	if err != nil {
		return spidermonkey.ValueOf(map[string]any{
			"__patternError": true, "message": err.Error(),
		}), nil
	}
	m := make(map[string]any, len(out))
	for k, v := range out {
		m[k] = v
	}
	return spidermonkey.ValueOf(m), nil
}

// ------------------------------------------------- compare and generate

// Part ordering. The ranks are ascending, so a full wildcard — the least
// specific thing a pattern can say — sorts first and literal text last.
func typeRank(k partType) int {
	switch k {
	case partFullWildcard:
		return 0
	case partSegmentWildcard:
		return 1
	case partRegexp:
		return 2
	}
	return 3
}

func modRank(m modifier) int {
	switch m {
	case modZeroOrMore:
		return 0
	case modOptional:
		return 1
	case modOneOrMore:
		return 2
	}
	return 3
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// comparePart orders two parts, most restrictive last. compareComponent is not
// in the URL Pattern Standard — it is a tentative API whose only definition is
// Chromium's implementation — so this follows that: the tuple
// (type, modifier, prefix, value, suffix), compared in exactly that order.
//
// Type ascends full-wildcard < segment-wildcard < regexp < fixed-text, because
// literal text is the most restrictive thing a pattern can say and an inline
// regular expression is more likely to constrain a match than to duplicate a
// wildcard. Modifier ascends zero-or-more < optional < one-or-more < none, for
// the same reason: requiring the group to be there restricts more than making it
// optional. A part's NAME is not compared — ":a" and ":b" match the same things.
func comparePart(l, r part) int {
	if c := cmpInt(typeRank(l.kind), typeRank(r.kind)); c != 0 {
		return c
	}
	if c := cmpInt(modRank(l.mod), modRank(r.mod)); c != 0 {
		return c
	}
	if c := cmpString(l.prefix, r.prefix); c != 0 {
		return c
	}
	if c := cmpString(l.value, r.value); c != 0 {
		return c
	}
	return cmpString(l.suffix, r.suffix)
}

// comparePatterns orders two component patterns by comparing their part lists
// pairwise. When one list runs out, its next part is taken to be empty literal
// text, which is what makes "/foo/" sort after — as more restrictive than —
// "/foo/*".
func comparePatterns(component, left, right string) (int, error) {
	if left == right {
		return 0, nil
	}
	opts := componentOptions(component)
	lp, err := parsePattern(left, opts, nil)
	if err != nil {
		return 0, err
	}
	rp, err := parsePattern(right, opts, nil)
	if err != nil {
		return 0, err
	}
	empty := part{kind: partFixedText}
	for i := 0; i < len(lp) || i < len(rp); i++ {
		l, r := empty, empty
		if i < len(lp) {
			l = lp[i]
		}
		if i < len(rp) {
			r = rp[i]
		}
		if c := comparePart(l, r); c != 0 {
			return c, nil
		}
	}
	return 0, nil
}

// opPatternCompare(component, leftPattern, rightPattern) -> -1, 0 or 1.
func (w *Web) opPatternCompare(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("pattern compare: (component, left, right) required")
	}
	c, err := comparePatterns(args[0].String(), strArg(args[1]), strArg(args[2]))
	if err != nil {
		return spidermonkey.ValueOf(map[string]any{"__patternError": true, "message": err.Error()}), nil
	}
	return spidermonkey.ValueOf(c), nil
}

// generateComponent fills a pattern in from group values. Only a pattern that
// says exactly one thing can be generated: a wildcard, an inline regular
// expression, or any modifier means the pattern describes a SET of components
// and there is no one answer to give.
func generateComponent(component, pattern, protocol string, special bool, groups map[string]string) (string, error) {
	opts := componentOptions(component)
	// The pattern is already canonical, so its literal text needs no further
	// encoding here; the whole result is canonicalized once at the end.
	parts, err := parsePattern(pattern, opts, nil)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, p := range parts {
		if p.mod != modNone {
			return "", fmt.Errorf("a repeated or optional group cannot be generated")
		}
		switch p.kind {
		case partFixedText:
			b.WriteString(p.value)
		case partSegmentWildcard:
			v, ok := groups[p.name]
			if !ok {
				return "", fmt.Errorf("no value given for group %q", p.name)
			}
			// The value has to be something the group could have matched: a
			// segment cannot contain the delimiter that ends it.
			if opts.delimiter != "" && strings.Contains(v, opts.delimiter) {
				return "", fmt.Errorf("group %q cannot contain %q", p.name, opts.delimiter)
			}
			b.WriteString(p.prefix)
			b.WriteString(v)
			b.WriteString(p.suffix)
		default:
			return "", fmt.Errorf("a wildcard or regular expression cannot be generated")
		}
	}
	enc := component
	if component == "hostname" && strings.HasPrefix(pattern, "[") {
		enc = "ipv6hostname"
	}
	return canonicalize(enc, b.String(), protocol, special)
}

// opPatternGenerate(component, pattern, protocol, special, groupsJSON) -> the
// generated component, or a failure the guest turns into a TypeError.
func (w *Web) opPatternGenerate(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 5 {
		return nil, fmt.Errorf("pattern generate: (component, pattern, protocol, special, groups) required")
	}
	groups := map[string]string{}
	if err := json.Unmarshal([]byte(strArg(args[4])), &groups); err != nil {
		return spidermonkey.ValueOf(map[string]any{"__patternError": true, "message": err.Error()}), nil
	}
	out, err := generateComponent(args[0].String(), strArg(args[1]), args[2].String(), args[3].Bool(), groups)
	if err != nil {
		return spidermonkey.ValueOf(map[string]any{"__patternError": true, "message": err.Error()}), nil
	}
	return spidermonkey.ValueOf(out), nil
}

// --------------------------------------------------- init processing

// patternInit is a URLPatternInit as it arrives from the guest. Each component
// is a pointer so that "absent" and "present but empty" stay distinct — the
// whole cascade below turns on which members exist.
type patternInit struct {
	Protocol *string `json:"protocol"`
	Username *string `json:"username"`
	Password *string `json:"password"`
	Hostname *string `json:"hostname"`
	Port     *string `json:"port"`
	Pathname *string `json:"pathname"`
	Search   *string `json:"search"`
	Hash     *string `json:"hash"`
	BaseURL  *string `json:"baseURL"`
}

// processInit resolves a URLPatternInit against its base URL. The rule is a
// cascade: a base URL contributes a component only when the init names nothing
// more specific than it. Once the init names a hostname, for instance, the base
// URL's port and path are no longer about the same resource and are not
// inherited.
//
// Inherited text is escaped as a pattern, because it is literal: a base path of
// "/a:b" must match "/a:b" and not introduce a group named "b".
func processInit(in patternInit) (map[string]string, error) {
	out := map[string]string{}
	set := func(k string, v *string) {
		if v != nil {
			out[k] = *v
		}
	}
	for k, v := range map[string]*string{
		"protocol": in.Protocol, "username": in.Username, "password": in.Password,
		"hostname": in.Hostname, "port": in.Port, "pathname": in.Pathname,
		"search": in.Search, "hash": in.Hash,
	} {
		set(k, v)
	}
	if in.BaseURL == nil {
		return out, nil
	}
	base, err := parseURL(*in.BaseURL, nil, nil, 0, false)
	if err != nil {
		return nil, fmt.Errorf("invalid baseURL %q", *in.BaseURL)
	}
	// none reports whether the INIT named any of the given components. It has to
	// be the init and not the accumulating result: each step writes into the
	// result, so asking the result would mean the first inherited component made
	// every later step believe the caller had named it.
	named := map[string]bool{
		"protocol": in.Protocol != nil, "hostname": in.Hostname != nil,
		"port": in.Port != nil, "pathname": in.Pathname != nil,
		"search": in.Search != nil, "hash": in.Hash != nil,
	}
	none := func(names ...string) bool {
		for _, n := range names {
			if named[n] {
				return false
			}
		}
		return true
	}
	if none("protocol") {
		out["protocol"] = escapePattern(base.scheme)
	}
	if none("protocol", "hostname") {
		out["hostname"] = escapePattern(base.host)
	}
	if none("protocol", "hostname", "port") {
		out["port"] = escapePattern(base.port)
	}
	if none("protocol", "hostname", "port", "pathname") {
		out["pathname"] = escapePattern(base.serializePath())
	} else if in.Pathname != nil && !strings.HasPrefix(*in.Pathname, "/") {
		// A relative pathname is resolved against the base's directory, the way a
		// relative URL is. Without this, "b" against ".../a" is the pattern "b",
		// which matches nothing the caller meant.
		basePath := escapePattern(base.serializePath())
		if i := strings.LastIndex(basePath, "/"); i >= 0 {
			out["pathname"] = basePath[:i+1] + *in.Pathname
		}
	}
	if none("protocol", "hostname", "port", "pathname", "search") && base.query != nil {
		out["search"] = escapePattern(*base.query)
	}
	if none("protocol", "hostname", "port", "pathname", "search", "hash") && base.fragment != nil {
		out["hash"] = escapePattern(*base.fragment)
	}
	return out, nil
}

// opPatternProcessInit(initJSON) -> the resolved components. A component the
// init does not name and the base URL does not contribute is absent, which the
// guest reads as "matches anything".
func (w *Web) opPatternProcessInit(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("pattern process init: (init) required")
	}
	var in patternInit
	if err := json.Unmarshal([]byte(strArg(args[0])), &in); err != nil {
		return spidermonkey.ValueOf(map[string]any{"__patternError": true, "message": err.Error()}), nil
	}
	out, err := processInit(in)
	if err != nil {
		return spidermonkey.ValueOf(map[string]any{"__patternError": true, "message": err.Error()}), nil
	}
	m := make(map[string]any, len(out))
	for k, v := range out {
		m[k] = v
	}
	return spidermonkey.ValueOf(m), nil
}

// opPatternCanonicalize(component, value, protocol, special) -> the component as
// the URL parser would write it. exec() and test() need this for an object
// input: a pattern's literal text is canonicalized when the pattern is compiled,
// so an input that is not canonicalized the same way cannot match it — a hash of
// "café" has to become "caf%C3%A9" on both sides.
func (w *Web) opPatternCanonicalize(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("pattern canonicalize: (component, value, protocol, special) required")
	}
	component := args[0].String()
	value := stripInitDelimiter(component, strArg(args[1]))
	if component == "hostname" && strings.HasPrefix(value, "[") {
		component = "ipv6hostname"
	}
	out, err := canonicalize(component, value, args[2].String(), args[3].Bool())
	if err != nil {
		return spidermonkey.ValueOf(map[string]any{"__patternError": true, "message": err.Error()}), nil
	}
	return spidermonkey.ValueOf(out), nil
}
