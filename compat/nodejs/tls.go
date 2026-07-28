package nodejs

// tls.go: node:tls and TLS-terminating https.createServer over Go crypto/tls.
// TLS sockets reuse the net.go conn table and pump (a *tls.Conn is a
// net.Conn), so tls.connect/createServer are thin wrappers that hand TLS
// connections to the same reader machinery. https servers are http servers
// with a TLS listener.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func (rt *Runtime) tlsOps() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"tls_connect":    rt.opTLSConnect,
		"tls_listen":     rt.opTLSListen,
		"https_listen":   rt.opHTTPSListen,
		"tls_selfsigned": rt.opTLSSelfSigned,
	}
}

// opTLSConnect(host, port, rejectUnauthorized, caPEM, servername,
// onData, onEnd, onError, onConnect) -> id | err. Uses the net conn table +
// pumpConn, so the JS TLSSocket is a plain socket over an encrypted conn.
//
// caPEM (may be empty) is one or more concatenated PEM certificates used to
// build a custom RootCAs pool — needed to verify a server presenting a
// private-CA chain. servername (may be empty) overrides the SNI/verification
// hostname; empty means verify against host.
func (rt *Runtime) opTLSConnect(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 9 {
		return nil, fmt.Errorf("tls_connect: (host, port, rejectUnauthorized, ca, servername, onData, onEnd, onError, onConnect) required")
	}
	host := args[0].String()
	port := args[1].Int()
	insecure := !args[2].Bool()
	caPEM := args[3].String()
	servername := args[4].String()
	onData := args[5].Object()
	onEnd := args[6].Object()
	onError := args[7].Object()
	onConnect := args[8].Object()

	// Build the verification hostname: an explicit servername wins; otherwise
	// the dial host. This value is both the SNI sent and the name the peer
	// certificate is checked against.
	verifyName := host
	if servername != "" {
		verifyName = servername
	}
	// A caller-supplied CA bundle replaces the system roots for verification,
	// so a server chaining to a private CA can be trusted without disabling
	// verification. An unparseable bundle is surfaced as a cert error.
	var rootCAs *x509.CertPool
	if caPEM != "" {
		rootCAs = x509.NewCertPool()
		if !rootCAs.AppendCertsFromPEM([]byte(caPEM)) {
			// Free the four callback handles this op pinned — every other exit
			// path frees them, and skipping it here leaks the guest closures
			// (and their captured TLSSocket) for the runtime's lifetime.
			freeObjects(onData, onEnd, onError, onConnect)
			return spidermonkey.ValueOf(map[string]any{
				"code":    "ERR_TLS_CERT_ALTNAME_INVALID",
				"message": "tls: failed to parse the provided ca certificate(s)",
			}), nil
		}
	}

	// Reserve the socket id synchronously, then resolve + TLS-dial OFF the loop
	// so a slow DNS/connect/handshake can't freeze the single event-loop
	// goroutine. Writes before the handshake completes buffer in the writer.
	st := rt.net
	st.mu.Lock()
	st.nextID++
	id := st.nextID
	w := newConnWriter()
	st.writers[id] = w
	st.mu.Unlock()

	rt.loop.AddPending("tls")
	go func() {
		addrs, derr := resolveDialAddrs(cfg, "tcp", host, port)
		var conn net.Conn
		if derr == nil {
			// Dial the exact authorized IPs (in order, so a loopback name whose
			// first family refuses still connects) but keep ServerName =
			// verifyName so SNI and certificate verification validate against
			// the hostname.
			for _, addr := range addrs {
				conn, derr = tls.DialWithDialer(&net.Dialer{Timeout: 30 * time.Second}, "tcp", addr, &tls.Config{
					ServerName:         verifyName,
					InsecureSkipVerify: insecure,
					RootCAs:            rootCAs,
				})
				if derr == nil {
					break
				}
			}
		}
		if derr != nil {
			st.mu.Lock()
			delete(st.writers, id)
			st.mu.Unlock()
			w.requestClose()
			// ACK any writes queued before the handshake failed (else the
			// socket's Writable hangs on a stranded _write callback).
			go w.run(func(error) {})
			rt.loop.Post(func() error {
				defer rt.loop.DonePending("tls")
				if onError != nil {
					// A certificate/verification failure must NOT masquerade as
					// ECONNREFUSED; map it to a TLS-ish code so guests can tell a
					// trust failure from an unreachable peer.
					onError.Call(tlsDialErr(derr))
				}
				for _, o := range []*spidermonkey.Object{onData, onEnd, onError, onConnect} {
					if o != nil {
						o.Free()
					}
				}
				return nil
			})
			return
		}
		// The guest may have destroyed the socket while the handshake was in
		// flight (opNetClose deleted writers[id]); don't resurrect it — that would
		// emit spurious connect/error/end on an abandoned socket.
		st.mu.Lock()
		_, stillOpen := st.writers[id]
		if stillOpen {
			st.conns[id] = conn
		}
		st.mu.Unlock()
		if !stillOpen {
			conn.Close()
			go w.run(func(error) {}) // ack any writes queued before destroy
			rt.loop.Post(func() error {
				defer rt.loop.DonePending("tls")
				for _, o := range []*spidermonkey.Object{onData, onEnd, onError, onConnect} {
					if o != nil {
						o.Free()
					}
				}
				return nil
			})
			return
		}
		w.attach(conn)
		go w.run(func(error) {})
		// Snapshot the peer certificate now (handshake is complete) so the guest's
		// getPeerCertificate() can return real fields instead of {}.
		cert := peerCertInfo(conn)
		if onConnect != nil {
			rt.loop.Post(func() error { onConnect.Call(cert); onConnect.Free(); return nil })
		}
		rt.pumpConn(id, conn, onData, onEnd, onError)
	}()
	return spidermonkey.ValueOf(id), nil
}

// opTLSListen(host, port, certPEM, keyPEM, onConnection) -> {id, port} | err.
func (rt *Runtime) opTLSListen(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 5 {
		return nil, fmt.Errorf("tls_listen: (host, port, cert, key, onConnection) required")
	}
	host := args[0].String()
	port := args[1].Int()
	// onConnection is GC-pinned at decode; free it on any error return that never
	// reaches the accept loop (which holds it for the listener's lifetime).
	tlsCfg, err := serverTLSConfig(args[2], args[3])
	if err != nil {
		freeObjects(args[4].Object())
		return netErr(err), nil
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	if cfg.Listen == nil || !cfg.Listen("tcp", addr) {
		freeObjects(args[4].Object())
		return spidermonkey.ValueOf(map[string]any{"code": "EACCES", "message": "listen " + addr + ": permission denied"}), nil
	}
	ln, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		freeObjects(args[4].Object())
		return spidermonkey.ValueOf(map[string]any{"code": "EADDRINUSE", "message": err.Error()}), nil
	}
	st := rt.net
	st.mu.Lock()
	st.nextID++
	id := st.nextID
	st.listeners[id] = ln
	st.mu.Unlock()

	onConn := args[4].Object()
	rt.loop.AddPending("tls")
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				// Listener closed: free the accept callback (posted after any
				// queued onConn.Call), then release the pending.
				rt.loop.Post(func() error { freeObjects(onConn); return nil })
				rt.loop.DonePending("tls")
				return
			}
			st.mu.Lock()
			st.nextID++
			cid := st.nextID
			st.mu.Unlock()
			rt.registerConn(cid, conn, newConnWriter(), func(error) {})
			rt.loop.Post(func() error {
				if onConn != nil {
					onConn.Call(spidermonkey.ValueOf(cid), spidermonkey.ValueOf(conn.RemoteAddr().String()))
				}
				return nil
			})
		}
	}()
	return spidermonkey.ValueOf(map[string]any{"id": id, "port": ln.Addr().(*net.TCPAddr).Port}), nil
}

// opHTTPSListen(serverId, host, port, cert, key) -> {id, port} | err. Serves
// the SAME dispatch machinery as http_listen, but over a TLS listener.
func (rt *Runtime) opHTTPSListen(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 5 {
		return nil, fmt.Errorf("https_listen: (serverId, host, port, cert, key) required")
	}
	// args[0] (a guest-supplied id) is IGNORED: allocating the id host-side from
	// the same counter as http_listen prevents a guest from forcing a collision
	// that would overwrite (and leak the fd/goroutine of) a live server. The guest
	// routes on the RETURNED id, so this is transparent.
	host := args[1].String()
	port := args[2].Int()
	tlsCfg, err := serverTLSConfig(args[3], args[4])
	if err != nil {
		return netErr(err), nil
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	if cfg.Listen == nil || !cfg.Listen("tcp", addr) {
		return spidermonkey.ValueOf(map[string]any{"code": "EACCES", "message": "listen " + addr + ": permission denied"}), nil
	}
	ln, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		return spidermonkey.ValueOf(map[string]any{"code": "EADDRINUSE", "message": err.Error()}), nil
	}
	hs := rt.http
	hs.mu.Lock()
	hs.nextID++
	s := &httpServer{id: hs.nextID, ln: ln, rt: rt}
	s.srv = &http.Server{Handler: s}
	hs.servers[s.id] = s
	hs.mu.Unlock()

	rt.loop.AddPending("tls")
	go s.srv.Serve(ln)
	return spidermonkey.ValueOf(map[string]any{"id": s.id, "port": ln.Addr().(*net.TCPAddr).Port}), nil
}

// opTLSSelfSigned(host) -> {cert, key} PEM strings. A convenience so tests
// and simple servers can get a working cert without an external CA.
func (rt *Runtime) opTLSSelfSigned(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	host := "localhost"
	if len(args) > 0 && !args[0].IsUndefined() {
		host = args[0].String()
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return netErr(err), nil
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * 365 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return netErr(err), nil
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(priv)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return spidermonkey.ValueOf(map[string]any{"cert": string(certPEM), "key": string(keyPEM)}), nil
}

// tlsDialErr maps a TLS dial/handshake failure to a guest error object. A
// certificate/verification failure gets a TLS-ish code and a message that
// mentions the certificate, so a guest never mistakes a trust failure for an
// unreachable peer (which netErr would label ECONNREFUSED). Non-cert failures
// (connection refused, timeout) fall through to netErr unchanged.
func tlsDialErr(err error) spidermonkey.Value {
	// verify:true marks a certificate-verification failure, so the guest sets
	// socket.authorizationError structurally instead of matching the code text.
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return spidermonkey.ValueOf(map[string]any{"code": "ERR_TLS_CERT_ALTNAME_INVALID", "message": err.Error(), "verify": true})
	}
	var uaErr x509.UnknownAuthorityError
	if errors.As(err, &uaErr) {
		return spidermonkey.ValueOf(map[string]any{"code": "UNABLE_TO_VERIFY_LEAF_SIGNATURE", "message": err.Error(), "verify": true})
	}
	var ciErr x509.CertificateInvalidError
	if errors.As(err, &ciErr) {
		return spidermonkey.ValueOf(map[string]any{"code": "DEPTH_ZERO_SELF_SIGNED_CERT", "message": err.Error(), "verify": true})
	}
	// tls.CertificateVerificationError wraps the x509 errors above; catch it (and
	// any other verification error) structurally rather than by message text.
	var cvErr *tls.CertificateVerificationError
	if errors.As(err, &cvErr) {
		return spidermonkey.ValueOf(map[string]any{"code": "ERR_TLS_CERT_VERIFICATION", "message": err.Error(), "verify": true})
	}
	return netErr(err)
}

// peerCertInfo snapshots the peer's leaf certificate as a Node-shaped
// getPeerCertificate() object (subject/issuer/validity/fingerprints/SAN/raw).
// raw is base64 (the guest rehydrates it into a Buffer). Returns undefined when
// there is no peer certificate (e.g. a non-TLS conn).
func peerCertInfo(conn net.Conn) spidermonkey.Value {
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return spidermonkey.Undefined()
	}
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return spidermonkey.Undefined()
	}
	c := certs[0]
	sha1Sum := sha1.Sum(c.Raw)
	sha256Sum := sha256.Sum256(c.Raw)
	m := map[string]any{
		"subject":        pkixName(c.Subject),
		"issuer":         pkixName(c.Issuer),
		"valid_from":     c.NotBefore.UTC().Format("Jan _2 15:04:05 2006 GMT"),
		"valid_to":       c.NotAfter.UTC().Format("Jan _2 15:04:05 2006 GMT"),
		"serialNumber":   strings.ToUpper(c.SerialNumber.Text(16)),
		"fingerprint":    colonHex(sha1Sum[:]),
		"fingerprint256": colonHex(sha256Sum[:]),
		"raw":            base64.StdEncoding.EncodeToString(c.Raw),
	}
	if san := subjectAltName(c); san != "" {
		m["subjectaltname"] = san
	}
	return spidermonkey.ValueOf(m)
}

// pkixName renders an X.509 name as Node's getPeerCertificate() subject/issuer
// object (CN/O/OU/C/L/ST). Only populated fields are included.
func pkixName(n pkix.Name) map[string]any {
	out := map[string]any{}
	if n.CommonName != "" {
		out["CN"] = n.CommonName
	}
	if len(n.Organization) > 0 {
		out["O"] = n.Organization[0]
	}
	if len(n.OrganizationalUnit) > 0 {
		out["OU"] = n.OrganizationalUnit[0]
	}
	if len(n.Country) > 0 {
		out["C"] = n.Country[0]
	}
	if len(n.Locality) > 0 {
		out["L"] = n.Locality[0]
	}
	if len(n.Province) > 0 {
		out["ST"] = n.Province[0]
	}
	return out
}

// colonHex renders bytes as upper-case colon-separated hex (Node's fingerprint
// format: "AB:CD:...").
func colonHex(b []byte) string {
	const hex = "0123456789ABCDEF"
	if len(b) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, v := range b {
		if i > 0 {
			sb.WriteByte(':')
		}
		sb.WriteByte(hex[v>>4])
		sb.WriteByte(hex[v&0x0f])
	}
	return sb.String()
}

// subjectAltName renders the cert's SANs in Node's subjectaltname string form
// ("DNS:host, IP Address:1.2.3.4").
func subjectAltName(c *x509.Certificate) string {
	var parts []string
	for _, d := range c.DNSNames {
		parts = append(parts, "DNS:"+d)
	}
	for _, ip := range c.IPAddresses {
		parts = append(parts, "IP Address:"+ip.String())
	}
	for _, e := range c.EmailAddresses {
		parts = append(parts, "email:"+e)
	}
	for _, u := range c.URIs {
		parts = append(parts, "URI:"+u.String())
	}
	return strings.Join(parts, ", ")
}

func serverTLSConfig(certV, keyV spidermonkey.Value) (*tls.Config, error) {
	certPEM, err := valueBytes(certV)
	if err != nil {
		return nil, err
	}
	keyPEM, err := valueBytes(keyV)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("bad cert/key: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}
