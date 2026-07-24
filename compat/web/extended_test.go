package web_test

import (
	"testing"

	spidermonkey "github.com/goccy/go-spidermonkey"
)

func TestBlobAndFile(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	runAsync(t, js, `
		(async () => {
			const blob = new Blob(["hello ", "world"], { type: "text/plain" });
			__c.size = blob.size;
			__c.type = blob.type;
			__c.text = await blob.text();
			const sliced = blob.slice(0, 5);
			__c.sliced = await sliced.text();
			const bytes = await blob.bytes();
			__c.firstByte = bytes[0];

			const file = new File(["content"], "note.txt", { type: "text/plain", lastModified: 123 });
			__c.fileName = file.name;
			__c.fileLM = file.lastModified;
			__c.isBlob = file instanceof Blob;
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	for expr, want := range map[string]string{
		"__c.size":      "11",
		"__c.type":      "text/plain",
		"__c.text":      "hello world",
		"__c.sliced":    "hello",
		"__c.firstByte": "104",
		"__c.fileName":  "note.txt",
		"__c.fileLM":    "123",
		"__c.isBlob":    "true",
	} {
		if got := evalString(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}

func TestFormData(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	if got := evalString(t, js, `(() => {
		const fd = new FormData();
		fd.append("name", "goccy");
		fd.append("tag", "a");
		fd.append("tag", "b");
		fd.set("name", "changed");
		return [fd.get("name"), fd.getAll("tag").join("+"), fd.has("missing"), [...fd.keys()].join(",")].join("|")
	})()`); got != "changed|a+b|false|name,tag,tag" {
		t.Errorf("FormData = %s", got)
	}
	// A File value round-trips.
	if got := evalString(t, js, `(() => {
		const fd = new FormData();
		fd.append("upload", new Blob(["data"]), "file.txt");
		const v = fd.get("upload");
		return [v instanceof File, v.name].join("|")
	})()`); got != "true|file.txt" {
		t.Errorf("FormData file = %s", got)
	}
}

func TestStructuredCloneFull(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	if got := evalString(t, js, `(() => {
		const orig = {
			map: new Map([["k", 1]]),
			set: new Set([1, 2, 3]),
			date: new Date(1000),
			re: /abc/gi,
			typed: new Uint8Array([1, 2, 3]),
			nested: { arr: [1, [2, 3]] },
		};
		const c = structuredClone(orig);
		// Mutate the clone; original must be untouched.
		c.map.set("k", 99);
		c.set.add(4);
		c.nested.arr[1][0] = 999;
		return [
			c.map instanceof Map, c.map.get("k"), orig.map.get("k"),
			c.set.has(4), orig.set.has(4),
			c.date.getTime(), c.re.source, c.re.flags,
			c.typed instanceof Uint8Array, c.typed[1],
			orig.nested.arr[1][0], c.nested.arr[1][0],
		].join("|")
	})()`); got != "true|99|1|true|false|1000|abc|gi|true|2|2|999" {
		t.Errorf("structuredClone = %s", got)
	}
	// Cycles are preserved.
	if got := evalString(t, js, `(() => {
		const a = { name: "a" }; a.self = a;
		const c = structuredClone(a);
		return String(c.self === c && c.self.name === "a")
	})()`); got != "true" {
		t.Errorf("structuredClone cycle = %s", got)
	}
	// Functions throw DataCloneError.
	if got := evalString(t, js, `
		(() => { try { structuredClone({ fn: () => {} }); return "no-throw"; } catch (e) { return e.name; } })()
	`); got != "DataCloneError" {
		t.Errorf("structuredClone function = %s", got)
	}
	// Error objects clone their (non-enumerable) message/name and subtype,
	// rather than collapsing to an empty {}.
	if got := evalString(t, js, `(() => {
		const e = new TypeError("boom");
		e.detail = { code: 42 };
		const c = structuredClone(e);
		return [
			c instanceof TypeError, c.name, c.message, c.detail.code,
		].join("|")
	})()`); got != "true|TypeError|boom|42" {
		t.Errorf("structuredClone Error = %s", got)
	}
}

func TestWebStreams(t *testing.T) {
	js, _ := newWeb(t, spidermonkey.Config{})

	runAsync(t, js, `
		(async () => {
			// TransformStream pipe-through.
			const upper = new TransformStream({
				transform(chunk, controller) { controller.enqueue(chunk.toUpperCase()); },
			});
			const readable = new ReadableStream({
				start(c) { c.enqueue("hello"); c.enqueue(" world"); c.close(); },
			});
			const out = readable.pipeThrough(upper);
			const reader = out.getReader();
			let result = "";
			for (;;) { const { value, done } = await reader.read(); if (done) break; result += value; }
			__c.transformed = result;

			// WritableStream sink.
			const sink = [];
			const ws = new WritableStream({ write(chunk) { sink.push(chunk); } });
			const w = ws.getWriter();
			await w.write("a"); await w.write("b"); await w.close();
			__c.sink = sink.join("");

			// TextEncoderStream / TextDecoderStream.
			const enc = new TextEncoderStream();
			const encW = enc.writable.getWriter();
			encW.write("héllo"); encW.close();
			const encR = enc.readable.getReader();
			const parts = [];
			for (;;) { const { value, done } = await encR.read(); if (done) break; parts.push(...value); }
			__c.encoded = parts.length; // utf-8 bytes of "héllo" = 6
		})().catch((e) => { __c.err = String(e && e.stack || e); });
	`)
	for expr, want := range map[string]string{
		"__c.transformed": "HELLO WORLD",
		"__c.sink":        "ab",
		"__c.encoded":     "6",
	} {
		if got := evalString(t, js, expr); got != want {
			t.Errorf("%s = %q, want %q", expr, got, want)
		}
	}
}
