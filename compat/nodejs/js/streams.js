// compat/nodejs: node:stream (compact behavioral implementation —
// Readable/Writable/Duplex/Transform/PassThrough, pipe, finished/pipeline),
// node:string_decoder, and the fs stream constructors. Evaluated after
// corelibs.js.
//
// Every constructor here is FUNCTION-style, never class syntax: the
// util.inherits generation of npm packages (send, iconv-lite, ...) calls
// `Stream.call(this)` / `Transform.call(this, opts)`, which class
// constructors reject.
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

	function StringDecoder(encoding) {
		this.encoding = String(encoding || "utf8").toLowerCase().replace("utf-8", "utf8");
		this._pending = null;
	}
	// unitSize returns the byte width that must not be split across chunks for a
	// non-utf8 encoding: 2 for utf16le (a code unit), 3 for base64 (a group that
	// encodes to 4 chars). Byte-independent encodings (hex/latin1/ascii) return 1.
	function unitSize(enc) {
		if (enc === "utf16le" || enc === "utf-16le" || enc === "ucs2" || enc === "ucs-2") return 2;
		if (enc === "base64" || enc === "base64url") return 3;
		return 1;
	}
	StringDecoder.prototype.write = function write(buf) {
		if (typeof buf === "string") return buf;
		let data = Buffer.from(buf.buffer ? new Uint8Array(buf.buffer, buf.byteOffset, buf.byteLength) : buf);
		if (this._pending) {
			data = Buffer.concat([this._pending, data]);
			this._pending = null;
		}
		if (this.encoding === "utf8") {
			const tail = utf8TailLength(data);
			if (tail > 0) {
				this._pending = Buffer.from(data.subarray(data.length - tail));
				data = data.subarray(0, data.length - tail);
			}
			return new TextDecoder().decode(data);
		}
		// Multi-byte encodings: hold back the incomplete trailing unit/group so a
		// code unit (utf16le) or base64 group isn't decoded across the boundary,
		// which would corrupt or drop characters.
		const unit = unitSize(this.encoding);
		if (unit > 1) {
			const rem = data.length % unit;
			if (rem > 0) {
				this._pending = Buffer.from(data.subarray(data.length - rem));
				data = data.subarray(0, data.length - rem);
			}
		}
		return data.toString(this.encoding);
	};
	StringDecoder.prototype.end = function end(buf) {
		let out = buf ? this.write(buf) : "";
		if (this._pending) {
			out += this.encoding === "utf8"
				? new TextDecoder().decode(this._pending) // incomplete -> U+FFFD
				: this._pending.toString(this.encoding);
			this._pending = null;
		}
		return out;
	};
	core.string_decoder = { StringDecoder };

	// ------------------------------------------------------------ Readable

	// emitClose fires 'close' exactly once, and for a Duplex only once BOTH halves
	// are done (ended/finished or destroyed). A plain Readable closes after 'end',
	// a plain Writable after 'finish'. Without this a Duplex (net.Socket, etc.)
	// emits 'close' twice — once per half — breaking pool/keep-alive teardown.
	// The writable half counts as done only once 'finish' has actually been
	// EMITTED (finishEmitted), not merely committed (finished): 'finish' is
	// deferred to a nextTick, and Node guarantees 'close' is the LAST event — a
	// Duplex whose 'end' handler calls end() (the auto-end-after-peer-FIN socket
	// path) must not slip 'close' out before the deferred 'finish'.
	function emitClose(self) {
		if (self._closeEmitted) return;
		// An HTTP ClientRequest finishes its writable half as soon as the body
		// is sent, but its 'close' means the ROUND TRIP is over — the response
		// completed, or the request was destroyed. It says so with this flag,
		// and emits its own close at those points.
		if (self._closeOnRoundTrip && !(self._ws && self._ws.destroyed)) return;
		const rs = self._rs, ws = self._ws;
		const rDone = !rs || rs.endEmitted || rs.destroyed;
		const wDone = !ws || ws.finishEmitted || ws.destroyed;
		if (!rDone || !wDone) return;
		// Node's autoDestroy: a stream whose halves have BOTH completed is
		// destroyed, and 'close' is emitted by that destroy. Emitting 'close'
		// without destroying left `destroyed` false and, more importantly, never
		// ran _destroy — so a net.Socket that ended normally kept its host
		// connection (and the event loop) alive for good.
		//
		// It is opt-in rather than Node's blanket default because this layer
		// reaches "both halves done" earlier than Node does for the HTTP
		// messages: a ClientRequest is a Writable that finishes as soon as the
		// body is sent, long before its response arrives, and destroying it
		// there would cancel the request.
		const alreadyDestroyed = (rs && rs.destroyed) || (ws && ws.destroyed);
		if (!alreadyDestroyed && self.autoDestroy === true && typeof self.destroy === "function") {
			self.destroy();
			return; // destroy() calls back here once _destroy has run
		}
		self._closeEmitted = true;
		self.emit("close");
	}

	function toChunk(chunk, encoding) {
		if (typeof chunk === "string") return Buffer.from(chunk, encoding || "utf8");
		return chunk;
	}

	function totalLength(chunks) {
		let n = 0;
		for (const c of chunks) n += c.length;
		return n;
	}

	function Readable(options = {}) {
		EventEmitter.call(this);
		this._rs = {
			buffer: [],
			length: 0, // objectMode: item count; byte mode: total bytes
			highWaterMark: options.highWaterMark ?? (options.objectMode ? 16 : 16384),
			objectMode: !!(options.objectMode || options.readableObjectMode),
			flowing: null,
			ended: false,
			endEmitted: false,
			destroyed: false,
			decoder: null,
			flowScheduled: false,
			consumed: false,
			needReadable: false,
			pipes: [],
			// Node's own fields, carried so a caller that branches on them sees a
			// value rather than undefined.
			closed: false, errored: null, errorEmitted: false, constructed: true,
			reading: false, readingMore: false, resumeScheduled: false, sync: true,
			emittedReadable: false, readableListening: false, awaitDrainWriters: null,
			defaultEncoding: options.defaultEncoding || "utf8",
			emitClose: options.emitClose !== false,
			autoDestroy: options.autoDestroy !== false,
			encoding: options.encoding || null,
		};
		this.readable = true;
		if (typeof options.read === "function") this._read = options.read;
		if (typeof options.destroy === "function") this._destroy = options.destroy;
		if (options.encoding) this.setEncoding(options.encoding);
	}
	Object.setPrototypeOf(Readable.prototype, EventEmitter.prototype);
	Object.setPrototypeOf(Readable, EventEmitter);

	function chunkSize(st, chunk) {
		return st.objectMode ? 1 : (chunk.length ?? 0);
	}

	// Getters must be defined with defineProperties, not Object.assign (which
	// would invoke them during the copy).
	// The public state accessors Node exposes on every stream: destroyed,
	// closed and errored read whichever side(s) the stream has. They live on
	// the PROTOTYPES — an instance property set at destroy time answered true
	// after the fact but undefined before it, and the suite asserts the
	// `false`s as strictly as the `true`s.
	const streamStateGetters = {
		destroyed: {
			get() {
				return !!((this._rs && this._rs.destroyed) || (this._ws && this._ws.destroyed));
			},
			set(v) {
				if (this._rs) this._rs.destroyed = !!v;
				if (this._ws) this._ws.destroyed = !!v;
			},
			configurable: true,
		},
		closed: {
			get() {
				return this._closeEmitted === true ||
					!!((this._rs && this._rs.closed) || (this._ws && this._ws.closed));
			},
			configurable: true,
		},
		errored: {
			get() {
				return (this._rs && this._rs.errored) || (this._ws && this._ws.errored) ||
					this._errored || null;
			},
			configurable: true,
		},
	};

	Object.defineProperties(Readable.prototype, {
		...streamStateGetters,
		readableEnded: { get() { return !!this._rs.endEmitted; }, configurable: true },
		readableAborted: {
			get() { return !!(this._rs.destroyed && !this._rs.endEmitted); },
			configurable: true,
		},
		// Node calls this state object _readableState, and reaches into it from
		// its own libraries as well as its suite — `stream._readableState.pipes`
		// is how you ask what a stream is piped to. Keeping it under a private
		// name of our own made every one of those reads a TypeError.
		_readableState: { get() { return this._rs; }, configurable: true },
		readableHighWaterMark: { get() { return this._rs.highWaterMark; }, configurable: true },
		readableLength: { get() { return this._rs.length; }, configurable: true },
		readableObjectMode: { get() { return this._rs.objectMode; }, configurable: true },
		readableFlowing: { get() { return this._rs.flowing; }, configurable: true },
	});

	Object.assign(Readable.prototype, {
		_read(size) {},
		push(chunk, encoding) {
			const st = this._rs;
			// A push after the stream is destroyed/closed is discarded (Node returns
			// false); without this a paused-mode push would emit 'readable' and
			// deliver data to a consumer AFTER 'close' already fired.
			if (st.destroyed) return false;
			if (chunk === null) {
				st.ended = true;
				// In paused mode, wake a read(size) consumer so it drains a
				// trailing residual smaller than its last requested size (which
				// read now withholds as null) before 'end'.
				if (st.flowing !== true && st.buffer.length > 0) {
					process.nextTick(() => { if (!st.endEmitted && !st.destroyed) this.emit("readable"); });
				}
				this._scheduleFlow();
				return false;
			}
			if (st.ended) {
				// A chunk after push(null) is a producer bug; Node errors the
				// stream rather than silently delivering data after 'end'.
				const err = new Error("stream.push() after EOF");
				err.code = "ERR_STREAM_PUSH_AFTER_EOF";
				this.destroy(err);
				return false;
			}
			const item = st.objectMode ? chunk : toChunk(chunk, encoding);
			st.buffer.push(item);
			st.length += chunkSize(st, item);
			if (st.flowing) this._scheduleFlow();
			else this.emit("readable");
			// Backpressure signal: false tells the producer to stop until read.
			return st.length < st.highWaterMark;
		},
		unshift(chunk, encoding) {
			const st = this._rs;
			const item = st.objectMode ? chunk : toChunk(chunk, encoding);
			st.buffer.unshift(item);
			st.length += chunkSize(st, item);
		},
		read(size) {
			const st = this._rs;
			if (st.destroyed) return null; // Node: read() after destroy() yields null, not buffered data
			st.consumed = true; // paused-mode consumer exists: 'end' may emit
			if (st.buffer.length === 0) {
				if (st.ended) this._scheduleFlow(); // deliver 'end'
				else this._callRead();
				if (st.buffer.length === 0) return null;
			}
			if (st.objectMode) {
				// One object per read (size is ignored in objectMode).
				const out = st.buffer.shift();
				st.length -= 1;
				if (st.ended && st.buffer.length === 0) this._scheduleFlow();
				return out;
			}
			const avail = totalLength(st.buffer);
			// Node contract: if `size` bytes aren't buffered and the stream
			// hasn't ended, read(size) returns null (wait for more) — length-
			// prefixed / fixed-header parsers depend on this.
			if (size !== undefined && size > avail && !st.ended) {
				this._callRead(); // nudge the source to produce more
				return null;
			}
			let out;
			if (size === undefined || size >= avail) {
				out = st.buffer.length === 1 ? st.buffer[0] : Buffer.concat(st.buffer);
				st.buffer = [];
				st.length = 0;
			} else {
				const joined = Buffer.concat(st.buffer);
				out = joined.subarray(0, size);
				st.buffer = [joined.subarray(size)];
				st.length = joined.length - size;
			}
			if (st.ended && st.buffer.length === 0) this._scheduleFlow();
			return st.decoder ? st.decoder.write(out) : out;
		},
		setEncoding(enc) {
			this._rs.decoder = new StringDecoder(enc);
			return this;
		},
		on(type, fn) {
			EventEmitter.prototype.on.call(this, type, fn);
			if (type === "data" && this._rs.flowing === null) this.resume();
			return this;
		},
		addListener(type, fn) { return this.on(type, fn); },
		resume() {
			const st = this._rs;
			if (st.flowing !== true) {
				st.flowing = true;
				this.emit("resume");
				this._scheduleFlow();
			}
			return this;
		},
		pause() {
			if (this._rs.flowing !== false) {
				this._rs.flowing = false;
				this.emit("pause");
			}
			return this;
		},
		isPaused() { return this._rs.flowing === false; },
		_callRead() {
			try { this._read(16384); } catch (e) { this.destroy(e); }
		},
		_scheduleFlow() {
			const st = this._rs;
			if (st.flowScheduled) return;
			st.flowScheduled = true;
			process.nextTick(() => {
				st.flowScheduled = false;
				this._flowNow();
			});
		},
		_flowNow() {
			const st = this._rs;
			if (st.destroyed) return;
			// Re-check destroyed EACH iteration: a 'data' listener commonly calls
			// stream.destroy() (early-abort / read-to-marker / size-limit), and the
			// remaining buffered chunks must NOT be emitted after 'close'.
			while (st.flowing && st.buffer.length && !st.destroyed) {
				let chunk = st.buffer.shift();
				st.length -= chunkSize(st, chunk);
				if (st.decoder && !st.objectMode) chunk = st.decoder.write(chunk);
				this.emit("data", chunk);
			}
			if (st.destroyed) return;
			if (st.flowing && st.buffer.length === 0 && !st.ended) this._callRead();
			// 'end' fires only once a consumer exists (flowing via 'data'/
			// resume, or paused-mode read()) — Node never ends a stream nobody
			// started reading, and late listeners must still get their data.
			if (st.ended && st.buffer.length === 0 && !st.endEmitted && (st.flowing === true || st.consumed)) {
				st.endEmitted = true;
				this.readable = false;
				if (st.decoder) {
					const rest = st.decoder.end();
					if (rest) this.emit("data", rest);
				}
				this.emit("end");
				emitClose(this);
			}
		},
		pipe(dest, options = {}) {
			const st = this._rs;
			const rec = { dest };
			rec.onData = (chunk) => {
				if (dest.write(chunk) === false) this.pause();
			};
			rec.onDrain = () => this.resume();
			rec.onEnd = () => { if (options.end !== false) dest.end(); };
			// Tear down when the destination dies (close/error): unpipe and, unless
			// the source already ended, destroy it — so an upstream (e.g. a proxied
			// http body) is released rather than leaking its connection/goroutine,
			// and no write-after-destroy error is emitted on the dead destination.
			rec.onClose = () => {
				this.unpipe(dest);
				if (options.end !== false && !st.ended && !st.destroyed) this.destroy();
			};
			rec.onError = () => rec.onClose();
			this.on("data", rec.onData);
			dest.on("drain", rec.onDrain);
			this.on("end", rec.onEnd);
			dest.once("close", rec.onClose);
			dest.once("error", rec.onError);
			st.pipes.push(rec);
			dest.emit("pipe", this);
			// Node's pipe() always starts the flow. Attaching a 'data' listener only
			// resumes a never-started source (flowing===null); an explicitly paused
			// source (flowing===false) needs an explicit resume, else nothing flows.
			if (st.flowing === false) this.resume();
			return dest;
		},
		unpipe(dest) {
			const st = this._rs;
			for (let i = st.pipes.length - 1; i >= 0; i--) {
				const rec = st.pipes[i];
				if (dest && rec.dest !== dest) continue;
				this.off("data", rec.onData);
				rec.dest.off("drain", rec.onDrain);
				this.off("end", rec.onEnd);
				rec.dest.off("close", rec.onClose);
				rec.dest.off("error", rec.onError);
				st.pipes.splice(i, 1);
				rec.dest.emit("unpipe", this);
			}
			return this;
		},
		destroy(err) {
			const st = this._rs;
			if (st.destroyed) return this;
			st.destroyed = true;
			st.closed = true;
			this.readable = false;
			if (err) { st.errored = err; this._errored = err; } // so a later finished() reports the error
			const done = (e) => {
				if (e) this.emit("error", e);
				emitClose(this);
			};
			if (this._destroy) this._destroy(err, done);
			else done(err);
			return this;
		},
	});

	Readable.prototype[Symbol.asyncIterator] = function asyncIterator() {
		const self = this;
		const st = self._rs;
		// Demand-driven, PAUSED-mode iteration. Each next() pulls at most one
		// chunk with read(); read() nudges the source (_callRead) only when the
		// buffer is empty, so the source is pulled exactly as fast as the
		// consumer consumes — bounded by highWaterMark. This is Node's behavior.
		//
		// The old implementation attached a plain 'data' listener, which the on()
		// override turns into resume() -> permanent FLOWING mode: _flowNow then
		// pulls the source as fast as it can produce, into an unbounded in-memory
		// array, regardless of how slowly the consumer iterates. A slow for-await
		// over a large source (e.g. a big HTTP body) buffered the ENTIRE body in
		// guest heap and aborted the instance at MaxMemoryBytes. We must NOT flip
		// the stream into flowing mode here.
		let error = null, ended = false, wake = null;
		// In paused mode the ONLY 'data' the stream emits is the decoder flush at
		// 'end' (setEncoding's trailing residual, from _flowNow). Capture it with a
		// listener attached via the raw EventEmitter.on so the on() override does
		// NOT resume()/flip us into flowing mode. read()'s decoder.write handles
		// every other chunk; this only rescues that final residual.
		const residual = [];
		const wakeUp = () => { if (wake) { const w = wake; wake = null; w(); } };
		const onData = (c) => { residual.push(c); wakeUp(); };
		const onReadable = () => wakeUp();
		const onEnd = () => { ended = true; wakeUp(); };
		const onError = (e) => { error = e; wakeUp(); };
		// destroy() emits 'close' WITHOUT 'end'/'error'; end the iteration (with a
		// premature-close error if it wasn't cleanly ended) so `for await` doesn't
		// hang forever. 'close' fires after 'end', so a clean end sets no error.
		const onClose = () => {
			if (!ended && !error) error = Object.assign(new Error("Premature close"), { code: "ERR_STREAM_PREMATURE_CLOSE" });
			ended = true;
			wakeUp();
		};
		EventEmitter.prototype.on.call(self, "data", onData);
		self.on("readable", onReadable);
		self.on("end", onEnd);
		self.on("error", onError);
		self.on("close", onClose);
		let cleaned = false;
		const cleanup = () => {
			if (cleaned) return;
			cleaned = true;
			self.off("data", onData); self.off("readable", onReadable);
			self.off("end", onEnd); self.off("error", onError); self.off("close", onClose);
		};
		const next = async () => {
			for (;;) {
				// Deliver a decoder-flush residual (if any) before ending.
				if (residual.length) return { value: residual.shift(), done: false };
				if (error) { cleanup(); const e = error; error = null; throw e; }
				// Pull one chunk on demand. read() releases exactly one source read
				// when the buffer is empty, keeping the source bounded by hwm.
				if (!self.destroyed && !st.destroyed) {
					const chunk = self.read();
					// objectMode chunks may legitimately be 0/""/false; only
					// null/undefined mean "nothing available".
					if (chunk !== null && chunk !== undefined) return { value: chunk, done: false };
				}
				// No data available. End only once 'end' has actually fired (or the
				// stream already ended before iteration) — gating on the event, not
				// merely st.ended, guarantees the decoder residual emitted just
				// before 'end' is captured first. read() above has already
				// scheduled the 'end' delivery when the source is exhausted, so
				// this never hangs.
				if (ended || st.endEmitted) {
					if (residual.length) continue;
					cleanup();
					return { value: undefined, done: true };
				}
				await new Promise((res) => { wake = res; });
			}
		};
		// return()/throw() run on `break`/an exception in the for-await body:
		// detach the listeners and destroy the source (Node's contract), so a
		// broken-out loop doesn't leak listeners or keep an infinite source flowing.
		return {
			next,
			async return(v) { cleanup(); self.destroy(); return { value: v, done: true }; },
			async throw(e) { cleanup(); self.destroy(e); throw e; },
			[Symbol.asyncIterator]() { return this; },
		};
	};

	Readable.from = (iterable, options = {}) => {
		// Node's Readable.from defaults to objectMode, so string/number/object
		// items pass through unchanged (not coerced to Buffer).
		const rs = new Readable({ objectMode: true, ...options });
		// Pull from the source LAZILY, one _read cycle at a time, honoring push()
		// backpressure. The old implementation eagerly drained the whole iterable
		// into the buffer in a detached async loop, ignoring push()'s return value
		// — so Readable.from(hugeIterable) consumed slowly buffered the entire
		// source in guest heap. Now the iterator is advanced only when the
		// consumer reads and the buffer is below highWaterMark.
		const iter = (iterable && iterable[Symbol.asyncIterator])
			? iterable[Symbol.asyncIterator]()
			: iterable[Symbol.iterator]();
		let pulling = false;
		rs._read = function _read() {
			if (pulling) return; // an async iter.next() is already in flight
			pulling = true;
			(async () => {
				try {
					let wantMore = true;
					while (wantMore) {
						const { value, done } = await iter.next();
						if (done) { rs.push(null); return; }
						// push() returns false at/over hwm: stop until the next _read.
						wantMore = rs.push(value);
						if (rs._rs.destroyed) return;
					}
				} catch (e) {
					rs.destroy(e);
				} finally {
					pulling = false;
				}
			})();
		};
		return rs;
	};

	// ------------------------------------------------------------ Writable

	function initWritable(self, options = {}) {
		self._ws = {
			ending: false, finished: false, finishEmitted: false, pending: 0, destroyed: false,
			buffered: 0, // bytes/items awaiting their write callback
			needDrain: false,
			// Node's own fields, carried so a caller that branches on them sees a
			// value rather than undefined.
			closed: false, errored: null, errorEmitted: false, constructed: true,
			defaultEncoding: options.defaultEncoding || "utf8", emitClose: options.emitClose !== false,
			autoDestroy: options.autoDestroy !== false, corked: 0, sync: true, writing: false,
			highWaterMark: options.highWaterMark ?? (options.objectMode || options.writableObjectMode ? 16 : 16384),
			objectMode: !!(options.objectMode || options.writableObjectMode),
		};
		self.writable = true;
		if (typeof options.write === "function") self._write = options.write;
		if (typeof options.final === "function") self._final = options.final;
		if (typeof options.destroy === "function" && !self._destroy) self._destroy = options.destroy;
	}

	const writableGetters = {
		...streamStateGetters,
		_writableState: { get() { return this._ws; }, configurable: true },
		writableHighWaterMark: { get() { return this._ws.highWaterMark; }, configurable: true },
		writableLength: { get() { return this._ws.buffered; }, configurable: true },
		writableObjectMode: { get() { return this._ws.objectMode; }, configurable: true },
		writableNeedDrain: { get() { return !!this._ws.needDrain; }, configurable: true },
		writableCorked: { get() { return this._ws.corked; }, configurable: true },
		writableEnded: { get() { return !!this._ws.ending; }, configurable: true },
		writableFinished: { get() { return !!this._ws.finishEmitted; }, configurable: true },
	};

	const writableMethods = {
		_write(chunk, encoding, callback) { callback(); },
		write(chunk, encoding, callback) {
			if (typeof encoding === "function") { callback = encoding; encoding = null; }
			const st = this._ws;
			if (st.ending || st.destroyed) {
				const err = new Error("write after end");
				err.code = "ERR_STREAM_WRITE_AFTER_END";
				// Node emits this error asynchronously so a listener attached on
				// the same tick still catches it (and it can't throw out of the
				// synchronous write() call).
				process.nextTick(() => {
					if (callback) callback(err);
					this.emit("error", err);
				});
				return false;
			}
			const payload = st.objectMode ? chunk : toChunk(chunk, encoding);
			const size = st.objectMode ? 1 : (payload.length ?? 0);
			st.pending++;
			st.buffered += size;
			// Queue and process one at a time: Node calls _write for the next
			// chunk only after the previous callback fires, so an async _write /
			// _transform can't run concurrently and reorder output.
			(st.writeQueue || (st.writeQueue = [])).push({ payload, size, encoding: encoding || "utf8", callback });
			this._processWriteQueue();
			// Real backpressure: false once the in-flight buffer reaches hwm.
			if (st.buffered >= st.highWaterMark) {
				st.needDrain = true;
				return false;
			}
			return true;
		},
		_processWriteQueue() {
			const st = this._ws;
			if (st.writing || !st.writeQueue || st.writeQueue.length === 0 || st.destroyed) return;
			st.writing = true;
			const { payload, size, encoding, callback } = st.writeQueue.shift();
			this._write(payload, encoding, (err) => {
				st.writing = false;
				st.pending--;
				st.buffered -= size;
				if (err) {
					if (callback) callback(err);
					// Node fails ALL still-pending writes when a writable errors
					// out — don't strand the callbacks of chunks queued behind
					// this one (which would hang a "wait for N callbacks" barrier).
					this._failWriteQueue(err);
					this.destroy(err);
					return;
				}
				if (callback) callback();
				if (st.needDrain && st.buffered < st.highWaterMark && !st.ending) {
					st.needDrain = false;
					this.emit("drain");
				}
				this._maybeFinish();
				this._processWriteQueue(); // next queued chunk, in order
			});
		},
		_failWriteQueue(err) {
			const st = this._ws;
			if (!st.writeQueue || st.writeQueue.length === 0) return;
			const q = st.writeQueue;
			st.writeQueue = [];
			for (const item of q) {
				st.pending--;
				st.buffered -= item.size;
				if (item.callback) item.callback(err);
			}
		},
		end(chunk, encoding, callback) {
			if (typeof chunk === "function") { callback = chunk; chunk = null; }
			else if (typeof encoding === "function") { callback = encoding; encoding = null; }
			if (chunk !== null && chunk !== undefined) this.write(chunk, encoding);
			const st = this._ws;
			st.ending = true;
			if (callback) this.once("finish", callback);
			this._maybeFinish();
			return this;
		},
		_maybeFinish() {
			const st = this._ws;
			if (!st.ending || st.finished || st.pending > 0 || st.destroyed) return;
			st.finished = true;
			// Node emits 'finish' on a later tick, so listeners attached right
			// after end() still fire.
			const done = () => process.nextTick(() => {
				this.finished = true;
				this.writable = false;
				st.finishEmitted = true;
				this.emit("finish");
				emitClose(this);
			});
			if (this._final) this._final((err) => { if (err) this.destroy(err); else done(); });
			else done();
		},
		destroy(err) {
			const st = this._ws;
			if (st.destroyed) return this;
			st.destroyed = true;
			st.closed = true;
			this.writable = false;
			if (err) { st.errored = err; this._errored = err; } // so a later finished() reports the error
			// Invoke the callbacks of any still-queued writes with an error
			// rather than stranding them.
			this._failWriteQueue(err || new Error("stream destroyed"));
			const done = (e) => {
				if (e) this.emit("error", e);
				emitClose(this);
			};
			if (this._destroy) this._destroy(err, done);
			else done(err);
			return this;
		},
		cork() {},
		uncork() {},
		setDefaultEncoding() { return this; },
	};

	function Writable(options) {
		EventEmitter.call(this);
		initWritable(this, options);
	}
	Object.setPrototypeOf(Writable.prototype, EventEmitter.prototype);
	Object.setPrototypeOf(Writable, EventEmitter);
	Object.assign(Writable.prototype, writableMethods);
	Object.defineProperties(Writable.prototype, writableGetters);

	// --------------------------------------- Duplex / Transform / PassThrough

	function Duplex(options) {
		Readable.call(this, options);
		initWritable(this, options);
	}
	Object.setPrototypeOf(Duplex.prototype, Readable.prototype);
	Object.setPrototypeOf(Duplex, Readable);
	for (const [name, fn] of Object.entries(writableMethods)) {
		if (name !== "destroy") Duplex.prototype[name] = fn;
	}
	Object.defineProperties(Duplex.prototype, writableGetters);
	// A Duplex/Transform destroy must stop BOTH halves. Marking only the
	// writable side (writableMethods.destroy) left the readable side flowing —
	// _flowNow guards on _rs.destroyed — so a scheduled flow could still emit
	// 'data'/'end' after 'error'/'close'.
	Duplex.prototype.destroy = function destroy(err) {
		const rs = this._rs;
		const ws = this._ws;
		if ((rs && rs.destroyed) || (ws && ws.destroyed)) return this;
		if (rs) { rs.destroyed = true; rs.closed = true; }
		if (ws) { ws.destroyed = true; ws.closed = true; }
		this.readable = false;
		this.writable = false;
		if (err) {
			if (rs) rs.errored = err;
			if (ws) ws.errored = err;
			this._errored = err; // so a later finished() reports the error
		}
		if (this._failWriteQueue) this._failWriteQueue(err || new Error("stream destroyed"));
		const done = (e) => {
			if (e) this.emit("error", e);
			emitClose(this);
		};
		if (this._destroy) this._destroy(err, done);
		else done(err);
		return this;
	};

	function Transform(options = {}) {
		Duplex.call(this, options);
		if (typeof options.transform === "function") this._transform = options.transform;
		if (typeof options.flush === "function") this._flush = options.flush;
	}
	Object.setPrototypeOf(Transform.prototype, Duplex.prototype);
	Object.setPrototypeOf(Transform, Duplex);
	Object.assign(Transform.prototype, {
		_transform(chunk, encoding, callback) { callback(null, chunk); },
		_write(chunk, encoding, callback) {
			// The transform callback may be called ONCE; a second call is the
			// error Node emits (not throws) on the stream.
			let called = false;
			this._transform(chunk, encoding, (err, out) => {
				if (called) {
					this.emit("error", Object.assign(new Error("Callback called multiple times"),
						{ code: "ERR_MULTIPLE_CALLBACK" }));
					return;
				}
				called = true;
				if (err) return callback(err);
				if (out !== null && out !== undefined) this.push(out);
				callback();
			});
		},
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
		},
	});

	function PassThrough(options) {
		Transform.call(this, options);
	}
	Object.setPrototypeOf(PassThrough.prototype, Transform.prototype);
	Object.setPrototypeOf(PassThrough, Transform);

	// ------------------------------------------------------------- helpers

	// The error a terminal stream should report to finished(): a stored destroy
	// error wins; a stream destroyed before it naturally ended/finished is a
	// premature close (Node's ERR_STREAM_PREMATURE_CLOSE — e.g. a destination
	// dying mid-transfer must fail pipeline(), not report success).
	function terminalError(stream) {
		if (stream._errored) return stream._errored;
		const rs = stream._rs, ws = stream._ws;
		const readableIncomplete = rs && rs.destroyed && !rs.endEmitted && stream.readableEnded !== true;
		const writableIncomplete = ws && ws.destroyed && !ws.finished && stream.writableFinished !== true;
		if (readableIncomplete || writableIncomplete) {
			return Object.assign(new Error("Premature close"), { code: "ERR_STREAM_PREMATURE_CLOSE" });
		}
		return null;
	}

	function finished(stream, options, callback) {
		if (typeof options === "function") { callback = options; options = {}; }
		let called = false;
		const done = (err) => {
			if (called) return;
			called = true;
			callback(err || null);
		};
		// If the stream is ALREADY in its terminal state (ended / finished /
		// destroyed) when finished() is attached, Node still invokes the callback
		// (on a later tick) rather than hanging on an 'end'/'finish'/'close' event
		// that has already fired. Detect that synchronously — otherwise
		// `await finished(alreadyEndedStream)` (a common pattern, and the basis of
		// stream/promises.finished) never resolves.
		const rs = stream._rs, ws = stream._ws;
		const alreadyDone =
			stream.destroyed === true ||
			(rs && (rs.endEmitted || rs.destroyed)) ||
			(ws && (ws.finished || ws.destroyed)) ||
			stream.readableEnded === true ||
			stream.writableFinished === true;
		if (alreadyDone) {
			// If the stream reached its terminal state by ERRORING or by being
			// destroyed before completing, report that (not success) — otherwise
			// await finished(erroredStream) resolves and masks the failure.
			process.nextTick(() => done(terminalError(stream)));
			return () => { called = true; };
		}
		stream.once("error", done);
		stream.once("end", () => done());
		stream.once("finish", () => done());
		stream.once("close", () => done(terminalError(stream)));
		return () => {};
	}

	function pipeline(...args) {
		const callback = typeof args[args.length - 1] === "function" ? args.pop() : () => {};
		const all = args.slice();
		let done = false;
		const finish = (err) => {
			if (done) return;
			done = true;
			// On error, destroy EVERY stream in the chain so none is left open
			// (Node cleans them all up; otherwise the source's fd/socket leaks).
			if (err) {
				for (const s of all) {
					try { if (s && typeof s.destroy === "function" && !s.destroyed) s.destroy(); } catch (_e) { /* best effort */ }
				}
			}
			callback(err || null);
		};
		let current = all[0];
		for (let i = 1; i < all.length; i++) {
			all[i - 1].once("error", finish);
			current = all[i - 1].pipe(all[i]);
		}
		finished(current, finish);
		return current;
	}

	// The legacy base class: packages subclass it via util.inherits and call
	// Stream.call(this).
	function Stream() {
		EventEmitter.call(this);
	}
	Object.setPrototypeOf(Stream.prototype, EventEmitter.prototype);
	Object.setPrototypeOf(Stream, EventEmitter);
	// The LEGACY pipe: 'data' in, write() out, with pause/resume for
	// backpressure. It cannot be Readable's, which reads a _readableState the
	// base Stream does not have — and a bare Stream is exactly what the
	// util.inherits generation of code (and Node's own suite) still builds.
	Stream.prototype.pipe = function pipe(dest, options) {
		const source = this;
		function ondata(chunk) {
			if (dest.writable && dest.write(chunk) === false && source.pause) source.pause();
		}
		function ondrain() { if (source.readable && source.resume) source.resume(); }
		source.on("data", ondata);
		dest.on("drain", ondrain);

		// Only end the destination we are not sharing: piping several sources
		// into one stdout must not close it when the first finishes.
		if (!dest._isStdio && (!options || options.end !== false)) {
			source.on("end", onend);
			source.on("close", onclose);
		}
		let didOnEnd = false;
		function onend() { if (didOnEnd) return; didOnEnd = true; dest.end(); }
		function onclose() { if (didOnEnd) return; didOnEnd = true; if (typeof dest.destroy === "function") dest.destroy(); }

		// An 'error' with no listener is fatal, so both ends get one — and the
		// pipe is torn down either way, since half a pipe is worse than none.
		function onerror(er) {
			cleanup();
			if (this.listenerCount("error") === 0) throw er;
		}
		source.on("error", onerror);
		dest.on("error", onerror);

		function cleanup() {
			source.removeListener("data", ondata);
			dest.removeListener("drain", ondrain);
			source.removeListener("end", onend);
			source.removeListener("close", onclose);
			source.removeListener("error", onerror);
			dest.removeListener("error", onerror);
			source.removeListener("end", cleanup);
			source.removeListener("close", cleanup);
			dest.removeListener("close", cleanup);
		}
		source.on("end", cleanup);
		source.on("close", cleanup);
		dest.on("close", cleanup);

		dest.emit("pipe", source);
		return dest;
	};

	// duplexPair returns two Duplexes wired back to back: what one writes, the
	// other reads. It is how the suite tests a Duplex without a socket, and it
	// is a documented part of node:stream.
	function duplexPair() {
		const a = new Duplex({
			write(chunk, enc, cb) { b.push(chunk); cb(); },
			final(cb) { b.push(null); cb(); },
			read() {},
		});
		const b = new Duplex({
			write(chunk, enc, cb) { a.push(chunk); cb(); },
			final(cb) { a.push(null); cb(); },
			read() {},
		});
		// Destroying one end destroys the other, but does NOT hand it the error:
		// the peer of a failed connection is torn down, not itself at fault, and
		// re-emitting the error there would crash code that only listens on the
		// side it owns.
		a.once("close", () => { if (!b.destroyed) b.destroy(); });
		b.once("close", () => { if (!a.destroyed) a.destroy(); });
		return [a, b];
	}

	// ------------------------------------------- iterator helpers and toWeb
	// Node 17+ gives a Readable the same operators an array has. They are lazy
	// and each returns a new Readable, so `readable.filter(f).map(g).take(3)`
	// reads only as far as it must — which is the point of having them on a
	// stream rather than collecting to an array first.
	const helperSource = async function* (rs) {
		for await (const chunk of rs) yield chunk;
	};
	const fromAsyncGen = (gen, options) => Readable.from(gen, { objectMode: true, ...options });
	// The iterator helpers validate like Node's: the function synchronously
	// (ERR_INVALID_ARG_TYPE), the concurrency option as a positive number.
	const helperFn = (fn, name) => {
		if (typeof fn !== "function") {
			const recv = fn === null || fn === undefined ? `Received ${fn}`
				: typeof fn === "object" ? `Received an instance of ${fn.constructor ? fn.constructor.name : "Object"}`
				: `Received type ${typeof fn} (${String(fn)})`;
			throw Object.assign(new TypeError(`The "fn" argument must be of type function. ${recv}`),
				{ code: "ERR_INVALID_ARG_TYPE" });
		}
	};
	const helperOptions = (options) => {
		if (options === undefined || options === null) return;
		if (typeof options !== "object") {
			throw Object.assign(new TypeError('The "options" argument must be of type object.'),
				{ code: "ERR_INVALID_ARG_TYPE" });
		}
		if (options.concurrency !== undefined && !(Number(options.concurrency) >= 1)) {
			throw Object.assign(new RangeError(`The value of "concurrency" is out of range. It must be >= 1. Received ${options.concurrency}`),
				{ code: "ERR_OUT_OF_RANGE" });
		}
		if (options.signal !== undefined && (options.signal === null || typeof options.signal !== "object" || typeof options.signal.aborted !== "boolean")) {
			throw Object.assign(new TypeError('The "options.signal" property must be an instance of AbortSignal.'),
				{ code: "ERR_INVALID_ARG_TYPE" });
		}
	};
	// drop/take size: a number, coerced per NumberIsNaN rules.
	// drop/take size: the iterator-helpers coercion ("the spec made me do
	// this" — Node's own comment): ToIntegerOrInfinity, NaN becomes 0, and
	// only a NEGATIVE result is an error.
	const helperCount = (n, name) => {
		let v = Number(n);
		if (Number.isNaN(v)) v = 0;
		if (v < 0) {
			throw Object.assign(new RangeError(`The value of "${name}" is out of range. It must be >= 0. Received ${n}`),
				{ code: "ERR_OUT_OF_RANGE" });
		}
		return Math.trunc(v);
	};
	const helperAbort = (options) => {
		const sig = options && options.signal;
		return () => {
			if (sig && sig.aborted) {
				throw Object.assign(new Error("The operation was aborted"), { name: "AbortError", code: "ABORT_ERR" });
			}
		};
	};
	Object.assign(Readable.prototype, {
		map(fn, options) {
			helperFn(fn, "fn");
			helperOptions(options);
			const self = this;
			const check = helperAbort(options);
			return fromAsyncGen((async function* () {
				check();
				let i = 0;
				for await (const c of helperSource(self)) { check(); yield await fn(c, i++); }
			})(), options);
		},
		filter(fn, options) {
			helperFn(fn, "fn");
			helperOptions(options);
			const self = this;
			const check = helperAbort(options);
			return fromAsyncGen((async function* () {
				check();
				let i = 0;
				for await (const c of helperSource(self)) { check(); if (await fn(c, i++)) yield c; }
			})(), options);
		},
		take(n, options) {
			const count = helperCount(n, "number");
			helperOptions(options);
			const check = helperAbort(options);
			const self = this;
			return fromAsyncGen((async function* () {
				check();
				if (count <= 0) return;
				let left = count;
				for await (const c of helperSource(self)) { check(); yield c; if (--left <= 0) return; }
			})(), options);
		},
		drop(n, options) {
			const count = helperCount(n, "number");
			helperOptions(options);
			const check = helperAbort(options);
			const self = this;
			return fromAsyncGen((async function* () {
				check();
				let left = count;
				for await (const c of helperSource(self)) { check(); if (left-- > 0) continue; yield c; }
			})(), options);
		},
		flatMap(fn, options) {
			helperFn(fn, "fn");
			helperOptions(options);
			const self = this;
			return fromAsyncGen((async function* () {
				let i = 0;
				for await (const c of helperSource(self)) {
					const out = await fn(c, i++);
					if (out && (out[Symbol.asyncIterator] || out[Symbol.iterator])) yield* out;
					else yield out;
				}
			})(), options);
		},
		async forEach(fn) { let i = 0; for await (const c of helperSource(this)) await fn(c, i++); },
		async toArray() { const out = []; for await (const c of helperSource(this)) out.push(c); return out; },
		async some(fn) { let i = 0; for await (const c of helperSource(this)) if (await fn(c, i++)) return true; return false; },
		async every(fn) { let i = 0; for await (const c of helperSource(this)) if (!(await fn(c, i++))) return false; return true; },
		async find(fn) { let i = 0; for await (const c of helperSource(this)) if (await fn(c, i++)) return c; return undefined; },
		async reduce(fn, initial) {
			let acc = initial, i = 0, seeded = arguments.length > 1;
			for await (const c of helperSource(this)) {
				if (!seeded) { acc = c; seeded = true; i++; continue; }
				acc = await fn(acc, c, i++);
			}
			if (!seeded) throw Object.assign(new TypeError("Reduce of an empty stream with no initial value"), { code: "ERR_INVALID_ARG_TYPE" });
			return acc;
		},
	});

	// The two stream worlds. A Node stream and a WHATWG stream are the same
	// idea with different shapes, and code that mixes them (a fetch body into a
	// pipeline, a Node stream into a Response) needs the conversion to exist.
	Readable.toWeb = (rs) => new globalThis.ReadableStream({
		start(controller) {
			rs.on("data", (c) => controller.enqueue(c));
			rs.once("end", () => { try { controller.close(); } catch { /* already closed */ } });
			rs.once("error", (e) => controller.error(e));
		},
		cancel(reason) { rs.destroy(reason); },
	});
	Readable.fromWeb = (ws, options) => Readable.from((async function* () {
		const reader = ws.getReader();
		for (;;) {
			const { value, done } = await reader.read();
			if (done) return;
			yield value;
		}
	})(), options);
	Writable.toWeb = (ws) => new globalThis.WritableStream({
		write(chunk) { return new Promise((res, rej) => ws.write(chunk, (e) => (e ? rej(e) : res()))); },
		close() { return new Promise((res) => ws.end(res)); },
		abort(reason) { ws.destroy(reason); },
	});
	Writable.fromWeb = (web, options) => {
		const writer = web.getWriter();
		return new Writable({
			...options,
			write(chunk, enc, cb) { writer.write(chunk).then(() => cb(), cb); },
			final(cb) { writer.close().then(() => cb(), cb); },
			destroy(err, cb) { writer.abort(err).then(() => cb(err), () => cb(err)); },
		});
	};
	Duplex.toWeb = (d) => ({ readable: Readable.toWeb(d), writable: Writable.toWeb(d) });
	Duplex.fromWeb = (pair, options) => {
		const readable = Readable.fromWeb(pair.readable, options);
		const writable = Writable.fromWeb(pair.writable, options);
		const d = new Duplex({
			...options,
			read() { readable.resume(); },
			write(chunk, enc, cb) { writable.write(chunk, enc, cb); },
			final(cb) { writable.end(cb); },
		});
		readable.on("data", (c) => d.push(c));
		readable.once("end", () => d.push(null));
		return d;
	};

	const streamMod = Object.assign(Stream, {
		Readable, Writable, Duplex, Transform, PassThrough, Stream, finished, pipeline,
		duplexPair,
	});
	core.stream = streamMod;

	core["stream/promises"] = {
		pipeline: (...streams) =>
			new Promise((resolve, reject) => pipeline(...streams, (err) => (err ? reject(err) : resolve()))),
		finished: (stream) =>
			new Promise((resolve, reject) => finished(stream, (err) => (err ? reject(err) : resolve()))),
	};
	// The web layer (web.Install) runs first, so these globals are the real,
	// working implementations — re-export them rather than shipping stubs that
	// throw despite globalThis.TextDecoderStream working.
	core["stream/web"] = {
		ReadableStream: globalThis.ReadableStream,
		WritableStream: globalThis.WritableStream,
		TransformStream: globalThis.TransformStream,
		TextEncoderStream: globalThis.TextEncoderStream,
		TextDecoderStream: globalThis.TextDecoderStream,
		ByteLengthQueuingStrategy: globalThis.ByteLengthQueuingStrategy,
		CountQueuingStrategy: globalThis.CountQueuingStrategy,
	};

	// -------------------------------------------------- fs stream flavors

	// Node exposes the two fs stream classes as constructors, and code reaches
	// for them by name: `new fs.ReadStream(path)`, `x instanceof fs.WriteStream`,
	// and prototype patching by the graceful-fs generation of packages. They are
	// real subclasses of Readable/Writable, so an instance answers to both, and
	// the create* functions below re-parent what they build onto them.
	// FileReadStream/FileWriteStream are Node's own legacy aliases.
	function ReadStream(p, options) { return fsMod.createReadStream(p, options); }
	function WriteStream(p, options) { return fsMod.createWriteStream(p, options); }
	ReadStream.prototype = Object.create(Readable.prototype, { constructor: { value: ReadStream } });
	WriteStream.prototype = Object.create(Writable.prototype, { constructor: { value: WriteStream } });
	fsMod.ReadStream = fsMod.FileReadStream = ReadStream;
	fsMod.WriteStream = fsMod.FileWriteStream = WriteStream;

	// The default open(): what a patched prototype replaces. It sets this.fd and
	// announces the stream is ready, which is the contract a replacement has to
	// honor too.
	function defaultOpen() {
		try {
			this.fd = fsMod.openSync(this.path, this.flags);
		} catch (e) {
			this.destroy(e);
			return;
		}
		process.nextTick(() => { this.emit("open", this.fd); this.emit("ready"); });
	}
	ReadStream.prototype.open = defaultOpen;
	WriteStream.prototype.open = defaultOpen;

	// `start`/`end` are byte offsets, and Node rejects a string there rather than
	// coercing it — a stream built from { start: "4" } would otherwise read from
	// a plausible-looking but wrong place.
	const V = core.__validate;
	const checkStreamRange = (options) => {
		if (options.start !== undefined) V.validateInteger(options.start, "start", 0, Number.MAX_SAFE_INTEGER);
		if (options.end !== undefined && options.end !== Infinity) V.validateInteger(options.end, "end", 0, Number.MAX_SAFE_INTEGER);
		if (options.fd !== undefined && options.fd !== null && typeof options.fd !== "object") V.validateFd(options.fd);
	};

	fsMod.createReadStream = (p, options = {}) => {
		if (typeof options === "string") options = { encoding: options };
		V.validateEncodingOpt(options);
		checkStreamRange(options);
		const hwm = options.highWaterMark || 64 * 1024;
		const ownFd = typeof options.fd !== "number";
		let pos = options.start ?? 0;
		const endInclusive = options.end; // fs stream `end` is INCLUSIVE
		let opened = false, fdClosed = false;
		const closeFd = () => { if (!fdClosed && ownFd && rs.fd !== null) { try { fsMod.closeSync(rs.fd); } catch { /* ignore */ } fdClosed = true; } };
		const rs = new Readable({
			// Stream the file in highWaterMark-sized chunks (constant memory) rather
			// than loading the WHOLE file into guest heap — a large file (or a small
			// {start,end} range of one) no longer risks OOM-killing the instance.
			read(n) {
				try {
					if (!opened) {
						opened = true;
						// Through this.open(), not a direct openSync: the whole point
						// of exposing fs.ReadStream is that its prototype can be
						// patched, which graceful-fs and its dependents do.
						if (rs.fd === null) this.open();
						if (rs.fd === null) return; // a patched open() defers it
					}
					const fd = rs.fd;
					let want = Math.min(hwm, Math.max(1, n || hwm));
					if (endInclusive !== undefined) {
						const remaining = endInclusive - pos + 1;
						if (remaining <= 0) { closeFd(); this.push(null); return; }
						want = Math.min(want, remaining);
					}
					const buf = Buffer.alloc(want);
					const bytesRead = fsMod.readSync(fd, buf, 0, want, pos);
					if (bytesRead <= 0) { closeFd(); this.push(null); return; }
					pos += bytesRead;
					this.push(buf.subarray(0, bytesRead));
				} catch (e) { closeFd(); this.destroy(e); }
			},
			destroy(err, cb) { closeFd(); cb(err); },
		});
		Object.setPrototypeOf(rs, ReadStream.prototype);
		rs.fd = typeof options.fd === "number" ? options.fd : null;
		rs.flags = options.flags || "r";
		if (options.encoding) rs.setEncoding(options.encoding);
		rs.on("end", closeFd);
		rs.path = p;
		if (ownFd) rs.open();
		else process.nextTick(() => rs.emit("ready"));
		rs.close = (cb) => { closeFd(); rs.destroy(); if (cb) process.nextTick(cb); };
		return rs;
	};

	fsMod.createWriteStream = (p, options = {}) => {
		if (typeof options === "string") options = { encoding: options };
		V.validateEncodingOpt(options);
		checkStreamRange(options);
		// A caller-supplied fd is used directly (and not closed by us); otherwise
		// open lazily on the first write, through this.open() so a patched
		// prototype (graceful-fs and its dependents) is honored. A numeric
		// `start` makes every write a positioned (pwrite) write from that
		// offset, which Node honors.
		const ownFd = typeof options.fd !== "number";
		let pos = typeof options.start === "number" ? options.start : null;
		const ws = new Writable({
			write(chunk, encoding, callback) {
				try {
					const buf = typeof chunk === "string" ? Buffer.from(chunk, encoding) : chunk;
					if (ws.fd === null) this.open();
					if (ws.fd === null) { callback(); return; }
					if (pos !== null) {
						fsMod.writeSync(ws.fd, buf, 0, buf.length, pos);
						pos += buf.length;
					} else {
						fsMod.writeSync(ws.fd, buf, 0, buf.length);
					}
					callback();
				} catch (e) {
					callback(e);
				}
			},
			final(callback) {
				try {
					// An empty stream must still create/truncate the file.
					if (ws.fd === null) this.open();
					if (ownFd && ws.fd !== null) { fsMod.closeSync(ws.fd); ws.fd = null; }
					callback();
				} catch (e) {
					callback(e);
				}
			},
			destroy(err, callback) {
				// destroy() does NOT run final(), so close our fd here too or it
				// leaks in the host fd table (and buffered bytes are lost, since
				// closeSync is what flushes them to the FS).
				try {
					if (ownFd && ws.fd !== null) { fsMod.closeSync(ws.fd); ws.fd = null; }
				} catch (_e) { /* best effort on the error/destroy path */ }
				callback(err);
			},
		});
		Object.setPrototypeOf(ws, WriteStream.prototype);
		ws.fd = typeof options.fd === "number" ? options.fd : null;
		ws.flags = options.flags || "w";
		ws.path = p;
		ws.close = (cb) => ws.end(cb);
		// Node opens at CONSTRUCTION, not on the first write: a caller that waits
		// for 'open' before writing would otherwise wait forever. 'open' belongs
		// to open() alone — emitting it here as well fired every handler twice.
		// A stream handed an existing fd opens nothing and is ready at once.
		if (ownFd) ws.open();
		else process.nextTick(() => ws.emit("ready"));
		return ws;
	};

})();
