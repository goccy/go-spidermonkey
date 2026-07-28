package nodejs_test

import (
	"strings"
	"testing"
	"testing/fstest"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

// util.styleText must accept every style Node's util.inspect.colors lists.
// It THROWS on an unknown one, so a missing entry is not a cosmetic gap: it
// turns a library's colourized output into an exception. @babel/code-frame asks
// for "bgRed" while formatting a syntax error, and the TypeError replaced the
// error Babel was actually reporting — which is how this was found.
func TestStyleTextKnowsEveryNodeColor(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{FS: fstest.MapFS{}})
	runScript(t, rt, `
		const { styleText } = require("util");
		globalThis.r = { bad: [], sample: "" };
		const styles = [
			"reset", "bold", "dim", "italic", "underline", "blink", "inverse",
			"hidden", "strikethrough", "doubleunderline", "framed", "overlined",
			"black", "red", "green", "yellow", "blue", "magenta", "cyan", "white",
			"gray", "grey", "blackBright", "redBright", "greenBright",
			"yellowBright", "blueBright", "magentaBright", "cyanBright",
			"whiteBright", "bgBlack", "bgRed", "bgGreen", "bgYellow", "bgBlue",
			"bgMagenta", "bgCyan", "bgWhite", "bgGray", "bgGrey", "bgBlackBright",
			"bgRedBright", "bgGreenBright", "bgYellowBright", "bgBlueBright",
			"bgMagentaBright", "bgCyanBright", "bgWhiteBright",
		];
		for (const s of styles) {
			try { styleText(s, "x"); } catch (e) { r.bad.push(s); }
		}
		r.sample = styleText(["bgRed", "white"], "x");
	`)
	if got := evalStr(t, js, `r.bad.join(",")`); got != "" {
		t.Errorf("styleText rejected: %s", got)
	}
	// The composed form wraps the text in both styles, innermost last.
	if got := evalStr(t, js, `JSON.stringify(r.sample)`); !strings.Contains(got, "41m") || !strings.Contains(got, "37m") {
		t.Errorf("styleText([\"bgRed\",\"white\"]) = %s, want both SGR codes", got)
	}
}
