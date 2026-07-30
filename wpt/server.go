package wpt

import (
	"crypto/x509"
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
	ln    net.Listener
	alt   *http.Server
	altLn net.Listener
	// The same two origins again over TLS, for the ".https." tests and the
	// websockets `?wss` variants. See tls.go.
	tlsLn    net.Listener
	altTLSLn net.Listener
	httpsURL string
	roots    *x509.CertPool
	base     string
	subVars  map[string]string
	stash    *stash
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
	cert, roots, err := selfSigned()
	if err != nil {
		ln.Close()
		altLn.Close()
		return nil, fmt.Errorf("minting the harness certificate: %w", err)
	}

	// The base host is "localhost" and the cross-origin host is "127.0.0.1".
	// They resolve to the same listener and are DIFFERENT origins, which is
	// exactly the shape get-host-info.sub.js expects — it derives its remote
	// host as "127.0.0.1" when the original host is "localhost". A made-up
	// name (www1.example) would not resolve and every cross-origin test would
	// fail as a network error instead of testing what it is about.
	srv := &Server{ln: ln, altLn: altLn, roots: roots, base: fmt.Sprintf("http://localhost:%d/", port)}
	srv.stash = newStash()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveWPT(root, srv, w, r)
	})
	srv.Server = &http.Server{Handler: handler}
	srv.alt = &http.Server{Handler: handler}

	// The same two origins again over TLS. Opened before the substitution table
	// is built, because the table has to name their ports.
	tlsLn, tlsPort, err := listenTLS(cert, srv.Server)
	if err != nil {
		ln.Close()
		altLn.Close()
		return nil, err
	}
	altTLSLn, altTLSPort, err := listenTLS(cert, srv.alt)
	if err != nil {
		ln.Close()
		altLn.Close()
		tlsLn.Close()
		return nil, err
	}
	srv.tlsLn, srv.altTLSLn = tlsLn, altTLSLn
	srv.httpsURL = fmt.Sprintf("https://localhost:%d/", tlsPort)
	srv.subVars = map[string]string{
		"host":            "localhost",
		"ports[http][0]":  fmt.Sprint(port),
		"ports[http][1]":  fmt.Sprint(altPort),
		"ports[https][0]": fmt.Sprint(tlsPort),
		"ports[https][1]": fmt.Sprint(altTLSPort),
		// ws shares the http listener and wss the https one, as they do in
		// wptserve: a WebSocket URL and the page that opens it are one origin.
		"ports[ws][0]":  fmt.Sprint(port),
		"ports[wss][0]": fmt.Sprint(tlsPort),
		// There is no HTTP/2 listener. The variable is still defined, because one
		// left unsubstituted makes a test fail while CONSTRUCTING its URL — a
		// harness error, which reads as "the runtime is broken" rather than "this
		// transport is not served here".
		"ports[h2][0]":       fmt.Sprint(tlsPort),
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
	}

	// A request whose header values net/http would reject on arrival is answered
	// by the harness itself, through the same handler. See permissive.go.
	answer := func(conn net.Conn, req *http.Request, head []byte) bool {
		w := &permissiveWriter{header: http.Header{}}
		handler.ServeHTTP(w, req)
		defer conn.Close()
		return w.writeTo(conn) == nil
	}
	go srv.Serve(&permissiveListener{Listener: ln, serve: answer})
	go srv.alt.Serve(&permissiveListener{Listener: altLn, serve: answer})
	return srv, nil
}

// BaseURL is the origin the tests see as their document/worker base.
func (s *Server) BaseURL() string { return s.base }

// SubVars are the suite's server-side substitution variables, which the runner
// also needs: it loads a test's own source and its META scripts from disk, so
// a ".sub." file reaching the guest that way must be substituted too.
func (s *Server) SubVars() map[string]string { return s.subVars }

// Close stops every listener.
func (s *Server) Close() error {
	for _, ln := range []net.Listener{s.tlsLn, s.altTLSLn} {
		if ln != nil {
			ln.Close()
		}
	}
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
	// An upgrade request to one of the suite's WebSocket fixtures is answered on
	// this same listener: {{ports[ws][0]}} IS the HTTP port here, as it is in
	// wptserve, so ws:// and http:// share an origin.
	if serveWebSocket(rel, w, r) {
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
