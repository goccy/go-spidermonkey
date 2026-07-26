package nodejs_test

import (
	"bytes"
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// util.inspect / console.log used to ignore Symbol.for('nodejs.util.inspect.custom'),
// so objects with a custom-inspect method logged as "{}". The hook must now be
// honored for both util.inspect and console.log formatting.
func TestUtilInspectCustomHook(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const util = require("util");
		globalThis.r = {};
		const custom = Symbol.for("nodejs.util.inspect.custom");
		r.symbolExported = util.inspect.custom === custom;

		// String result is used verbatim.
		const obj = { secret: 42, [custom]() { return "CustomRepr"; } };
		r.stringResult = util.inspect(obj);

		// Non-string result is inspected in turn.
		const wrap = { [custom]() { return { shown: true }; } };
		r.nonString = util.inspect(wrap);

		// The hook receives (depth, opts, inspect).
		let sawArgs = null;
		const probe = { [custom](depth, opts, inspect) { sawArgs = [typeof depth, typeof opts, typeof inspect]; return "ok"; } };
		util.inspect(probe);
		r.args = (sawArgs || []).join(",");

		// A custom error still renders via its hook, not as {}.
		class MyErr extends Error { [custom]() { return "MyErr<custom>"; } }
		r.errInspect = util.inspect(new MyErr("x"));
	`)
	for expr, want := range map[string]string{
		"r.symbolExported": "true",
		"r.stringResult":   "CustomRepr",
		"r.nonString":      "{ shown: true }",
		"r.args":           "number,object,function",
		"r.errInspect":     "MyErr<custom>",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// console.log must route object formatting through the custom-inspect hook too.
func TestConsoleLogUsesCustomInspect(t *testing.T) {
	var stdout bytes.Buffer
	js, rt := newRuntime(t, spidermonkey.Config{Stdout: &stdout})
	_ = js
	runScript(t, rt, `
		const custom = Symbol.for("nodejs.util.inspect.custom");
		const logger = { level: "info", [custom]() { return "Pino<level=info>"; } };
		console.log(logger);
		console.log("prefix", logger);
	`)
	got := stdout.String()
	if want := "Pino<level=info>\nprefix Pino<level=info>\n"; got != want {
		t.Errorf("console.log output = %q, want %q", got, want)
	}
}
