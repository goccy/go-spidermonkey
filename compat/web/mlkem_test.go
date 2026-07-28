package web_test

import (
	"context"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/web"
)

// ML-KEM is the post-quantum key encapsulation mechanism of FIPS 203, and the
// four operations it adds to crypto.subtle (encapsulate/decapsulate, each in a
// bits and a key form) exist nowhere else in the Web Crypto surface. The test
// pins the property that makes it a KEM — both sides reach the same secret —
// plus every key format, since those are hand-encoded DER here.
func TestMLKEM(t *testing.T) {
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
			for (const name of ["ML-KEM-768", "ML-KEM-1024"]) {
				const kp = await crypto.subtle.generateKey({ name }, true,
					["encapsulateBits", "decapsulateBits", "encapsulateKey", "decapsulateKey"]);

				// The KEM property: what the sender encapsulates is what the
				// holder of the private key decapsulates.
				const enc = await crypto.subtle.encapsulateBits({ name }, kp.publicKey);
				const back = await crypto.subtle.decapsulateBits({ name }, kp.privateKey, enc.ciphertext);
				const same = new Uint8Array(enc.sharedKey).join() === new Uint8Array(back).join();
				out.push(name + ":bits:" + enc.sharedKey.byteLength + "/" + enc.ciphertext.byteLength + ":" + (same ? "agree" : "DIFFER"));

				// The key form wraps the same secret as an AES key.
				const ek = await crypto.subtle.encapsulateKey({ name }, kp.publicKey,
					{ name: "AES-GCM", length: 256 }, true, ["encrypt", "decrypt"]);
				const dk = await crypto.subtle.decapsulateKey({ name }, kp.privateKey, ek.ciphertext,
					{ name: "AES-GCM", length: 256 }, true, ["encrypt", "decrypt"]);
				const a = new Uint8Array(await crypto.subtle.exportKey("raw", ek.sharedKey));
				const b = new Uint8Array(await crypto.subtle.exportKey("raw", dk));
				out.push(name + ":key:" + ek.sharedKey.algorithm.name + ":" + (a.join() === b.join() ? "agree" : "DIFFER"));

				// Every format round-trips, and an imported key still decapsulates
				// what the original public key encapsulated.
				for (const [fmt, key] of [["raw-seed", kp.privateKey], ["pkcs8", kp.privateKey],
					["raw-public", kp.publicKey], ["spki", kp.publicKey], ["jwk", kp.privateKey]]) {
					const data = await crypto.subtle.exportKey(fmt, key);
					const usages = key.type === "private" ? ["decapsulateBits"] : ["encapsulateBits"];
					const again = await crypto.subtle.importKey(fmt, data, { name }, true, usages);
					let ok = again.algorithm.name === name && again.type === key.type;
					if (again.type === "private") {
						const s = await crypto.subtle.decapsulateBits({ name }, again, enc.ciphertext);
						ok = ok && new Uint8Array(s).join() === new Uint8Array(enc.sharedKey).join();
					}
					out.push(name + ":" + fmt + ":" + (ok ? "ok" : "BAD"));
				}
			}

			// ML-KEM-512 has no host implementation, and says so rather than
			// producing a key from a different parameter set.
			try { await crypto.subtle.generateKey({ name: "ML-KEM-512" }, true, ["decapsulateBits"]); out.push("512:NO-THROW"); }
			catch (e) { out.push("512:" + e.name); }

			// A usage ML-KEM cannot have is a SyntaxError, as for any algorithm.
			try { await crypto.subtle.generateKey({ name: "ML-KEM-768" }, true, ["sign"]); out.push("usage:NO-THROW"); }
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
	want := "ML-KEM-768:bits:32/1088:agree | ML-KEM-768:key:AES-GCM:agree | " +
		"ML-KEM-768:raw-seed:ok | ML-KEM-768:pkcs8:ok | ML-KEM-768:raw-public:ok | ML-KEM-768:spki:ok | ML-KEM-768:jwk:ok | " +
		"ML-KEM-1024:bits:32/1568:agree | ML-KEM-1024:key:AES-GCM:agree | " +
		"ML-KEM-1024:raw-seed:ok | ML-KEM-1024:pkcs8:ok | ML-KEM-1024:raw-public:ok | ML-KEM-1024:spki:ok | ML-KEM-1024:jwk:ok | " +
		"512:NotSupportedError | usage:SyntaxError"
	if got := v.Value.String(); got != want {
		t.Errorf("ML-KEM =\n %s\nwant\n %s", got, want)
	}
}
