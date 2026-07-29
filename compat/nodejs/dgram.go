package nodejs

// dgram.go: UDP sockets for node:dgram over Go net.UDPConn. bind() opens a
// listening socket whose inbound datagrams are posted to the guest as
// 'message' events; send() writes a datagram (Dial-gated).

import (
	"fmt"
	"net"
	"strconv"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func (rt *Runtime) dgramOps() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{
		"udp_bind":  rt.opUDPBind,
		"udp_send":  rt.opUDPSend,
		"udp_close": rt.opUDPClose,
	}
}

// opUDPBind(host, port, onMessage) -> {id, port} | err. onMessage is called
// with (data, rinfoJSON) for each datagram.
func (rt *Runtime) opUDPBind(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("udp_bind: (host, port, onMessage) required")
	}
	host := args[0].String()
	if host == "" {
		host = "127.0.0.1"
	}
	port := args[1].Int()
	onMessage := args[2].Object()
	// The socket's family: a udp4 socket resolves and binds as IPv4 only, which
	// is what makes "localhost" mean 127.0.0.1 rather than ::1 for it.
	network := "udp"
	if len(args) > 3 {
		if n := args[3].String(); n == "udp4" || n == "udp6" {
			network = n
		}
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	if cfg.Listen == nil || !cfg.Listen("udp", addr) {
		freeObjects(onMessage) // no pump will hold this callback for its lifetime
		return spidermonkey.ValueOf(map[string]any{"code": "EACCES", "message": "bind " + addr + ": permission denied"}), nil
	}
	udpAddr, err := net.ResolveUDPAddr(network, addr)
	if err != nil {
		freeObjects(onMessage)
		return netErr(err), nil
	}
	conn, err := net.ListenUDP(network, udpAddr)
	if err != nil {
		freeObjects(onMessage)
		return spidermonkey.ValueOf(map[string]any{"code": "EADDRINUSE", "message": err.Error()}), nil
	}
	st := rt.net
	st.mu.Lock()
	st.nextID++
	id := st.nextID
	st.udp[id] = conn
	st.mu.Unlock()

	rt.loop.AddPending("dgram")
	go rt.pumpUDP(id, conn, onMessage)
	local := conn.LocalAddr().(*net.UDPAddr)
	family := "IPv6"
	if local.IP.To4() != nil || local.IP == nil {
		// A wildcard/unspecified bind reports IPv4 for udp4 (the common case);
		// an explicit IPv6 address reports IPv6.
		family = "IPv4"
	}
	return spidermonkey.ValueOf(map[string]any{
		"id":      id,
		"port":    local.Port,
		"address": local.IP.String(),
		"family":  family,
	}), nil
}

// maxUDPInFlight bounds datagrams read from the socket but not yet delivered to
// the guest. UDP is connectionless — once a port is bound, ANY remote can flood
// it — and message events have no pull-based backpressure, so without this a
// flood would post datagrams onto the loop faster than the guest drains them and
// grow host/guest memory without bound. When the cap is hit we drop, which is
// exactly what the kernel receive buffer does under load (UDP is unreliable).
const maxUDPInFlight = 1024

func (rt *Runtime) pumpUDP(id int64, conn *net.UDPConn, onMessage *spidermonkey.Object) {
	buf := make([]byte, 64<<10)
	credit := make(chan struct{}, maxUDPInFlight)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		// A datagram with no payload is still a datagram — UDP delivers it and
		// Node emits 'message' with an empty Buffer. Gating delivery on n > 0
		// dropped it, so the tests that send an empty packet waited forever.
		delivered := err == nil && addr != nil
		if delivered {
			select {
			case credit <- struct{}{}:
			default:
				// Too many undelivered datagrams in flight: drop this one.
				delivered = false
			}
		}
		if delivered {
			data := append([]byte(nil), buf[:n]...)
			family := "IPv6"
			if addr.IP.To4() != nil {
				family = "IPv4"
			}
			rinfo := map[string]any{"address": addr.IP.String(), "port": addr.Port, "family": family, "size": n}
			rt.loop.Post(func() error {
				defer func() { <-credit }() // release the in-flight slot
				if onMessage != nil {
					u8, uerr := rt.js.NewBytes(data)
					if uerr != nil {
						return nil
					}
					onMessage.Call(u8, spidermonkey.ValueOf(rinfo))
					u8.Free()
				}
				return nil
			})
		}
		if err != nil {
			rt.net.mu.Lock()
			delete(rt.net.udp, id)
			rt.net.mu.Unlock()
			if onMessage != nil {
				rt.loop.Post(func() error { onMessage.Free(); return nil })
			}
			rt.loop.DonePending("dgram")
			return
		}
	}
}

// opUDPSend(id, data, port, host, cb) sends a datagram. The resolve+authorize
// (which may do a blocking DNS lookup for a hostname destination) and the write
// run OFF the loop goroutine — doing them inline would freeze every timer,
// socket, and HTTP response for the resolver timeout. cb(err|null) is posted
// back on the loop.
func (rt *Runtime) opUDPSend(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("udp_send: (id, data, port, host, cb?) required")
	}
	rt.net.mu.Lock()
	conn := rt.net.udp[int64(args[0].Float())]
	rt.net.mu.Unlock()
	data, err := valueBytes(args[1])
	if err != nil {
		return nil, err
	}
	port := args[2].Int()
	host := args[3].String()
	var cb *spidermonkey.Object
	if len(args) > 4 {
		cb = args[4].Object()
	}
	fire := func(e error) {
		rt.loop.Post(func() error {
			if cb != nil {
				if e != nil {
					cb.Call(netErr(e))
				} else {
					cb.Call()
				}
				cb.Free()
			}
			return nil
		})
	}
	if conn == nil {
		rt.loop.AddPending("dgram")
		go func() { defer rt.loop.DonePending("dgram"); fire(fmt.Errorf("socket closed")) }()
		return spidermonkey.Undefined(), nil
	}
	rt.loop.AddPending("dgram")
	go func() {
		defer rt.loop.DonePending("dgram")
		// Resolve+authorize once, then send only to the approved IP.
		// Resolve in the SOCKET's family, taken from what it is bound to: sending
		// an IPv6 destination from a udp4 socket is an error, not a fallback.
		network := "udp"
		if la, ok := conn.LocalAddr().(*net.UDPAddr); ok && la.IP != nil {
			if la.IP.To4() != nil {
				network = "udp4"
			} else {
				network = "udp6"
			}
		}
		dialAddr, e := resolveDialAddr(cfg, network, host, port)
		if e == nil {
			var dst *net.UDPAddr
			if dst, e = net.ResolveUDPAddr(network, dialAddr); e == nil {
				_, e = conn.WriteToUDP(data, dst)
			}
		}
		fire(e)
	}()
	return spidermonkey.Undefined(), nil
}

func (rt *Runtime) opUDPClose(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.Undefined(), nil
	}
	rt.net.mu.Lock()
	conn := rt.net.udp[int64(args[0].Float())]
	rt.net.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
	return spidermonkey.Undefined(), nil
}
