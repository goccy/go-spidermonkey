package web

// mimetype.go: the WHATWG "parse a MIME type" and "serialize a MIME type"
// algorithms (mimesniff §4.4-4.5).
//
// A data: URL's Content-Type is not the text between "data:" and the comma —
// it is that text PARSED and SERIALIZED BACK, which lowercases the type,
// normalizes whitespace and drops parameters that are not well formed. Doing
// that faithfully is most of what fetch/data-urls' processing tests check.

import "strings"

type mimeType struct {
	typ, subtype string
	// params keeps insertion order: serialization must preserve it.
	params []mimeParam
}

type mimeParam struct{ name, value string }

// httpWhitespace is the set the algorithm trims (not Unicode whitespace).
const httpWhitespace = "\t\n\f\r "

// isTokenChar reports whether r is an HTTP token code point.
func isTokenChar(r rune) bool {
	switch r {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isTokenChar(r) {
			return false
		}
	}
	return true
}

// isQuotableValue reports whether every code point may appear in a parameter
// value (HTTP quoted-string code points: tab, space, printable ASCII except
// the delete character, plus non-ASCII).
func isQuotableValue(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r == '\t' || (r >= ' ' && r != 0x7f) {
			continue
		}
		return false
	}
	return true
}

// parseMIMEType implements "parse a MIME type"; ok is false for a failure,
// which callers turn into the default type.
func parseMIMEType(input string) (m mimeType, ok bool) {
	input = strings.Trim(input, httpWhitespace)
	slash := strings.IndexByte(input, '/')
	if slash < 0 {
		return m, false
	}
	m.typ = strings.ToLower(input[:slash])
	if !isToken(m.typ) {
		return m, false
	}
	rest := input[slash+1:]
	sub := rest
	if i := strings.IndexByte(rest, ';'); i >= 0 {
		sub = rest[:i]
		rest = rest[i:]
	} else {
		rest = ""
	}
	m.subtype = strings.ToLower(strings.TrimRight(sub, httpWhitespace))
	if !isToken(m.subtype) {
		return m, false
	}

	seen := map[string]bool{}
	for rest != "" {
		rest = rest[1:] // consume ";"
		rest = strings.TrimLeft(rest, httpWhitespace)
		// Parameter name: up to "=" or ";".
		i := 0
		for i < len(rest) && rest[i] != ';' && rest[i] != '=' {
			i++
		}
		name := strings.ToLower(rest[:i])
		rest = rest[i:]
		if rest == "" {
			break
		}
		if rest[0] == ';' {
			continue // a name with no value is dropped
		}
		rest = rest[1:] // consume "="

		var value string
		if strings.HasPrefix(rest, `"`) {
			// A quoted string, with backslash escapes.
			rest = rest[1:]
			var b strings.Builder
			for len(rest) > 0 {
				c := rest[0]
				rest = rest[1:]
				if c == '\\' && len(rest) > 0 {
					b.WriteByte(rest[0])
					rest = rest[1:]
					continue
				}
				if c == '"' {
					break
				}
				b.WriteByte(c)
			}
			value = b.String()
			// Anything up to the next ";" after the closing quote is discarded.
			if i := strings.IndexByte(rest, ';'); i >= 0 {
				rest = rest[i:]
			} else {
				rest = ""
			}
		} else {
			end := strings.IndexByte(rest, ';')
			if end < 0 {
				value = rest
				rest = ""
			} else {
				value = rest[:end]
				rest = rest[end:]
			}
			value = strings.TrimRight(value, httpWhitespace)
			if value == "" {
				continue
			}
		}
		if name != "" && isToken(name) && isQuotableValue(value) && !seen[name] {
			seen[name] = true
			m.params = append(m.params, mimeParam{name, value})
		}
	}
	return m, true
}

// String implements "serialize a MIME type".
func (m mimeType) String() string {
	var b strings.Builder
	b.WriteString(m.typ)
	b.WriteByte('/')
	b.WriteString(m.subtype)
	for _, p := range m.params {
		b.WriteByte(';')
		b.WriteString(p.name)
		b.WriteByte('=')
		if isToken(p.value) {
			b.WriteString(p.value)
			continue
		}
		b.WriteByte('"')
		for _, c := range []byte(p.value) {
			if c == '"' || c == '\\' {
				b.WriteByte('\\')
			}
			b.WriteByte(c)
		}
		b.WriteByte('"')
	}
	return b.String()
}
