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
			if p.isNonSpecialPatternChar(p.i, "/") || p.isNonSpecialPatternChar(p.i, "?") ||
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
				p.isNonSpecialPatternChar(p.i, "?") || p.isNonSpecialPatternChar(p.i, "#") || t.kind == tokEnd):
				p.set("hostname")
				p.startComponentAt(p.i)
			}
		case csPort:
			if p.isNonSpecialPatternChar(p.i, "/") || p.isNonSpecialPatternChar(p.i, "?") ||
				p.isNonSpecialPatternChar(p.i, "#") || t.kind == tokEnd {
				p.set("port")
				p.startComponentAt(p.i)
			}
		case csPathname:
			if p.isNonSpecialPatternChar(p.i, "?") || p.isNonSpecialPatternChar(p.i, "#") || t.kind == tokEnd {
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
