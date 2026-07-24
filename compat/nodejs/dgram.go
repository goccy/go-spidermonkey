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

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	if cfg.Listen == nil || !cfg.Listen("udp", addr) {
		return spidermonkey.ValueOf(map[string]any{"code": "EACCES", "message": "bind " + addr + ": permission denied"}), nil
	}
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return netErr(err), nil
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return spidermonkey.ValueOf(map[string]any{"code": "EADDRINUSE", "message": err.Error()}), nil
	}
	st := rt.net
	st.mu.Lock()
	st.nextID++
	id := st.nextID
	st.udp[id] = conn
	st.mu.Unlock()

	rt.loop.AddPending()
	go rt.pumpUDP(id, conn, onMessage)
	return spidermonkey.ValueOf(map[string]any{"id": id, "port": conn.LocalAddr().(*net.UDPAddr).Port}), nil
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
		if n > 0 {
			select {
			case credit <- struct{}{}:
			default:
				// Too many undelivered datagrams in flight: drop this one.
				n = 0
			}
		}
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			rinfo := map[string]any{"address": addr.IP.String(), "port": addr.Port, "family": "IPv4", "size": n}
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
			rt.loop.DonePending()
			return
		}
	}
}

func (rt *Runtime) opUDPSend(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 4 {
		return nil, fmt.Errorf("udp_send: (id, data, port, host) required")
	}
	rt.net.mu.Lock()
	conn := rt.net.udp[int64(args[0].Float())]
	rt.net.mu.Unlock()
	if conn == nil {
		return netErr(fmt.Errorf("socket closed")), nil
	}
	data, err := valueBytes(args[1])
	if err != nil {
		return nil, err
	}
	port := args[2].Int()
	host := args[3].String()
	// Resolve+authorize once, then send only to the approved IP.
	dialAddr, err := resolveDialAddr(cfg, "udp", host, port)
	if err != nil {
		return netErr(err), nil
	}
	dst, err := net.ResolveUDPAddr("udp", dialAddr)
	if err != nil {
		return netErr(err), nil
	}
	if _, err := conn.WriteToUDP(data, dst); err != nil {
		return netErr(err), nil
	}
	return spidermonkey.ValueOf(map[string]any{"ok": true}), nil
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
