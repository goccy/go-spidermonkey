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
type Server struct {
	*http.Server
	ln   net.Listener
	base string
}

// StartServer serves root on a loopback port until Close.
func StartServer(root string) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	srv := &Server{ln: ln, base: fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)}
	srv.Server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveWPTFile(root, w, r)
	})}
	go srv.Serve(ln)
	return srv, nil
}

// BaseURL is the origin the tests see as their document/worker base.
func (s *Server) BaseURL() string { return s.base }

// serveWPTFile serves one file, honouring the two suite conventions that
// matter offline: a sibling "<name>.headers" file supplies extra response
// headers, and the pipe-separated query handlers WPT uses for its own
// server are not supported (those tests report their own failure).
func serveWPTFile(root string, w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/")
	if rel == "" || strings.Contains(rel, "..") {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	data, err := os.ReadFile(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
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
