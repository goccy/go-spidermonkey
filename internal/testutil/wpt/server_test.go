package wpt_test

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-spidermonkey/internal/testutil/wpt"
)

// The suite's own server expands `{{name}}` variables in ".sub." files, and
// a large part of fetch/api is written against that: without it every URL
// those tests build contains a literal "{{ports[http][0]}}" and fails to
// parse, which was 381 failing subtests reported only as "Invalid URL: bad
// port". The two ports matter as much as the substitution — they are what
// makes a same-host cross-origin request possible.
func TestServerSubstitutesSuiteVariables(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "vars.sub.js"), []byte(
		`A={{host}}|{{ports[http][0]}}|{{ports[http][1]}}|{{domains[www1]}}|{{unknown[thing]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file without ".sub." in its name is served verbatim, as in WPT.
	if err := os.WriteFile(filepath.Join(root, "plain.js"), []byte(`B={{host}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, err := wpt.StartServer(root)
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	defer srv.Close()

	get := func(path string) string {
		res, err := http.Get(srv.BaseURL() + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}

	vars := srv.SubVars()
	base, err := url.Parse(srv.BaseURL())
	if err != nil {
		t.Fatal(err)
	}
	if base.Hostname() != vars["host"] {
		t.Errorf("BaseURL host = %q, want %q so that {{host}} names the origin under test", base.Hostname(), vars["host"])
	}
	if vars["ports[http][0]"] == vars["ports[http][1]"] {
		t.Errorf("both http ports are %s; the suite needs two distinct origins on one host", vars["ports[http][0]"])
	}
	if vars["domains[www1]"] == vars["host"] {
		t.Errorf("the cross-origin host equals the base host (%q), so no test can be cross-origin", vars["host"])
	}

	got := get("vars.sub.js")
	want := "A=" + vars["host"] + "|" + vars["ports[http][0]"] + "|" + vars["ports[http][1]"] +
		"|" + vars["domains[www1]"] + "|{{unknown[thing]}}"
	if got != want {
		t.Errorf("substituted = %q\nwant           %q", got, want)
	}
	// An unimplemented variable must survive verbatim: blanking it would turn a
	// missing substitution into a silently different request.
	if !strings.Contains(got, "{{unknown[thing]}}") {
		t.Error("an unknown variable was rewritten; it must be left in place so the test fails loudly")
	}
	if got := get("plain.js"); got != "B={{host}}" {
		t.Errorf("non-.sub file = %q, want it served verbatim", got)
	}

	// The second port serves the same tree, which is what makes it usable as a
	// cross-origin peer rather than a dead address.
	res, err := http.Get("http://" + vars["host"] + ":" + vars["ports[http][1]"] + "/plain.js")
	if err != nil {
		t.Fatalf("GET on the alternate port: %v", err)
	}
	defer res.Body.Close()
	if b, _ := io.ReadAll(res.Body); string(b) != "B={{host}}" {
		t.Errorf("alternate port served %q", b)
	}
}
