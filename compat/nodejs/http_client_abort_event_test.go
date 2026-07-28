package nodejs_test

import (
	"context"
	"testing"
	"testing/fstest"
	"time"

	spidermonkey "github.com/goccy/go-spidermonkey"
	"github.com/goccy/go-spidermonkey/compat/nodejs"
)

// http.ClientRequest#abort() emits 'abort' exactly once, however many times it
// is called, and sets the deprecated `aborted` flag.
//
// It used to only destroy the request, so nothing that waited on 'abort' ever
// continued — and because such code typically closes its server from that
// handler, the server stayed open and the event loop never went idle. That one
// missing event is why ~195 http tests in Node's suite hang here.
func TestClientRequestAbortEmitsAbortOnce(t *testing.T) {
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
		const http = require("http");
		globalThis.log = [];
		const server = http.createServer((req, res) => res.end());
		server.listen(0, function () {
			const req = http.request({ port: this.address().port }, () => log.push("response"));
			req.on("abort", () => {
				log.push("abort:" + req.aborted);
				server.close(() => log.push("closed"));
			});
			req.end();
			req.abort();
			req.abort(); // a second abort must NOT produce a second event
		});
	`)
	if err != nil {
		t.Fatalf("the run did not finish (%v) — 'abort' never fired and the server stayed open", err)
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
	if got := v.Value.String(); got != "abort:true,closed" {
		t.Errorf("events = %q, want \"abort:true,closed\"", got)
	}
}
