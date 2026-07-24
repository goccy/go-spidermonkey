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
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strconv"
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

// opTLSConnect(host, port, rejectUnauthorized, onData, onEnd, onError, onConnect)
// -> id | err. Uses the net conn table + pumpConn, so the JS TLSSocket is a
// plain socket over an encrypted conn.
func (rt *Runtime) opTLSConnect(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 7 {
		return nil, fmt.Errorf("tls_connect: (host, port, rejectUnauthorized, onData, onEnd, onError, onConnect) required")
	}
	host := args[0].String()
	port := args[1].Int()
	insecure := !args[2].Bool()
	onData := args[3].Object()
	onEnd := args[4].Object()
	onError := args[5].Object()
	onConnect := args[6].Object()

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

	rt.loop.AddPending()
	go func() {
		addr, derr := resolveDialAddr(cfg, "tcp", host, port)
		var conn net.Conn
		if derr == nil {
			// Dial the exact authorized IP (addr) but keep ServerName = host so
			// SNI and certificate verification validate against the hostname.
			conn, derr = tls.DialWithDialer(&net.Dialer{Timeout: 30 * time.Second}, "tcp", addr, &tls.Config{
				ServerName:         host,
				InsecureSkipVerify: insecure,
			})
		}
		if derr != nil {
			st.mu.Lock()
			delete(st.writers, id)
			st.mu.Unlock()
			w.requestClose()
			rt.loop.Post(func() error {
				defer rt.loop.DonePending()
				if onError != nil {
					onError.Call(netErr(derr))
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
		st.mu.Lock()
		st.conns[id] = conn
		st.mu.Unlock()
		w.attach(conn)
		go w.run(func(error) {})
		if onConnect != nil {
			rt.loop.Post(func() error { onConnect.Call(); onConnect.Free(); return nil })
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
	tlsCfg, err := serverTLSConfig(args[2], args[3])
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
	st := rt.net
	st.mu.Lock()
	st.nextID++
	id := st.nextID
	st.listeners[id] = ln
	st.mu.Unlock()

	onConn := args[4].Object()
	rt.loop.AddPending()
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				rt.loop.DonePending()
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
	serverJSID := args[0].Int()
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
	s := &httpServer{id: int64(serverJSID), ln: ln, rt: rt}
	s.srv = &http.Server{Handler: s}
	hs.servers[s.id] = s
	hs.mu.Unlock()

	rt.loop.AddPending()
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
