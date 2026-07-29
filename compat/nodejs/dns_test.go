package nodejs_test

import (
	"context"
	"testing"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// node:dns was a stub that failed every call with ENOTSUP, so a program could
// not look a name up at all except as a side effect of connecting. The test
// resolves only "localhost", which every machine answers without a network.
func TestDNSModule(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{
		Resolve: func(host string) bool { return host == "localhost" || host == "127.0.0.1" },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := rt.RunScript(ctx, `
		const dns = require("dns");
		globalThis.r = {};
		const done = [];
		const step = (name, p) => done.push(p.then((v) => { r[name] = v; }, (e) => { r[name] = "!" + e.code; }));

		// The shapes are Node's: lookup gives one address, lookup({all}) gives
		// the list, resolve4 gives bare address strings.
		step("lookup", new Promise((res, rej) =>
			dns.lookup("localhost", (e, addr, fam) => e ? rej(e) : res(typeof addr + ":" + (fam === 4 || fam === 6)))));
		step("all", new Promise((res, rej) =>
			dns.lookup("localhost", { all: true }, (e, list) => e ? rej(e) : res(Array.isArray(list) && list.length > 0 && "address" in list[0]))));
		step("resolve4", new Promise((res, rej) =>
			dns.resolve4("localhost", (e, list) => e ? rej(e) : res(Array.isArray(list) && typeof list[0] === "string"))));

		// The policy gate applies here exactly as it does to a connect.
		step("denied", new Promise((res) =>
			dns.lookup("example.invalid", (e) => res(e ? e.code : "NO-ERROR"))));

		// An unknown record type is rejected before any lookup happens.
		try { dns.resolve("localhost", "NOPE", () => {}); r.badtype = "NO-THROW"; }
		catch (e) { r.badtype = e.code; }

		// The promise form, and a Resolver instance that round-trips its servers.
		step("promises", dns.promises.lookup("localhost").then((v) => typeof v.address === "string"));
		const res = new dns.Resolver();
		res.setServers(["1.2.3.4"]);
		r.servers = res.getServers().join(",");

		globalThis.__wait = Promise.all(done);
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	for _, c := range []struct{ expr, want string }{
		{`r.lookup`, "string:true"},
		{`String(r.all)`, "true"},
		{`String(r.resolve4)`, "true"},
		{`r.denied`, "EPERM"},
		{`r.badtype`, "ERR_INVALID_ARG_VALUE"},
		{`String(r.promises)`, "true"},
		{`r.servers`, "1.2.3.4"},
	} {
		if got := evalStr(t, js, c.expr); got != c.want {
			t.Errorf("%s = %q, want %q", c.expr, got, c.want)
		}
	}
}
