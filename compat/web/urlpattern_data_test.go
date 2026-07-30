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
