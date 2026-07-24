package nodejs_test

import (
	"bytes"
	"strings"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func TestConsoleExtended(t *testing.T) {
	var stdout bytes.Buffer
	_, rt := newRuntime(t, spidermonkey.Config{Stdout: &stdout})

	runScript(t, rt, `
		console.group("outer");
		console.log("inside");
		console.groupEnd();
		console.count("x");
		console.count("x");
		console.countReset("x");
		console.count("x");
		console.table([{ a: 1, b: 2 }, { a: 3, b: 4 }]);
	`)
	out := stdout.String()
	if !strings.Contains(out, "outer\n  inside") {
		t.Errorf("group indent missing: %q", out)
	}
	if !strings.Contains(out, "x: 1") || !strings.Contains(out, "x: 2") {
		t.Errorf("count output missing: %q", out)
	}
	if !strings.Contains(out, "(index)") || !strings.Contains(out, "a | b") {
		t.Errorf("table header missing: %q", out)
	}
}

func TestEventsExtended(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const EventEmitter = require("events");
		globalThis.r = {};

		// on() async iterator
		const em = new EventEmitter();
		(async () => {
			const collected = [];
			const iter = EventEmitter.on(em, "tick");
			(async () => {
				for await (const [n] of iter) {
					collected.push(n);
					if (collected.length === 3) { iter.return(); }
				}
			})();
			// Emit after the iterator is set up.
			await Promise.resolve();
			em.emit("tick", 1);
			em.emit("tick", 2);
			em.emit("tick", 3);
			await new Promise((res) => setTimeout(res, 20));
			r.collected = collected.join(",");
		})();

		// statics
		const em2 = new EventEmitter();
		const listener = () => {};
		em2.on("evt", listener);
		r.count = EventEmitter.listenerCount(em2, "evt");
		r.listeners = EventEmitter.getEventListeners(em2, "evt").length;
		r.errorMonitor = typeof EventEmitter.errorMonitor;
	`)
	if got := evalStr(t, js, `r.collected`); got != "1,2,3" {
		t.Errorf("events.on async iterator = %q", got)
	}
	if got := evalStr(t, js, `String(r.count)`); got != "1" {
		t.Errorf("listenerCount = %q", got)
	}
	if got := evalStr(t, js, `r.errorMonitor`); got != "symbol" {
		t.Errorf("errorMonitor = %q", got)
	}
}

func TestUtilExtended(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const util = require("util");
		globalThis.r = {};

		// parseArgs
		const { values, positionals } = util.parseArgs({
			args: ["--name", "goccy", "--verbose", "-c", "5", "file.txt"],
			options: {
				name: { type: "string" },
				verbose: { type: "boolean" },
				count: { type: "string", short: "c" },
			},
			allowPositionals: true,
		});
		r.name = values.name;
		r.verbose = values.verbose;
		r.count = values.count;
		r.positional = positionals.join(",");

		// styleText
		r.styled = util.styleText("red", "x");

		// stripVTControlCharacters
		r.stripped = util.stripVTControlCharacters("\x1b[31mred\x1b[39m");

		// MIMEType
		const mt = new util.MIMEType("text/html; charset=utf-8");
		r.mime = mt.type + "/" + mt.subtype + " " + mt.params.get("charset");
	`)
	for expr, want := range map[string]string{
		"r.name":       "goccy",
		"r.verbose":    "true",
		"r.count":      "5",
		"r.positional": "file.txt",
		"r.styled":     "\x1b[31mx\x1b[39m",
		"r.stripped":   "red",
		"r.mime":       "text/html utf-8",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestAliasModules(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		globalThis.r = {};
		r.assertStrict = typeof require("assert/strict").strictEqual;
		r.pathPosix = require("path/posix").sep;
		r.sys = require("sys") === require("util");
		const consumers = require("stream/consumers");
		r.hasConsumers = typeof consumers.text === "function" && typeof consumers.json === "function";
	`)
	for expr, want := range map[string]string{
		"r.assertStrict": "function",
		"r.pathPosix":    "/",
		"r.sys":          "true",
		"r.hasConsumers": "true",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestStreamConsumers(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})

	runScript(t, rt, `
		const { Readable } = require("stream");
		const consumers = require("stream/consumers");
		globalThis.r = {};
		(async () => {
			r.text = await consumers.text(Readable.from(["hello ", "world"]));
			r.json = JSON.stringify(await consumers.json(Readable.from(['{"a":', '1}'])));
			const buf = await consumers.buffer(Readable.from([Buffer.from("ab"), Buffer.from("cd")]));
			r.buffer = buf.toString();
		})().catch((e) => { r.err = String(e); });
	`)
	if got := evalStr(t, js, `r.err ?? ""`); got != "" {
		t.Fatalf("consumers rejected: %s", got)
	}
	for expr, want := range map[string]string{
		"r.text":   "hello world",
		"r.json":   `{"a":1}`,
		"r.buffer": "abcd",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestOSExtended(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const os = require("os");
		globalThis.r = {};
		r.hasConstants = typeof os.constants.signals.SIGINT === "number";
		r.devNull = os.devNull;
		r.eol = JSON.stringify(os.EOL);
		r.userInfo = os.userInfo().username;
	`)
	if got := evalStr(t, js, `r.hasConstants`); got != "true" {
		t.Error("os.constants.signals missing")
	}
	if got := evalStr(t, js, `r.devNull`); got != "/dev/null" {
		t.Errorf("os.devNull = %q", got)
	}
	if got := evalStr(t, js, `r.eol`); got != `"\n"` {
		t.Errorf("os.EOL = %q", got)
	}
}

func TestProcessExtended(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		globalThis.r = {};
		r.builtinModule = typeof process.getBuiltinModule("path").join;
		process.ref(); process.unref(); // must not throw
		r.hrtime = Array.isArray(process.hrtime());
		r.hrtimeBig = typeof process.hrtime.bigint() === "bigint";
	`)
	if got := evalStr(t, js, `r.builtinModule`); got != "function" {
		t.Errorf("process.getBuiltinModule = %q", got)
	}
	if got := evalStr(t, js, `r.hrtime`); got != "true" {
		t.Error("process.hrtime not array")
	}
	if got := evalStr(t, js, `r.hrtimeBig`); got != "true" {
		t.Error("process.hrtime.bigint not bigint")
	}
}

func TestVMBasic(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const vm = require("vm");
		globalThis.r = {};
		r.result = vm.runInThisContext("1 + 2 + 3");
		const fn = vm.compileFunction("return a * b", ["a", "b"]);
		r.fn = fn(6, 7);
	`)
	if got := evalStr(t, js, `String(r.result)`); got != "6" {
		t.Errorf("vm.runInThisContext = %q", got)
	}
	if got := evalStr(t, js, `String(r.fn)`); got != "42" {
		t.Errorf("vm.compileFunction = %q", got)
	}
	_ = spidermonkey.Undefined
}
