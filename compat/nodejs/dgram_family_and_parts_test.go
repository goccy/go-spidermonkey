package nodejs_test

import (
	"context"
	"testing"
	"testing/fstest"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
)

// A udp4 socket sending to "localhost" must resolve in the IPv4 family, and
// send() must accept a LIST of parts and deliver their concatenation as one
// datagram.
//
// Both were broken together, and the visible symptom was a HANG rather than a
// failure: "localhost" resolved to ::1, the send from the v4 socket errored, so
// the 'message' event never arrived, so nothing closed the socket and the event
// loop never went idle. That is the shape of 32 quarantined dgram tests.
func TestDgramResolvesInSocketFamilyAndSendsParts(t *testing.T) {
	js, err := spidermonkey.New(spidermonkey.Config{
		FS:      fstest.MapFS{},
		Listen:  func(network, addr string) bool { return true },
		Dial:    func(network, host, ip string, port int) bool { return true },
		Resolve: func(host string) bool { return true },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer js.Close()
	rt, err := nodejs.Install(js)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	defer rt.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	r, err := rt.RunScript(ctx, `
		const dgram = require("dgram");
		globalThis.log = [];
		const socket = dgram.createSocket("udp4");
		socket.on("message", (msg) => { log.push("msg:" + msg.toString()); socket.close(); });
		socket.on("error", (e) => log.push("error:" + e.message));
		socket.bind(() => socket.send(["foo", "bar", "baz"], socket.address().port, "localhost"));
	`)
	if err != nil {
		t.Fatalf("the run did not finish (%v) — the datagram never arrived, so nothing closed the socket", err)
	}
	if r.Error != nil {
		t.Fatalf("threw: %v", r.Error)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %v; the loop only ended at the deadline", elapsed)
	}
	v, err := js.Eval(context.Background(), `globalThis.log.join(",")`)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Value.String(); got != "msg:foobarbaz" {
		t.Errorf("events = %q, want \"msg:foobarbaz\"", got)
	}
}
