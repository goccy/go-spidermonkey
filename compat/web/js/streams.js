// compat/web: the WHATWG Streams Standard (https://streams.spec.whatwg.org/).
//
// This is the specification's own algorithms, named after them, rather than an
// approximation of what streams do. The approximation it replaces got the easy
// shapes right and then diverged wherever the standard is precise: it had no
// write queue (so backpressure did not exist and `ready` was always resolved),
// no start-promise gate, no erroring state, no in-flight request bookkeeping,
// no pull-into descriptors (so a BYOB read copied out of a queue and a source's
// byobRequest was permanently null), and a pipeTo written as an await loop
// instead of the shutdown machinery the standard specifies. Each of those is a
// visible behaviour, not an internal detail, and everything downstream — fetch
// bodies, compression, WebSocket, TransformStream — inherits it.
//
// Ordering matters here in ways worth stating once. Promise reactions are the
// observable clock of this API: the standard is written in terms of which
// microtask a reaction runs in, and the tests measure it. So the algorithms
// below settle promises where the standard settles them and not one step
// earlier, even where an earlier settle would look equivalent.
//
// This file defines every stream class and is evaluated before builtins.js,
// which uses them.
(() => {
	"use strict";

	// ------------------------------------------------------------- utilities

	// Brands. An IDL interface's members are unusable on anything that is not an
	// instance of it, and "has the right shape" is not the test — an object made
	// with Object.create(ReadableStream.prototype) is not a ReadableStream. Each
	// creator stamps its brand; each member checks for it.
	const BRAND = Symbol("streams.brand");
	const brandCheck = (o, name) => {
		if (o === null || typeof o !== "object" || o[BRAND] !== name) {
			throw new TypeError(`Illegal invocation: the receiver is not a ${name}`);
		}
	};
	const isBranded = (o, name) => o !== null && typeof o === "object" && o[BRAND] === name;

	const newPromise = () => {
		let resolve, reject;
		const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
		return { promise, resolve, reject };
	};
	// A deferred that KNOWS whether it has settled. The standard distinguishes
	// "reject the promise" from "replace it with a rejected one", and which of the
	// two applies depends on whether the promise is still pending — a writer whose
	// `ready` already fulfilled gets a NEW rejected `ready` when the stream errors,
	// where one still waiting simply rejects. Settling is one-shot, so without the
	// state the second case silently did nothing and `ready` stayed fulfilled.
	const deferred = () => {
		const d = newPromise();
		d.state = "pending";
		const res = d.resolve, rej = d.reject;
		d.resolve = (v) => { if (d.state === "pending") { d.state = "fulfilled"; res(v); } };
		d.reject = (e) => { if (d.state === "pending") { d.state = "rejected"; rej(e); } };
		return d;
	};
	const settledDeferred = (value, isReject) => ({
		state: isReject ? "rejected" : "fulfilled",
		promise: isReject ? markHandled(Promise.reject(value)) : Promise.resolve(value),
		resolve: () => {}, reject: () => {},
	});
	// "Reject it if pending, otherwise replace it with a rejected one."
	const rejectOrReplace = (d, error) => {
		if (d.state === "pending") {
			d.reject(error);
			markHandled(d.promise);
			return d;
		}
		return settledDeferred(error, true);
	};
	// The standard's "set promise is handled to true": a promise the
	// implementation settles for its own bookkeeping must not be reported as an
	// unhandled rejection just because no caller happened to look at it.
	const markHandled = (p) => { p.then(undefined, () => {}); return p; };
	const resolvedPromise = (v) => Promise.resolve(v);
	const rejectedPromise = (e) => markHandled(Promise.reject(e));

	// A queue of {value, size} with a running total, shared by both controllers.
	// The total is recomputed from the queue when it empties so accumulated
	// floating-point error cannot leave a residue behind (the standard says so).
	const resetQueue = (c) => { c._queue = []; c._queueTotalSize = 0; };
	const enqueueValueWithSize = (c, value, size) => {
		const n = Number(size);
		if (!Number.isFinite(n) || Number.isNaN(n) || n < 0) {
			throw new RangeError("Size must be a finite, non-negative number");
		}
		c._queue.push({ value, size: n });
		c._queueTotalSize += n;
	};
	const dequeueValue = (c) => {
		const pair = c._queue.shift();
		c._queueTotalSize -= pair.size;
		if (c._queueTotalSize < 0) c._queueTotalSize = 0;
		return pair.value;
	};
	const peekQueueValue = (c) => c._queue[0].value;

	// IDL conversions the constructors need. A strategy's highWaterMark is a
	// double whose NaN/negative check belongs to ExtractHighWaterMark — later
	// than the dictionary conversion, which only coerces.
	const convertHWM = (value) => {
		const n = Number(value);
		if (Number.isNaN(n) || n < 0) throw new RangeError("Invalid highWaterMark");
		return n;
	};
	const makeSizeAlgorithm = (size) => {
		if (size === undefined) return () => 1;
		return (chunk) => callback(size, undefined, [chunk]);
	};
	// A dictionary's members are converted in LEXICOGRAPHIC order, and a
	// dictionary argument is converted before the body of the operation runs —
	// which is why a strategy is read before the underlying source even though it
	// is the second argument. Getters make both orders observable.
	const convertStrategy = (value, who) => {
		const strat = value === undefined || value === null ? {} : value;
		if (typeof strat !== "object" && typeof strat !== "function") {
			throw new TypeError(`${who}: the queuing strategy must be an object`);
		}
		const highWaterMark = strat.highWaterMark;
		const size = method(strat, "size", who);
		return { highWaterMark, size };
	};
	// A callback member is read ONCE, at construction, and must be callable if
	// present — a later mutation of the object is not observed, and a
	// non-callable member is a TypeError then rather than a mysterious failure at
	// the first pull.
	const method = (obj, name, who) => {
		const fn = obj[name];
		if (fn === undefined || fn === null) return undefined;
		if (typeof fn !== "function") throw new TypeError(`${who}.${name} must be a function`);
		return fn;
	};
	// Callbacks are invoked through Reflect, never through Function.prototype
	// .call/.apply: those are ordinary properties a page may replace, and calling
	// a user's algorithm through them would run their code as well as ours.
	const callback = (fn, thisArg, args) => Reflect.apply(fn, thisArg, args);
	// An IDL operation that returns a Promise invokes its callback IMMEDIATELY and
	// converts what comes back; only the RESULT is asynchronous. Deferring the
	// call itself by a microtask is observable — a sink's write must have run by
	// the time writer.write() returns — and it reordered every test that counts
	// how far a stream has got at a given moment.
	const promiseCall = (fn, thisArg, args) => {
		try {
			return Promise.resolve(callback(fn, thisArg, args));
		} catch (e) {
			return rejectedPromise(e);
		}
	};

	const isView = (v) => ArrayBuffer.isView(v);
	const isDetached = (buffer) => {
		// A detached ArrayBuffer reports zero length and cannot be viewed. There is
		// no predicate for it in the language, so the observable consequence is the
		// test: byteLength is 0 and any view over it is empty. A genuinely empty
		// buffer answers the same way, which is why every caller of this treats
		// "detached" and "empty" alike — both are unusable as a transfer target.
		try {
			return buffer.byteLength === 0;
		} catch {
			return true;
		}
	};
	// The standard TRANSFERS a buffer between the source and the reader so the
	// two can never write to the same memory. Without ArrayBuffer.prototype
	// .transfer this copies instead, which preserves the isolation the transfer
	// exists for; what it cannot preserve is the detachment the caller observes.
	const transferBuffer = (buffer) => {
		if (typeof buffer.transfer === "function") return buffer.transfer();
		return buffer.slice(0);
	};

	// --------------------------------------------------- ReadableStream: core

	class ReadableStream {
		constructor(underlyingSource = undefined, strategy = undefined) {
			// The strategy is a dictionary ARGUMENT, converted before the body runs;
			// the source is an `object`, converted inside it. So the strategy's
			// getters run first, whichever argument came first in the source text.
			const strat = convertStrategy(strategy, "ReadableStream");
			const source = underlyingSource === undefined || underlyingSource === null ? {} : underlyingSource;
			if (typeof source !== "object" && typeof source !== "function") {
				throw new TypeError("ReadableStream: the underlying source must be an object");
			}
			// UnderlyingSource's members, in the order the dictionary declares them.
			const autoAllocateChunkSize = source.autoAllocateChunkSize;
			const cancelFn = method(source, "cancel", "underlyingSource");
			const pullFn = method(source, "pull", "underlyingSource");
			const startFn = method(source, "start", "underlyingSource");
			const type = source.type;

			initializeReadableStream(this);

			if (type !== undefined && String(type) === "bytes") {
				if (strat.size !== undefined) {
					throw new RangeError("A byte stream has no size function: its chunks are measured in bytes");
				}
				const hwm = strat.highWaterMark === undefined ? 0 : convertHWM(strat.highWaterMark);
				let chunkSize;
				if (autoAllocateChunkSize !== undefined) {
					chunkSize = Number(autoAllocateChunkSize);
					if (!Number.isInteger(chunkSize) || chunkSize <= 0 || !Number.isFinite(chunkSize)) {
						throw new TypeError("autoAllocateChunkSize must be a positive integer");
					}
				}
				setUpReadableByteStreamController(this, source, startFn, pullFn, cancelFn, hwm, chunkSize);
				return;
			}
			if (type !== undefined) {
				throw new TypeError(`${String(type)} is not a valid ReadableStream type`);
			}
			const sizeAlgorithm = makeSizeAlgorithm(strat.size);
			const hwm = strat.highWaterMark === undefined ? 1 : convertHWM(strat.highWaterMark);
			setUpReadableStreamDefaultController(this, source, startFn, pullFn, cancelFn, hwm, sizeAlgorithm);
		}

		get locked() {
			brandCheck(this, "ReadableStream");
			return isReadableStreamLocked(this);
		}

		cancel(reason = undefined) {
			try {
				brandCheck(this, "ReadableStream");
			} catch (e) {
				return rejectedPromise(e);
			}
			if (isReadableStreamLocked(this)) {
				return rejectedPromise(new TypeError("Cannot cancel a stream that already has a reader"));
			}
			return readableStreamCancel(this, reason);
		}

		getReader(options = undefined) {
			brandCheck(this, "ReadableStream");
			const opts = options === undefined || options === null ? {} : options;
			if (typeof opts !== "object" && typeof opts !== "function") {
				throw new TypeError("getReader: options must be an object");
			}
			const mode = opts.mode;
			if (mode === undefined) return new ReadableStreamDefaultReader(this);
			if (String(mode) !== "byob") {
				throw new TypeError(`${String(mode)} is not a valid reader mode`);
			}
			return new ReadableStreamBYOBReader(this);
		}

		pipeThrough(transform, options = undefined) {
			brandCheck(this, "ReadableStream");
			if (transform === null || typeof transform !== "object") {
				throw new TypeError("pipeThrough expects a { readable, writable } pair");
			}
			// The members are read ONE AT A TIME in declaration order and each is
			// validated before the next is touched — observable through getters, and
			// the suite checks exactly that.
			const readable = transform.readable;
			if (!isBranded(readable, "ReadableStream")) {
				throw new TypeError("pipeThrough: transform.readable is not a ReadableStream");
			}
			const writable = transform.writable;
			if (!isBranded(writable, "WritableStream")) {
				throw new TypeError("pipeThrough: transform.writable is not a WritableStream");
			}
			const opts = convertPipeOptions(options, "pipeThrough");
			if (isReadableStreamLocked(this)) throw new TypeError("pipeThrough: the source stream is locked");
			if (isWritableStreamLocked(writable)) throw new TypeError("pipeThrough: transform.writable is locked");
			markHandled(readableStreamPipeTo(this, writable, opts.preventClose, opts.preventAbort, opts.preventCancel, opts.signal));
			return readable;
		}

		pipeTo(destination, options = undefined) {
			try {
				brandCheck(this, "ReadableStream");
				if (!isBranded(destination, "WritableStream")) {
					throw new TypeError("pipeTo: the destination is not a WritableStream");
				}
				const opts = convertPipeOptions(options, "pipeTo");
				if (isReadableStreamLocked(this)) throw new TypeError("pipeTo: the source stream is locked");
				if (isWritableStreamLocked(destination)) throw new TypeError("pipeTo: the destination is locked");
				return readableStreamPipeTo(this, destination, opts.preventClose, opts.preventAbort, opts.preventCancel, opts.signal);
			} catch (e) {
				return rejectedPromise(e);
			}
		}

		tee() {
			brandCheck(this, "ReadableStream");
			return readableStreamTee(this, false);
		}

		values(options = undefined) {
			brandCheck(this, "ReadableStream");
			const opts = options === undefined || options === null ? {} : options;
			return acquireReadableStreamAsyncIterator(this, Boolean(opts.preventCancel));
		}

		// ReadableStream.from(anyIterable) pulls the (sync or async) iterator
		// lazily — one next() per pull — and forwards cancel to iterator.return so
		// a generator's finally blocks run.
		static from(asyncIterable) {
			return readableStreamFromIterable(asyncIterable);
		}
	}
	Object.defineProperty(ReadableStream.prototype, Symbol.asyncIterator, {
		value: ReadableStream.prototype.values, writable: true, configurable: true,
	});
	Object.defineProperty(ReadableStream.prototype, Symbol.toStringTag, {
		value: "ReadableStream", configurable: true,
	});

	function initializeReadableStream(stream) {
		stream[BRAND] = "ReadableStream";
		stream._state = "readable";
		stream._reader = undefined;
		stream._storedError = undefined;
		stream._disturbed = false;
		stream._controller = undefined;
	}

	const isReadableStreamLocked = (stream) => stream._reader !== undefined;

	function createReadableStream(startAlgorithm, pullAlgorithm, cancelAlgorithm, highWaterMark = 1, sizeAlgorithm = () => 1) {
		const stream = Object.create(ReadableStream.prototype);
		initializeReadableStream(stream);
		const controller = Object.create(ReadableStreamDefaultController.prototype);
		setUpReadableStreamDefaultControllerFromAlgorithms(stream, controller, startAlgorithm, pullAlgorithm, cancelAlgorithm, highWaterMark, sizeAlgorithm);
		return stream;
	}

	function createReadableByteStream(startAlgorithm, pullAlgorithm, cancelAlgorithm) {
		const stream = Object.create(ReadableStream.prototype);
		initializeReadableStream(stream);
		const controller = Object.create(ReadableByteStreamController.prototype);
		setUpReadableByteStreamControllerFromAlgorithms(stream, controller, startAlgorithm, pullAlgorithm, cancelAlgorithm, 0, undefined);
		return stream;
	}

	function readableStreamCancel(stream, reason) {
		stream._disturbed = true;
		if (stream._state === "closed") return resolvedPromise(undefined);
		if (stream._state === "errored") return rejectedPromise(stream._storedError);
		readableStreamClose(stream);
		const reader = stream._reader;
		if (reader !== undefined && reader._readIntoRequests !== undefined) {
			// A BYOB reader's outstanding reads settle as done with an EMPTY view
			// over the buffer they were given, not with the view itself.
			const requests = reader._readIntoRequests;
			reader._readIntoRequests = [];
			for (const req of requests) req.closeSteps(undefined);
		}
		return stream._controller._cancelAlgorithm(reason).then(() => undefined);
	}

	function readableStreamClose(stream) {
		if (stream._state !== "readable") return;
		stream._state = "closed";
		const reader = stream._reader;
		if (reader === undefined) return;
		reader._closed.resolve(undefined);
		if (reader._readRequests !== undefined) {
			const requests = reader._readRequests;
			reader._readRequests = [];
			for (const req of requests) req.closeSteps();
		}
	}

	function readableStreamError(stream, e) {
		if (stream._state !== "readable") return;
		stream._state = "errored";
		stream._storedError = e;
		const reader = stream._reader;
		if (reader === undefined) return;
		reader._closed.reject(e);
		markHandled(reader._closed.promise);
		if (reader._readRequests !== undefined) {
			const requests = reader._readRequests;
			reader._readRequests = [];
			for (const req of requests) req.errorSteps(e);
		} else {
			const requests = reader._readIntoRequests;
			reader._readIntoRequests = [];
			for (const req of requests) req.errorSteps(e);
		}
	}

	// ------------------------------------------------- ReadableStream: readers

	function readableStreamReaderGenericInitialize(reader, stream) {
		reader._stream = stream;
		stream._reader = reader;
		reader._closed = deferred();
		if (stream._state === "closed") {
			reader._closed.resolve(undefined);
		} else if (stream._state === "errored") {
			reader._closed.reject(stream._storedError);
			markHandled(reader._closed.promise);
		}
	}

	function readableStreamReaderGenericRelease(reader) {
		const stream = reader._stream;
		const e = new TypeError("Reader was released and can no longer be used to monitor the stream's closedness");
		reader._closed = rejectOrReplace(reader._closed, e);
		markHandled(reader._closed.promise);
		stream._controller._releaseSteps();
		stream._reader = undefined;
		reader._stream = undefined;
	}

	function readableStreamReaderGenericCancel(reader, reason) {
		return readableStreamCancel(reader._stream, reason);
	}

	class ReadableStreamDefaultReader {
		constructor(stream) {
			if (!isBranded(stream, "ReadableStream")) {
				throw new TypeError("ReadableStreamDefaultReader: argument must be a ReadableStream");
			}
			if (isReadableStreamLocked(stream)) throw new TypeError("ReadableStream is locked");
			this[BRAND] = "ReadableStreamDefaultReader";
			readableStreamReaderGenericInitialize(this, stream);
			this._readRequests = [];
		}

		get closed() {
			try { brandCheck(this, "ReadableStreamDefaultReader"); } catch (e) { return rejectedPromise(e); }
			return this._closed.promise;
		}

		read() {
			try { brandCheck(this, "ReadableStreamDefaultReader"); } catch (e) { return rejectedPromise(e); }
			if (this._stream === undefined) {
				return rejectedPromise(new TypeError("Cannot read from a released reader"));
			}
			const d = newPromise();
			readableStreamDefaultReaderRead(this, {
				chunkSteps: (chunk) => d.resolve({ value: chunk, done: false }),
				closeSteps: () => d.resolve({ value: undefined, done: true }),
				errorSteps: (e) => d.reject(e),
			});
			return d.promise;
		}

		releaseLock() {
			brandCheck(this, "ReadableStreamDefaultReader");
			if (this._stream === undefined) return;
			readableStreamDefaultReaderRelease(this);
		}

		cancel(reason = undefined) {
			try { brandCheck(this, "ReadableStreamDefaultReader"); } catch (e) { return rejectedPromise(e); }
			if (this._stream === undefined) {
				return rejectedPromise(new TypeError("Cannot cancel a released reader"));
			}
			return readableStreamReaderGenericCancel(this, reason);
		}
	}
	Object.defineProperty(ReadableStreamDefaultReader.prototype, Symbol.toStringTag, {
		value: "ReadableStreamDefaultReader", configurable: true,
	});

	function readableStreamDefaultReaderRead(reader, readRequest) {
		const stream = reader._stream;
		stream._disturbed = true;
		if (stream._state === "closed") readRequest.closeSteps();
		else if (stream._state === "errored") readRequest.errorSteps(stream._storedError);
		else stream._controller._pullSteps(readRequest);
	}

	function readableStreamDefaultReaderRelease(reader) {
		readableStreamReaderGenericRelease(reader);
		const e = new TypeError("Reader was released and can no longer be used to read from its previous stream");
		const requests = reader._readRequests;
		reader._readRequests = [];
		for (const req of requests) req.errorSteps(e);
	}

	class ReadableStreamBYOBReader {
		constructor(stream) {
			if (!isBranded(stream, "ReadableStream")) {
				throw new TypeError("ReadableStreamBYOBReader: argument must be a ReadableStream");
			}
			if (!isBranded(stream._controller, "ReadableByteStreamController")) {
				throw new TypeError("Cannot use a BYOB reader with a non-byte stream");
			}
			if (isReadableStreamLocked(stream)) throw new TypeError("ReadableStream is locked");
			this[BRAND] = "ReadableStreamBYOBReader";
			readableStreamReaderGenericInitialize(this, stream);
			this._readIntoRequests = [];
		}

		get closed() {
			try { brandCheck(this, "ReadableStreamBYOBReader"); } catch (e) { return rejectedPromise(e); }
			return this._closed.promise;
		}

		// read(view, { min }) fills view and does not resolve until at least `min`
		// ELEMENTS have arrived. A stream that closes first resolves with whatever
		// did arrive and done: true — but only when nothing was outstanding; a
		// partially filled view at close is a TypeError, because the caller asked
		// for a record that the stream cannot now complete.
		read(view, options = undefined) {
			try {
				brandCheck(this, "ReadableStreamBYOBReader");
				if (!isView(view)) throw new TypeError("read: the argument must be an ArrayBufferView");
				if (view.byteLength === 0) throw new TypeError("read: the view must not be empty");
				if (view.buffer.byteLength === 0) throw new TypeError("read: the view's buffer must not be detached");
				const opts = options === undefined || options === null ? {} : options;
				let min = 1;
				if (opts.min !== undefined) {
					const raw = Number(opts.min);
					if (!Number.isInteger(raw)) throw new TypeError("read: min must be an integer");
					if (raw <= 0) throw new TypeError("read: min must be greater than 0");
					const elements = view.byteLength / (view.BYTES_PER_ELEMENT || 1);
					if (raw > elements) throw new RangeError("read: min is larger than the view");
					min = raw;
				}
				if (this._stream === undefined) throw new TypeError("Cannot read from a released reader");
			} catch (e) {
				return rejectedPromise(e);
			}
			const opts = options === undefined || options === null ? {} : options;
			const min = opts.min === undefined ? 1 : Number(opts.min);
			const d = newPromise();
			readableStreamBYOBReaderRead(this, view, min, {
				chunkSteps: (chunk) => d.resolve({ value: chunk, done: false }),
				closeSteps: (chunk) => d.resolve({ value: chunk, done: true }),
				errorSteps: (e) => d.reject(e),
			});
			return d.promise;
		}

		releaseLock() {
			brandCheck(this, "ReadableStreamBYOBReader");
			if (this._stream === undefined) return;
			readableStreamBYOBReaderRelease(this);
		}

		cancel(reason = undefined) {
			try { brandCheck(this, "ReadableStreamBYOBReader"); } catch (e) { return rejectedPromise(e); }
			if (this._stream === undefined) {
				return rejectedPromise(new TypeError("Cannot cancel a released reader"));
			}
			return readableStreamReaderGenericCancel(this, reason);
		}
	}
	Object.defineProperty(ReadableStreamBYOBReader.prototype, Symbol.toStringTag, {
		value: "ReadableStreamBYOBReader", configurable: true,
	});

	function readableStreamBYOBReaderRead(reader, view, min, readIntoRequest) {
		const stream = reader._stream;
		stream._disturbed = true;
		if (stream._state === "errored") readIntoRequest.errorSteps(stream._storedError);
		else readableByteStreamControllerPullInto(stream._controller, view, min, readIntoRequest);
	}

	function readableStreamBYOBReaderRelease(reader) {
		readableStreamReaderGenericRelease(reader);
		const e = new TypeError("Reader was released and can no longer be used to read from its previous stream");
		const requests = reader._readIntoRequests;
		reader._readIntoRequests = [];
		for (const req of requests) req.errorSteps(e);
	}

	// ------------------------------------ ReadableStreamDefaultController

	class ReadableStreamDefaultController {
		constructor() {
			throw new TypeError("Illegal constructor");
		}

		get desiredSize() {
			brandCheck(this, "ReadableStreamDefaultController");
			return readableStreamDefaultControllerGetDesiredSize(this);
		}

		close() {
			brandCheck(this, "ReadableStreamDefaultController");
			if (!readableStreamDefaultControllerCanCloseOrEnqueue(this)) {
				throw new TypeError("The stream is not in a state that permits close");
			}
			readableStreamDefaultControllerClose(this);
		}

		enqueue(chunk = undefined) {
			brandCheck(this, "ReadableStreamDefaultController");
			if (!readableStreamDefaultControllerCanCloseOrEnqueue(this)) {
				throw new TypeError("The stream is not in a state that permits enqueue");
			}
			readableStreamDefaultControllerEnqueue(this, chunk);
		}

		error(e = undefined) {
			brandCheck(this, "ReadableStreamDefaultController");
			readableStreamDefaultControllerError(this, e);
		}

		// The internal slots the stream drives the controller through. They are
		// methods rather than a switch on the controller's type so a byte
		// controller can answer the same three questions differently.
		_cancelSteps(reason) {
			resetQueue(this);
			const result = this._cancelAlgorithm(reason);
			readableStreamDefaultControllerClearAlgorithms(this);
			return result;
		}

		_pullSteps(readRequest) {
			const stream = this._stream;
			if (this._queue.length > 0) {
				const chunk = dequeueValue(this);
				if (this._closeRequested && this._queue.length === 0) {
					readableStreamDefaultControllerClearAlgorithms(this);
					readableStreamClose(stream);
				} else {
					readableStreamDefaultControllerCallPullIfNeeded(this);
				}
				readRequest.chunkSteps(chunk);
				return;
			}
			stream._reader._readRequests.push(readRequest);
			readableStreamDefaultControllerCallPullIfNeeded(this);
		}

		_releaseSteps() { /* a default controller keeps its queue across readers */ }
	}
	Object.defineProperty(ReadableStreamDefaultController.prototype, Symbol.toStringTag, {
		value: "ReadableStreamDefaultController", configurable: true,
	});

	function readableStreamDefaultControllerClearAlgorithms(controller) {
		controller._pullAlgorithm = undefined;
		controller._cancelAlgorithm = undefined;
		controller._strategySizeAlgorithm = undefined;
	}

	function readableStreamDefaultControllerCanCloseOrEnqueue(controller) {
		return !controller._closeRequested && controller._stream._state === "readable";
	}

	function readableStreamDefaultControllerGetDesiredSize(controller) {
		const state = controller._stream._state;
		if (state === "errored") return null;
		if (state === "closed") return 0;
		return controller._strategyHWM - controller._queueTotalSize;
	}

	function readableStreamDefaultControllerShouldCallPull(controller) {
		if (!readableStreamDefaultControllerCanCloseOrEnqueue(controller)) return false;
		if (!controller._started) return false;
		const stream = controller._stream;
		if (isReadableStreamLocked(stream) && stream._reader._readRequests !== undefined
			&& stream._reader._readRequests.length > 0) {
			return true;
		}
		return readableStreamDefaultControllerGetDesiredSize(controller) > 0;
	}

	function readableStreamDefaultControllerCallPullIfNeeded(controller) {
		if (!readableStreamDefaultControllerShouldCallPull(controller)) return;
		if (controller._pulling) { controller._pullAgain = true; return; }
		controller._pulling = true;
		controller._pullAlgorithm().then(
			() => {
				controller._pulling = false;
				if (controller._pullAgain) {
					controller._pullAgain = false;
					readableStreamDefaultControllerCallPullIfNeeded(controller);
				}
			},
			(e) => readableStreamDefaultControllerError(controller, e),
		);
	}

	function readableStreamDefaultControllerClose(controller) {
		if (!readableStreamDefaultControllerCanCloseOrEnqueue(controller)) return;
		const stream = controller._stream;
		controller._closeRequested = true;
		if (controller._queue.length === 0) {
			readableStreamDefaultControllerClearAlgorithms(controller);
			readableStreamClose(stream);
		}
	}

	function readableStreamDefaultControllerEnqueue(controller, chunk) {
		if (!readableStreamDefaultControllerCanCloseOrEnqueue(controller)) return;
		const stream = controller._stream;
		if (isReadableStreamLocked(stream) && stream._reader._readRequests !== undefined
			&& stream._reader._readRequests.length > 0) {
			stream._reader._readRequests.shift().chunkSteps(chunk);
		} else {
			// The size function runs for EVERY chunk, and a throw from it errors the
			// stream and is rethrown to the enqueuer.
			let size;
			try {
				size = controller._strategySizeAlgorithm(chunk);
			} catch (e) {
				readableStreamDefaultControllerError(controller, e);
				throw e;
			}
			try {
				enqueueValueWithSize(controller, chunk, size);
			} catch (e) {
				readableStreamDefaultControllerError(controller, e);
				throw e;
			}
		}
		readableStreamDefaultControllerCallPullIfNeeded(controller);
	}

	function readableStreamDefaultControllerError(controller, e) {
		const stream = controller._stream;
		if (stream._state !== "readable") return;
		resetQueue(controller);
		readableStreamDefaultControllerClearAlgorithms(controller);
		readableStreamError(stream, e);
	}

	function setUpReadableStreamDefaultControllerFromAlgorithms(stream, controller, startAlgorithm, pullAlgorithm, cancelAlgorithm, highWaterMark, sizeAlgorithm) {
		controller[BRAND] = "ReadableStreamDefaultController";
		controller._stream = stream;
		resetQueue(controller);
		controller._started = false;
		controller._closeRequested = false;
		controller._pullAgain = false;
		controller._pulling = false;
		controller._strategySizeAlgorithm = sizeAlgorithm;
		controller._strategyHWM = highWaterMark;
		controller._pullAlgorithm = pullAlgorithm;
		controller._cancelAlgorithm = cancelAlgorithm;
		stream._controller = controller;
		// start() runs SYNCHRONOUSLY — a source that throws from it throws from the
		// constructor, and one that fills the queue there has done so before the
		// constructor returns. What is deferred is the PULLING: nothing is pulled
		// until the value start returned has settled.
		const startResult = startAlgorithm();
		Promise.resolve(startResult).then(
			() => {
				controller._started = true;
				readableStreamDefaultControllerCallPullIfNeeded(controller);
			},
			(e) => readableStreamDefaultControllerError(controller, e),
		);
	}

	function setUpReadableStreamDefaultController(stream, source, startFn, pullFn, cancelFn, highWaterMark, sizeAlgorithm) {
		const controller = Object.create(ReadableStreamDefaultController.prototype);
		const startAlgorithm = startFn === undefined ? () => undefined : () => callback(startFn, source, [controller]);
		const pullAlgorithm = pullFn === undefined ? () => resolvedPromise(undefined)
			: () => promiseCall(pullFn, source, [controller]);
		const cancelAlgorithm = cancelFn === undefined ? () => resolvedPromise(undefined)
			: (reason) => promiseCall(cancelFn, source, [reason]);
		setUpReadableStreamDefaultControllerFromAlgorithms(stream, controller, startAlgorithm, pullAlgorithm, cancelAlgorithm, highWaterMark, sizeAlgorithm);
	}

	// -------------------------------------- ReadableByteStreamController

	// A pull-into descriptor is one outstanding request for the source to fill a
	// caller's buffer. It is the whole reason a byte stream is a distinct type:
	// the bytes are written into memory the READER owns, so the source and the
	// reader must never hold the same buffer at once, and the descriptor is what
	// tracks the handover.
	class ReadableStreamBYOBRequest {
		constructor() {
			throw new TypeError("Illegal constructor");
		}

		get view() {
			brandCheck(this, "ReadableStreamBYOBRequest");
			return this._view;
		}

		respond(bytesWritten) {
			brandCheck(this, "ReadableStreamBYOBRequest");
			if (this._controller === undefined) {
				throw new TypeError("respond called on a request that is no longer pending");
			}
			const n = Number(bytesWritten);
			if (!Number.isInteger(n) || n < 0) throw new TypeError("respond: bytesWritten must be a non-negative integer");
			if (this._view.buffer.byteLength === 0) throw new TypeError("respond: the view's buffer is detached");
			readableByteStreamControllerRespond(this._controller, n);
		}

		respondWithNewView(view) {
			brandCheck(this, "ReadableStreamBYOBRequest");
			if (this._controller === undefined) {
				throw new TypeError("respondWithNewView called on a request that is no longer pending");
			}
			if (!isView(view)) throw new TypeError("respondWithNewView: the argument must be an ArrayBufferView");
			if (view.buffer.byteLength === 0) throw new TypeError("respondWithNewView: the view's buffer is detached");
			readableByteStreamControllerRespondWithNewView(this._controller, view);
		}
	}
	Object.defineProperty(ReadableStreamBYOBRequest.prototype, Symbol.toStringTag, {
		value: "ReadableStreamBYOBRequest", configurable: true,
	});

	class ReadableByteStreamController {
		constructor() {
			throw new TypeError("Illegal constructor");
		}

		get byobRequest() {
			brandCheck(this, "ReadableByteStreamController");
			return readableByteStreamControllerGetBYOBRequest(this);
		}

		get desiredSize() {
			brandCheck(this, "ReadableByteStreamController");
			return readableByteStreamControllerGetDesiredSize(this);
		}

		close() {
			brandCheck(this, "ReadableByteStreamController");
			if (this._closeRequested) throw new TypeError("The stream has already been closed");
			if (this._stream._state !== "readable") throw new TypeError("The stream is not readable");
			readableByteStreamControllerClose(this);
		}

		enqueue(chunk) {
			brandCheck(this, "ReadableByteStreamController");
			if (!isView(chunk)) throw new TypeError("enqueue: a byte stream takes an ArrayBufferView");
			if (chunk.byteLength === 0) throw new TypeError("enqueue: the view must not be empty");
			if (chunk.buffer.byteLength === 0) throw new TypeError("enqueue: the view's buffer must not be detached");
			if (this._closeRequested) throw new TypeError("The stream has already been closed");
			if (this._stream._state !== "readable") throw new TypeError("The stream is not readable");
			readableByteStreamControllerEnqueue(this, chunk);
		}

		error(e = undefined) {
			brandCheck(this, "ReadableByteStreamController");
			readableByteStreamControllerError(this, e);
		}

		_cancelSteps(reason) {
			readableByteStreamControllerClearPendingPullIntos(this);
			resetQueue(this);
			const result = this._cancelAlgorithm(reason);
			readableByteStreamControllerClearAlgorithms(this);
			return result;
		}

		_pullSteps(readRequest) {
			const stream = this._stream;
			if (this._queueTotalSize > 0) {
				readableByteStreamControllerFillReadRequestFromQueue(this, readRequest);
				return;
			}
			const autoChunk = this._autoAllocateChunkSize;
			if (autoChunk !== undefined) {
				let buffer;
				try {
					buffer = new ArrayBuffer(autoChunk);
				} catch (e) {
					readRequest.errorSteps(e);
					return;
				}
				this._pendingPullIntos.push({
					buffer, bufferByteLength: autoChunk, byteOffset: 0, byteLength: autoChunk,
					bytesFilled: 0, minimumFill: 1, elementSize: 1, viewConstructor: Uint8Array,
					readerType: "default",
				});
			}
			stream._reader._readRequests.push(readRequest);
			readableByteStreamControllerCallPullIfNeeded(this);
		}

		// Releasing a reader mid-pull-into drops the descriptor: the buffer belongs
		// to the reader that is going away, and the next reader must not be handed
		// bytes written into it.
		_releaseSteps() {
			if (this._pendingPullIntos.length > 0) {
				const first = this._pendingPullIntos[0];
				first.readerType = "none";
				this._pendingPullIntos = [first];
			}
		}
	}
	Object.defineProperty(ReadableByteStreamController.prototype, Symbol.toStringTag, {
		value: "ReadableByteStreamController", configurable: true,
	});

	function readableByteStreamControllerClearAlgorithms(controller) {
		controller._pullAlgorithm = undefined;
		controller._cancelAlgorithm = undefined;
	}

	function readableByteStreamControllerClearPendingPullIntos(controller) {
		readableByteStreamControllerInvalidateBYOBRequest(controller);
		controller._pendingPullIntos = [];
	}

	function readableByteStreamControllerInvalidateBYOBRequest(controller) {
		const request = controller._byobRequest;
		if (request === null) return;
		request._controller = undefined;
		request._view = null;
		controller._byobRequest = null;
	}

	function readableByteStreamControllerGetBYOBRequest(controller) {
		if (controller._byobRequest === null && controller._pendingPullIntos.length > 0) {
			const first = controller._pendingPullIntos[0];
			const view = new Uint8Array(first.buffer, first.byteOffset + first.bytesFilled, first.byteLength - first.bytesFilled);
			const request = Object.create(ReadableStreamBYOBRequest.prototype);
			request[BRAND] = "ReadableStreamBYOBRequest";
			request._controller = controller;
			request._view = view;
			controller._byobRequest = request;
		}
		return controller._byobRequest;
	}

	function readableByteStreamControllerGetDesiredSize(controller) {
		const state = controller._stream._state;
		if (state === "errored") return null;
		if (state === "closed") return 0;
		return controller._strategyHWM - controller._queueTotalSize;
	}

	function readableByteStreamControllerShouldCallPull(controller) {
		const stream = controller._stream;
		if (stream._state !== "readable") return false;
		if (controller._closeRequested) return false;
		if (!controller._started) return false;
		const reader = stream._reader;
		if (reader !== undefined && reader._readRequests !== undefined && reader._readRequests.length > 0) return true;
		if (reader !== undefined && reader._readIntoRequests !== undefined && reader._readIntoRequests.length > 0) return true;
		return readableByteStreamControllerGetDesiredSize(controller) > 0;
	}

	function readableByteStreamControllerCallPullIfNeeded(controller) {
		if (!readableByteStreamControllerShouldCallPull(controller)) return;
		if (controller._pulling) { controller._pullAgain = true; return; }
		controller._pulling = true;
		controller._pullAlgorithm().then(
			() => {
				controller._pulling = false;
				if (controller._pullAgain) {
					controller._pullAgain = false;
					readableByteStreamControllerCallPullIfNeeded(controller);
				}
			},
			(e) => readableByteStreamControllerError(controller, e),
		);
	}

	function readableByteStreamControllerError(controller, e) {
		const stream = controller._stream;
		if (stream._state !== "readable") return;
		readableByteStreamControllerClearPendingPullIntos(controller);
		resetQueue(controller);
		readableByteStreamControllerClearAlgorithms(controller);
		readableStreamError(stream, e);
	}

	function readableByteStreamControllerClose(controller) {
		const stream = controller._stream;
		if (controller._closeRequested || stream._state !== "readable") return;
		if (controller._queueTotalSize > 0) {
			// The close waits for the queue to drain; a pending pull-into that has
			// already taken bytes cannot be satisfied and is the error the standard
			// requires here.
			controller._closeRequested = true;
			return;
		}
		if (controller._pendingPullIntos.length > 0) {
			const first = controller._pendingPullIntos[0];
			if (first.bytesFilled % first.elementSize !== 0) {
				const e = new TypeError("Insufficient bytes to fill the requested elements");
				readableByteStreamControllerError(controller, e);
				throw e;
			}
		}
		readableByteStreamControllerClearAlgorithms(controller);
		readableStreamClose(stream);
	}

	function readableByteStreamControllerEnqueue(controller, chunk) {
		const stream = controller._stream;
		if (controller._closeRequested || stream._state !== "readable") return;
		// Read before the transfer detaches the view (see pullInto).
		const chunkOffset = chunk.byteOffset;
		const chunkLength = chunk.byteLength;
		const transferred = transferBuffer(chunk.buffer);
		if (controller._pendingPullIntos.length > 0) {
			const first = controller._pendingPullIntos[0];
			if (first.buffer.byteLength === 0) throw new TypeError("The BYOB request's buffer has been detached");
			readableByteStreamControllerInvalidateBYOBRequest(controller);
			first.buffer = transferBuffer(first.buffer);
			if (first.readerType === "none") {
				readableByteStreamControllerEnqueueDetachedPullIntoToQueue(controller, first);
			}
		}
		const reader = stream._reader;
		if (reader !== undefined && reader._readRequests !== undefined) {
			readableByteStreamControllerProcessReadRequestsUsingQueue(controller);
			if (reader._readRequests.length === 0) {
				readableByteStreamControllerEnqueueChunkToQueue(controller, transferred, chunkOffset, chunkLength);
			} else {
				if (controller._pendingPullIntos.length > 0) readableByteStreamControllerShiftPendingPullInto(controller);
				const view = new Uint8Array(transferred, chunkOffset, chunkLength);
				reader._readRequests.shift().chunkSteps(view);
			}
		} else if (reader !== undefined && reader._readIntoRequests !== undefined) {
			readableByteStreamControllerEnqueueChunkToQueue(controller, transferred, chunkOffset, chunkLength);
			readableByteStreamControllerProcessPullIntoDescriptorsUsingQueue(controller);
		} else {
			readableByteStreamControllerEnqueueChunkToQueue(controller, transferred, chunkOffset, chunkLength);
		}
		readableByteStreamControllerCallPullIfNeeded(controller);
	}

	function readableByteStreamControllerEnqueueChunkToQueue(controller, buffer, byteOffset, byteLength) {
		controller._queue.push({ buffer, byteOffset, byteLength });
		controller._queueTotalSize += byteLength;
	}

	function readableByteStreamControllerEnqueueClonedChunkToQueue(controller, buffer, byteOffset, byteLength) {
		let cloned;
		try {
			cloned = buffer.slice(byteOffset, byteOffset + byteLength);
		} catch (e) {
			readableByteStreamControllerError(controller, e);
			throw e;
		}
		readableByteStreamControllerEnqueueChunkToQueue(controller, cloned, 0, byteLength);
	}

	function readableByteStreamControllerEnqueueDetachedPullIntoToQueue(controller, descriptor) {
		if (descriptor.bytesFilled > 0) {
			readableByteStreamControllerEnqueueClonedChunkToQueue(controller, descriptor.buffer, descriptor.byteOffset, descriptor.bytesFilled);
		}
		readableByteStreamControllerShiftPendingPullInto(controller);
	}

	function readableByteStreamControllerShiftPendingPullInto(controller) {
		const descriptor = controller._pendingPullIntos.shift();
		return descriptor;
	}

	function readableByteStreamControllerFillPullIntoDescriptorFromQueue(controller, descriptor) {
		const maxBytesToCopy = Math.min(controller._queueTotalSize, descriptor.byteLength - descriptor.bytesFilled);
		const maxBytesFilled = descriptor.bytesFilled + maxBytesToCopy;
		let totalBytesToCopyRemaining = maxBytesToCopy;
		let ready = false;
		const remainderBytes = maxBytesFilled % descriptor.elementSize;
		const maxAlignedBytes = maxBytesFilled - remainderBytes;
		if (maxAlignedBytes >= descriptor.minimumFill) {
			totalBytesToCopyRemaining = maxAlignedBytes - descriptor.bytesFilled;
			ready = true;
		}
		while (totalBytesToCopyRemaining > 0) {
			const head = controller._queue[0];
			const bytesToCopy = Math.min(totalBytesToCopyRemaining, head.byteLength);
			const destStart = descriptor.byteOffset + descriptor.bytesFilled;
			new Uint8Array(descriptor.buffer).set(new Uint8Array(head.buffer, head.byteOffset, bytesToCopy), destStart);
			if (head.byteLength === bytesToCopy) controller._queue.shift();
			else {
				head.byteOffset += bytesToCopy;
				head.byteLength -= bytesToCopy;
			}
			controller._queueTotalSize -= bytesToCopy;
			descriptor.bytesFilled += bytesToCopy;
			totalBytesToCopyRemaining -= bytesToCopy;
		}
		return ready;
	}

	function readableByteStreamControllerProcessReadRequestsUsingQueue(controller) {
		const reader = controller._stream._reader;
		while (reader._readRequests.length > 0) {
			if (controller._queueTotalSize === 0) return;
			readableByteStreamControllerFillReadRequestFromQueue(controller, reader._readRequests.shift());
		}
	}

	function readableByteStreamControllerFillReadRequestFromQueue(controller, readRequest) {
		const entry = controller._queue.shift();
		controller._queueTotalSize -= entry.byteLength;
		readableByteStreamControllerHandleQueueDrain(controller);
		readRequest.chunkSteps(new Uint8Array(entry.buffer, entry.byteOffset, entry.byteLength));
	}

	function readableByteStreamControllerHandleQueueDrain(controller) {
		if (controller._queueTotalSize === 0 && controller._closeRequested) {
			readableByteStreamControllerClearAlgorithms(controller);
			readableStreamClose(controller._stream);
		} else {
			readableByteStreamControllerCallPullIfNeeded(controller);
		}
	}

	function readableByteStreamControllerProcessPullIntoDescriptorsUsingQueue(controller) {
		while (controller._pendingPullIntos.length > 0) {
			if (controller._queueTotalSize === 0) return;
			const descriptor = controller._pendingPullIntos[0];
			if (readableByteStreamControllerFillPullIntoDescriptorFromQueue(controller, descriptor)) {
				readableByteStreamControllerShiftPendingPullInto(controller);
				readableByteStreamControllerCommitPullIntoDescriptor(controller._stream, descriptor);
			} else {
				return;
			}
		}
	}

	function readableByteStreamControllerCommitPullIntoDescriptor(stream, descriptor) {
		let done = false;
		if (stream._state === "closed") done = true;
		const filled = readableByteStreamControllerConvertPullIntoDescriptor(descriptor);
		if (descriptor.readerType === "default") {
			stream._reader._readRequests.shift().chunkSteps(filled);
		} else {
			const request = stream._reader._readIntoRequests.shift();
			if (done) request.closeSteps(filled);
			else request.chunkSteps(filled);
		}
	}

	function readableByteStreamControllerConvertPullIntoDescriptor(descriptor) {
		const bytesFilled = descriptor.bytesFilled;
		const elementSize = descriptor.elementSize;
		return new descriptor.viewConstructor(descriptor.buffer, descriptor.byteOffset, bytesFilled / elementSize);
	}

	function readableByteStreamControllerPullInto(controller, view, min, readIntoRequest) {
		const stream = controller._stream;
		const ctor = view.constructor;
		const elementSize = view.BYTES_PER_ELEMENT || 1;
		const minimumFill = min * elementSize;
		// Everything the view describes is recorded BEFORE the transfer: taking the
		// buffer detaches the view, and a detached view reports offset and length
		// zero — so a descriptor built from it afterwards asked for no bytes at all.
		const byteOffset = view.byteOffset;
		const byteLength = view.byteLength;
		let buffer;
		try {
			buffer = transferBuffer(view.buffer);
		} catch (e) {
			readIntoRequest.errorSteps(e);
			return;
		}
		const descriptor = {
			buffer, bufferByteLength: buffer.byteLength,
			byteOffset, byteLength,
			bytesFilled: 0, minimumFill, elementSize,
			viewConstructor: ctor === DataView ? DataView : ctor,
			readerType: "byob",
		};
		if (controller._pendingPullIntos.length > 0) {
			controller._pendingPullIntos.push(descriptor);
			stream._reader._readIntoRequests.push(readIntoRequest);
			return;
		}
		if (stream._state === "closed") {
			const empty = new descriptor.viewConstructor(descriptor.buffer, descriptor.byteOffset, 0);
			readIntoRequest.closeSteps(empty);
			return;
		}
		if (controller._queueTotalSize > 0) {
			if (readableByteStreamControllerFillPullIntoDescriptorFromQueue(controller, descriptor)) {
				const filled = readableByteStreamControllerConvertPullIntoDescriptor(descriptor);
				readableByteStreamControllerHandleQueueDrain(controller);
				readIntoRequest.chunkSteps(filled);
				return;
			}
			if (controller._closeRequested) {
				const e = new TypeError("Insufficient bytes to fill the requested elements");
				readableByteStreamControllerError(controller, e);
				readIntoRequest.errorSteps(e);
				return;
			}
		}
		controller._pendingPullIntos.push(descriptor);
		stream._reader._readIntoRequests.push(readIntoRequest);
		readableByteStreamControllerCallPullIfNeeded(controller);
	}

	function readableByteStreamControllerRespond(controller, bytesWritten) {
		const first = controller._pendingPullIntos[0];
		const state = controller._stream._state;
		if (state === "closed") {
			if (bytesWritten !== 0) throw new TypeError("respond: the stream is closed, so bytesWritten must be 0");
		} else if (first.bytesFilled + bytesWritten > first.byteLength) {
			throw new RangeError("respond: bytesWritten is larger than the view");
		}
		first.buffer = transferBuffer(first.buffer);
		readableByteStreamControllerRespondInternal(controller, bytesWritten);
	}

	function readableByteStreamControllerRespondWithNewView(controller, view) {
		const first = controller._pendingPullIntos[0];
		const state = controller._stream._state;
		if (state === "closed") {
			if (view.byteLength !== 0) throw new TypeError("respondWithNewView: the stream is closed, so the view must be empty");
		} else if (view.byteLength === 0) {
			throw new TypeError("respondWithNewView: the view must not be empty");
		}
		if (first.byteOffset + first.bytesFilled !== view.byteOffset) {
			throw new RangeError("respondWithNewView: the view must start where the request left off");
		}
		if (first.bufferByteLength !== view.buffer.byteLength) {
			throw new RangeError("respondWithNewView: the view's buffer is a different size");
		}
		if (first.bytesFilled + view.byteLength > first.byteLength) {
			throw new RangeError("respondWithNewView: the view is larger than the request");
		}
		const written = view.byteLength;
		first.buffer = transferBuffer(view.buffer);
		readableByteStreamControllerRespondInternal(controller, written);
	}

	function readableByteStreamControllerRespondInternal(controller, bytesWritten) {
		const first = controller._pendingPullIntos[0];
		readableByteStreamControllerInvalidateBYOBRequest(controller);
		if (controller._stream._state === "closed") {
			readableByteStreamControllerRespondInClosedState(controller, first);
		} else {
			readableByteStreamControllerRespondInReadableState(controller, bytesWritten, first);
		}
		readableByteStreamControllerCallPullIfNeeded(controller);
	}

	function readableByteStreamControllerRespondInClosedState(controller, first) {
		if (first.readerType === "none") readableByteStreamControllerShiftPendingPullInto(controller);
		const stream = controller._stream;
		if (stream._reader !== undefined && stream._reader._readIntoRequests !== undefined) {
			while (stream._reader._readIntoRequests.length > 0) {
				const descriptor = readableByteStreamControllerShiftPendingPullInto(controller);
				readableByteStreamControllerCommitPullIntoDescriptor(stream, descriptor);
			}
		}
	}

	function readableByteStreamControllerRespondInReadableState(controller, bytesWritten, descriptor) {
		descriptor.bytesFilled += bytesWritten;
		if (descriptor.readerType === "none") {
			readableByteStreamControllerEnqueueDetachedPullIntoToQueue(controller, descriptor);
			readableByteStreamControllerProcessPullIntoDescriptorsUsingQueue(controller);
			return;
		}
		if (descriptor.bytesFilled < descriptor.minimumFill) return;
		readableByteStreamControllerShiftPendingPullInto(controller);
		const remainder = descriptor.bytesFilled % descriptor.elementSize;
		if (remainder > 0) {
			const end = descriptor.byteOffset + descriptor.bytesFilled;
			readableByteStreamControllerEnqueueClonedChunkToQueue(controller, descriptor.buffer, end - remainder, remainder);
		}
		descriptor.bytesFilled -= remainder;
		readableByteStreamControllerCommitPullIntoDescriptor(controller._stream, descriptor);
		readableByteStreamControllerProcessPullIntoDescriptorsUsingQueue(controller);
	}

	function setUpReadableByteStreamControllerFromAlgorithms(stream, controller, startAlgorithm, pullAlgorithm, cancelAlgorithm, highWaterMark, autoAllocateChunkSize) {
		controller[BRAND] = "ReadableByteStreamController";
		controller._stream = stream;
		resetQueue(controller);
		controller._started = false;
		controller._closeRequested = false;
		controller._pullAgain = false;
		controller._pulling = false;
		controller._byobRequest = null;
		controller._strategyHWM = highWaterMark;
		controller._pullAlgorithm = pullAlgorithm;
		controller._cancelAlgorithm = cancelAlgorithm;
		controller._autoAllocateChunkSize = autoAllocateChunkSize;
		controller._pendingPullIntos = [];
		stream._controller = controller;
		const startResult = startAlgorithm();
		Promise.resolve(startResult).then(
			() => {
				controller._started = true;
				readableByteStreamControllerCallPullIfNeeded(controller);
			},
			(e) => readableByteStreamControllerError(controller, e),
		);
	}

	function setUpReadableByteStreamController(stream, source, startFn, pullFn, cancelFn, highWaterMark, autoAllocateChunkSize) {
		const controller = Object.create(ReadableByteStreamController.prototype);
		const startAlgorithm = startFn === undefined ? () => undefined : () => callback(startFn, source, [controller]);
		const pullAlgorithm = pullFn === undefined ? () => resolvedPromise(undefined)
			: () => promiseCall(pullFn, source, [controller]);
		const cancelAlgorithm = cancelFn === undefined ? () => resolvedPromise(undefined)
			: (reason) => promiseCall(cancelFn, source, [reason]);
		setUpReadableByteStreamControllerFromAlgorithms(stream, controller, startAlgorithm, pullAlgorithm, cancelAlgorithm, highWaterMark, autoAllocateChunkSize);
	}

	// -------------------------------------------------- tee, from, iteration

	function readableStreamTee(stream, cloneForBranch2) {
		if (isBranded(stream._controller, "ReadableByteStreamController")) {
			return readableByteStreamTee(stream);
		}
		return readableStreamDefaultTee(stream, cloneForBranch2);
	}

	function readableStreamDefaultTee(stream) {
		const reader = new ReadableStreamDefaultReader(stream);
		let reading = false, readAgain = false, canceled1 = false, canceled2 = false;
		let reason1, reason2, branch1, branch2;
		const cancelDeferred = newPromise();

		const pullAlgorithm = () => {
			if (reading) { readAgain = true; return resolvedPromise(undefined); }
			reading = true;
			readableStreamDefaultReaderRead(reader, {
				chunkSteps: (chunk) => {
					// The enqueues happen in a microtask so that a branch's own pull,
					// triggered by the enqueue, cannot re-enter this read synchronously.
					queueMicrotask(() => {
						readAgain = false;
						if (!canceled1) readableStreamDefaultControllerEnqueue(branch1._controller, chunk);
						if (!canceled2) readableStreamDefaultControllerEnqueue(branch2._controller, chunk);
						reading = false;
						if (readAgain) pullAlgorithm();
					});
				},
				closeSteps: () => {
					reading = false;
					if (!canceled1) readableStreamDefaultControllerClose(branch1._controller);
					if (!canceled2) readableStreamDefaultControllerClose(branch2._controller);
					if (!canceled1 || !canceled2) cancelDeferred.resolve(undefined);
				},
				errorSteps: () => { reading = false; },
			});
			return resolvedPromise(undefined);
		};
		const cancel1 = (reason) => {
			canceled1 = true;
			reason1 = reason;
			if (canceled2) cancelDeferred.resolve(readableStreamCancel(stream, [reason1, reason2]));
			return cancelDeferred.promise;
		};
		const cancel2 = (reason) => {
			canceled2 = true;
			reason2 = reason;
			if (canceled1) cancelDeferred.resolve(readableStreamCancel(stream, [reason1, reason2]));
			return cancelDeferred.promise;
		};
		branch1 = createReadableStream(() => undefined, pullAlgorithm, cancel1);
		branch2 = createReadableStream(() => undefined, pullAlgorithm, cancel2);
		markHandled(reader._closed.promise.catch((e) => {
			readableStreamDefaultControllerError(branch1._controller, e);
			readableStreamDefaultControllerError(branch2._controller, e);
			if (!canceled1 || !canceled2) cancelDeferred.resolve(undefined);
		}));
		return [branch1, branch2];
	}

	// A byte stream's branches are byte streams too, which means each has to be
	// able to serve a BYOB read of its own — so the tee holds BOTH a default
	// reader and a BYOB reader over the source and swaps between them.
	function readableByteStreamTee(stream) {
		let reader = new ReadableStreamDefaultReader(stream);
		let reading = false, readAgainForBranch1 = false, readAgainForBranch2 = false;
		let canceled1 = false, canceled2 = false, reason1, reason2, branch1, branch2;
		const cancelDeferred = newPromise();

		const forwardReaderError = (thisReader) => {
			markHandled(thisReader._closed.promise.catch((e) => {
				if (thisReader !== reader) return;
				readableByteStreamControllerError(branch1._controller, e);
				readableByteStreamControllerError(branch2._controller, e);
				if (!canceled1 || !canceled2) cancelDeferred.resolve(undefined);
			}));
		};

		const pullWithDefaultReader = () => {
			if (isBranded(reader, "ReadableStreamBYOBReader")) {
				readableStreamBYOBReaderRelease(reader);
				reader = new ReadableStreamDefaultReader(stream);
				forwardReaderError(reader);
			}
			readableStreamDefaultReaderRead(reader, {
				chunkSteps: (chunk) => {
					queueMicrotask(() => {
						readAgainForBranch1 = false;
						readAgainForBranch2 = false;
						const chunk1 = chunk;
						let chunk2 = chunk;
						if (!canceled1 && !canceled2) {
							// Each branch gets its OWN copy: a branch may transfer the
							// buffer it is handed, and the other branch's chunk must not
							// go with it.
							chunk2 = new Uint8Array(chunk.buffer.slice(chunk.byteOffset, chunk.byteOffset + chunk.byteLength));
						}
						if (!canceled1) readableByteStreamControllerEnqueue(branch1._controller, chunk1);
						if (!canceled2) readableByteStreamControllerEnqueue(branch2._controller, chunk2);
						reading = false;
						if (readAgainForBranch1) pull1();
						else if (readAgainForBranch2) pull2();
					});
				},
				closeSteps: () => {
					reading = false;
					if (!canceled1) readableByteStreamControllerClose(branch1._controller);
					if (!canceled2) readableByteStreamControllerClose(branch2._controller);
					if (branch1._controller._pendingPullIntos.length > 0) readableByteStreamControllerRespond(branch1._controller, 0);
					if (branch2._controller._pendingPullIntos.length > 0) readableByteStreamControllerRespond(branch2._controller, 0);
					if (!canceled1 || !canceled2) cancelDeferred.resolve(undefined);
				},
				errorSteps: () => { reading = false; },
			});
		};

		const pullWithBYOBReader = (view, forBranch2) => {
			if (isBranded(reader, "ReadableStreamDefaultReader")) {
				readableStreamDefaultReaderRelease(reader);
				reader = new ReadableStreamBYOBReader(stream);
				forwardReaderError(reader);
			}
			const byobBranch = forBranch2 ? branch2 : branch1;
			const otherBranch = forBranch2 ? branch1 : branch2;
			readableStreamBYOBReaderRead(reader, view, 1, {
				chunkSteps: (chunk) => {
					queueMicrotask(() => {
						readAgainForBranch1 = false;
						readAgainForBranch2 = false;
						const byobCanceled = forBranch2 ? canceled2 : canceled1;
						const otherCanceled = forBranch2 ? canceled1 : canceled2;
						if (!otherCanceled) {
							const clone = new Uint8Array(chunk.buffer.slice(chunk.byteOffset, chunk.byteOffset + chunk.byteLength));
							if (!byobCanceled) readableByteStreamControllerRespondWithNewView(byobBranch._controller, chunk);
							readableByteStreamControllerEnqueue(otherBranch._controller, clone);
						} else if (!byobCanceled) {
							readableByteStreamControllerRespondWithNewView(byobBranch._controller, chunk);
						}
						reading = false;
						if (readAgainForBranch1) pull1();
						else if (readAgainForBranch2) pull2();
					});
				},
				closeSteps: (chunk) => {
					reading = false;
					const byobCanceled = forBranch2 ? canceled2 : canceled1;
					const otherCanceled = forBranch2 ? canceled1 : canceled2;
					if (!byobCanceled) readableByteStreamControllerClose(byobBranch._controller);
					if (!otherCanceled) readableByteStreamControllerClose(otherBranch._controller);
					if (chunk !== undefined && !byobCanceled) readableByteStreamControllerRespondWithNewView(byobBranch._controller, chunk);
					if (chunk !== undefined && !otherCanceled && otherBranch._controller._pendingPullIntos.length > 0) {
						readableByteStreamControllerRespond(otherBranch._controller, 0);
					}
					if (!byobCanceled || !otherCanceled) cancelDeferred.resolve(undefined);
				},
				errorSteps: () => { reading = false; },
			});
		};

		const pull1 = () => {
			if (reading) { readAgainForBranch1 = true; return resolvedPromise(undefined); }
			reading = true;
			const request = readableByteStreamControllerGetBYOBRequest(branch1._controller);
			if (request === null) pullWithDefaultReader();
			else pullWithBYOBReader(request._view, false);
			return resolvedPromise(undefined);
		};
		const pull2 = () => {
			if (reading) { readAgainForBranch2 = true; return resolvedPromise(undefined); }
			reading = true;
			const request = readableByteStreamControllerGetBYOBRequest(branch2._controller);
			if (request === null) pullWithDefaultReader();
			else pullWithBYOBReader(request._view, true);
			return resolvedPromise(undefined);
		};
		const cancel1 = (reason) => {
			canceled1 = true;
			reason1 = reason;
			if (canceled2) cancelDeferred.resolve(readableStreamCancel(stream, [reason1, reason2]));
			return cancelDeferred.promise;
		};
		const cancel2 = (reason) => {
			canceled2 = true;
			reason2 = reason;
			if (canceled1) cancelDeferred.resolve(readableStreamCancel(stream, [reason1, reason2]));
			return cancelDeferred.promise;
		};
		branch1 = createReadableByteStream(() => undefined, pull1, cancel1);
		branch2 = createReadableByteStream(() => undefined, pull2, cancel2);
		forwardReaderError(reader);
		return [branch1, branch2];
	}

	function readableStreamFromIterable(asyncIterable) {
		if (asyncIterable === null || (typeof asyncIterable !== "object" && typeof asyncIterable !== "function")) {
			// A primitive is not an iterable here even when the language would
			// iterate it: from() takes an object, and a string that silently became
			// a stream of characters would be a surprise, not a convenience.
			throw new TypeError("ReadableStream.from requires an object");
		}
		let iterator, usingAsync = false;
		if (typeof asyncIterable[Symbol.asyncIterator] === "function") {
			iterator = asyncIterable[Symbol.asyncIterator]();
			usingAsync = true;
		} else if (typeof asyncIterable[Symbol.iterator] === "function") {
			iterator = asyncIterable[Symbol.iterator]();
		} else if (typeof asyncIterable.next === "function") {
			iterator = asyncIterable; // a bare iterator
		} else {
			throw new TypeError("ReadableStream.from called on a non-iterable");
		}
		if (iterator === null || (typeof iterator !== "object" && typeof iterator !== "function")) {
			throw new TypeError("ReadableStream.from: the iterator method did not return an object");
		}
		const nextAlgorithm = async () => {
			const result = await iterator.next();
			if (result === null || typeof result !== "object") {
				throw new TypeError("The iterator's next() did not return an object");
			}
			return result;
		};
		const stream = createReadableStream(
			() => undefined,
			async () => {
				const result = await nextAlgorithm();
				if (result.done) readableStreamDefaultControllerClose(stream._controller);
				else readableStreamDefaultControllerEnqueue(stream._controller, usingAsync ? result.value : await result.value);
			},
			async (reason) => {
				const returnMethod = iterator.return;
				if (returnMethod === undefined || returnMethod === null) return undefined;
				if (typeof returnMethod !== "function") {
					throw new TypeError("ReadableStream.from: the iterator's return is not a method");
				}
				const returned = await iterator.return(reason);
				if (usingAsync && (returned === null || typeof returned !== "object")) {
					throw new TypeError("The iterator's return() did not return an object");
				}
				return undefined;
			},
			0,
		);
		return stream;
	}

	// The async-iterator prototype is a real object with its own prototype
	// chain, not a literal: `Object.getPrototypeOf` of one has to be the same
	// object every time, and it has to inherit from %AsyncIteratorPrototype%.
	const AsyncIteratorPrototype = Object.getPrototypeOf(Object.getPrototypeOf(async function* () {}).prototype);
	const ReadableStreamAsyncIteratorPrototype = Object.create(AsyncIteratorPrototype);
	Object.defineProperties(ReadableStreamAsyncIteratorPrototype, {
		next: {
			value: function next() {
				if (!isBranded(this, "ReadableStreamAsyncIterator")) {
					return rejectedPromise(new TypeError("next called on a non-iterator"));
				}
				const reader = this._reader;
				if (reader._stream === undefined) {
					return rejectedPromise(new TypeError("Cannot get the next iteration result once the reader has been released"));
				}
				const d = newPromise();
				readableStreamDefaultReaderRead(reader, {
					chunkSteps: (chunk) => d.resolve({ value: chunk, done: false }),
					closeSteps: () => {
						readableStreamDefaultReaderRelease(reader);
						d.resolve({ value: undefined, done: true });
					},
					errorSteps: (e) => {
						readableStreamDefaultReaderRelease(reader);
						d.reject(e);
					},
				});
				return d.promise;
			},
			writable: true, enumerable: true, configurable: true,
		},
		return: {
			value: function returnFn(value = undefined) {
				if (!isBranded(this, "ReadableStreamAsyncIterator")) {
					return rejectedPromise(new TypeError("return called on a non-iterator"));
				}
				const reader = this._reader;
				if (reader._stream === undefined) {
					return rejectedPromise(new TypeError("Cannot finish iterating once the reader has been released"));
				}
				if (reader._readRequests.length > 0) {
					return rejectedPromise(new TypeError("Cannot finish iterating with a read in progress"));
				}
				if (!this._preventCancel) {
					const result = readableStreamReaderGenericCancel(reader, value);
					readableStreamDefaultReaderRelease(reader);
					return result.then(() => ({ value, done: true }));
				}
				readableStreamDefaultReaderRelease(reader);
				return resolvedPromise({ value, done: true });
			},
			writable: true, enumerable: true, configurable: true,
		},
	});

	function acquireReadableStreamAsyncIterator(stream, preventCancel) {
		const reader = new ReadableStreamDefaultReader(stream);
		const iterator = Object.create(ReadableStreamAsyncIteratorPrototype);
		iterator[BRAND] = "ReadableStreamAsyncIterator";
		iterator._reader = reader;
		iterator._preventCancel = preventCancel;
		return iterator;
	}

	// ------------------------------------------------------------- pipeTo

	function convertPipeOptions(options, who) {
		const opts = options === undefined || options === null ? {} : options;
		if (typeof opts !== "object" && typeof opts !== "function") {
			throw new TypeError(`${who}: options must be an object`);
		}
		// The members are read in the order the IDL dictionary declares them, which
		// for a dictionary is alphabetical — observable through getters, and what a
		// throwing one is measured against.
		const preventAbort = Boolean(opts.preventAbort);
		const preventCancel = Boolean(opts.preventCancel);
		const preventClose = Boolean(opts.preventClose);
		const signal = opts.signal;
		// `AbortSignal signal` is not nullable, so null is a TypeError rather than
		// "no signal" — a caller that passes null has made a mistake, and silently
		// piping without a signal would hide it.
		if (signal !== undefined && !(typeof AbortSignal === "function" && signal instanceof AbortSignal)) {
			throw new TypeError(`${who}: options.signal is not an AbortSignal`);
		}
		return { preventClose, preventAbort, preventCancel, signal };
	}

	// The standard's pipeTo is a state machine, not a loop: a source that stops
	// producing, a destination that errors mid-write, and an abort signal all
	// have to be able to end the pipe, and each ends it differently. Written as
	// an await loop, an abort could not interrupt a read that never settles, and
	// the shutdown ordering (finish the in-flight write, THEN act) was lost.
	function readableStreamPipeTo(source, dest, preventClose, preventAbort, preventCancel, signal) {
		const reader = new ReadableStreamDefaultReader(source);
		const writer = acquireWritableStreamDefaultWriter(dest);
		source._disturbed = true;

		let shuttingDown = false;
		let currentWrite = resolvedPromise(undefined);
		const promise = newPromise();
		let abortAlgorithm;

		const finalize = (isError, error) => {
			writableStreamDefaultWriterRelease(writer);
			readableStreamDefaultReaderRelease(reader);
			if (signal !== undefined) signal.removeEventListener("abort", abortAlgorithm);
			if (isError) promise.reject(error);
			else promise.resolve(undefined);
		};

		const waitForWritesToFinish = () => {
			const oldCurrentWrite = currentWrite;
			return markHandled(oldCurrentWrite.catch(() => {})).then(
				() => (oldCurrentWrite !== currentWrite ? waitForWritesToFinish() : undefined),
			);
		};

		const shutdownWithAction = (action, originalIsError, originalError) => {
			if (shuttingDown) return;
			shuttingDown = true;
			const doTheRest = () => markHandled(action().then(
				() => finalize(originalIsError, originalError),
				(newError) => finalize(true, newError),
			));
			if (dest._state === "writable" && !writableStreamCloseQueuedOrInFlight(dest)) {
				markHandled(waitForWritesToFinish().then(doTheRest));
			} else {
				doTheRest();
			}
		};

		const shutdown = (isError, error) => {
			if (shuttingDown) return;
			shuttingDown = true;
			if (dest._state === "writable" && !writableStreamCloseQueuedOrInFlight(dest)) {
				markHandled(waitForWritesToFinish().then(() => finalize(isError, error)));
			} else {
				finalize(isError, error);
			}
		};

		if (signal !== undefined) {
			abortAlgorithm = () => {
				const error = signal.reason !== undefined ? signal.reason
					: new DOMException("The operation was aborted", "AbortError");
				const actions = [];
				if (!preventAbort) {
					actions.push(() => (dest._state === "writable"
						? writableStreamAbort(dest, error) : resolvedPromise(undefined)));
				}
				if (!preventCancel) {
					actions.push(() => (source._state === "readable"
						? readableStreamCancel(source, error) : resolvedPromise(undefined)));
				}
				shutdownWithAction(() => Promise.all(actions.map((a) => a())), true, error);
			};
			if (signal.aborted) {
				abortAlgorithm();
				return promise.promise;
			}
			signal.addEventListener("abort", abortAlgorithm);
		}

		const isOrBecomesErrored = (stream, p, action) => {
			if (stream._state === "errored") action(stream._storedError);
			else markHandled(p.catch(action));
		};
		const isOrBecomesClosed = (stream, p, action) => {
			if (stream._state === "closed") action();
			else markHandled(p.then(action, () => {}));
		};

		// Errors propagate in both directions, and each direction has its own rule
		// about whether the other side is cancelled or aborted.
		isOrBecomesErrored(source, reader._closed.promise, (storedError) => {
			if (!preventAbort) shutdownWithAction(() => writableStreamAbort(dest, storedError), true, storedError);
			else shutdown(true, storedError);
		});
		isOrBecomesErrored(dest, writer._closed.promise, (storedError) => {
			if (!preventCancel) shutdownWithAction(() => readableStreamCancel(source, storedError), true, storedError);
			else shutdown(true, storedError);
		});
		isOrBecomesClosed(source, reader._closed.promise, () => {
			if (!preventClose) shutdownWithAction(() => writableStreamDefaultWriterCloseWithErrorPropagation(writer));
			else shutdown();
		});
		if (writableStreamCloseQueuedOrInFlight(dest) || dest._state === "closed") {
			const destClosed = new TypeError("the destination writable stream closed before all the data could be piped to it");
			if (!preventCancel) shutdownWithAction(() => readableStreamCancel(source, destClosed), true, destClosed);
			else shutdown(true, destClosed);
		}

		const pipeStep = () => {
			if (shuttingDown) return resolvedPromise(true);
			return writer._ready.promise.then(() => new Promise((resolve, reject) => {
				readableStreamDefaultReaderRead(reader, {
					// In a microtask, because a read request is fulfilled INSIDE
					// controller.enqueue(): writing from there would run the
					// destination's sink synchronously from the source's enqueue, which
					// no producer expects and the standard forbids.
					chunkSteps: (chunk) => queueMicrotask(() => {
						currentWrite = markHandled(writableStreamDefaultWriterWrite(writer, chunk).catch(() => {}));
						resolve(false);
					}),
					closeSteps: () => resolve(true),
					errorSteps: reject,
				});
			}));
		};
		const pipeLoop = () => new Promise((resolveLoop, rejectLoop) => {
			const next = (done) => {
				if (done) resolveLoop(undefined);
				else markHandled(pipeStep().then(next, rejectLoop));
			};
			next(false);
		});
		markHandled(pipeLoop().catch(() => {}));

		return promise.promise;
	}

	// -------------------------------------------------------- WritableStream

	class WritableStream {
		constructor(underlyingSink = undefined, strategy = undefined) {
			const strat = convertStrategy(strategy, "WritableStream");
			const sink = underlyingSink === undefined || underlyingSink === null ? {} : underlyingSink;
			if (typeof sink !== "object" && typeof sink !== "function") {
				throw new TypeError("WritableStream: the underlying sink must be an object");
			}
			const abortFn = method(sink, "abort", "underlyingSink");
			const closeFn = method(sink, "close", "underlyingSink");
			const startFn = method(sink, "start", "underlyingSink");
			if (sink.type !== undefined) throw new RangeError("WritableStream: an underlying sink has no type");
			const writeFn = method(sink, "write", "underlyingSink");
			initializeWritableStream(this);
			const sizeAlgorithm = makeSizeAlgorithm(strat.size);
			const hwm = strat.highWaterMark === undefined ? 1 : convertHWM(strat.highWaterMark);
			setUpWritableStreamDefaultController(this, sink, startFn, writeFn, closeFn, abortFn, hwm, sizeAlgorithm);
		}

		get locked() {
			brandCheck(this, "WritableStream");
			return isWritableStreamLocked(this);
		}

		abort(reason = undefined) {
			try { brandCheck(this, "WritableStream"); } catch (e) { return rejectedPromise(e); }
			if (isWritableStreamLocked(this)) {
				return rejectedPromise(new TypeError("Cannot abort a stream that already has a writer"));
			}
			return writableStreamAbort(this, reason);
		}

		close() {
			try { brandCheck(this, "WritableStream"); } catch (e) { return rejectedPromise(e); }
			if (isWritableStreamLocked(this)) {
				return rejectedPromise(new TypeError("Cannot close a stream that already has a writer"));
			}
			if (writableStreamCloseQueuedOrInFlight(this)) {
				return rejectedPromise(new TypeError("Cannot close an already-closing stream"));
			}
			return writableStreamClose(this);
		}

		getWriter() {
			brandCheck(this, "WritableStream");
			return acquireWritableStreamDefaultWriter(this);
		}
	}
	Object.defineProperty(WritableStream.prototype, Symbol.toStringTag, {
		value: "WritableStream", configurable: true,
	});

	function initializeWritableStream(stream) {
		stream[BRAND] = "WritableStream";
		stream._state = "writable";
		stream._storedError = undefined;
		stream._writer = undefined;
		stream._controller = undefined;
		stream._inFlightWriteRequest = undefined;
		stream._closeRequest = undefined;
		stream._inFlightCloseRequest = undefined;
		stream._pendingAbortRequest = undefined;
		stream._writeRequests = [];
		stream._backpressure = false;
	}

	const isWritableStreamLocked = (stream) => stream._writer !== undefined;

	function createWritableStream(startAlgorithm, writeAlgorithm, closeAlgorithm, abortAlgorithm, highWaterMark = 1, sizeAlgorithm = () => 1) {
		const stream = Object.create(WritableStream.prototype);
		initializeWritableStream(stream);
		const controller = Object.create(WritableStreamDefaultController.prototype);
		setUpWritableStreamDefaultControllerFromAlgorithms(stream, controller, startAlgorithm, writeAlgorithm, closeAlgorithm, abortAlgorithm, highWaterMark, sizeAlgorithm);
		return stream;
	}

	function writableStreamAbort(stream, reason) {
		const state = stream._state;
		if (state === "closed" || state === "errored") return resolvedPromise(undefined);
		stream._controller._abortController.abort(reason);
		if (stream._state === "closed" || stream._state === "errored") return resolvedPromise(undefined);
		if (stream._pendingAbortRequest !== undefined) return stream._pendingAbortRequest.promise;
		let wasAlreadyErroring = false;
		if (stream._state === "erroring") {
			wasAlreadyErroring = true;
			reason = undefined;
		}
		const d = newPromise();
		stream._pendingAbortRequest = { promise: d.promise, resolve: d.resolve, reject: d.reject, reason, wasAlreadyErroring };
		if (!wasAlreadyErroring) writableStreamStartErroring(stream, reason);
		return d.promise;
	}

	function writableStreamClose(stream) {
		if (stream._state === "closed" || stream._state === "errored") {
			return rejectedPromise(new TypeError("Cannot close a stream that is already closed or errored"));
		}
		const d = newPromise();
		stream._closeRequest = d;
		const writer = stream._writer;
		if (writer !== undefined && stream._backpressure && stream._state === "writable") {
			writer._ready.resolve(undefined);
		}
		writableStreamDefaultControllerClose(stream._controller);
		return d.promise;
	}

	const writableStreamCloseQueuedOrInFlight = (stream) =>
		stream._closeRequest !== undefined || stream._inFlightCloseRequest !== undefined;

	function writableStreamDealWithRejection(stream, error) {
		if (stream._state === "writable") { writableStreamStartErroring(stream, error); return; }
		writableStreamFinishErroring(stream);
	}

	function writableStreamStartErroring(stream, reason) {
		const controller = stream._controller;
		stream._state = "erroring";
		stream._storedError = reason;
		const writer = stream._writer;
		if (writer !== undefined) writableStreamDefaultWriterEnsureReadyPromiseRejected(writer, reason);
		if (!writableStreamHasOperationMarkedInFlight(stream) && controller._started) {
			writableStreamFinishErroring(stream);
		}
	}

	function writableStreamFinishErroring(stream) {
		stream._state = "errored";
		stream._controller._errorSteps();
		const storedError = stream._storedError;
		for (const req of stream._writeRequests) req.reject(storedError);
		stream._writeRequests = [];
		if (stream._pendingAbortRequest === undefined) {
			writableStreamRejectCloseAndClosedPromiseIfNeeded(stream);
			return;
		}
		const abortRequest = stream._pendingAbortRequest;
		stream._pendingAbortRequest = undefined;
		if (abortRequest.wasAlreadyErroring) {
			abortRequest.reject(storedError);
			writableStreamRejectCloseAndClosedPromiseIfNeeded(stream);
			return;
		}
		markHandled(stream._controller._abortAlgorithm(abortRequest.reason).then(
			() => {
				abortRequest.resolve(undefined);
				writableStreamRejectCloseAndClosedPromiseIfNeeded(stream);
			},
			(reason) => {
				abortRequest.reject(reason);
				writableStreamRejectCloseAndClosedPromiseIfNeeded(stream);
			},
		));
	}

	function writableStreamFinishInFlightWrite(stream) {
		stream._inFlightWriteRequest.resolve(undefined);
		stream._inFlightWriteRequest = undefined;
	}

	function writableStreamFinishInFlightWriteWithError(stream, error) {
		stream._inFlightWriteRequest.reject(error);
		stream._inFlightWriteRequest = undefined;
		writableStreamDealWithRejection(stream, error);
	}

	function writableStreamFinishInFlightClose(stream) {
		stream._inFlightCloseRequest.resolve(undefined);
		stream._inFlightCloseRequest = undefined;
		if (stream._state === "erroring") {
			stream._storedError = undefined;
			if (stream._pendingAbortRequest !== undefined) {
				stream._pendingAbortRequest.resolve(undefined);
				stream._pendingAbortRequest = undefined;
			}
		}
		stream._state = "closed";
		const writer = stream._writer;
		if (writer !== undefined) writer._closed.resolve(undefined);
	}

	function writableStreamFinishInFlightCloseWithError(stream, error) {
		stream._inFlightCloseRequest.reject(error);
		stream._inFlightCloseRequest = undefined;
		if (stream._pendingAbortRequest !== undefined) {
			stream._pendingAbortRequest.reject(error);
			stream._pendingAbortRequest = undefined;
		}
		writableStreamDealWithRejection(stream, error);
	}

	const writableStreamHasOperationMarkedInFlight = (stream) =>
		stream._inFlightWriteRequest !== undefined || stream._inFlightCloseRequest !== undefined;

	function writableStreamMarkCloseRequestInFlight(stream) {
		stream._inFlightCloseRequest = stream._closeRequest;
		stream._closeRequest = undefined;
	}

	function writableStreamMarkFirstWriteRequestInFlight(stream) {
		stream._inFlightWriteRequest = stream._writeRequests.shift();
	}

	function writableStreamRejectCloseAndClosedPromiseIfNeeded(stream) {
		if (stream._closeRequest !== undefined) {
			stream._closeRequest.reject(stream._storedError);
			stream._closeRequest = undefined;
		}
		const writer = stream._writer;
		if (writer !== undefined) {
			writer._closed = rejectOrReplace(writer._closed, stream._storedError);
		}
	}

	function writableStreamUpdateBackpressure(stream, backpressure) {
		const writer = stream._writer;
		if (writer !== undefined && backpressure !== stream._backpressure) {
			if (backpressure) writer._ready = deferred();
			else writer._ready.resolve(undefined);
		}
		stream._backpressure = backpressure;
	}

	class WritableStreamDefaultWriter {
		constructor(stream) {
			if (!isBranded(stream, "WritableStream")) {
				throw new TypeError("WritableStreamDefaultWriter: argument must be a WritableStream");
			}
			if (isWritableStreamLocked(stream)) throw new TypeError("WritableStream is locked");
			this[BRAND] = "WritableStreamDefaultWriter";
			this._stream = stream;
			stream._writer = this;
			const state = stream._state;
			if (state === "writable") {
				this._ready = (!writableStreamCloseQueuedOrInFlight(stream) && stream._backpressure)
					? deferred() : settledDeferred(undefined, false);
				this._closed = deferred();
			} else if (state === "erroring") {
				this._ready = settledDeferred(stream._storedError, true);
				this._closed = deferred();
			} else if (state === "closed") {
				this._ready = settledDeferred(undefined, false);
				this._closed = settledDeferred(undefined, false);
			} else {
				this._ready = settledDeferred(stream._storedError, true);
				this._closed = settledDeferred(stream._storedError, true);
			}
		}

		get closed() {
			try { brandCheck(this, "WritableStreamDefaultWriter"); } catch (e) { return rejectedPromise(e); }
			return this._closed.promise;
		}

		get desiredSize() {
			brandCheck(this, "WritableStreamDefaultWriter");
			if (this._stream === undefined) throw new TypeError("The writer has released its lock");
			return writableStreamDefaultWriterGetDesiredSize(this);
		}

		get ready() {
			try { brandCheck(this, "WritableStreamDefaultWriter"); } catch (e) { return rejectedPromise(e); }
			return this._ready.promise;
		}

		abort(reason = undefined) {
			try { brandCheck(this, "WritableStreamDefaultWriter"); } catch (e) { return rejectedPromise(e); }
			if (this._stream === undefined) {
				return rejectedPromise(new TypeError("Cannot abort a stream using a released writer"));
			}
			return writableStreamAbort(this._stream, reason);
		}

		close() {
			try { brandCheck(this, "WritableStreamDefaultWriter"); } catch (e) { return rejectedPromise(e); }
			const stream = this._stream;
			if (stream === undefined) {
				return rejectedPromise(new TypeError("Cannot close a stream using a released writer"));
			}
			if (writableStreamCloseQueuedOrInFlight(stream)) {
				return rejectedPromise(new TypeError("Cannot close an already-closing stream"));
			}
			return writableStreamClose(stream);
		}

		releaseLock() {
			brandCheck(this, "WritableStreamDefaultWriter");
			if (this._stream === undefined) return;
			writableStreamDefaultWriterRelease(this);
		}

		write(chunk = undefined) {
			try { brandCheck(this, "WritableStreamDefaultWriter"); } catch (e) { return rejectedPromise(e); }
			if (this._stream === undefined) {
				return rejectedPromise(new TypeError("Cannot write to a stream using a released writer"));
			}
			return writableStreamDefaultWriterWrite(this, chunk);
		}
	}
	Object.defineProperty(WritableStreamDefaultWriter.prototype, Symbol.toStringTag, {
		value: "WritableStreamDefaultWriter", configurable: true,
	});

	const acquireWritableStreamDefaultWriter = (stream) => new WritableStreamDefaultWriter(stream);

	function writableStreamDefaultWriterEnsureClosedPromiseRejected(writer, error) {
		writer._closed = rejectOrReplace(writer._closed, error);
	}

	function writableStreamDefaultWriterEnsureReadyPromiseRejected(writer, error) {
		writer._ready = rejectOrReplace(writer._ready, error);
	}

	function writableStreamDefaultWriterGetDesiredSize(writer) {
		const state = writer._stream._state;
		if (state === "errored" || state === "erroring") return null;
		if (state === "closed") return 0;
		return writableStreamDefaultControllerGetDesiredSize(writer._stream._controller);
	}

	function writableStreamDefaultWriterRelease(writer) {
		const stream = writer._stream;
		const released = new TypeError("Writer was released and can no longer be used to monitor the stream's closedness");
		writableStreamDefaultWriterEnsureReadyPromiseRejected(writer, released);
		writableStreamDefaultWriterEnsureClosedPromiseRejected(writer, released);
		stream._writer = undefined;
		writer._stream = undefined;
	}

	// close(), but a stream that has already errored reports THAT rather than a
	// close failure — a pipe closing its destination must surface the real cause.
	function writableStreamDefaultWriterCloseWithErrorPropagation(writer) {
		const stream = writer._stream;
		const state = stream._state;
		if (writableStreamCloseQueuedOrInFlight(stream) || state === "closed") return resolvedPromise(undefined);
		if (state === "errored") return rejectedPromise(stream._storedError);
		return writableStreamClose(stream);
	}

	function writableStreamDefaultWriterWrite(writer, chunk) {
		const stream = writer._stream;
		const controller = stream._controller;
		let chunkSize;
		try {
			chunkSize = controller._strategySizeAlgorithm(chunk);
		} catch (e) {
			writableStreamDefaultControllerErrorIfNeeded(controller, e);
			return rejectedPromise(e);
		}
		if (stream !== writer._stream) {
			return rejectedPromise(new TypeError("Cannot write to a stream using a released writer"));
		}
		const state = stream._state;
		if (state === "errored") return rejectedPromise(stream._storedError);
		if (writableStreamCloseQueuedOrInFlight(stream) || state === "closed") {
			return rejectedPromise(new TypeError("The stream is closing or closed and cannot be written to"));
		}
		if (state === "erroring") return rejectedPromise(stream._storedError);
		const d = newPromise();
		stream._writeRequests.push(d);
		writableStreamDefaultControllerWrite(controller, chunk, chunkSize);
		return d.promise;
	}

	class WritableStreamDefaultController {
		constructor() {
			throw new TypeError("Illegal constructor");
		}

		get abortReason() {
			brandCheck(this, "WritableStreamDefaultController");
			return this._abortReason;
		}

		get signal() {
			brandCheck(this, "WritableStreamDefaultController");
			return this._abortController.signal;
		}

		error(e = undefined) {
			brandCheck(this, "WritableStreamDefaultController");
			if (this._stream._state !== "writable") return;
			writableStreamDefaultControllerError(this, e);
		}

		_abortSteps(reason) {
			const result = this._abortAlgorithm(reason);
			writableStreamDefaultControllerClearAlgorithms(this);
			return result;
		}

		_errorSteps() { resetQueue(this); }
	}
	Object.defineProperty(WritableStreamDefaultController.prototype, Symbol.toStringTag, {
		value: "WritableStreamDefaultController", configurable: true,
	});

	function setUpWritableStreamDefaultControllerFromAlgorithms(stream, controller, startAlgorithm, writeAlgorithm, closeAlgorithm, abortAlgorithm, highWaterMark, sizeAlgorithm) {
		controller[BRAND] = "WritableStreamDefaultController";
		controller._stream = stream;
		resetQueue(controller);
		controller._abortReason = undefined;
		controller._abortController = new AbortController();
		controller._started = false;
		controller._strategySizeAlgorithm = sizeAlgorithm;
		controller._strategyHWM = highWaterMark;
		controller._writeAlgorithm = writeAlgorithm;
		controller._closeAlgorithm = closeAlgorithm;
		controller._abortAlgorithm = abortAlgorithm;
		stream._controller = controller;
		const backpressure = writableStreamDefaultControllerGetBackpressure(controller);
		writableStreamUpdateBackpressure(stream, backpressure);
		const startResult = startAlgorithm();
		Promise.resolve(startResult).then(
			() => {
				controller._started = true;
				writableStreamDefaultControllerAdvanceQueueIfNeeded(controller);
			},
			(r) => {
				controller._started = true;
				writableStreamDealWithRejection(stream, r);
			},
		);
	}

	function setUpWritableStreamDefaultController(stream, sink, startFn, writeFn, closeFn, abortFn, highWaterMark, sizeAlgorithm) {
		const controller = Object.create(WritableStreamDefaultController.prototype);
		const startAlgorithm = startFn === undefined ? () => undefined : () => callback(startFn, sink, [controller]);
		const writeAlgorithm = writeFn === undefined ? () => resolvedPromise(undefined)
			: (chunk) => promiseCall(writeFn, sink, [chunk, controller]);
		const closeAlgorithm = closeFn === undefined ? () => resolvedPromise(undefined)
			: () => promiseCall(closeFn, sink, []);
		const abortAlgorithm = abortFn === undefined ? () => resolvedPromise(undefined)
			: (reason) => promiseCall(abortFn, sink, [reason]);
		setUpWritableStreamDefaultControllerFromAlgorithms(stream, controller, startAlgorithm, writeAlgorithm, closeAlgorithm, abortAlgorithm, highWaterMark, sizeAlgorithm);
	}

	function writableStreamDefaultControllerClearAlgorithms(controller) {
		controller._writeAlgorithm = undefined;
		controller._closeAlgorithm = undefined;
		controller._abortAlgorithm = undefined;
		controller._strategySizeAlgorithm = undefined;
	}

	// The queue holds a close marker as well as chunks, so a close takes its
	// turn behind the writes that were queued before it.
	const CLOSE_SENTINEL = Symbol("close-sentinel");

	function writableStreamDefaultControllerClose(controller) {
		enqueueValueWithSize(controller, CLOSE_SENTINEL, 0);
		writableStreamDefaultControllerAdvanceQueueIfNeeded(controller);
	}

	function writableStreamDefaultControllerGetChunkSize(controller, chunk) {
		try {
			return controller._strategySizeAlgorithm(chunk);
		} catch (e) {
			writableStreamDefaultControllerErrorIfNeeded(controller, e);
			return 1;
		}
	}

	const writableStreamDefaultControllerGetDesiredSize = (controller) =>
		controller._strategyHWM - controller._queueTotalSize;

	function writableStreamDefaultControllerWrite(controller, chunk, chunkSize) {
		try {
			enqueueValueWithSize(controller, chunk, chunkSize);
		} catch (e) {
			writableStreamDefaultControllerErrorIfNeeded(controller, e);
			return;
		}
		const stream = controller._stream;
		if (!writableStreamCloseQueuedOrInFlight(stream) && stream._state === "writable") {
			writableStreamUpdateBackpressure(stream, writableStreamDefaultControllerGetBackpressure(controller));
		}
		writableStreamDefaultControllerAdvanceQueueIfNeeded(controller);
	}

	function writableStreamDefaultControllerAdvanceQueueIfNeeded(controller) {
		const stream = controller._stream;
		if (!controller._started) return;
		if (stream._inFlightWriteRequest !== undefined) return;
		const state = stream._state;
		// A stream that has finished — closed or errored — has no queue left to
		// advance, and running its sink's close after the algorithms were cleared
		// is how a transform stream ended up calling an undefined flush.
		if (state === "closed" || state === "errored") return;
		if (state === "erroring") { writableStreamFinishErroring(stream); return; }
		if (controller._queue.length === 0) return;
		const value = peekQueueValue(controller);
		if (value === CLOSE_SENTINEL) writableStreamDefaultControllerProcessClose(controller);
		else writableStreamDefaultControllerProcessWrite(controller, value);
	}

	function writableStreamDefaultControllerErrorIfNeeded(controller, error) {
		if (controller._stream._state === "writable") writableStreamDefaultControllerError(controller, error);
	}

	function writableStreamDefaultControllerProcessClose(controller) {
		const stream = controller._stream;
		writableStreamMarkCloseRequestInFlight(stream);
		dequeueValue(controller);
		const sinkClosePromise = controller._closeAlgorithm();
		writableStreamDefaultControllerClearAlgorithms(controller);
		markHandled(sinkClosePromise.then(
			() => writableStreamFinishInFlightClose(stream),
			(reason) => writableStreamFinishInFlightCloseWithError(stream, reason),
		));
	}

	function writableStreamDefaultControllerProcessWrite(controller, chunk) {
		const stream = controller._stream;
		writableStreamMarkFirstWriteRequestInFlight(stream);
		markHandled(controller._writeAlgorithm(chunk).then(
			() => {
				writableStreamFinishInFlightWrite(stream);
				const state = stream._state;
				dequeueValue(controller);
				if (!writableStreamCloseQueuedOrInFlight(stream) && state === "writable") {
					writableStreamUpdateBackpressure(stream, writableStreamDefaultControllerGetBackpressure(controller));
				}
				writableStreamDefaultControllerAdvanceQueueIfNeeded(controller);
			},
			(reason) => {
				if (stream._state === "writable") writableStreamDefaultControllerClearAlgorithms(controller);
				writableStreamFinishInFlightWriteWithError(stream, reason);
			},
		));
	}

	const writableStreamDefaultControllerGetBackpressure = (controller) =>
		writableStreamDefaultControllerGetDesiredSize(controller) <= 0;

	function writableStreamDefaultControllerError(controller, error) {
		writableStreamDefaultControllerClearAlgorithms(controller);
		writableStreamStartErroring(controller._stream, error);
	}

	// -------------------------------------------------------- TransformStream

	class TransformStream {
		constructor(transformer = undefined, writableStrategy = undefined, readableStrategy = undefined) {
			// Both strategies are dictionary arguments and are converted, in
			// argument order, before the transformer is looked at.
			const ws = convertStrategy(writableStrategy, "TransformStream");
			const rs = convertStrategy(readableStrategy, "TransformStream");
			const t = transformer === undefined || transformer === null ? {} : transformer;
			if (typeof t !== "object" && typeof t !== "function") {
				throw new TypeError("TransformStream: the transformer must be an object");
			}
			const cancelFn = method(t, "cancel", "transformer");
			const flushFn = method(t, "flush", "transformer");
			if (t.readableType !== undefined) throw new RangeError("TransformStream: readableType is not supported");
			const startFn = method(t, "start", "transformer");
			const transformFn = method(t, "transform", "transformer");
			if (t.writableType !== undefined) throw new RangeError("TransformStream: writableType is not supported");

			const readableHWM = rs.highWaterMark === undefined ? 0 : convertHWM(rs.highWaterMark);
			const readableSize = makeSizeAlgorithm(rs.size);
			const writableHWM = ws.highWaterMark === undefined ? 1 : convertHWM(ws.highWaterMark);
			const writableSize = makeSizeAlgorithm(ws.size);

			const startDeferred = newPromise();
			initializeTransformStream(this, startDeferred.promise, writableHWM, writableSize, readableHWM, readableSize);
			const controller = Object.create(TransformStreamDefaultController.prototype);
			setUpTransformStreamDefaultController(this, controller, t, transformFn, flushFn, cancelFn);
			if (startFn !== undefined) startDeferred.resolve(callback(startFn, t, [controller]));
			else startDeferred.resolve(undefined);
		}

		get readable() {
			brandCheck(this, "TransformStream");
			return this._readable;
		}

		get writable() {
			brandCheck(this, "TransformStream");
			return this._writable;
		}
	}
	Object.defineProperty(TransformStream.prototype, Symbol.toStringTag, {
		value: "TransformStream", configurable: true,
	});

	function initializeTransformStream(stream, startPromise, writableHWM, writableSize, readableHWM, readableSize) {
		stream[BRAND] = "TransformStream";
		stream._backpressure = undefined;
		stream._backpressureChangePromise = undefined;
		stream._backpressureChangeResolve = undefined;
		transformStreamSetBackpressure(stream, true);
		stream._writable = createWritableStream(
			() => startPromise,
			(chunk) => transformStreamDefaultSinkWriteAlgorithm(stream, chunk),
			() => transformStreamDefaultSinkCloseAlgorithm(stream),
			(reason) => transformStreamDefaultSinkAbortAlgorithm(stream, reason),
			writableHWM, writableSize,
		);
		stream._readable = createReadableStream(
			() => startPromise,
			() => transformStreamDefaultSourcePullAlgorithm(stream),
			(reason) => transformStreamDefaultSourceCancelAlgorithm(stream, reason),
			readableHWM, readableSize,
		);
		stream._controller = undefined;
	}

	function transformStreamError(stream, e) {
		readableStreamDefaultControllerError(stream._readable._controller, e);
		transformStreamErrorWritableAndUnblockWrite(stream, e);
	}

	function transformStreamErrorWritableAndUnblockWrite(stream, e) {
		transformStreamDefaultControllerClearAlgorithms(stream._controller);
		writableStreamDefaultControllerErrorIfNeeded(stream._writable._controller, e);
		transformStreamUnblockWrite(stream);
	}

	function transformStreamUnblockWrite(stream) {
		if (stream._backpressure) transformStreamSetBackpressure(stream, false);
	}

	function transformStreamSetBackpressure(stream, backpressure) {
		if (stream._backpressureChangeResolve !== undefined) stream._backpressureChangeResolve(undefined);
		const d = newPromise();
		stream._backpressureChangePromise = d.promise;
		stream._backpressureChangeResolve = d.resolve;
		stream._backpressure = backpressure;
	}

	class TransformStreamDefaultController {
		constructor() {
			throw new TypeError("Illegal constructor");
		}

		get desiredSize() {
			brandCheck(this, "TransformStreamDefaultController");
			return readableStreamDefaultControllerGetDesiredSize(this._stream._readable._controller);
		}

		enqueue(chunk = undefined) {
			brandCheck(this, "TransformStreamDefaultController");
			transformStreamDefaultControllerEnqueue(this, chunk);
		}

		error(reason = undefined) {
			brandCheck(this, "TransformStreamDefaultController");
			transformStreamError(this._stream, reason);
		}

		terminate() {
			brandCheck(this, "TransformStreamDefaultController");
			const stream = this._stream;
			const readableController = stream._readable._controller;
			readableStreamDefaultControllerClose(readableController);
			const error = new TypeError("The stream has been terminated");
			transformStreamErrorWritableAndUnblockWrite(stream, error);
		}
	}
	Object.defineProperty(TransformStreamDefaultController.prototype, Symbol.toStringTag, {
		value: "TransformStreamDefaultController", configurable: true,
	});

	// t is the transformer object: its methods are invoked WITH IT as the this
	// value, which is what lets a transformer be a class instance whose transform
	// reads its own fields.
	function setUpTransformStreamDefaultController(stream, controller, t, transformFn, flushFn, cancelFn) {
		controller[BRAND] = "TransformStreamDefaultController";
		controller._stream = stream;
		controller._finishPromise = undefined;
		stream._controller = controller;
		controller._transformAlgorithm = transformFn === undefined
			? (chunk) => {
				try {
					transformStreamDefaultControllerEnqueue(controller, chunk);
					return resolvedPromise(undefined);
				} catch (e) {
					return rejectedPromise(e);
				}
			}
			: (chunk) => promiseCall(transformFn, t, [chunk, controller]);
		controller._flushAlgorithm = flushFn === undefined ? () => resolvedPromise(undefined)
			: () => promiseCall(flushFn, t, [controller]);
		controller._cancelAlgorithm = cancelFn === undefined ? () => resolvedPromise(undefined)
			: (reason) => promiseCall(cancelFn, t, [reason]);
	}

	function transformStreamDefaultControllerClearAlgorithms(controller) {
		if (controller === undefined) return;
		controller._transformAlgorithm = undefined;
		controller._flushAlgorithm = undefined;
		controller._cancelAlgorithm = undefined;
	}

	function transformStreamDefaultControllerEnqueue(controller, chunk) {
		const stream = controller._stream;
		const readableController = stream._readable._controller;
		if (!readableStreamDefaultControllerCanCloseOrEnqueue(readableController)) {
			throw new TypeError("The readable side is not in a state that permits enqueue");
		}
		try {
			readableStreamDefaultControllerEnqueue(readableController, chunk);
		} catch (e) {
			transformStreamErrorWritableAndUnblockWrite(stream, e);
			throw stream._readable._storedError;
		}
		const backpressure = readableStreamDefaultControllerGetDesiredSize(readableController) <= 0;
		if (backpressure !== stream._backpressure) transformStreamSetBackpressure(stream, true);
	}

	function transformStreamDefaultControllerPerformTransform(controller, chunk) {
		return controller._transformAlgorithm(chunk).catch((e) => {
			transformStreamError(controller._stream, e);
			throw e;
		});
	}

	function transformStreamDefaultSinkWriteAlgorithm(stream, chunk) {
		const controller = stream._controller;
		if (stream._backpressure) {
			// The write waits for the readable side to make room. It must ALSO stop
			// waiting if the stream errors meanwhile, which is why the state is
			// re-checked after the wait rather than assumed.
			return stream._backpressureChangePromise.then(() => {
				const writable = stream._writable;
				if (writable._state === "erroring") throw writable._storedError;
				return transformStreamDefaultControllerPerformTransform(controller, chunk);
			});
		}
		return transformStreamDefaultControllerPerformTransform(controller, chunk);
	}

	// close, the writable's abort and the readable's cancel all end the transform,
	// and any two of them can be asked for at once — a pipe that aborts its
	// destination while the consumer cancels its source, say. They share ONE
	// outcome: whichever arrives first runs the transformer's algorithm and the
	// rest wait on the same promise. Without that, the second one found the
	// algorithms already cleared and called undefined.
	function transformStreamDefaultSinkAbortAlgorithm(stream, reason) {
		const controller = stream._controller;
		if (controller._finishPromise !== undefined) return controller._finishPromise;
		const readable = stream._readable;
		const d = newPromise();
		controller._finishPromise = d.promise;
		markHandled(d.promise);
		const cancelPromise = controller._cancelAlgorithm(reason);
		transformStreamDefaultControllerClearAlgorithms(controller);
		cancelPromise.then(
			() => {
				if (readable._state === "errored") d.reject(readable._storedError);
				else {
					readableStreamDefaultControllerError(readable._controller, reason);
					d.resolve(undefined);
				}
			},
			(r) => {
				readableStreamDefaultControllerError(readable._controller, r);
				d.reject(r);
			},
		);
		return controller._finishPromise;
	}

	function transformStreamDefaultSinkCloseAlgorithm(stream) {
		const controller = stream._controller;
		if (controller._finishPromise !== undefined) return controller._finishPromise;
		const readable = stream._readable;
		const d = newPromise();
		controller._finishPromise = d.promise;
		markHandled(d.promise);
		const flushPromise = controller._flushAlgorithm();
		transformStreamDefaultControllerClearAlgorithms(controller);
		flushPromise.then(
			() => {
				if (readable._state === "errored") d.reject(readable._storedError);
				else {
					readableStreamDefaultControllerClose(readable._controller);
					d.resolve(undefined);
				}
			},
			(r) => {
				readableStreamDefaultControllerError(readable._controller, r);
				d.reject(r);
			},
		);
		return controller._finishPromise;
	}

	function transformStreamDefaultSourcePullAlgorithm(stream) {
		transformStreamSetBackpressure(stream, false);
		return stream._backpressureChangePromise;
	}

	function transformStreamDefaultSourceCancelAlgorithm(stream, reason) {
		const controller = stream._controller;
		if (controller._finishPromise !== undefined) return controller._finishPromise;
		const writable = stream._writable;
		const d = newPromise();
		controller._finishPromise = d.promise;
		markHandled(d.promise);
		const cancelPromise = controller._cancelAlgorithm(reason);
		transformStreamDefaultControllerClearAlgorithms(controller);
		cancelPromise.then(
			() => {
				if (writable._state === "errored") d.reject(writable._storedError);
				else {
					writableStreamDefaultControllerErrorIfNeeded(writable._controller, reason);
					transformStreamUnblockWrite(stream);
					d.resolve(undefined);
				}
			},
			(r) => {
				writableStreamDefaultControllerErrorIfNeeded(writable._controller, r);
				transformStreamUnblockWrite(stream);
				d.reject(r);
			},
		);
		return controller._finishPromise;
	}

	// ------------------------------------------------------ queuing strategies

	// The size function of each strategy is a single shared function per realm,
	// not a fresh one per instance: the standard says so, and a test that takes
	// `new CountQueuingStrategy({highWaterMark: 1}).size` twice compares them.
	// Written as method shorthand so they are not constructors and carry no
	// prototype property, which is what an IDL operation is.
	const { countSizeFunction, byteLengthSizeFunction } = {
		countSizeFunction: { size() { return 1; } }.size,
		byteLengthSizeFunction: { size(chunk) { return chunk.byteLength; } }.size,
	};

	class CountQueuingStrategy {
		constructor(init) {
			if (init === null || typeof init !== "object" || init.highWaterMark === undefined) {
				throw new TypeError("CountQueuingStrategy requires {highWaterMark}");
			}
			this[BRAND] = "CountQueuingStrategy";
			this._highWaterMark = Number(init.highWaterMark);
		}
		get highWaterMark() {
			brandCheck(this, "CountQueuingStrategy");
			return this._highWaterMark;
		}
		get size() {
			brandCheck(this, "CountQueuingStrategy");
			return countSizeFunction;
		}
	}
	Object.defineProperty(CountQueuingStrategy.prototype, Symbol.toStringTag, {
		value: "CountQueuingStrategy", configurable: true,
	});

	class ByteLengthQueuingStrategy {
		constructor(init) {
			if (init === null || typeof init !== "object" || init.highWaterMark === undefined) {
				throw new TypeError("ByteLengthQueuingStrategy requires {highWaterMark}");
			}
			this[BRAND] = "ByteLengthQueuingStrategy";
			this._highWaterMark = Number(init.highWaterMark);
		}
		get highWaterMark() {
			brandCheck(this, "ByteLengthQueuingStrategy");
			return this._highWaterMark;
		}
		get size() {
			brandCheck(this, "ByteLengthQueuingStrategy");
			return byteLengthSizeFunction;
		}
	}
	Object.defineProperty(ByteLengthQueuingStrategy.prototype, Symbol.toStringTag, {
		value: "ByteLengthQueuingStrategy", configurable: true,
	});

	// ------------------------------------------------------------------ exports

	globalThis.ReadableStream = ReadableStream;
	globalThis.ReadableStreamDefaultReader = ReadableStreamDefaultReader;
	globalThis.ReadableStreamBYOBReader = ReadableStreamBYOBReader;
	globalThis.ReadableStreamDefaultController = ReadableStreamDefaultController;
	globalThis.ReadableByteStreamController = ReadableByteStreamController;
	globalThis.ReadableStreamBYOBRequest = ReadableStreamBYOBRequest;
	globalThis.WritableStream = WritableStream;
	globalThis.WritableStreamDefaultWriter = WritableStreamDefaultWriter;
	globalThis.WritableStreamDefaultController = WritableStreamDefaultController;
	globalThis.TransformStream = TransformStream;
	globalThis.TransformStreamDefaultController = TransformStreamDefaultController;
	globalThis.CountQueuingStrategy = CountQueuingStrategy;
	globalThis.ByteLengthQueuingStrategy = ByteLengthQueuingStrategy;
})();
