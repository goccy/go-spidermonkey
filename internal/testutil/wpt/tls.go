package wpt

// tls.go: serving the suite over https as well as http.
//
// A file whose name contains ".https." is one wptserve serves over TLS, and the
// test knows it: it asserts on `location.protocol`, builds `https://` URLs from
// get-host-info.sub.js, or checks something that only exists in a secure
// context. Served over http those tests do not fail for the reason they are
// about — they fail while constructing a URL. The same is true of every
// `variant=?wss` case in websockets/, which is a third of that directory.
//
// So the harness listens on TLS too, with a certificate it mints for itself at
// start-up and hands to the guest as a trusted root. Nothing is written to disk
// and no CA is installed anywhere: the certificate lives as long as the run, and
// only the interpreters this harness creates trust it.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"time"
)

// selfSigned mints a certificate for the loopback names the harness serves
// under. Both hostnames are present because they are two ORIGINS on one
// listener — which is what makes the suite's cross-origin tests testable
// offline (see StartServer).
func selfSigned() (tls.Certificate, *x509.CertPool, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "web-platform-tests harness"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool, nil
}

// HTTPSBaseURL is the origin a ".https." test is loaded from.
func (s *Server) HTTPSBaseURL() string { return s.httpsURL }

// RootCAs is the pool an interpreter must trust for the https listeners to be
// reachable from the guest. The runner passes it to the compat layer; nothing
// outside this process ever sees the certificate.
func (s *Server) RootCAs() *x509.CertPool { return s.roots }

// listenTLS starts one TLS listener on a free loopback port.
func listenTLS(cert tls.Certificate, handler interface {
	Serve(net.Listener) error
}) (net.Listener, int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	// The permissive listener goes UNDER the TLS one: it exists to inspect a
	// request head, and a head is only visible after decryption. A request whose
	// header values net/http refuses therefore reaches the handler over http
	// only, which is where the suite checks it.
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})
	go func() { _ = handler.Serve(tlsLn) }()
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}

// isHTTPSTest applies wptserve's own rule: a file whose name contains ".https."
// is served over TLS. The file NAME is where the suite states this — it is not
// information that existed structurally somewhere else and is being recovered
// from a string here.
func isHTTPSTest(rel string) bool { return strings.Contains(rel, ".https.") }
