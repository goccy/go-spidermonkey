package web_test

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// ML-DSA is the post-quantum signature scheme of FIPS 204. The known-answer
// vectors live in the Web Platform Tests; what this pins is what those do not
// reach — that every key format round-trips (the DER is hand-encoded here), that
// a signature is bound to the context string it was made under, and that a
// tampered signature is rejected rather than merely reported differently.
func TestMLDSA(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	w, err := web.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer w.Close()

	if _, err := js.Eval(context.Background(), `
		globalThis.__out = [];
		globalThis.__done = (async () => {
			const out = globalThis.__out;
			const msg = new TextEncoder().encode("post-quantum");
			for (const name of ["ML-DSA-44", "ML-DSA-65", "ML-DSA-87"]) {
				const kp = await crypto.subtle.generateKey({ name }, true, ["sign", "verify"]);
				const sig = await crypto.subtle.sign({ name }, kp.privateKey, msg);
				const ok = await crypto.subtle.verify({ name }, kp.publicKey, sig, msg);
				out.push(name + ":sign:" + sig.byteLength + ":" + (ok ? "ok" : "BAD"));

				// A flipped bit must not verify.
				const bad = new Uint8Array(sig.slice(0));
				bad[0] ^= 1;
				out.push(name + ":tampered:" + (await crypto.subtle.verify({ name }, kp.publicKey, bad, msg) ? "ACCEPTED" : "rejected"));

				// The context string is part of what is signed: a signature made
				// under one context does not verify under another, or under none.
				const ctx = new TextEncoder().encode("ctx");
				const csig = await crypto.subtle.sign({ name, context: ctx }, kp.privateKey, msg);
				const inCtx = await crypto.subtle.verify({ name, context: ctx }, kp.publicKey, csig, msg);
				const noCtx = await crypto.subtle.verify({ name }, kp.publicKey, csig, msg);
				out.push(name + ":context:" + (inCtx ? "ok" : "BAD") + ":" + (noCtx ? "LEAKED" : "bound"));

				// Every format round-trips, and an imported key still produces or
				// checks signatures the original pair agrees with.
				for (const [fmt, key] of [["raw-seed", kp.privateKey], ["pkcs8", kp.privateKey],
					["raw-public", kp.publicKey], ["spki", kp.publicKey], ["jwk", kp.privateKey]]) {
					const data = await crypto.subtle.exportKey(fmt, key);
					const usages = key.type === "private" ? ["sign"] : ["verify"];
					const again = await crypto.subtle.importKey(fmt, data, { name }, true, usages);
					let good = again.algorithm.name === name && again.type === key.type;
					if (again.type === "private") {
						const s = await crypto.subtle.sign({ name }, again, msg);
						good = good && await crypto.subtle.verify({ name }, kp.publicKey, s, msg);
					} else {
						good = good && await crypto.subtle.verify({ name }, again, sig, msg);
					}
					out.push(name + ":" + fmt + ":" + (good ? "ok" : "BAD"));
				}
			}

			// A usage ML-DSA cannot have is a SyntaxError, as for any algorithm.
			try { await crypto.subtle.generateKey({ name: "ML-DSA-44" }, true, ["encrypt"]); out.push("usage:NO-THROW"); }
			catch (e) { out.push("usage:" + e.name); }

			globalThis.__r = out.join(" | ");
		})().catch((e) => { globalThis.__r = "THREW " + e.name + ": " + e.message + " after " + globalThis.__out.join(" | "); });
	`); err != nil {
		t.Fatalf("eval: %v", err)
	}
	if err := w.Wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	v, err := js.Eval(context.Background(), `String(globalThis.__r)`)
	if err != nil {
		t.Fatal(err)
	}
	// The signature sizes are each parameter set's own, and are spelled out so a
	// key silently built for the wrong set cannot pass.
	line := func(name, size string) string {
		return name + ":sign:" + size + ":ok | " + name + ":tampered:rejected | " + name + ":context:ok:bound | " +
			name + ":raw-seed:ok | " + name + ":pkcs8:ok | " + name + ":raw-public:ok | " + name + ":spki:ok | " + name + ":jwk:ok | "
	}
	want := line("ML-DSA-44", "2420") + line("ML-DSA-65", "3309") + line("ML-DSA-87", "4627") + "usage:SyntaxError"
	if got := v.Value.String(); got != want {
		t.Errorf("ML-DSA =\n %s\nwant\n %s", got, want)
	}
}
