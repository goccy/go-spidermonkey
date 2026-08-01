package web

import (
	spidermonkey "github.com/goccy/go-spidermonkey"
	"golang.org/x/net/publicsuffix"
)

// opRegistrableDomain answers with the registrable domain of a host — the
// public suffix plus one label — which is what "same site" compares. The empty
// string means the host HAS no registrable domain (an IP address, a single
// label, or a public suffix itself), in which case same-site falls back to
// exact host equality.
func (w *Web) opRegistrableDomain(cfg spidermonkey.Config, args []spidermonkey.Value) (spidermonkey.Value, error) {
	if len(args) < 1 {
		return spidermonkey.ValueOf(""), nil
	}
	rd, err := publicsuffix.EffectiveTLDPlusOne(strArg(args[0]))
	if err != nil {
		return spidermonkey.ValueOf(""), nil
	}
	return spidermonkey.ValueOf(rd), nil
}
