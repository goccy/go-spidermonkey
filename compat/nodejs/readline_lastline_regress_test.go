package nodejs_test

// readline used to drop a final unterminated line: when the input stream
// ended with data still in the line buffer, that partial line was discarded.
// Node emits it as a final 'line' before 'close' — in both the event form and
// async iteration. The flush belongs to input EOF ONLY: an explicit close()
// discards the buffer and cancels (never answers) a pending question().

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func TestReadlineEmitsFinalUnterminatedLine(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `{
		const readline = require("readline");
		const { PassThrough } = require("stream");
		globalThis.r = { order: [] };

		// Event form: input ends without a trailing newline.
		const input = new PassThrough();
		const rl = readline.createInterface({ input });
		rl.on("line", (l) => r.order.push("line:" + l));
		rl.on("close", () => r.order.push("close"));
		input.write("first\nsecond\nlast-without-newline");
		input.end();

		// Explicit close() does NOT flush the partial buffer (Node emits only
		// 'close'), and a pending question() is cancelled, not answered.
		const input2 = new PassThrough();
		const rl2 = readline.createInterface({ input: input2 });
		globalThis.r2 = { order: [], answer: "NOT-CALLED" };
		rl2.on("line", (l) => r2.order.push("line:" + l));
		rl2.on("close", () => r2.order.push("close"));
		rl2.question("q? ", (a) => { r2.answer = a; });
		input2.write("partial");
		setTimeout(() => rl2.close(), 10);

		// CRLF: the trailing \r of a final CRLF-less-\n line is stripped.
		const input3 = new PassThrough();
		const rl3 = readline.createInterface({ input: input3 });
		globalThis.r3 = { lines: [] };
		rl3.on("line", (l) => r3.lines.push(l));
		input3.write("a\r\ntail\r");
		input3.end();
	}`)
	if got, want := evalStr(t, js, `r.order.join("|")`), "line:first|line:second|line:last-without-newline|close"; got != want {
		t.Errorf("event-form order = %q, want %q (final line must precede close)", got, want)
	}
	if got, want := evalStr(t, js, `r2.order.join("|")`), "close"; got != want {
		t.Errorf("explicit close() order = %q, want %q (no partial-line flush)", got, want)
	}
	if got, want := evalStr(t, js, `r2.answer`), "NOT-CALLED"; got != want {
		t.Errorf("question() cb after close() = %q, want %q (close cancels, never answers)", got, want)
	}
	if got, want := evalStr(t, js, `r3.lines.join("|")`), "a|tail"; got != want {
		t.Errorf("CRLF final line = %q, want %q", got, want)
	}
}

func TestReadlineAsyncIterationIncludesFinalLine(t *testing.T) {
	js, rt := newRuntime(t, spidermonkey.Config{})
	runScript(t, rt, `{
		const readline = require("readline");
		const { PassThrough } = require("stream");
		globalThis.r = {};
		const input = new PassThrough();
		const rl = readline.createInterface({ input });
		(async () => {
			const lines = [];
			for await (const line of rl) lines.push(line);
			r.lines = lines.join("|");
		})();
		input.write("alpha\nbeta\ngamma-no-newline");
		input.end();
	}`)
	if got, want := evalStr(t, js, `r.lines`), "alpha|beta|gamma-no-newline"; got != want {
		t.Errorf("async iteration lines = %q, want %q (final unterminated line dropped)", got, want)
	}
}
