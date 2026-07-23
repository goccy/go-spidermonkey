// compat/nodejs: node:stream (compact behavioral implementation —
// Readable/Writable/Duplex/Transform/PassThrough, pipe, finished/pipeline),
// node:string_decoder, and the fs stream constructors. Evaluated after
// corelibs.js. Sized for the Express dependency tree (body parsing reads
// 'data'/'end'; responses write/end; send pipes file streams), not spec
// completeness.
(() => {
	"use strict";
	const core = globalThis.__node_core_registry;
	const EventEmitter = core.events;
	const fsMod = core.fs;

	// ----------------------------------------------------- string_decoder

	function utf8TailLength(u8) {
		// Bytes of an incomplete trailing UTF-8 sequence (0 if none).
		const n = u8.length;
		for (let i = 1; i <= 3 && i <= n; i++) {
			const b = u8[n - i];
			if ((b & 0xc0) !== 0x80) { // lead byte
				let expected = 1;
				if ((b & 0xe0) === 0xc0) expected = 2;
				else if ((b & 0xf0) === 0xe0) expected = 3;
				else if ((b & 0xf8) === 0xf0) expected = 4;
				return i < expected ? i : 0;
			}
		}
		return 0;
	}

	// A function-style constructor on purpose: iconv-lite invokes it as
	// `StringDecoder.call(this, enc)`, which a class constructor rejects.
	function StringDecoder(encoding) {
		this.encoding = String(encoding || "utf8").toLowerCase().replace("utf-8", "utf8");
		this._pending = null;
	}
	StringDecoder.prototype.write = function write(buf) {
		if (typeof buf === "string") return buf;
		let data = Buffer.from(buf.buffer ? new Uint8Array(buf.buffer, buf.byteOffset, buf.byteLength) : buf);
		if (this.encoding !== "utf8") return data.toString(this.encoding);
		if (this._pending) {
			data = Buffer.concat([this._pending, data]);
			this._pending = null;
		}
		const tail = utf8TailLength(data);
		if (tail > 0) {
			this._pending = Buffer.from(data.subarray(data.length - tail));
			data = data.subarray(0, data.length - tail);
		}
		return new TextDecoder().decode(data);
	};
	StringDecoder.prototype.end = function end(buf) {
		let out = buf ? this.write(buf) : "";
		if (this._pending) {
			out += new TextDecoder().decode(this._pending); // incomplete -> U+FFFD
			this._pending = null;
		}
		return out;
	};
	core.string_decoder = { StringDecoder };

	// ------------------------------------------------------------ Readable

	function toChunk(chunk, encoding) {
		if (typeof chunk === "string") return Buffer.from(chunk, encoding || "utf8");
		return chunk;
	}

	class Readable extends EventEmitter {
		constructor(options = {}) {
			super();
			this._rs = {
				buffer: [],
				flowing: null,
				ended: false,
				endEmitted: false,
				destroyed: false,
				decoder: null,
				flowScheduled: false,
				pipes: [],
			};
			this.readable = true;
			this.readableEnded = false;
			if (typeof options.read === "function") this._read = options.read;
			if (typeof options.destroy === "function") this._destroy = options.destroy;
			if (options.encoding) this.setEncoding(options.encoding);
		}
		_read(size) {}
		push(chunk, encoding) {
			const st = this._rs;
			if (chunk === null) {
				st.ended = true;
				this._scheduleFlow();
				return false;
			}
			st.buffer.push(toChunk(chunk, encoding));
			if (st.flowing) this._scheduleFlow();
			else this.emit("readable");
			return true;
		}
		unshift(chunk, encoding) {
			this._rs.buffer.unshift(toChunk(chunk, encoding));
		}
		read(size) {
			const st = this._rs;
			if (st.buffer.length === 0) {
				if (st.ended) this._scheduleFlow(); // deliver 'end'
				else this._callRead();
				if (st.buffer.length === 0) return null;
			}
			let out;
			if (size === undefined || size >= totalLength(st.buffer)) {
				out = st.buffer.length === 1 ? st.buffer[0] : Buffer.concat(st.buffer);
				st.buffer = [];
			} else {
				const joined = Buffer.concat(st.buffer);
				out = joined.subarray(0, size);
				st.buffer = [joined.subarray(size)];
			}
			if (st.ended && st.buffer.length === 0) this._scheduleFlow();
			return st.decoder ? st.decoder.write(out) : out;
		}
		setEncoding(enc) {
			this._rs.decoder = new StringDecoder(enc);
			return this;
		}
		on(type, fn) {
			super.on(type, fn);
			if (type === "data" && this._rs.flowing === null) this.resume();
			return this;
		}
		addListener(type, fn) { return this.on(type, fn); }
		resume() {
			const st = this._rs;
			if (st.flowing !== true) {
				st.flowing = true;
				this.emit("resume");
				this._scheduleFlow();
			}
			return this;
		}
		pause() {
			if (this._rs.flowing !== false) {
				this._rs.flowing = false;
				this.emit("pause");
			}
			return this;
		}
		isPaused() { return this._rs.flowing === false; }
		_callRead() {
			try { this._read(16384); } catch (e) { this.destroy(e); }
		}
		_scheduleFlow() {
			const st = this._rs;
			if (st.flowScheduled) return;
			st.flowScheduled = true;
			process.nextTick(() => {
				st.flowScheduled = false;
				this._flowNow();
			});
		}
		_flowNow() {
			const st = this._rs;
			if (st.destroyed) return;
			while (st.flowing && st.buffer.length) {
				let chunk = st.buffer.shift();
				if (st.decoder) chunk = st.decoder.write(chunk);
				this.emit("data", chunk);
			}
			if (st.flowing && st.buffer.length === 0 && !st.ended) this._callRead();
			if (st.ended && st.buffer.length === 0 && !st.endEmitted && st.flowing !== false) {
				st.endEmitted = true;
				this.readable = false;
				this.readableEnded = true;
				if (st.decoder) {
					const rest = st.decoder.end();
					if (rest) this.emit("data", rest);
				}
				this.emit("end");
				this.emit("close");
			}
		}
		pipe(dest, options = {}) {
			const st = this._rs;
			const rec = { dest };
			rec.onData = (chunk) => {
				if (dest.write(chunk) === false) this.pause();
			};
			rec.onDrain = () => this.resume();
			rec.onEnd = () => { if (options.end !== false) dest.end(); };
			this.on("data", rec.onData);
			dest.on("drain", rec.onDrain);
			this.on("end", rec.onEnd);
			st.pipes.push(rec);
			dest.emit("pipe", this);
			return dest;
		}
		unpipe(dest) {
			const st = this._rs;
			for (let i = st.pipes.length - 1; i >= 0; i--) {
				const rec = st.pipes[i];
				if (dest && rec.dest !== dest) continue;
				this.off("data", rec.onData);
				rec.dest.off("drain", rec.onDrain);
				this.off("end", rec.onEnd);
				st.pipes.splice(i, 1);
				rec.dest.emit("unpipe", this);
			}
			return this;
		}
		destroy(err) {
			const st = this._rs;
			if (st.destroyed) return this;
			st.destroyed = true;
			this.readable = false;
			this.destroyed = true;
			const done = (e) => {
				if (e) this.emit("error", e);
				this.emit("close");
			};
			if (this._destroy) this._destroy(err, done);
			else done(err);
			return this;
		}
		[Symbol.asyncIterator]() {
			const chunks = [];
			let ended = false, error = null, wake = null;
			this.on("data", (c) => { chunks.push(c); if (wake) wake(); });
			this.on("end", () => { ended = true; if (wake) wake(); });
			this.on("error", (e) => { error = e; if (wake) wake(); });
			const next = async () => {
				for (;;) {
					if (chunks.length) return { value: chunks.shift(), done: false };
					if (error) throw error;
					if (ended) return { value: undefined, done: true };
					await new Promise((res) => { wake = res; });
					wake = null;
				}
			};
			return { next, [Symbol.asyncIterator]() { return this; } };
		}
	}

	function totalLength(chunks) {
		let n = 0;
		for (const c of chunks) n += c.length;
		return n;
	}

	// ------------------------------------------------------------ Writable

	function initWritable(self, options = {}) {
		self._ws = { ending: false, finished: false, pending: 0, destroyed: false };
		self.writable = true;
		self.writableEnded = false;
		self.writableFinished = false;
		if (typeof options.write === "function") self._write = options.write;
		if (typeof options.final === "function") self._final = options.final;
		if (typeof options.destroy === "function" && !self._destroy) self._destroy = options.destroy;
	}

	const writableMethods = {
		_write(chunk, encoding, callback) { callback(); },
		write(chunk, encoding, callback) {
			if (typeof encoding === "function") { callback = encoding; encoding = null; }
			const st = this._ws;
			if (st.ending || st.destroyed) {
				const err = new Error("write after end");
				err.code = "ERR_STREAM_WRITE_AFTER_END";
				if (callback) process.nextTick(() => callback(err));
				this.emit("error", err);
				return false;
			}
			st.pending++;
			let returned = true;
			this._write(toChunk(chunk, encoding), encoding || "utf8", (err) => {
				st.pending--;
				if (err) { this.destroy(err); if (callback) callback(err); return; }
				if (callback) callback();
				if (st.pending === 0 && !st.ending) this.emit("drain");
				this._maybeFinish();
			});
			return returned;
		},
		end(chunk, encoding, callback) {
			if (typeof chunk === "function") { callback = chunk; chunk = null; }
			else if (typeof encoding === "function") { callback = encoding; encoding = null; }
			if (chunk !== null && chunk !== undefined) this.write(chunk, encoding);
			const st = this._ws;
			st.ending = true;
			this.writableEnded = true;
			if (callback) this.once("finish", callback);
			this._maybeFinish();
			return this;
		},
		_maybeFinish() {
			const st = this._ws;
			if (!st.ending || st.finished || st.pending > 0 || st.destroyed) return;
			st.finished = true;
			const done = () => {
				this.finished = true;
				this.writable = false;
				this.writableFinished = true;
				this.emit("finish");
				this.emit("close");
			};
			if (this._final) this._final((err) => { if (err) this.destroy(err); else done(); });
			else done();
		},
		destroy(err) {
			const st = this._ws;
			if (st.destroyed) return this;
			st.destroyed = true;
			this.writable = false;
			this.destroyed = true;
			const done = (e) => {
				if (e) this.emit("error", e);
				this.emit("close");
			};
			if (this._destroy) this._destroy(err, done);
			else done(err);
			return this;
		},
		cork() {},
		uncork() {},
		setDefaultEncoding() { return this; },
	};

	class Writable extends EventEmitter {
		constructor(options) {
			super();
			initWritable(this, options);
		}
	}
	Object.assign(Writable.prototype, writableMethods);

	// --------------------------------------- Duplex / Transform / PassThrough

	class Duplex extends Readable {
		constructor(options) {
			super(options);
			initWritable(this, options);
		}
	}
	for (const [name, fn] of Object.entries(writableMethods)) {
		if (name !== "destroy") Duplex.prototype[name] = fn;
	}
	Duplex.prototype.destroy = function destroy(err) {
		return writableMethods.destroy.call(this, err);
	};

	class Transform extends Duplex {
		constructor(options = {}) {
			super(options);
			if (typeof options.transform === "function") this._transform = options.transform;
			if (typeof options.flush === "function") this._flush = options.flush;
		}
		_transform(chunk, encoding, callback) { callback(null, chunk); }
		_write(chunk, encoding, callback) {
			this._transform(chunk, encoding, (err, out) => {
				if (err) return callback(err);
				if (out !== null && out !== undefined) this.push(out);
				callback();
			});
		}
		_final(callback) {
			const finish = (err) => {
				this.push(null);
				callback(err);
			};
			if (this._flush) this._flush((err, out) => {
				if (out !== null && out !== undefined) this.push(out);
				finish(err);
			});
			else finish();
		}
	}

	class PassThrough extends Transform {}

	// ------------------------------------------------------------- helpers

	function finished(stream, options, callback) {
		if (typeof options === "function") { callback = options; options = {}; }
		let called = false;
		const done = (err) => {
			if (called) return;
			called = true;
			callback(err || null);
		};
		stream.once("error", done);
		stream.once("end", () => done());
		stream.once("finish", () => done());
		stream.once("close", () => done());
		return () => {};
	}

	function pipeline(...args) {
		const callback = typeof args[args.length - 1] === "function" ? args.pop() : () => {};
		let current = args[0];
		for (let i = 1; i < args.length; i++) {
			args[i - 1].once("error", (e) => callback(e));
			current = args[i - 1].pipe(args[i]);
		}
		finished(current, callback);
		return current;
	}

	const streamMod = Object.assign(
		class Stream extends EventEmitter {},
		{ Readable, Writable, Duplex, Transform, PassThrough, Stream: null, finished, pipeline },
	);
	streamMod.Stream = streamMod;
	core.stream = streamMod;

	// -------------------------------------------------- fs stream flavors

	fsMod.createReadStream = (p, options = {}) => {
		if (typeof options === "string") options = { encoding: options };
		let delivered = false;
		const rs = new Readable({
			read() {
				if (delivered) return;
				delivered = true;
				try {
					let data = fsMod.readFileSync(p);
					const start = options.start ?? 0;
					// fs stream `end` is INCLUSIVE.
					const end = options.end !== undefined ? options.end + 1 : data.length;
					if (start !== 0 || end !== data.length) data = data.subarray(start, end);
					this.push(data);
					this.push(null);
					process.nextTick(() => rs.emit("open"), 0);
				} catch (e) {
					this.destroy(e);
				}
			},
		});
		if (options.encoding) rs.setEncoding(options.encoding);
		rs.path = p;
		rs.close = (cb) => { rs.destroy(); if (cb) process.nextTick(cb); };
		return rs;
	};

	fsMod.createWriteStream = (p, options = {}) => {
		let first = true;
		const ws = new Writable({
			write(chunk, encoding, callback) {
				try {
					if (first && !(options.flags || "").includes("a")) {
						fsMod.writeFileSync(p, chunk);
						first = false;
					} else {
						fsMod.appendFileSync(p, chunk);
						first = false;
					}
					callback();
				} catch (e) {
					callback(e);
				}
			},
		});
		ws.path = p;
		ws.close = (cb) => ws.end(cb);
		return ws;
	};
})();
