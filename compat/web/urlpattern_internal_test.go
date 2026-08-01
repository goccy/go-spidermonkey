package web

// The pattern parser and its serializer are checked against the URLPattern
// standard's own test data, the same way the URL parser is (see url_test.go).
// The canonical pattern string is the part worth pinning here: it is the inverse
// of the parser, so every disagreement is a case where the two do not describe
// the same pattern.

import (
	"encoding/json"
	"strings"
	"testing"
)

type patternCase struct {
	Pattern     []json.RawMessage `json:"pattern"`
	ExpectedObj json.RawMessage   `json:"expected_obj"`
}

func TestURLPatternStringsAgainstWPTData(t *testing.T) {
	var raw []json.RawMessage
	loadJSON(t, "urlpattern/resources/urlpatterntestdata.json", &raw)

	var pass, fail int
	for _, entry := range raw {
		var c patternCase
		if json.Unmarshal(entry, &c) != nil || len(c.Pattern) == 0 || len(c.ExpectedObj) == 0 {
			continue
		}
		// Only the object form is exercised here; a constructor string goes
		// through a different state machine, tested separately.
		var init map[string]string
		if json.Unmarshal(c.Pattern[0], &init) != nil {
			continue
		}
		// A baseURL contributes components this test does not resolve; those
		// cases are covered through the suite.
		if _, ok := init["baseURL"]; ok {
			continue
		}
		var want map[string]string
		if json.Unmarshal(c.ExpectedObj, &want) != nil {
			continue // "error" — a case about rejection, not about serialization
		}
		protocol := init["protocol"]
		bad := false
		for component, input := range init {
			expected, ok := want[component]
			if !ok {
				continue
			}
			input = stripInitDelimiter(component, input)
			opts := componentOptions(component)
			enc := component
			if component == "hostname" && len(input) > 0 && input[0] == '[' {
				enc = "ipv6hostname"
			}
			// A protocol pattern is special when it matches any special scheme; the
			// literal cases here are decided by the scheme itself, and an absent
			// protocol is a wildcard, which matches every scheme.
			special := protocol == "" || isSpecialScheme(protocol)
			for _, sch := range []string{"http", "https", "ws", "wss", "ftp", "file"} {
				if strings.Contains(protocol, sch) {
					special = true
				}
			}
			parts, err := parsePattern(input, opts, encoderFor(enc, protocol, special))
			if err != nil {
				t.Errorf("%s %q: %v", component, input, err)
				bad = true
				continue
			}
			if got := partsToPatternString(parts, opts); got != expected {
				t.Errorf("%s %q: pattern = %q, want %q", component, input, got, expected)
				bad = true
			}
		}
		if bad {
			fail++
		} else {
			pass++
		}
	}
	t.Logf("urlpattern object patterns: %d/%d cases pass", pass, pass+fail)
}

// compareComponent and generate are checked against their own data files, which
// are the only statement of the ordering and of what "cannot be generated"
// means precise enough to implement against.

type compareCase struct {
	Component string            `json:"component"`
	Left      json.RawMessage   `json:"left"`
	Right     json.RawMessage   `json:"right"`
	Expected  int               `json:"expected"`
	Note      map[string]string `json:"-"`
}

// patternOf compiles one component of a pattern init the way the class does, so
// the comparison sees the canonical strings and not the raw input.
func patternOf(t *testing.T, raw json.RawMessage, component string) (string, bool) {
	t.Helper()
	var init map[string]string
	if json.Unmarshal(raw, &init) != nil {
		// A constructor string; those cases are covered through the suite.
		return "", false
	}
	input := "*"
	if v, ok := init[component]; ok {
		input = stripInitDelimiter(component, v)
	}
	protocol := init["protocol"]
	special := protocol == "" || isSpecialScheme(protocol)
	parts, err := parsePattern(input, componentOptions(component), encoderFor(component, protocol, special))
	if err != nil {
		t.Errorf("%s %q: %v", component, input, err)
		return "", false
	}
	return partsToPatternString(parts, componentOptions(component)), true
}

func TestURLPatternCompareAgainstWPTData(t *testing.T) {
	var cases []compareCase
	loadJSON(t, "urlpattern/resources/urlpattern-compare-test-data.json", &cases)

	var pass, fail int
	for _, c := range cases {
		left, ok1 := patternOf(t, c.Left, c.Component)
		right, ok2 := patternOf(t, c.Right, c.Component)
		if !ok1 || !ok2 {
			continue
		}
		got, err := comparePatterns(c.Component, left, right)
		if err != nil {
			t.Errorf("%s %q vs %q: %v", c.Component, left, right, err)
			fail++
			continue
		}
		rev, _ := comparePatterns(c.Component, right, left)
		self, _ := comparePatterns(c.Component, left, left)
		if got != c.Expected || rev != -c.Expected || self != 0 {
			t.Errorf("%s %q vs %q: got %d (reverse %d, self %d), want %d",
				c.Component, left, right, got, rev, self, c.Expected)
			fail++
			continue
		}
		pass++
	}
	t.Logf("urlpattern compare: %d/%d cases pass", pass, pass+fail)
}

type generateCase struct {
	Pattern   json.RawMessage   `json:"pattern"`
	Component string            `json:"component"`
	Groups    map[string]string `json:"groups"`
	Expected  *string           `json:"expected"`
}

func TestURLPatternGenerateAgainstWPTData(t *testing.T) {
	var cases []generateCase
	loadJSON(t, "urlpattern/resources/urlpattern-generate-test-data.json", &cases)

	var pass, fail int
	for _, c := range cases {
		var init map[string]string
		if json.Unmarshal(c.Pattern, &init) != nil {
			continue // a constructor string; covered through the suite
		}
		known := false
		for _, comp := range patternComponents {
			if comp == c.Component {
				known = true
			}
		}
		if !known {
			// An unknown component name is a TypeError, which the guest raises
			// before reaching the host.
			if c.Expected == nil {
				pass++
			} else {
				t.Errorf("component %q: expected %q", c.Component, *c.Expected)
				fail++
			}
			continue
		}
		input := "*"
		if v, ok := init[c.Component]; ok {
			input = stripInitDelimiter(c.Component, v)
		}
		protocol := init["protocol"]
		special := protocol == "" || isSpecialScheme(protocol)
		parts, err := parsePattern(input, componentOptions(c.Component), encoderFor(c.Component, protocol, special))
		if err != nil {
			t.Errorf("%s %q: %v", c.Component, input, err)
			fail++
			continue
		}
		canonical := partsToPatternString(parts, componentOptions(c.Component))
		got, gerr := generateComponent(c.Component, canonical, protocol, special, c.Groups)
		switch {
		case c.Expected == nil && gerr == nil:
			t.Errorf("%s %q groups %v: generated %q, want a failure", c.Component, canonical, c.Groups, got)
			fail++
		case c.Expected != nil && gerr != nil:
			t.Errorf("%s %q groups %v: %v, want %q", c.Component, canonical, c.Groups, gerr, *c.Expected)
			fail++
		case c.Expected != nil && got != *c.Expected:
			t.Errorf("%s %q groups %v: generated %q, want %q", c.Component, canonical, c.Groups, got, *c.Expected)
			fail++
		default:
			pass++
		}
	}
	t.Logf("urlpattern generate: %d/%d cases pass", pass, pass+fail)
}
