package web

// The URL parser is checked directly against the Web Platform Tests' own data
// files rather than only through the JavaScript surface. Those files ARE the
// specification's test suite; running them here means a parser change is judged
// by the standard in a second, not by a suite run in a minute, and it keeps the
// host-side algorithm honest independently of the shell over it.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// urlCase mirrors one entry of url/resources/urltestdata.json. A "failure"
// entry expects the parse to be rejected; every other entry lists the value of
// each attribute of the resulting URL.
type urlCase struct {
	Input    string  `json:"input"`
	Base     *string `json:"base"`
	Failure  bool    `json:"failure"`
	Relative *bool   `json:"relativeFlag"`
	Href     string  `json:"href"`
	Origin   *string `json:"origin"`
	Protocol string  `json:"protocol"`
	Username string  `json:"username"`
	Password string  `json:"password"`
	Host     string  `json:"host"`
	Hostname string  `json:"hostname"`
	Port     string  `json:"port"`
	Pathname string  `json:"pathname"`
	Search   string  `json:"search"`
	Hash     string  `json:"hash"`
}

func suitePath(t *testing.T, rel string) string {
	t.Helper()
	p := filepath.Join("..", "..", "wpt", "suite", rel)
	if _, err := os.Stat(p); err != nil {
		t.Skipf("web-platform-tests checkout not present (%s); run `make wpt-fetch`", rel)
	}
	return p
}

func loadJSON(t *testing.T, rel string, into any) {
	t.Helper()
	b, err := os.ReadFile(suitePath(t, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
}

func TestURLParserAgainstWPTData(t *testing.T) {
	// The file mixes comment strings in among the case objects.
	var raw []json.RawMessage
	loadJSON(t, "url/resources/urltestdata.json", &raw)

	var pass, fail int
	for _, entry := range raw {
		var comment string
		if json.Unmarshal(entry, &comment) == nil {
			continue
		}
		var c urlCase
		if err := json.Unmarshal(entry, &c); err != nil {
			t.Fatalf("case: %v", err)
		}
		var base *urlRecord
		if c.Base != nil {
			b, err := parseURL(*c.Base, nil, nil, 0, false)
			if err != nil {
				// A case whose base does not parse is a case about the base.
				if !c.Failure {
					t.Errorf("base %q did not parse (input %q)", *c.Base, c.Input)
					fail++
				} else {
					pass++
				}
				continue
			}
			base = b
		}
		u, err := parseURL(c.Input, base, nil, 0, false)
		if c.Failure {
			if err == nil {
				t.Errorf("input %q base %v: parsed to %q, want failure", c.Input, c.Base, u.href())
				fail++
			} else {
				pass++
			}
			continue
		}
		if err != nil {
			t.Errorf("input %q base %v: %v", c.Input, c.Base, err)
			fail++
			continue
		}
		got := u.components()
		bad := false
		for attr, want := range map[string]string{
			"href": c.Href, "protocol": c.Protocol, "username": c.Username,
			"password": c.Password, "host": c.Host, "hostname": c.Hostname,
			"port": c.Port, "pathname": c.Pathname, "search": c.Search, "hash": c.Hash,
		} {
			if got[attr] != want {
				t.Errorf("input %q base %v: %s = %q, want %q", c.Input, c.Base, attr, got[attr], want)
				bad = true
			}
		}
		if c.Origin != nil && got["origin"] != *c.Origin {
			t.Errorf("input %q base %v: origin = %q, want %q", c.Input, c.Base, got["origin"], *c.Origin)
			bad = true
		}
		if bad {
			fail++
		} else {
			pass++
		}
	}
	t.Logf("urltestdata: %d/%d cases pass", pass, pass+fail)
}

// setterCase mirrors one entry of url/resources/setters_tests.json.
type setterCase struct {
	Href     string            `json:"href"`
	New      string            `json:"new_value"`
	Expected map[string]string `json:"expected"`
	Comment  string            `json:"comment"`
}

func TestURLSettersAgainstWPTData(t *testing.T) {
	var file map[string]json.RawMessage
	loadJSON(t, "url/resources/setters_tests.json", &file)

	var pass, fail int
	for attr, raw := range file {
		if attr == "comment" {
			continue
		}
		if _, ok := setterStates[attr]; !ok && attr != "href" {
			continue
		}
		var cases []setterCase
		if err := json.Unmarshal(raw, &cases); err != nil {
			t.Fatalf("%s: %v", attr, err)
		}
		for _, c := range cases {
			u, err := parseURL(c.Href, nil, nil, 0, false)
			if err != nil {
				t.Errorf("%s: href %q did not parse: %v", attr, c.Href, err)
				fail++
				continue
			}
			got := applySetter(u, attr, c.New)
			bad := false
			for k, want := range c.Expected {
				if got[k] != want {
					t.Errorf("%s on %q = %q: %s = %q, want %q", attr, c.Href, c.New, k, got[k], want)
					bad = true
				}
			}
			if bad {
				fail++
			} else {
				pass++
			}
		}
	}
	t.Logf("setters_tests: %d/%d cases pass", pass, pass+fail)
}
