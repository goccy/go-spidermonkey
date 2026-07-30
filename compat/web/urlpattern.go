package web

// urlpattern.go: the pattern syntax of the URLPattern standard — its tokenizer,
// its parser, and the two things it generates from a parsed pattern: the regular
// expression that does the matching, and the canonical pattern string that a
// URLPattern reports back for each component.
//
// The canonical string is not cosmetic. "{:foo}bar" and ":foobar" are different
// patterns, "/" and "*" match different things, and a URLPattern is required to
// say which one it holds — so the serializer has to be the inverse of the
// parser, not an echo of the input. Reporting the input back is what made the
// suite's compiled-property assertions fail in a dozen different shapes.
//
// The regular expression itself is built as a source string and compiled by the
// guest: the standard defines matching in terms of a JavaScript regular
// expression, and its own syntax is embedded in patterns as "(...)" groups, so
// the engine that runs it has to be the JavaScript one.

import (
	"fmt"
	"strconv"
	"strings"
)

// ------------------------------------------------------------------ tokens

type tokenType int

const (
	tokOpen tokenType = iota
	tokClose
	tokRegexp
	tokName
	tokChar
	tokEscapedChar
	tokOtherModifier // "?" or "+" where it is not a modifier
	tokAsterisk
	tokEnd
	tokInvalidChar
)

type token struct {
	kind  tokenType
	index int
	value string
}

// isValidNameCodePoint follows the standard, which defers to the identifier
// code points of ECMAScript. The ASCII subset is what patterns use in practice
// and is spelled out; everything above it is accepted, matching the guest's
// identifier rules rather than duplicating Unicode ID tables here.
func isValidNameCodePoint(r rune, first bool) bool {
	if first {
		return r == '$' || r == '_' || isASCIIAlpha(r) || r > 0x7f
	}
	return r == '$' || r == '_' || isASCIIAlnum(r) || r > 0x7f
}

// tokenizeStrict reports the first syntax error it finds. The standard also has
// a lenient policy, used only while parsing a constructor string, where an
// error becomes an invalid-char token instead.
func tokenize(input string, strict bool) ([]token, error) {
	src := []rune(input)
	var out []token
	add := func(kind tokenType, index int, value string) {
		out = append(out, token{kind, index, value})
	}
	fail := func(i int, msg string) error {
		if strict {
			return fmt.Errorf("%s at index %d", msg, i)
		}
		add(tokInvalidChar, i, string(src[i]))
		return nil
	}

	for i := 0; i < len(src); {
		switch c := src[i]; c {
		case '*':
			add(tokAsterisk, i, "*")
			i++
		case '+', '?':
			add(tokOtherModifier, i, string(c))
			i++
		case '\\':
			if i == len(src)-1 {
				if err := fail(i, "trailing escape"); err != nil {
					return nil, err
				}
				i++
				continue
			}
			add(tokEscapedChar, i, string(src[i+1]))
			i += 2
		case '{':
			add(tokOpen, i, "{")
			i++
		case '}':
			add(tokClose, i, "}")
			i++
		case ':':
			j := i + 1
			for j < len(src) && isValidNameCodePoint(src[j], j == i+1) {
				j++
			}
			if j == i+1 {
				if err := fail(i, "missing pattern name"); err != nil {
					return nil, err
				}
				i++
				continue
			}
			add(tokName, i, string(src[i+1:j]))
			i = j
		case '(':
			depth, j := 1, i+1
			bad := ""
			for j < len(src) {
				r := src[j]
				if r > 0x7f {
					bad = "non-ASCII character in regular expression"
					break
				}
				if j == i+1 && r == '?' {
					bad = "regular expression cannot start with '?'"
					break
				}
				if r == '\\' {
					if j == len(src)-1 {
						bad = "trailing escape in regular expression"
						break
					}
					j += 2
					continue
				}
				if r == ')' {
					depth--
					if depth == 0 {
						j++
						break
					}
				} else if r == '(' {
					depth++
					if j == len(src)-1 {
						bad = "unbalanced '(' in regular expression"
						break
					}
					if src[j+1] != '?' {
						bad = "capturing group in regular expression"
						break
					}
				}
				j++
			}
			if bad == "" && depth != 0 {
				bad = "unbalanced parenthesis"
			}
			if bad == "" && j-i-2 == 0 {
				bad = "empty regular expression"
			}
			if bad != "" {
				if err := fail(i, bad); err != nil {
					return nil, err
				}
				i++
				continue
			}
			add(tokRegexp, i, string(src[i+1:j-1]))
			i = j
		default:
			add(tokChar, i, string(c))
			i++
		}
	}
	add(tokEnd, len(src), "")
	return out, nil
}

// -------------------------------------------------------------------- parts

type partType int

const (
	partFixedText partType = iota
	partRegexp
	partSegmentWildcard
	partFullWildcard
)

type modifier int

const (
	modNone modifier = iota
	modOptional
	modZeroOrMore
	modOneOrMore
)

type part struct {
	kind   partType
	value  string
	mod    modifier
	name   string
	prefix string
	suffix string
}

// patternOptions are the per-component knobs the standard defines: which code
// point separates segments, which one may be absorbed as a prefix, and whether
// matching ignores case.
type patternOptions struct {
	delimiter  string
	prefix     string
	ignoreCase bool
}

// componentOptions is the table of those knobs. Only hostname and pathname have
// structure; the rest are flat strings.
func componentOptions(component string) patternOptions {
	switch component {
	case "hostname":
		return patternOptions{delimiter: "."}
	case "pathname":
		return patternOptions{delimiter: "/", prefix: "/"}
	}
	return patternOptions{}
}

// escapeRegexp escapes a string for literal use inside a JavaScript regular
// expression.
func escapeRegexp(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(".+*?^${}()[]|/\\", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// escapePattern escapes a string for literal use inside a PATTERN string, which
// is a different set from the regular-expression one.
func escapePattern(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("+*?:{}()\\", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func segmentWildcardRegexp(o patternOptions) string {
	return "[^" + escapeRegexp(o.delimiter) + "]+?"
}

// patternParser walks the token list.
type patternParser struct {
	tokens   []token
	i        int
	pending  strings.Builder
	parts    []part
	opts     patternOptions
	nextName int
	encode   func(string) (string, error)
}

func (p *patternParser) tryConsume(kinds ...tokenType) *token {
	t := &p.tokens[p.i]
	for _, k := range kinds {
		if t.kind == k {
			p.i++
			return t
		}
	}
	return nil
}

func (p *patternParser) mustConsume(kind tokenType) error {
	if p.tryConsume(kind) == nil {
		return fmt.Errorf("unexpected token at index %d", p.tokens[p.i].index)
	}
	return nil
}

// consumeText gathers the run of literal characters that forms a prefix or a
// suffix inside a "{...}" group.
func (p *patternParser) consumeText() string {
	var b strings.Builder
	for {
		t := p.tryConsume(tokChar, tokEscapedChar)
		if t == nil {
			return b.String()
		}
		b.WriteString(t.value)
	}
}

// flushPending turns the accumulated literal text into a fixed-text part.
func (p *patternParser) flushPending() error {
	if p.pending.Len() == 0 {
		return nil
	}
	encoded, err := p.encode(p.pending.String())
	if err != nil {
		return err
	}
	p.pending.Reset()
	p.parts = append(p.parts, part{kind: partFixedText, value: encoded, mod: modNone})
	return nil
}

func modifierOf(t *token) modifier {
	if t == nil {
		return modNone
	}
	switch t.value {
	case "?":
		return modOptional
	case "*":
		return modZeroOrMore
	case "+":
		return modOneOrMore
	}
	return modNone
}

// addPart is the standard's "add a part" step: it decides the part's type, name
// and value from the tokens consumed, and encodes the literal pieces.
func (p *patternParser) addPart(prefix string, nameTok, regexpTok *token, suffix string, modTok *token) error {
	mod := modifierOf(modTok)
	// A group with neither a name nor a pattern is just literal text, and can be
	// merged into whatever text is pending — unless it carries a modifier, which
	// applies to the group rather than to the characters.
	if nameTok == nil && regexpTok == nil && mod == modNone {
		p.pending.WriteString(prefix)
		return nil
	}
	if err := p.flushPending(); err != nil {
		return err
	}
	if nameTok == nil && regexpTok == nil {
		if prefix == "" {
			return nil
		}
		encoded, err := p.encode(prefix)
		if err != nil {
			return err
		}
		p.parts = append(p.parts, part{kind: partFixedText, value: encoded, mod: mod})
		return nil
	}

	kind, value := partSegmentWildcard, ""
	if regexpTok != nil {
		switch {
		case regexpTok.kind == tokAsterisk:
			kind = partFullWildcard
		default:
			kind, value = partRegexp, regexpTok.value
			// A pattern that says exactly what a segment wildcard says IS one, and
			// a full wildcard likewise: the standard normalizes them so the
			// canonical string comes out as ":name" or "*".
			if value == segmentWildcardRegexp(p.opts) {
				kind, value = partSegmentWildcard, ""
			} else if value == ".*" {
				kind, value = partFullWildcard, ""
			}
		}
	}
	name := ""
	if nameTok != nil {
		name = nameTok.value
	} else if regexpTok != nil || kind != partSegmentWildcard {
		name = strconv.Itoa(p.nextName)
		p.nextName++
	} else {
		name = strconv.Itoa(p.nextName)
		p.nextName++
	}
	if name == "" {
		return fmt.Errorf("a pattern group needs a name")
	}
	for _, existing := range p.parts {
		if existing.name == name {
			return fmt.Errorf("duplicate pattern name %q", name)
		}
	}
	encPrefix, err := p.encode(prefix)
	if err != nil {
		return err
	}
	encSuffix, err := p.encode(suffix)
	if err != nil {
		return err
	}
	p.parts = append(p.parts, part{
		kind: kind, value: value, mod: mod, name: name,
		prefix: encPrefix, suffix: encSuffix,
	})
	return nil
}

// parsePattern is the standard's pattern parser. encode canonicalizes literal
// text for the component being parsed (a hostname's text through the host
// parser, and so on); it is what makes "EXAMPLE.com" and "example.com" the same
// pattern.
func parsePattern(input string, opts patternOptions, encode func(string) (string, error)) ([]part, error) {
	if encode == nil {
		encode = func(s string) (string, error) { return s, nil }
	}
	toks, err := tokenize(input, true)
	if err != nil {
		return nil, err
	}
	p := &patternParser{tokens: toks, opts: opts, encode: encode}

	for p.tokens[p.i].kind != tokEnd {
		charTok := p.tryConsume(tokChar)
		nameTok := p.tryConsume(tokName)
		// An asterisk is a WILDCARD only when there is no name: with one, it is
		// the zero-or-more modifier on that name.
		regexpTok := p.tryConsume(tokRegexp)
		if regexpTok == nil && nameTok == nil {
			regexpTok = p.tryConsume(tokAsterisk)
		}
		if nameTok != nil || regexpTok != nil {
			prefix := ""
			if charTok != nil {
				prefix = charTok.value
			}
			// Only the component's own prefix code point is absorbed as a prefix;
			// any other character stays literal text.
			if prefix != "" && prefix != p.opts.prefix {
				p.pending.WriteString(prefix)
				prefix = ""
			}
			if err := p.flushPending(); err != nil {
				return nil, err
			}
			modTok := p.tryConsume(tokOtherModifier, tokAsterisk)
			if err := p.addPart(prefix, nameTok, regexpTok, "", modTok); err != nil {
				return nil, err
			}
			continue
		}
		fixed := charTok
		if fixed == nil {
			fixed = p.tryConsume(tokEscapedChar)
		}
		if fixed != nil {
			p.pending.WriteString(fixed.value)
			continue
		}
		if p.tryConsume(tokOpen) != nil {
			prefix := p.consumeText()
			nameTok := p.tryConsume(tokName)
			regexpTok := p.tryConsume(tokRegexp)
			if regexpTok == nil && nameTok == nil {
				regexpTok = p.tryConsume(tokAsterisk)
			}
			suffix := p.consumeText()
			if err := p.mustConsume(tokClose); err != nil {
				return nil, err
			}
			modTok := p.tryConsume(tokOtherModifier, tokAsterisk)
			if err := p.addPart(prefix, nameTok, regexpTok, suffix, modTok); err != nil {
				return nil, err
			}
			continue
		}
		if err := p.flushPending(); err != nil {
			return nil, err
		}
		if err := p.mustConsume(tokEnd); err != nil {
			return nil, err
		}
	}
	if err := p.flushPending(); err != nil {
		return nil, err
	}
	return p.parts, nil
}

// ------------------------------------------------- regular expression

func modSuffix(m modifier) string {
	switch m {
	case modOptional:
		return "?"
	case modZeroOrMore:
		return "*"
	case modOneOrMore:
		return "+"
	}
	return ""
}

// partsToRegexp builds the match expression and the group names that go with it.
func partsToRegexp(parts []part, opts patternOptions) (string, []string) {
	var b strings.Builder
	var names []string
	b.WriteByte('^')
	for _, p := range parts {
		if p.kind == partFixedText {
			if p.mod == modNone {
				b.WriteString(escapeRegexp(p.value))
				continue
			}
			b.WriteString("(?:")
			b.WriteString(escapeRegexp(p.value))
			b.WriteString(")")
			b.WriteString(modSuffix(p.mod))
			continue
		}
		names = append(names, p.name)
		var inner string
		switch p.kind {
		case partSegmentWildcard:
			inner = segmentWildcardRegexp(opts)
		case partFullWildcard:
			inner = ".*"
		default:
			inner = p.value
		}
		if p.prefix == "" && p.suffix == "" {
			if p.mod == modNone || p.mod == modOptional {
				b.WriteString("(" + inner + ")" + modSuffix(p.mod))
			} else {
				b.WriteString("((?:" + inner + ")" + modSuffix(p.mod) + ")")
			}
			continue
		}
		if p.mod == modNone || p.mod == modOptional {
			b.WriteString("(?:" + escapeRegexp(p.prefix) + "(" + inner + ")" + escapeRegexp(p.suffix) + ")" + modSuffix(p.mod))
			continue
		}
		// A repeated group with affixes repeats the affixes too, so the first
		// iteration is written separately from the rest.
		b.WriteString("(?:" + escapeRegexp(p.prefix))
		b.WriteString("((?:" + inner + ")(?:")
		b.WriteString(escapeRegexp(p.suffix) + escapeRegexp(p.prefix))
		b.WriteString("(?:" + inner + "))*)" + escapeRegexp(p.suffix) + ")")
		if p.mod == modZeroOrMore {
			b.WriteString("?")
		}
	}
	b.WriteByte('$')
	return b.String(), names
}

// ----------------------------------------------------- pattern string

// partsToPatternString is the inverse of the parser: the canonical spelling of
// the pattern the parts describe. The grouping rules are the standard's, in its
// order, and each one exists because without it the result would parse back as
// a DIFFERENT pattern — ":foo" before the text "bar" reads as the name "foobar",
// and "*" after a literal "/" reads as one part with a prefix rather than two.
func partsToPatternString(parts []part, opts patternOptions) string {
	var b strings.Builder
	for i := range parts {
		p := parts[i]
		var prev, next *part
		if i > 0 {
			prev = &parts[i-1]
		}
		if i+1 < len(parts) {
			next = &parts[i+1]
		}
		if p.kind == partFixedText {
			if p.mod == modNone {
				b.WriteString(escapePattern(p.value))
				continue
			}
			b.WriteString("{" + escapePattern(p.value) + "}" + modSuffix(p.mod))
			continue
		}
		// A name the parser generated is a number; one the pattern gave is not.
		customName := !startsWithASCIIDigit(p.name)
		needsGrouping := p.suffix != "" || (p.prefix != "" && p.prefix != opts.prefix)

		if !needsGrouping && customName && p.kind == partSegmentWildcard && p.mod == modNone &&
			next != nil && next.prefix == "" && next.suffix == "" {
			if next.kind == partFixedText {
				needsGrouping = isValidNameCodePoint(firstRune(next.value), false)
			} else {
				needsGrouping = startsWithASCIIDigit(next.name)
			}
		}
		if !needsGrouping && p.prefix == "" && prev != nil && prev.kind == partFixedText &&
			lastRuneString(prev.value) == opts.prefix && opts.prefix != "" {
			needsGrouping = true
		}

		if needsGrouping {
			b.WriteString("{")
		}
		b.WriteString(escapePattern(p.prefix))
		if customName {
			b.WriteString(":" + p.name)
		}
		switch p.kind {
		case partRegexp:
			b.WriteString("(" + p.value + ")")
		case partSegmentWildcard:
			if !customName {
				b.WriteString("(" + segmentWildcardRegexp(opts) + ")")
			}
		case partFullWildcard:
			// "*" is only unambiguous where it cannot be read as belonging to what
			// came before; everywhere else the wildcard is written out.
			if !customName && (prev == nil || prev.kind == partFixedText || prev.mod != modNone ||
				needsGrouping || p.prefix != "") {
				b.WriteString("*")
			} else {
				b.WriteString("(.*)")
			}
		}
		// A suffix that begins with a name code point would be read as more of the
		// name, so it is separated by an escape: "{:foo\bar}" is the name "foo"
		// with the suffix "bar", while "{:foobar}" is a name nobody wrote.
		if p.kind == partSegmentWildcard && customName &&
			isValidNameCodePoint(firstRune(p.suffix), false) {
			b.WriteString("\\")
		}
		b.WriteString(escapePattern(p.suffix))
		if needsGrouping {
			b.WriteString("}")
		}
		b.WriteString(modSuffix(p.mod))
	}
	return b.String()
}

func startsWithASCIIDigit(s string) bool {
	r := firstRune(s)
	return r >= '0' && r <= '9'
}

func lastRune(s string) rune {
	last := rune(-1)
	for _, r := range s {
		last = r
	}
	return last
}

func lastRuneString(s string) string {
	if r := lastRune(s); r >= 0 {
		return string(r)
	}
	return ""
}

func isNumericName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return -1
}
