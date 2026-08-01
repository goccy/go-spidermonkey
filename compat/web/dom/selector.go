package dom

import (
	"fmt"
	"strconv"
	"strings"
)

// The Selectors grammar, parsed to an AST:
//
//	list     = []complex
//	complex  = []step            — first step's combinator is ""
//	step     = {C: combinator, S: compound}
//	compound = {T: tag ("" = any, "*" explicit any), I: id, Cl: classes,
//	            A: []attr, P: []pseudo}
//	attr     = {N: name, Op: "" | "=" | "~=" | "|=" | "^=" | "$=" | "*=",
//	            V: value, CI: case-insensitive flag}
//	pseudo   = {N: name, AB: [a, b] for the nth-* family,
//	            L: nested list for :not/:is/:where}
//
// The parser covers the selectors the conformance suites drive: type, id,
// class, attribute (all operators, with the `i` flag), the structural
// pseudo-classes, :not/:is/:where, and the four combinators. What it does not
// know it REJECTS — an unknown pseudo-class is a parse error, exactly the
// SyntaxError a conforming querySelector reports, never a silent
// match-nothing.

type selStep struct {
	C string      `json:"c"`
	S selCompound `json:"s"`
}

type selCompound struct {
	T  string      `json:"t,omitempty"`
	I  string      `json:"i,omitempty"`
	Cl []string    `json:"cl,omitempty"`
	A  []selAttr   `json:"a,omitempty"`
	P  []selPseudo `json:"p,omitempty"`
}

type selAttr struct {
	N  string `json:"n"`
	Op string `json:"op,omitempty"`
	V  string `json:"v,omitempty"`
	CI bool   `json:"ci,omitempty"`
}

type selPseudo struct {
	N  string      `json:"n"`
	AB *[2]int     `json:"ab,omitempty"`
	L  [][]selStep `json:"l,omitempty"`
}

type selParser struct {
	s string
	i int
}

func parseSelectorList(s string) ([][]selStep, error) {
	p := &selParser{s: s}
	list, err := p.parseList(false)
	if err != nil {
		return nil, err
	}
	if p.i < len(p.s) {
		return nil, fmt.Errorf("unexpected %q", p.s[p.i:])
	}
	return list, nil
}

func (p *selParser) parseList(nested bool) ([][]selStep, error) {
	var list [][]selStep
	for {
		c, err := p.parseComplex(nested)
		if err != nil {
			return nil, err
		}
		list = append(list, c)
		p.skipSpace()
		if p.i < len(p.s) && p.s[p.i] == ',' {
			p.i++
			continue
		}
		return list, nil
	}
}

// parseRelativeList parses :has()'s argument: each complex selector may open
// with a combinator, recorded on its FIRST step (" " when none is written).
func (p *selParser) parseRelativeList() ([][]selStep, error) {
	var list [][]selStep
	for {
		p.skipSpace()
		lead := " "
		if p.i < len(p.s) && (p.s[p.i] == '>' || p.s[p.i] == '+' || p.s[p.i] == '~') {
			lead = string(p.s[p.i])
			p.i++
		}
		c, err := p.parseComplex(true)
		if err != nil {
			return nil, err
		}
		c[0].C = lead
		list = append(list, c)
		p.skipSpace()
		if p.i < len(p.s) && p.s[p.i] == ',' {
			p.i++
			continue
		}
		return list, nil
	}
}

func (p *selParser) parseComplex(nested bool) ([]selStep, error) {
	p.skipSpace()
	first, err := p.parseCompound()
	if err != nil {
		return nil, err
	}
	steps := []selStep{{C: "", S: first}}
	for {
		// A combinator is >, +, ~, or plain whitespace followed by another
		// compound; whitespace before , or ) is not one.
		hadSpace := p.skipSpace()
		if p.i >= len(p.s) {
			return steps, nil
		}
		ch := p.s[p.i]
		comb := ""
		switch {
		case ch == '>' || ch == '+' || ch == '~':
			comb = string(ch)
			p.i++
			p.skipSpace()
		case ch == ',' || (nested && ch == ')'):
			return steps, nil
		case hadSpace:
			comb = " "
		default:
			return nil, fmt.Errorf("unexpected %q", string(ch))
		}
		next, err := p.parseCompound()
		if err != nil {
			return nil, err
		}
		steps = append(steps, selStep{C: comb, S: next})
	}
}

func (p *selParser) parseCompound() (selCompound, error) {
	var out selCompound
	any := false
	for p.i < len(p.s) {
		ch := p.s[p.i]
		switch {
		case ch == '*' && out.T == "" && !any:
			out.T = "*"
			p.i++
		case ch == '#':
			p.i++
			id, err := p.ident()
			if err != nil {
				return out, err
			}
			out.I = id
		case ch == '.':
			p.i++
			cl, err := p.ident()
			if err != nil {
				return out, err
			}
			out.Cl = append(out.Cl, cl)
		case ch == '[':
			p.i++
			a, err := p.parseAttr()
			if err != nil {
				return out, err
			}
			out.A = append(out.A, a)
		case ch == ':':
			p.i++
			if p.i < len(p.s) && p.s[p.i] == ':' {
				return out, fmt.Errorf("pseudo-elements are not supported")
			}
			ps, err := p.parsePseudo()
			if err != nil {
				return out, err
			}
			out.P = append(out.P, ps)
		case isIdentStart(ch) && out.T == "" && !any && out.I == "" && out.Cl == nil && out.A == nil && out.P == nil:
			t, err := p.ident()
			if err != nil {
				return out, err
			}
			// HTML type selectors match case-insensitively; normalize here so
			// the matcher compares directly.
			out.T = strings.ToLower(t)
		default:
			goto done
		}
		any = true
	}
done:
	if !any {
		return out, fmt.Errorf("expected a selector")
	}
	return out, nil
}

func (p *selParser) parseAttr() (selAttr, error) {
	var a selAttr
	p.skipSpace()
	name, err := p.ident()
	if err != nil {
		return a, err
	}
	a.N = strings.ToLower(name)
	p.skipSpace()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return a, nil
	}
	for _, op := range []string{"~=", "|=", "^=", "$=", "*=", "="} {
		if strings.HasPrefix(p.s[p.i:], op) {
			a.Op = op
			p.i += len(op)
			break
		}
	}
	if a.Op == "" {
		return a, fmt.Errorf("expected an attribute operator")
	}
	p.skipSpace()
	v, err := p.identOrString()
	if err != nil {
		return a, err
	}
	a.V = v
	p.skipSpace()
	// The case-sensitivity flag: [attr=v i] (or s, the default behaviour).
	if p.i < len(p.s) && (p.s[p.i] == 'i' || p.s[p.i] == 'I') {
		a.CI = true
		p.i++
		p.skipSpace()
	} else if p.i < len(p.s) && (p.s[p.i] == 's' || p.s[p.i] == 'S') {
		p.i++
		p.skipSpace()
	}
	if p.i >= len(p.s) || p.s[p.i] != ']' {
		return a, fmt.Errorf("expected ]")
	}
	p.i++
	return a, nil
}

var simplePseudos = map[string]bool{
	"first-child": true, "last-child": true, "only-child": true,
	"first-of-type": true, "last-of-type": true, "only-of-type": true,
	"root": true, "empty": true, "scope": true,
	"checked": true, "disabled": true, "enabled": true,
	"link": true, "any-link": true, "visited": true,
	"defined": true,
	// The user-action and target pseudo-classes PARSE — querySelector must
	// not report a SyntaxError for them — and match nothing, since there is
	// no interaction state to consult.
	"hover": true, "active": true, "focus": true,
	"focus-within": true, "focus-visible": true, "target": true,
	// Constraint validation, answered from attribute state (see the matcher).
	"valid": true, "invalid": true, "required": true, "optional": true,
}

func (p *selParser) parsePseudo() (selPseudo, error) {
	var ps selPseudo
	name, err := p.ident()
	if err != nil {
		return ps, err
	}
	ps.N = strings.ToLower(name)
	hasArgs := p.i < len(p.s) && p.s[p.i] == '('
	switch ps.N {
	case "has":
		// :has takes RELATIVE selectors — a leading combinator anchors the
		// argument to the element itself.
		if !hasArgs {
			return ps, fmt.Errorf(":has needs arguments")
		}
		p.i++
		list, err := p.parseRelativeList()
		if err != nil {
			return ps, err
		}
		ps.L = list
		p.skipSpace()
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return ps, fmt.Errorf("expected )")
		}
		p.i++
		return ps, nil
	case "not", "is", "where":
		if !hasArgs {
			return ps, fmt.Errorf(":%s needs arguments", ps.N)
		}
		p.i++
		list, err := p.parseList(true)
		if err != nil {
			return ps, err
		}
		ps.L = list
		p.skipSpace()
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return ps, fmt.Errorf("expected )")
		}
		p.i++
		return ps, nil
	case "nth-child", "nth-last-child", "nth-of-type", "nth-last-of-type":
		if !hasArgs {
			return ps, fmt.Errorf(":%s needs arguments", ps.N)
		}
		p.i++
		ab, err := p.parseNth()
		if err != nil {
			return ps, err
		}
		ps.AB = &ab
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return ps, fmt.Errorf("expected )")
		}
		p.i++
		return ps, nil
	default:
		if hasArgs || !simplePseudos[ps.N] {
			return ps, fmt.Errorf("unsupported pseudo-class :%s", ps.N)
		}
		return ps, nil
	}
}

// parseNth reads the An+B microsyntax: odd, even, N, An, An+B, +n-2, -n+3.
func (p *selParser) parseNth() ([2]int, error) {
	p.skipSpace()
	start := p.i
	for p.i < len(p.s) && p.s[p.i] != ')' {
		p.i++
	}
	raw := strings.ToLower(strings.TrimSpace(p.s[start:p.i]))
	switch raw {
	case "odd":
		return [2]int{2, 1}, nil
	case "even":
		return [2]int{2, 0}, nil
	}
	if !strings.ContainsRune(raw, 'n') {
		b, err := strconv.Atoi(raw)
		if err != nil {
			return [2]int{}, fmt.Errorf("bad An+B: %q", raw)
		}
		return [2]int{0, b}, nil
	}
	aPart, bPart, _ := strings.Cut(raw, "n")
	aPart = strings.TrimSpace(aPart)
	a := 1
	switch aPart {
	case "", "+":
	case "-":
		a = -1
	default:
		v, err := strconv.Atoi(aPart)
		if err != nil {
			return [2]int{}, fmt.Errorf("bad An+B: %q", raw)
		}
		a = v
	}
	b := 0
	bPart = strings.ReplaceAll(bPart, " ", "")
	if bPart != "" {
		v, err := strconv.Atoi(bPart)
		if err != nil {
			return [2]int{}, fmt.Errorf("bad An+B: %q", raw)
		}
		b = v
	}
	return [2]int{a, b}, nil
}

func (p *selParser) skipSpace() bool {
	start := p.i
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r', '\f':
			p.i++
		default:
			return p.i > start
		}
	}
	return p.i > start
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '-' || c == '\\' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func (p *selParser) ident() (string, error) {
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '\\' && p.i+1 < len(p.s) {
			// The simple-escape form: the escaped character stands for itself.
			// (Hex escapes can be added when a suite drives them.)
			b.WriteByte(p.s[p.i+1])
			p.i += 2
			continue
		}
		if b.Len() == 0 && !isIdentStart(c) {
			break
		}
		if b.Len() > 0 && !isIdentChar(c) {
			break
		}
		b.WriteByte(c)
		p.i++
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("expected an identifier")
	}
	return b.String(), nil
}

func (p *selParser) identOrString() (string, error) {
	if p.i < len(p.s) && (p.s[p.i] == '"' || p.s[p.i] == '\'') {
		quote := p.s[p.i]
		p.i++
		var b strings.Builder
		for p.i < len(p.s) && p.s[p.i] != quote {
			if p.s[p.i] == '\\' && p.i+1 < len(p.s) {
				b.WriteByte(p.s[p.i+1])
				p.i += 2
				continue
			}
			b.WriteByte(p.s[p.i])
			p.i++
		}
		if p.i >= len(p.s) {
			return "", fmt.Errorf("unterminated string")
		}
		p.i++
		return b.String(), nil
	}
	return p.ident()
}
