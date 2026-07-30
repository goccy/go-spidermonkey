package web

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

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"strings"
	"unicode/utf8"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"golang.org/x/net/idna"
)

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
