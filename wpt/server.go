package wpt

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Server serves the WPT checkout over loopback HTTP. A large share of the
// suite fetches its own fixtures (urltestdata.json, the encoding tables, the
// fetch/api resources), which needs a real origin rather than a filesystem
// read: the tests use fetch(), and fetch() is one of the things under test.
//
// It listens on TWO loopback ports, because the suite's own server does and a
// good part of fetch/api is written against that: `{{ports[http][0]}}` and
// `{{ports[http][1]}}` are meant to be distinct origins on the same host.
type Server struct {
	*http.Server
	ln      net.Listener
	alt     *http.Server
	altLn   net.Listener
	base    string
	subVars map[string]string
	stash   *stash
}

// StartServer serves root on two loopback ports until Close.
func StartServer(root string) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	altLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		ln.Close()
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	altPort := altLn.Addr().(*net.TCPAddr).Port

	// The base host is "localhost" and the cross-origin host is "127.0.0.1".
	// They resolve to the same listener and are DIFFERENT origins, which is
	// exactly the shape get-host-info.sub.js expects — it derives its remote
	// host as "127.0.0.1" when the original host is "localhost". A made-up
	// name (www1.example) would not resolve and every cross-origin test would
	// fail as a network error instead of testing what it is about.
	srv := &Server{
		ln:    ln,
		altLn: altLn,
		base:  fmt.Sprintf("http://localhost:%d/", port),
		subVars: map[string]string{
			"host":               "localhost",
			"ports[http][0]":     fmt.Sprint(port),
			"ports[http][1]":     fmt.Sprint(altPort),
			"ports[https][0]":    fmt.Sprint(port),
			"ports[https][1]":    fmt.Sprint(altPort),
			"ports[ws][0]":       fmt.Sprint(port),
			"ports[wss][0]":      fmt.Sprint(altPort),
			"domains[]":          "localhost",
			"domains[www]":       "127.0.0.1",
			"domains[www1]":      "127.0.0.1",
			"domains[www2]":      "127.0.0.1",
			"hosts[alt][]":       "127.0.0.1",
			"hosts[alt][www]":    "127.0.0.1",
			"hosts[alt][www1]":   "127.0.0.1",
			"hosts[alt][www2]":   "127.0.0.1",
			"hosts[][]":          "localhost",
			"location[host]":     fmt.Sprintf("localhost:%d", port),
			"location[hostname]": "localhost",
			"location[port]":     fmt.Sprint(port),
		},
	}
	srv.stash = newStash()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveWPT(root, srv, w, r)
	})
	srv.Server = &http.Server{Handler: handler}
	srv.alt = &http.Server{Handler: handler}
	go srv.Serve(ln)
	go srv.alt.Serve(altLn)
	return srv, nil
}

// BaseURL is the origin the tests see as their document/worker base.
func (s *Server) BaseURL() string { return s.base }

// SubVars are the suite's server-side substitution variables, which the runner
// also needs: it loads a test's own source and its META scripts from disk, so
// a ".sub." file reaching the guest that way must be substituted too.
func (s *Server) SubVars() map[string]string { return s.subVars }

// Close stops both listeners.
func (s *Server) Close() error {
	if s.alt != nil {
		s.alt.Close()
	}
	return s.Server.Close()
}

// substituteWPT expands the suite's `{{name}}` server-side variables. WPT
// applies this to files whose name contains ".sub."; anything else is served
// verbatim. An unknown variable is LEFT IN PLACE rather than blanked, so a
// substitution this server does not implement fails loudly (as a bad URL)
// instead of silently becoming a different, wrong request.
func substituteWPT(rel string, data []byte, vars map[string]string) []byte {
	if !strings.Contains(rel, ".sub.") || !strings.Contains(string(data), "{{") {
		return data
	}
	out := string(data)
	for name, value := range vars {
		out = strings.ReplaceAll(out, "{{"+name+"}}", value)
	}
	return []byte(out)
}

// serveWPTFile serves one file, honouring the suite conventions that matter
// offline: a sibling "<name>.headers" file supplies extra response headers,
// and ".sub." files get their server-side variables substituted. The
// pipe-separated query handlers WPT uses for its own server are not supported
// (those tests report their own failure).
func serveWPT(root string, srv *Server, w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/")
	if rel == "" || strings.Contains(rel, "..") {
		http.NotFound(w, r)
		return
	}
	// A ported fixture handler answers instead of the file. Anything without
	// one falls through and is served as before.
	if h := handlerFor(rel); h != nil && h(srv.stash, w, r) {
		return
	}
	vars := srv.subVars
	full := filepath.Join(root, filepath.FromSlash(rel))
	data, err := os.ReadFile(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data = substituteWPT(rel, data, vars)
	if hdr, err := os.ReadFile(full + ".headers"); err == nil {
		for _, line := range strings.Split(string(hdr), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
				w.Header().Add(strings.TrimSpace(k), strings.TrimSpace(v))
			}
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", contentType(rel))
	}
	// WPT's cross-origin tests need the suite's own server; permissive CORS
	// here is what lets the same-origin majority run.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(data)
}

func contentType(rel string) string {
	switch {
	case strings.HasSuffix(rel, ".json"):
		return "application/json"
	case strings.HasSuffix(rel, ".js"):
		return "text/javascript"
	case strings.HasSuffix(rel, ".html"):
		return "text/html"
	case strings.HasSuffix(rel, ".txt"):
		return "text/plain"
	case strings.HasSuffix(rel, ".css"):
		return "text/css"
	}
	return "application/octet-stream"
}
