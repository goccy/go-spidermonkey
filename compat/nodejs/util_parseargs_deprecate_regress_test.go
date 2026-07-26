package nodejs_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// util.parseArgs used to ignore tokens:true. With it set, the result must also
// carry a `tokens` array in Node's shape (option/positional/option-terminator).
func TestParseArgsTokens(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const util = require("util");
		globalThis.r = {};
		const { values, positionals, tokens } = util.parseArgs({
			args: ["--name=foo", "-v", "pos1", "--", "after"],
			options: { name: { type: "string" }, verbose: { type: "boolean", short: "v" } },
			allowPositionals: true,
			tokens: true,
		});
		r.values = JSON.stringify(values);
		r.positionals = positionals.join(",");
		r.kinds = tokens.map(t => t.kind).join(",");
		const nameTok = tokens.find(t => t.name === "name");
		r.nameInline = nameTok.inlineValue;
		r.nameValue = nameTok.value;
		r.nameRaw = nameTok.rawName;
		const vTok = tokens.find(t => t.name === "verbose");
		r.vValue = String(vTok.value);
		r.vInline = String(vTok.inlineValue);
		const term = tokens.find(t => t.kind === "option-terminator");
		r.termIndex = term.index;
		const posTok = tokens.filter(t => t.kind === "positional").map(t => t.value).join(",");
		r.posValues = posTok;

		// Without tokens:true, no tokens key is present.
		const plain = util.parseArgs({ args: [], options: {} });
		r.noTokens = ("tokens" in plain);
	`)
	for expr, want := range map[string]string{
		"r.values":      `{"name":"foo","verbose":true}`,
		"r.positionals": "pos1,after",
		"r.kinds":       "option,option,positional,option-terminator,positional",
		"r.nameInline":  "true",
		"r.nameValue":   "foo",
		"r.nameRaw":     "--name",
		"r.vValue":      "undefined",
		"r.vInline":     "undefined",
		"r.termIndex":   "3",
		"r.posValues":   "pos1,after",
		"r.noTokens":    "false",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

// util.deprecate used to be the identity function. It must now return a wrapper
// that calls through and emits a one-time DeprecationWarning via emitWarning.
func TestUtilDeprecateEmitsOnce(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `
		const util = require("util");
		globalThis.r = { warnings: [] };
		process.on("warning", (w) => r.warnings.push(w.name + ":" + w.message + ":" + (w.code || "")));
		const add = util.deprecate((a, b) => a + b, "add() is deprecated, use plus()", "DEP0001");
		r.call1 = add(2, 3);
		r.call2 = add(10, 20);
		r.call3 = add(1, 1);
	`)
	// The warning is emitted on nextTick; runScript drains it.
	for expr, want := range map[string]string{
		"r.call1": "5",
		"r.call2": "30",
		"r.call3": "2",
	} {
		if got := evalStr(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
	if got := evalStr(t, js, `r.warnings.length`); got != "1" {
		t.Errorf("deprecation warning count = %s, want 1 (emitted once)", got)
	}
	if got := evalStr(t, js, `r.warnings[0]`); got != "DeprecationWarning:add() is deprecated, use plus():DEP0001" {
		t.Errorf("warning = %q", got)
	}
}
