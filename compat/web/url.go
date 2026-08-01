package web

import (
	"errors"
	"fmt"
	spidermonkey "github.com/goccy/go-spidermonkey"
	"golang.org/x/net/idna"
	"math"
	"net/netip"
	"strconv"
	"strings"
	"unicode/utf8"
)

// url.go: the WHATWG URL parser.
//
// The URL Standard defines parsing as a state machine over code points, and it
// has to be implemented as one. It was previously approximated in JavaScript
// with a handful of anchored regular expressions, which passed the ordinary
// cases and failed 304 of the suite's subtests across 155 distinct causes —
// the signature of an approximation rather than of missing features. Things
// like "sc://" (empty host, not no host), the "/." a serializer must insert so
// an opaque path starting "//" does not read back as an authority, and a query
// that is empty rather than absent do not survive being split by a regex.
//
// The heavy lifting comes from libraries: golang.org/x/net/idna for
// domain-to-ASCII (UTS-46, whose mapping tables are not something to
// hand-write) and net/netip for IPv6. Serialization is spec-exact here rather
// than delegated, because the two disagree — netip renders an IPv4-mapped
// address as "::ffff:1.2.3.4" where the URL Standard requires "::ffff:102:304".
//
// The guest holds no parser state: a serialized URL round-trips through the
// parser to the same record (which is what the "/." rule above exists to
// guarantee), so a setter re-parses the href and re-runs the parser with the
// state override the spec names. That keeps the JavaScript side a shell over
// these two ops and needs no host-side handle table.

// specialSchemes are the schemes the standard treats specially, mapped to their
// default port ("" for file, which has none).
var specialSchemes = map[string]string{
	"ftp": "21", "file": "", "http": "80", "https": "443", "ws": "80", "wss": "443",
}

func isSpecialScheme(s string) bool { _, ok := specialSchemes[s]; return ok }

// urlRecord is a parsed URL. The distinctions a string cannot carry are what
// this exists for: a host that is empty versus absent, a query that is empty
// versus absent, and a path that is opaque versus a list of segments.
type urlRecord struct {
	scheme   string
	username string
	password string
	host     string // valid only when hasHost
	hasHost  bool
	port     string // "" for none
	path     []string
	opaque   bool // path is one opaque string in path[0]
	query    *string
	fragment *string
}

func (u *urlRecord) special() bool { return isSpecialScheme(u.scheme) }

// includesCredentials reports whether the userinfo is non-empty.
func (u *urlRecord) includesCredentials() bool { return u.username != "" || u.password != "" }

// cannotHaveCredentialsOrPort is the spec predicate of the same name.
func (u *urlRecord) cannotHaveCredentialsOrPort() bool {
	return !u.hasHost || u.host == "" || u.scheme == "file"
}

// ---------------------------------------------------------------- encoding

// The percent-encode sets. Every set implicitly contains the C0 control set —
// C0 controls and everything above U+007E — and the strings below list only the
// extra ASCII members, in the order the standard builds them up.
const (
	fragmentSet     = " \"<>`"
	querySet        = " \"#<>"
	specialQuerySet = querySet + "'"
	pathSet         = querySet + "?`{}^"
	userinfoSet     = pathSet + "/:;=@[\\]^|"
)

func inEncodeSet(r rune, set string) bool {
	return r <= 0x1f || r > 0x7e || strings.ContainsRune(set, r)
}

// pctEncode appends s to a builder, percent-encoding the members of set. UTF-8
// is encoded byte by byte, which is what makes a non-ASCII code point become a
// run of escapes rather than one.
func pctEncode(s, set string) string {
	var b strings.Builder
	for _, r := range s {
		if inEncodeSet(r, set) {
			var buf [4]byte
			n := utf8.EncodeRune(buf[:], r)
			for _, c := range buf[:n] {
				fmt.Fprintf(&b, "%%%02X", c)
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// pctDecode decodes %XX sequences, leaving a stray "%" alone. It is byte-wise
// on purpose: the result is a byte string that the caller interprets.
func pctDecode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' || i+2 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		hi, ok1 := hexVal(s[i+1])
		lo, ok2 := hexVal(s[i+2])
		if !ok1 || !ok2 {
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(hi<<4 | lo)
		i += 2
	}
	return b.String()
}

// ------------------------------------------------------------------- hosts

// forbiddenHost are the code points a host may never contain. A DOMAIN forbids
// these plus every C0 control, "%" and U+007F — which is why a control
// character inside a host name is a parse failure rather than something to
// escape.
const forbiddenHost = "\x00\t\n\r #/:<>?@[\\]^|"

func hasForbiddenHost(s string, domain bool) bool {
	if strings.ContainsAny(s, forbiddenHost) {
		return true
	}
	if !domain {
		return false
	}
	if strings.ContainsAny(s, "%\x7f") {
		return true
	}
	for _, r := range s {
		if r <= 0x1f {
			return true
		}
	}
	return false
}

// whatwgIDNA is UTS-46 with the flags the URL Standard's domain-to-ASCII names:
// no STD3 rules, non-transitional, labels validated, bidi and joiner rules on.
var whatwgIDNA = idna.New(
	idna.MapForLookup(),
	// UseSTD3ASCIIRules=false and CheckHyphens=false: MapForLookup turns both on,
	// and a URL host is allowed the symbols and hyphen placements a registrable
	// domain is not.
	idna.StrictDomainName(false),
	idna.CheckHyphens(false),
	// ValidateLabels(false) drops the re-validation of an already-encoded A-label,
	// which would reject "xn--pokxncvks" for decoding to characters UTS-46 maps
	// away. It also clears CheckJoiners, which does apply, so that is set back.
	idna.ValidateLabels(false),
	idna.CheckJoiners(true),
	idna.BidiRule(),
	idna.Transitional(false),
	idna.VerifyDNSLength(false),
)

func domainToASCII(domain string) (string, error) {
	// An all-ASCII domain needs nothing but lower-casing: ToASCII of a label that
	// is already encoded is that same label. Handing it to the library instead
	// round-trips it through punycode, which turns "xn--" — a valid label with an
	// empty payload — into the empty string.
	if isASCIIString(domain) {
		return strings.ToLower(domain), nil
	}
	out, err := whatwgIDNA.ToASCII(domain)
	if err != nil {
		return "", fmt.Errorf("domain %q is not valid: %w", domain, err)
	}
	if out == "" {
		return "", fmt.Errorf("domain %q is empty once encoded", domain)
	}
	return out, nil
}

func isASCIIString(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7f {
			return false
		}
	}
	return true
}

// endsInNumber is the spec check that decides whether a domain is really an
// IPv4 address: the last (non-empty) label is all digits, or is a valid number
// in one of the three radixes the IPv4 parser accepts.
func endsInNumber(input string) bool {
	parts := strings.Split(input, ".")
	if len(parts) > 1 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		return false
	}
	last := parts[len(parts)-1]
	if last != "" && strings.IndexFunc(last, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
		return true
	}
	_, err := parseIPv4Number(last)
	return err == nil
}

// parseIPv4Number reads one part of an IPv4 address. The radix is chosen by
// prefix — "0x" hex, a leading "0" octal, otherwise decimal — and the caller is
// told whether a prefix was stripped, which the standard reports as validation
// error but still accepts.
func parseIPv4Number(input string) (uint64, error) {
	if input == "" {
		return 0, fmt.Errorf("empty IPv4 part")
	}
	radix := 10
	if len(input) >= 2 && input[0] == '0' && (input[1] == 'x' || input[1] == 'X') {
		input, radix = input[2:], 16
	} else if len(input) >= 2 && input[0] == '0' {
		input, radix = input[1:], 8
	}
	if input == "" {
		return 0, nil
	}
	// Every character must be a digit of the radix BEFORE the value is read:
	// strconv.ParseUint reports ErrRange the moment the value overflows,
	// without looking at the rest, so "111…1b" — sixty ones and a letter —
	// came back as a range error and was mistaken for a number below.
	for i := 0; i < len(input); i++ {
		c := input[i]
		var ok bool
		switch radix {
		case 16:
			ok = c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
		case 8:
			ok = c >= '0' && c <= '7'
		default:
			ok = c >= '0' && c <= '9'
		}
		if !ok {
			return 0, fmt.Errorf("bad IPv4 part: %q is not a digit", c)
		}
	}
	n, err := strconv.ParseUint(input, radix, 64)
	if err == nil {
		return n, nil
	}
	// The standard reads the part as a mathematical integer, with no width. One
	// too large for uint64 is therefore a NUMBER — so the host is an IPv4
	// address, not a domain — and it is the range check in parseIPv4 that
	// rejects it. Reporting it as a syntax error instead made
	// "foo.0XFfFfFfFfFfFfFfFfFfAcE123" parse as a perfectly good domain.
	if errors.Is(err, strconv.ErrRange) {
		return math.MaxUint64, nil
	}
	return 0, fmt.Errorf("bad IPv4 part: %w", err)
}

// parseIPv4 implements the standard's own IPv4 parser, which is deliberately
// more permissive than net/netip: it accepts hex and octal parts and fewer than
// four of them ("0x7f.1" is 127.0.0.1). netip would reject every one of those.
func parseIPv4(input string) (string, error) {
	parts := strings.Split(input, ".")
	if len(parts) > 1 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) > 4 {
		return "", fmt.Errorf("too many IPv4 parts")
	}
	nums := make([]uint64, 0, 4)
	for _, p := range parts {
		n, err := parseIPv4Number(p)
		if err != nil {
			return "", err
		}
		nums = append(nums, n)
	}
	for _, n := range nums[:len(nums)-1] {
		if n > 255 {
			return "", fmt.Errorf("IPv4 part out of range")
		}
	}
	last := nums[len(nums)-1]
	if last >= 1<<(8*(5-uint(len(nums)))) {
		return "", fmt.Errorf("last IPv4 part out of range")
	}
	ipv4 := last
	for i, n := range nums[:len(nums)-1] {
		ipv4 += n << (8 * (3 - uint(i)))
	}
	return fmt.Sprintf("%d.%d.%d.%d", ipv4>>24&0xff, ipv4>>16&0xff, ipv4>>8&0xff, ipv4&0xff), nil
}

// parseIPv6 parses the contents of the brackets. net/netip does the parsing;
// what it accepts differs from the standard in two ways that are excluded here
// — a zone identifier, and a bare IPv4 address — and its serialization differs
// too, so the address is re-serialized below from the 8 pieces.
func parseIPv6(input string) (string, error) {
	if strings.Contains(input, "%") {
		return "", fmt.Errorf("IPv6 address must not carry a zone")
	}
	addr, err := netip.ParseAddr(input)
	if err != nil {
		return "", fmt.Errorf("bad IPv6 address: %w", err)
	}
	if !addr.Is6() || addr.Is4() {
		return "", fmt.Errorf("not an IPv6 address")
	}
	return serializeIPv6(addr.As16()), nil
}

// serializeIPv6 is the standard's serializer: compress the longest run of zero
// pieces longer than one, and render every piece as lowercase hex. It never
// renders an embedded IPv4 address, which is where netip's own String differs.
func serializeIPv6(b [16]byte) string {
	var pieces [8]uint16
	for i := range pieces {
		pieces[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	bestIdx, bestLen := -1, 0
	for i := 0; i < 8; i++ {
		if pieces[i] != 0 {
			continue
		}
		j := i
		for j < 8 && pieces[j] == 0 {
			j++
		}
		if j-i > bestLen {
			bestIdx, bestLen = i, j-i
		}
		i = j
	}
	if bestLen < 2 {
		bestIdx = -1
	}
	var out strings.Builder
	for i := 0; i < 8; i++ {
		if i == bestIdx {
			if i == 0 {
				out.WriteByte(':')
			}
			out.WriteByte(':')
			i += bestLen - 1
			continue
		}
		out.WriteString(strconv.FormatUint(uint64(pieces[i]), 16))
		if i != 7 {
			out.WriteByte(':')
		}
	}
	return out.String()
}

// parseOpaqueHost is the host of a non-special URL: no IDNA, no IPv4, just a
// forbidden-code-point check and percent-encoding of the C0 control set.
func parseOpaqueHost(input string) (string, error) {
	if hasForbiddenHost(input, false) {
		return "", fmt.Errorf("host %q contains a forbidden code point", input)
	}
	return pctEncode(input, ""), nil
}

func parseHost(input string, isSpecial bool) (string, error) {
	if strings.HasPrefix(input, "[") {
		if !strings.HasSuffix(input, "]") {
			return "", fmt.Errorf("unclosed IPv6 address")
		}
		addr, err := parseIPv6(input[1 : len(input)-1])
		if err != nil {
			return "", err
		}
		return "[" + addr + "]", nil
	}
	if !isSpecial {
		return parseOpaqueHost(input)
	}
	if input == "" {
		return "", fmt.Errorf("empty host")
	}
	decoded := pctDecode(input)
	// The decoded bytes are interpreted as UTF-8, so a sequence that is not
	// valid UTF-8 is a failure rather than something to pass through: "%80" is
	// not a character, and reading it as one produced a plausible-looking
	// punycode label out of nothing.
	if !utf8.ValidString(decoded) {
		return "", fmt.Errorf("host %q is not valid UTF-8 once decoded", input)
	}
	ascii, err := domainToASCII(decoded)
	if err != nil {
		return "", err
	}
	if hasForbiddenHost(ascii, true) {
		return "", fmt.Errorf("domain %q contains a forbidden code point", ascii)
	}
	if endsInNumber(ascii) {
		return parseIPv4(ascii)
	}
	return ascii, nil
}

// strArg reads a string argument that may contain a NUL. The engine's string
// bridge stops at the first NUL, and a URL may legitimately contain one — where
// the standard requires a parse failure or an escape, never a silently truncated
// input — so these arguments cross as UTF-8 bytes instead.
func strArg(v spidermonkey.Value) string {
	if o := v.Object(); o != nil {
		defer o.Free()
		if b, err := o.Bytes(); err == nil {
			return string(b)
		}
		return ""
	}
	return v.String()
}

// opDomainToASCII / opDomainToUnicode back url.domainToASCII and
// url.domainToUnicode of the Node compat layer, and are the only IDNA the guest
// can reach. They exist because the guest used to carry a hand-written RFC 3492
// punycode codec and an "IDNA-lite" ToASCII beside it — work x/net/idna already
// does correctly, including the UTS-46 mapping tables that made the guest
// version an approximation. An invalid domain comes back as "", which is what
// Node's own binding reports.
// nodeIDNA is the STRICT profile. Node's url.domainToASCII validates a domain
// the way a resolver would and answers "" when it does not hold up, so
// "ma\u00f1 ana.com" is rejected there. A URL host is not held to that — its
// parser must accept the symbols a domain name may not contain — which is why
// these are two profiles and not one.
var nodeIDNA = idna.New(
	idna.MapForLookup(),
	idna.BidiRule(),
	idna.Transitional(false),
	idna.VerifyDNSLength(false),
)

func (w *Web) opDomainToASCII(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("domain to ascii: (domain) required")
	}
	out, err := nodeIDNA.ToASCII(strArg(args[0]))
	if err != nil {
		return spidermonkey.ValueOf(""), nil
	}
	return spidermonkey.ValueOf(out), nil
}

func (w *Web) opDomainToUnicode(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("domain to unicode: (domain) required")
	}
	out, err := whatwgIDNA.ToUnicode(strArg(args[0]))
	if err != nil {
		// ToUnicode reports a label it could not decode but still returns the
		// rest; Node's binding keeps what it got rather than failing.
		if out == "" {
			return spidermonkey.ValueOf(strings.ToLower(strArg(args[0]))), nil
		}
	}
	return spidermonkey.ValueOf(out), nil
}

// ------------------------- the basic URL parser of the WHATWG URL Standard,
// as the state
// machine the standard defines. See url.go for why this is not a set of regular
// expressions, and for the host and encoding pieces it builds on.
//
// The states, their names, and the order of the checks inside each one follow
// the specification directly. That is the point: the cases this has to get
// right are the ones nobody would think to write a test for, so the text is the
// test, and departing from its structure is how those cases get lost.

type urlState int

const (
	stSchemeStart urlState = iota
	stScheme
	stNoScheme
	stSpecialRelativeOrAuthority
	stPathOrAuthority
	stRelative
	stRelativeSlash
	stSpecialAuthoritySlashes
	stSpecialAuthorityIgnoreSlashes
	stAuthority
	stHost
	stHostname
	stPort
	stFile
	stFileSlash
	stFileHost
	stPathStart
	stPath
	stOpaquePath
	stQuery
	stFragment
)

func isASCIIAlpha(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isASCIIAlnum(r rune) bool {
	return isASCIIAlpha(r) || (r >= '0' && r <= '9')
}

// isWindowsDriveLetter and its "normalized" variant drive the file-URL rules,
// where "C:" and "C|" name a drive and must not be treated as a path segment.
func isWindowsDriveLetter(s string) bool {
	return len(s) == 2 && isASCIIAlpha(rune(s[0])) && (s[1] == ':' || s[1] == '|')
}

func isNormalizedWindowsDriveLetter(s string) bool {
	return len(s) == 2 && isASCIIAlpha(rune(s[0])) && s[1] == ':'
}

// startsWithWindowsDriveLetter is the spec predicate: a drive letter that is the
// whole remainder, or is followed by one of /, \, ? or #.
func startsWithWindowsDriveLetter(s string) bool {
	if len(s) < 2 || !isWindowsDriveLetter(s[:2]) {
		return false
	}
	return len(s) == 2 || strings.ContainsRune("/\\?#", rune(s[2]))
}

func isSingleDot(seg string) bool {
	l := strings.ToLower(seg)
	return seg == "." || l == "%2e"
}

func isDoubleDot(seg string) bool {
	l := strings.ToLower(seg)
	return seg == ".." || l == ".%2e" || l == "%2e." || l == "%2e%2e"
}

// shortenPath drops the last path segment, except that a file URL whose only
// segment is a drive letter keeps it.
func (u *urlRecord) shortenPath() {
	if u.scheme == "file" && len(u.path) == 1 && isNormalizedWindowsDriveLetter(u.path[0]) {
		return
	}
	if len(u.path) > 0 {
		u.path = u.path[:len(u.path)-1]
	}
}

// parseURL is the basic URL parser. base and stateOverride are optional; a
// non-nil url means the caller is re-running the parser over an existing record
// (which is what every setter does).
func parseURL(input string, base *urlRecord, url *urlRecord, override urlState, hasOverride bool) (*urlRecord, error) {
	if url == nil {
		url = &urlRecord{}
		// Strip leading and trailing C0 controls and space, then remove every
		// ASCII tab and newline from what is left. Only done for a fresh parse:
		// a state override is given an already-trimmed value.
		input = strings.Trim(input, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\v\f\r\x0e\x0f"+
			"\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	}
	input = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(input)

	state := stSchemeStart
	if hasOverride {
		state = override
	}
	var buf strings.Builder
	atSignSeen, insideBrackets, passwordTokenSeen := false, false, false

	// The parser walks one code point past the end; `eof` marks that position.
	runes := []rune(input)
	const eof rune = -1
	at := func(i int) rune {
		if i < 0 || i >= len(runes) {
			return eof
		}
		return runes[i]
	}
	remaining := func(i int) string {
		if i+1 >= len(runes) {
			return ""
		}
		return string(runes[i+1:])
	}

	for i := 0; i <= len(runes); i++ {
		c := at(i)
		switch state {
		case stSchemeStart:
			if isASCIIAlpha(c) {
				buf.WriteRune(toLowerASCII(c))
				state = stScheme
				continue
			}
			if hasOverride {
				return nil, fmt.Errorf("scheme must start with a letter")
			}
			state, i = stNoScheme, i-1

		case stScheme:
			if isASCIIAlnum(c) || c == '+' || c == '-' || c == '.' {
				buf.WriteRune(toLowerASCII(c))
				continue
			}
			if c == ':' {
				s := buf.String()
				if hasOverride {
					// A setter may not move a URL between the special and
					// non-special worlds, nor give credentials or a port to a
					// scheme that cannot hold them.
					if isSpecialScheme(url.scheme) != isSpecialScheme(s) {
						return url, nil
					}
					if (isSpecialScheme(s) && url.includesCredentials()) ||
						(url.port != "" && s == "file") {
						return url, nil
					}
					if url.scheme == "file" && (!url.hasHost || url.host == "") {
						return url, nil
					}
				}
				url.scheme = s
				if hasOverride {
					if url.port == specialSchemes[url.scheme] {
						url.port = ""
					}
					return url, nil
				}
				buf.Reset()
				switch {
				case url.scheme == "file":
					state = stFile
				case url.special() && base != nil && base.scheme == url.scheme:
					state = stSpecialRelativeOrAuthority
				case url.special():
					state = stSpecialAuthoritySlashes
				case at(i+1) == '/':
					state, i = stPathOrAuthority, i+1
				default:
					url.opaque = true
					url.path = []string{""}
					state = stOpaquePath
				}
				continue
			}
			if hasOverride {
				return nil, fmt.Errorf("invalid scheme")
			}
			buf.Reset()
			state, i = stNoScheme, -1

		case stNoScheme:
			if base == nil || (base.opaque && c != '#') {
				return nil, fmt.Errorf("missing scheme")
			}
			if base.opaque && c == '#' {
				url.scheme = base.scheme
				url.path = append([]string(nil), base.path...)
				url.opaque = base.opaque
				url.query = base.query
				empty := ""
				url.fragment = &empty
				state = stFragment
				continue
			}
			if base.scheme != "file" {
				state, i = stRelative, i-1
				continue
			}
			state, i = stFile, i-1

		case stSpecialRelativeOrAuthority:
			if c == '/' && at(i+1) == '/' {
				state, i = stSpecialAuthorityIgnoreSlashes, i+1
				continue
			}
			state, i = stRelative, i-1

		case stPathOrAuthority:
			if c == '/' {
				state = stAuthority
				continue
			}
			state, i = stPath, i-1

		case stRelative:
			url.scheme = base.scheme
			if c == '/' || (url.special() && c == '\\') {
				state = stRelativeSlash
				continue
			}
			url.username, url.password = base.username, base.password
			url.host, url.hasHost, url.port = base.host, base.hasHost, base.port
			url.path = append([]string(nil), base.path...)
			url.opaque = base.opaque
			url.query = base.query
			switch c {
			case '?':
				empty := ""
				url.query = &empty
				state = stQuery
			case '#':
				empty := ""
				url.fragment = &empty
				state = stFragment
			case eof:
			default:
				url.query = nil
				url.shortenPath()
				state, i = stPath, i-1
			}

		case stRelativeSlash:
			if url.special() && (c == '/' || c == '\\') {
				state = stSpecialAuthorityIgnoreSlashes
				continue
			}
			if c == '/' {
				state = stAuthority
				continue
			}
			url.username, url.password = base.username, base.password
			url.host, url.hasHost, url.port = base.host, base.hasHost, base.port
			state, i = stPath, i-1

		case stSpecialAuthoritySlashes:
			if c == '/' && at(i+1) == '/' {
				state, i = stSpecialAuthorityIgnoreSlashes, i+1
				continue
			}
			state, i = stSpecialAuthorityIgnoreSlashes, i-1

		case stSpecialAuthorityIgnoreSlashes:
			if c == '/' || c == '\\' {
				continue
			}
			state, i = stAuthority, i-1

		case stAuthority:
			if c == '@' {
				if atSignSeen {
					// Prepended, not appended: a second "@" means everything seen
					// so far was userinfo containing an escaped one.
					rest := buf.String()
					buf.Reset()
					buf.WriteString("%40")
					buf.WriteString(rest)
				}
				atSignSeen = true
				for _, r := range buf.String() {
					if r == ':' && !passwordTokenSeen {
						passwordTokenSeen = true
						continue
					}
					enc := pctEncode(string(r), userinfoSet)
					if passwordTokenSeen {
						url.password += enc
					} else {
						url.username += enc
					}
				}
				buf.Reset()
				continue
			}
			if c == eof || c == '/' || c == '?' || c == '#' || (url.special() && c == '\\') {
				if atSignSeen && buf.Len() == 0 {
					return nil, fmt.Errorf("credentials with no host")
				}
				i -= utf8.RuneCountInString(buf.String()) + 1
				buf.Reset()
				state = stHost
				continue
			}
			buf.WriteRune(c)

		case stHost, stHostname:
			if hasOverride && url.scheme == "file" {
				state, i = stFileHost, i-1
				continue
			}
			if c == ':' && !insideBrackets {
				if buf.Len() == 0 {
					return nil, fmt.Errorf("empty host before port")
				}
				if hasOverride && state == stHostname {
					return url, nil
				}
				h, err := parseHost(buf.String(), url.special())
				if err != nil {
					return nil, err
				}
				url.host, url.hasHost = h, true
				buf.Reset()
				state = stPort
				continue
			}
			if c == eof || c == '/' || c == '?' || c == '#' || (url.special() && c == '\\') {
				// One back, not back over the buffer: the host has been consumed
				// into the buffer already, and the terminator is re-read by the
				// state that follows.
				i--
				if url.special() && buf.Len() == 0 {
					return nil, fmt.Errorf("special scheme requires a host")
				}
				if hasOverride && buf.Len() == 0 && (url.includesCredentials() || url.port != "") {
					return url, nil
				}
				h, err := parseHost(buf.String(), url.special())
				if err != nil {
					return nil, err
				}
				url.host, url.hasHost = h, true
				buf.Reset()
				state = stPathStart
				if hasOverride {
					return url, nil
				}
				continue
			}
			if c == '[' {
				insideBrackets = true
			} else if c == ']' {
				insideBrackets = false
			}
			buf.WriteRune(c)

		case stPort:
			if c >= '0' && c <= '9' {
				buf.WriteRune(c)
				continue
			}
			if c == eof || c == '/' || c == '?' || c == '#' || (url.special() && c == '\\') || hasOverride {
				if buf.Len() > 0 {
					n, err := strconv.Atoi(buf.String())
					if err != nil || n > 0xffff {
						return nil, fmt.Errorf("port out of range")
					}
					url.port = strconv.Itoa(n)
					if url.port == specialSchemes[url.scheme] {
						url.port = ""
					}
					buf.Reset()
				}
				if hasOverride {
					return url, nil
				}
				state, i = stPathStart, i-1
				continue
			}
			return nil, fmt.Errorf("invalid port")

		case stFile:
			url.scheme = "file"
			url.host, url.hasHost = "", true
			if c == '/' || c == '\\' {
				state = stFileSlash
				continue
			}
			if base != nil && base.scheme == "file" {
				url.host, url.hasHost = base.host, base.hasHost
				url.path = append([]string(nil), base.path...)
				url.opaque = base.opaque
				url.query = base.query
				switch c {
				case '?':
					empty := ""
					url.query = &empty
					state = stQuery
				case '#':
					empty := ""
					url.fragment = &empty
					state = stFragment
				case eof:
				default:
					url.query = nil
					if !startsWithWindowsDriveLetter(string(runes[min(i, len(runes)):])) {
						url.shortenPath()
					} else {
						url.path = nil
					}
					state, i = stPath, i-1
				}
				continue
			}
			state, i = stPath, i-1

		case stFileSlash:
			if c == '/' || c == '\\' {
				state = stFileHost
				continue
			}
			if base != nil && base.scheme == "file" {
				url.host, url.hasHost = base.host, base.hasHost
				// A base whose first segment is a drive letter contributes it, so
				// that "/foo" against "file:///C:/bar" stays on drive C.
				if !startsWithWindowsDriveLetter(string(runes[min(i, len(runes)):])) &&
					len(base.path) > 0 && isNormalizedWindowsDriveLetter(base.path[0]) {
					url.path = append(url.path, base.path[0])
				}
			}
			state, i = stPath, i-1

		case stFileHost:
			if c == eof || c == '/' || c == '\\' || c == '?' || c == '#' {
				i--
				switch {
				case !hasOverride && isWindowsDriveLetter(buf.String()):
					// "file:///C:/" — a drive letter is not a host.
					state = stPath
				case buf.Len() == 0:
					url.host, url.hasHost = "", true
					if hasOverride {
						return url, nil
					}
					state = stPathStart
				default:
					h, err := parseHost(buf.String(), url.special())
					if err != nil {
						return nil, err
					}
					if h == "localhost" {
						h = ""
					}
					url.host, url.hasHost = h, true
					if hasOverride {
						return url, nil
					}
					buf.Reset()
					state = stPathStart
				}
				continue
			}
			buf.WriteRune(c)

		case stPathStart:
			if url.special() {
				state = stPath
				if c != '/' && c != '\\' {
					i--
				}
				continue
			}
			if !hasOverride && c == '?' {
				empty := ""
				url.query = &empty
				state = stQuery
				continue
			}
			if !hasOverride && c == '#' {
				empty := ""
				url.fragment = &empty
				state = stFragment
				continue
			}
			if c != eof {
				state = stPath
				if c != '/' {
					i--
				}
				continue
			}
			if hasOverride && !url.hasHost {
				url.path = append(url.path, "")
			}

		case stPath:
			atEnd := c == eof || c == '/' || (url.special() && c == '\\') ||
				(!hasOverride && (c == '?' || c == '#'))
			if !atEnd {
				buf.WriteRune(c)
				continue
			}
			seg := buf.String()
			buf.Reset()
			switch {
			case isDoubleDot(seg):
				url.shortenPath()
				if c != '/' && !(url.special() && c == '\\') {
					url.path = append(url.path, "")
				}
			case isSingleDot(seg):
				if c != '/' && !(url.special() && c == '\\') {
					url.path = append(url.path, "")
				}
			default:
				// A Windows drive letter reached through a file URL is normalized
				// to use ":", whatever separator was written.
				if url.scheme == "file" && len(url.path) == 0 && isWindowsDriveLetter(seg) {
					seg = seg[:1] + ":"
				}
				url.path = append(url.path, pctEncode(seg, pathSet))
			}
			if c == '?' {
				empty := ""
				url.query = &empty
				state = stQuery
			} else if c == '#' {
				empty := ""
				url.fragment = &empty
				state = stFragment
			}

		case stOpaquePath:
			switch c {
			case '?':
				url.path = []string{buf.String()}
				buf.Reset()
				empty := ""
				url.query = &empty
				state = stQuery
			case '#':
				url.path = []string{buf.String()}
				buf.Reset()
				empty := ""
				url.fragment = &empty
				state = stFragment
			case eof:
				url.path = []string{buf.String()}
			case ' ':
				// A space is kept literal unless the input ends, or a query or
				// fragment begins, immediately after it — in which case encoding
				// it is what stops it being trimmed away when the serialized URL
				// is parsed again.
				r := remaining(i)
				if r == "" || r[0] == '?' || r[0] == '#' {
					buf.WriteString("%20")
				} else {
					buf.WriteByte(' ')
				}
			default:
				// The C0 control set only, so an opaque path keeps the characters
				// a path would escape: "a: foo.com" is not "a:%20foo.com".
				buf.WriteString(pctEncode(string(c), ""))
			}

		case stQuery:
			if c == '#' && !hasOverride {
				set := querySet
				if url.special() {
					set = specialQuerySet
				}
				q := pctEncode(buf.String(), set)
				url.query = &q
				buf.Reset()
				empty := ""
				url.fragment = &empty
				state = stFragment
				continue
			}
			if c == eof {
				set := querySet
				if url.special() {
					set = specialQuerySet
				}
				q := pctEncode(buf.String(), set)
				url.query = &q
				buf.Reset()
				continue
			}
			buf.WriteRune(c)

		case stFragment:
			if c == eof {
				f := pctEncode(buf.String(), fragmentSet)
				url.fragment = &f
				buf.Reset()
				continue
			}
			buf.WriteRune(c)
		}
	}
	return url, nil
}

func toLowerASCII(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return r
}

// ------------------------------------------------------------- serialize

// serializePath writes the path. The "/." prefix is not decoration: without it
// an opaque-path URL whose path begins "//" would parse back as an authority,
// and the whole design here depends on a serialized URL round-tripping.
func (u *urlRecord) serializePath() string {
	if u.opaque {
		if len(u.path) == 0 {
			return ""
		}
		return u.path[0]
	}
	var b strings.Builder
	for _, seg := range u.path {
		b.WriteByte('/')
		b.WriteString(seg)
	}
	return b.String()
}

func (u *urlRecord) serializeHost() string {
	if !u.hasHost {
		return ""
	}
	if u.port == "" {
		return u.host
	}
	return u.host + ":" + u.port
}

func (u *urlRecord) href() string {
	var b strings.Builder
	b.WriteString(u.scheme)
	b.WriteByte(':')
	if u.hasHost {
		b.WriteString("//")
		if u.includesCredentials() {
			b.WriteString(u.username)
			if u.password != "" {
				b.WriteByte(':')
				b.WriteString(u.password)
			}
			b.WriteByte('@')
		}
		b.WriteString(u.serializeHost())
	}
	// The "/." is the URL serializer's, not the path serializer's: href needs it
	// so the result does not read back as an authority, while pathname reports
	// the path itself.
	if !u.hasHost && !u.opaque && len(u.path) > 1 && u.path[0] == "" {
		b.WriteString("/.")
	}
	b.WriteString(u.serializePath())
	if u.query != nil {
		b.WriteByte('?')
		b.WriteString(*u.query)
	}
	if u.fragment != nil {
		b.WriteByte('#')
		b.WriteString(*u.fragment)
	}
	return b.String()
}

// origin is the "opaque origin" serialization: "null" for anything but the
// schemes whose origin is a tuple.
func (u *urlRecord) origin() string {
	switch u.scheme {
	case "http", "https", "ws", "wss", "ftp":
		return u.scheme + "://" + u.serializeHost()
	case "blob":
		// A blob URL takes the origin of the URL inside it, but only when that is
		// one of the three schemes the standard names; anything else — including
		// another blob URL — is an opaque origin.
		if inner, err := parseURL(u.serializePath(), nil, nil, 0, false); err == nil {
			switch inner.scheme {
			case "http", "https", "file":
				return inner.origin()
			}
		}
	}
	return "null"
}

// components is what the guest sees: every getter of the URL interface, already
// serialized, so the JavaScript side holds no parsing logic of its own.
func (u *urlRecord) components() map[string]any {
	search := ""
	if u.query != nil && *u.query != "" {
		search = "?" + *u.query
	}
	hash := ""
	if u.fragment != nil && *u.fragment != "" {
		hash = "#" + *u.fragment
	}
	return map[string]any{
		"href":     u.href(),
		"protocol": u.scheme + ":",
		"username": u.username,
		"password": u.password,
		"host":     u.serializeHost(),
		"hostname": u.host,
		"port":     u.port,
		"pathname": u.serializePath(),
		"search":   search,
		"hash":     hash,
		"origin":   u.origin(),
	}
}

// ------------------------------------------------------------------- ops

// setterStates maps a URL attribute to the parser state the standard says its
// setter re-enters. "href" has none: it re-parses from scratch.
var setterStates = map[string]urlState{
	"protocol": stSchemeStart,
	"username": stAuthority,
	"password": stAuthority,
	"host":     stHost,
	"hostname": stHostname,
	"port":     stPort,
	"pathname": stPathStart,
	"search":   stQuery,
	"hash":     stFragment,
}

func urlFail(msg string) spidermonkey.Value {
	return spidermonkey.ValueOf(map[string]any{"__urlError": true, "message": msg})
}

// opURLParse(input, base) -> components, or a failure the guest turns into a
// TypeError. An absent base is the empty string, which is never a valid URL.
func (w *Web) opURLParse(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("url parse: (input, base?) required")
	}
	var base *urlRecord
	if len(args) > 1 {
		if s := strArg(args[1]); s != "" {
			b, err := parseURL(s, nil, nil, 0, false)
			if err != nil {
				return urlFail("Invalid base URL: " + s), nil
			}
			base = b
		}
	}
	u, err := parseURL(strArg(args[0]), base, nil, 0, false)
	if err != nil {
		return urlFail("Invalid URL: " + strArg(args[0])), nil
	}
	return spidermonkey.ValueOf(u.components()), nil
}

// applySetter runs the attribute setter the standard defines for attr. It
// returns the resulting components; a setter the standard says to ignore
// returns the URL unchanged, because these setters have no way to report an
// error to their caller.
func applySetter(u *urlRecord, attr, value string) map[string]any {
	if attr == "href" {
		n, err := parseURL(value, nil, nil, 0, false)
		if err != nil {
			return u.components()
		}
		return n.components()
	}
	state, ok := setterStates[attr]
	if !ok {
		return u.components()
	}

	// The attributes that are not simply "re-parse from this state" come first,
	// because the standard states them as edits to the record.
	switch attr {
	case "protocol":
		value += ":"
	case "username", "password":
		if u.cannotHaveCredentialsOrPort() {
			return u.components()
		}
		enc := pctEncode(value, userinfoSet)
		if attr == "username" {
			u.username = enc
		} else {
			u.password = enc
		}
		return u.components()
	case "host", "hostname":
		if u.opaque {
			return u.components()
		}
	case "port":
		if u.opaque || u.cannotHaveCredentialsOrPort() {
			return u.components()
		}
		if value == "" {
			u.port = ""
			return u.components()
		}
	case "pathname":
		if u.opaque {
			return u.components()
		}
		u.path = nil
	case "search":
		if value == "" {
			u.query = nil
			return u.components()
		}
		value = strings.TrimPrefix(value, "?")
		empty := ""
		u.query = &empty
	case "hash":
		if value == "" {
			u.fragment = nil
			return u.components()
		}
		value = strings.TrimPrefix(value, "#")
		empty := ""
		u.fragment = &empty
	}

	if n, err := parseURL(value, nil, u, state, true); err == nil {
		u = n
	}
	return u.components()
}

// opURLSet(href, attribute, value) -> components.
func (w *Web) opURLSet(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("url set: (href, attribute, value) required")
	}
	u, err := parseURL(strArg(args[0]), nil, nil, 0, false)
	if err != nil {
		return urlFail("Invalid URL: " + strArg(args[0])), nil
	}
	return spidermonkey.ValueOf(applySetter(u, args[1].String(), strArg(args[2]))), nil
}
