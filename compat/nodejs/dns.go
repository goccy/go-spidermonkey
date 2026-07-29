package nodejs

// dns.go: node:dns over Go's resolver.
//
// The module was a stub that failed every call with ENOTSUP, which meant a
// program could not look a name up at all except as a side effect of
// connecting. Node's dns is used directly — service discovery, MX lookups,
// reverse lookups, `dns.promises.resolve` — so the stub was a real capability
// gap, not just a missing test surface.
//
// Every lookup goes through Config.Resolve, the same policy gate the connect
// paths use: a runtime that may not resolve a name may not resolve it here
// either, and the failure is reported with Node's own error shape.

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func (rt *Runtime) dnsOps() map[string]spidermonkey.Func {
	return map[string]spidermonkey.Func{"dns_resolve": rt.opDNSResolve}
}

// dnsTimeout bounds one lookup. Node has no default timeout, but a host call
// that never returns would hold the loop open, and the resolver is off-loop.
const dnsTimeout = 10 * time.Second

// dnsErrValue is Node's error shape for a failed lookup: a code, the syscall
// that failed, and the name it was looking up.
func dnsErrValue(code, syscall, host string) spidermonkey.Value {
	return spidermonkey.ValueOf(map[string]any{
		"__dnsError": true,
		"code":       code,
		"syscall":    syscall,
		"hostname":   host,
		"message":    code + " " + syscall + " " + host,
	})
}

// dnsCode maps a Go resolver error to the code Node reports.
func dnsCode(err error) string {
	var derr *net.DNSError
	if ok := asDNSError(err, &derr); ok {
		switch {
		case derr.IsNotFound:
			return "ENOTFOUND"
		case derr.IsTimeout:
			return "ETIMEOUT"
		}
	}
	return "ESERVFAIL"
}

func asDNSError(err error, out **net.DNSError) bool {
	for err != nil {
		if d, ok := err.(*net.DNSError); ok {
			*out = d
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// opDNSResolve(kind, host) runs one lookup off the loop and posts the result
// back. `kind` is the Node record type ("A", "AAAA", "MX", …) or "lookup" /
// "reverse".
func (rt *Runtime) opDNSResolve(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("dns_resolve: (kind, host, callback) required")
	}
	kind := args[0].String()
	host := args[1].String()
	cb := args[2].Object()

	// The same gate the connect paths use. A reverse lookup names an address
	// rather than a host, and is gated on that address.
	if cfg.Resolve == nil || !cfg.Resolve(host) {
		if cb != nil {
			rt.loop.Post(func() error {
				defer cb.Free()
				cb.Call(dnsErrValue("EPERM", "queryA", host), spidermonkey.Null())
				return nil
			})
		}
		return spidermonkey.Undefined(), nil
	}

	rt.loop.AddPending("dns")
	go func() {
		result, err := dnsLookup(kind, host)
		rt.loop.Post(func() error {
			defer rt.loop.DonePending("dns")
			defer cb.Free()
			if cb == nil {
				return nil
			}
			if err != nil {
				cb.Call(dnsErrValue(dnsCode(err), "query"+strings.ToUpper(kind), host), spidermonkey.Null())
				return nil
			}
			cb.Call(spidermonkey.Null(), spidermonkey.ValueOf(result))
			return nil
		})
	}()
	return spidermonkey.Undefined(), nil
}

// dnsLookup performs the record query. Each shape matches what Node's callback
// receives, so the guest hands it straight to the caller.
func dnsLookup(kind, host string) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dnsTimeout)
	defer cancel()
	r := net.DefaultResolver

	switch strings.ToUpper(kind) {
	case "LOOKUP", "A", "AAAA":
		network := "ip"
		if strings.EqualFold(kind, "A") {
			network = "ip4"
		} else if strings.EqualFold(kind, "AAAA") {
			network = "ip6"
		}
		ips, err := r.LookupIP(ctx, network, host)
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, len(ips))
		for _, ip := range ips {
			family := 6
			if ip.To4() != nil {
				family = 4
			}
			out = append(out, map[string]any{"address": ip.String(), "family": family})
		}
		return out, nil
	case "CNAME":
		name, err := r.LookupCNAME(ctx, host)
		if err != nil {
			return nil, err
		}
		return []any{strings.TrimSuffix(name, ".")}, nil
	case "MX":
		recs, err := r.LookupMX(ctx, host)
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, len(recs))
		for _, m := range recs {
			out = append(out, map[string]any{"exchange": strings.TrimSuffix(m.Host, "."), "priority": int(m.Pref)})
		}
		return out, nil
	case "TXT":
		recs, err := r.LookupTXT(ctx, host)
		if err != nil {
			return nil, err
		}
		// Node returns an array of chunk arrays, one per record.
		out := make([]any, 0, len(recs))
		for _, t := range recs {
			out = append(out, []any{t})
		}
		return out, nil
	case "NS":
		recs, err := r.LookupNS(ctx, host)
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, len(recs))
		for _, n := range recs {
			out = append(out, strings.TrimSuffix(n.Host, "."))
		}
		return out, nil
	case "SRV":
		_, recs, err := r.LookupSRV(ctx, "", "", host)
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, len(recs))
		for _, s := range recs {
			out = append(out, map[string]any{
				"name": strings.TrimSuffix(s.Target, "."), "port": int(s.Port),
				"priority": int(s.Priority), "weight": int(s.Weight),
			})
		}
		return out, nil
	case "PTR", "REVERSE":
		names, err := r.LookupAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, len(names))
		for _, n := range names {
			out = append(out, strings.TrimSuffix(n, "."))
		}
		return out, nil
	}
	return nil, &net.DNSError{Err: "unsupported record type " + kind, Name: host}
}
