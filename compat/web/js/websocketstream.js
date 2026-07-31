// compat/web: WebSocketStream (https://whatpr.org/websockets/48.html).
//
// The same protocol as WebSocket, with the message flow expressed as streams
// instead of events. That is the whole difference, and it is why this is built
// ON the WebSocket here rather than beside it: a second path to the host would
// be a second set of answers about handshakes, close codes and backpressure.
//
// What the stream shape buys is backpressure that the event form cannot have.
// A `message` handler is called whether or not the reader is keeping up; a
// readable stream's queue is only filled when someone pulls, and the socket's
// incoming messages are held until then.
(() => {
	"use strict";

	// A WebSocketError carries the close information along with being an
	// exception, so a failed stream reports the SAME thing the close handshake
	// would have said. It is a DOMException subtype, which is what lets
	// `instanceof DOMException` and the name check keep working.
	class WebSocketError extends DOMException {
		constructor(message = "", init = undefined) {
			super(String(message), "WebSocketError");
			const opts = init === null || init === undefined ? {} : init;
			let closeCode = null;
			let reason = "";
			if (opts.reason !== undefined && opts.reason !== null) reason = String(opts.reason);
			if (opts.closeCode !== undefined && opts.closeCode !== null) {
				const n = Number(opts.closeCode);
				if (!(n === 1000 || (n >= 3000 && n <= 4999))) {
					throw new DOMException(`WebSocketError: ${opts.closeCode} is not a permitted close code`, "InvalidAccessError");
				}
				closeCode = n;
			} else if (reason !== "") {
				// A reason with no code means the default clean code: a reason cannot
				// be sent without one.
				closeCode = 1000;
			}
			if (utf8Length(reason) > 123) {
				throw new DOMException("WebSocketError: the reason is longer than 123 bytes", "SyntaxError");
			}
			Object.defineProperties(this, {
				_closeCode: { value: closeCode, configurable: true },
				_reason: { value: reason, configurable: true },
			});
		}
		get closeCode() { return this._closeCode; }
		get reason() { return this._reason; }
	}
	Object.defineProperty(WebSocketError.prototype, Symbol.toStringTag, {
		value: "WebSocketError", configurable: true,
	});

	// The close information a FAILED connection reports is not always something
	// script could have asked for — 1006 (abnormal closure) is the obvious case,
	// and the constructor refuses it because nothing may SEND it. This is how the
	// implementation states what actually happened.
	function closeError(message, closeCode, reason) {
		const e = new WebSocketError(message);
		Object.defineProperty(e, "_closeCode", { value: closeCode, configurable: true });
		Object.defineProperty(e, "_reason", { value: reason, configurable: true });
		return e;
	}

	// The UTF-8 length of a close reason, which is what the 123-byte limit is
	// measured in — not the number of characters.
	function utf8Length(s) {
		return new TextEncoder().encode(s).length;
	}

	const deferred = () => {
		let resolve, reject;
		const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
		return { promise, resolve, reject };
	};

	class WebSocketStream {
		constructor(url, options = undefined) {
			if (arguments.length < 1) throw new TypeError("WebSocketStream: a url is required");
			const opts = options === undefined || options === null ? {} : options;
			if (typeof opts !== "object" && typeof opts !== "function") {
				throw new TypeError("WebSocketStream: options must be an object");
			}
			const signal = opts.signal;
			if (signal !== undefined && signal !== null
				&& !(typeof AbortSignal === "function" && signal instanceof AbortSignal)) {
				throw new TypeError("WebSocketStream: options.signal is not an AbortSignal");
			}
			// The URL and protocol checks are the WebSocket constructor's own, reached
			// through the registry: a WebSocketStream that accepted a URL the event
			// form rejects would be two answers to one question. They are made
			// SEPARATELY from connecting, because a signal that is already aborted
			// means no connection may be attempted at all — and a socket opened and
			// then closed has already attempted one.
			const check = globalThis[Symbol.for("go-spidermonkey.checkWebSocketArguments")];
			const { parsed } = check(url, opts.protocols === undefined ? [] : opts.protocols);

			const opened = deferred();
			const closed = deferred();
			// Nothing here requires the caller to look at either promise, and a
			// rejection nobody asked for must not be reported as unhandled.
			opened.promise.catch(() => {});
			closed.promise.catch(() => {});

			if (signal && signal.aborted) {
				const reason = signal.reason ?? new DOMException("The operation was aborted", "AbortError");
				opened.reject(reason);
				closed.reject(reason);
				Object.defineProperties(this, {
					_socket: { value: { url: parsed.href, close() {} } },
					_opened: { value: opened },
					_closed: { value: closed },
					_setClosing: { value: () => {} },
				});
				return;
			}
			const socket = new WebSocket(url, opts.protocols === undefined ? [] : opts.protocols);
			socket.binaryType = "arraybuffer";

			let readableController = null;
			let writableController = null;
			let closing = false;
			// Messages that arrived before the reader asked for them. A socket cannot
			// be told to stop delivering, so what backpressure means here is that the
			// queue is what grows, and the reader drains it.
			const pending = [];
			// Ending either stream ends the connection, and the REASON given is how
			// the close code travels: a WebSocketError says what to send, anything
			// else says only that it is over.
			const endWith = (reason) => {
				closing = true;
				const info = reason instanceof WebSocketError ? reason : null;
				try {
					socket.close(
						info && info.closeCode !== null ? info.closeCode : undefined,
						info && info.reason !== "" ? info.reason : undefined);
				} catch { /* already closing, or a code the socket refuses */ }
				// Neither cancel nor close is finished until the handshake is: the
				// promise they return is the connection's own ending.
				return closed.promise.then(() => undefined, () => undefined);
			};
			const readable = new ReadableStream({
				start(c) { readableController = c; },
				pull() { /* fed by the socket's message events */ },
				cancel: (reason) => endWith(reason),
			});
			const writable = new WritableStream({
				start(c) { writableController = c; },
				write: (chunk) => {
					if (socket.readyState !== WebSocket.OPEN) {
						throw new WebSocketError("the connection is not open");
					}
					socket.send(chunk);
				},
				close: () => endWith(undefined),
				abort: (reason) => endWith(reason),
			});

			socket.addEventListener("open", () => {
				opened.resolve({
					readable, writable,
					extensions: socket.extensions,
					protocol: socket.protocol,
				});
			});
			socket.addEventListener("message", (ev) => {
				// A binary message arrives as a Uint8Array, not as the raw buffer: a
				// stream carries bytes, and a view is what a reader can use directly.
				const chunk = ev.data instanceof ArrayBuffer ? new Uint8Array(ev.data) : ev.data;
				if (readableController === null) { pending.push(chunk); return; }
				for (const held of pending.splice(0)) readableController.enqueue(held);
				readableController.enqueue(chunk);
			});
			// The end of the connection ends BOTH streams. A reader that is waiting
			// learns from the readable; a writer that is waiting learns from the
			// writable, and one that only ever held a writer would otherwise wait
			// forever on a socket that is gone.
			const failStreams = (err) => {
				try { readableController.error(err); } catch { /* already errored */ }
				try { writableController.error(err); } catch { /* already errored */ }
			};
			socket.addEventListener("close", (ev) => {
				// A close that was not clean is a failure of the streams as well as
				// the end of the socket, whether or not the caller asked for it.
				if (ev.wasClean) {
					try { readableController.close(); } catch { /* already closed */ }
					// A clean close still ENDS the writable: nothing more can be sent,
					// and the standard errors it rather than closing it, so a pending
					// write is refused rather than silently dropped.
					try {
						writableController.error(new DOMException(
							"the connection is closed", "InvalidStateError"));
					} catch { /* already errored */ }
					opened.reject(new WebSocketError("the connection closed before it opened"));
					closed.resolve({ closeCode: ev.code, reason: ev.reason });
					return;
				}
				const err = closeError("the connection closed uncleanly", ev.code || 1006, ev.reason);
				failStreams(err);
				opened.reject(err);
				closed.reject(err);
			});
			socket.addEventListener("error", () => {
				const err = closeError("the connection failed", 1006, "");
				failStreams(err);
				opened.reject(err);
				closed.reject(err);
			});

			if (signal) {
				// The signal governs the HANDSHAKE only. Once the connection is open
				// the streams are the way to end it, and an abort that arrived after
				// must do nothing — which is what the standard says and what a caller
				// holding a signal for the whole page needs.
				const onAbort = () => {
					if (socket.readyState !== WebSocket.CONNECTING) return;
					const reason = signal.reason ?? new DOMException("The operation was aborted", "AbortError");
					opened.reject(reason);
					closed.reject(reason);
					failStreams(reason);
					try { socket.close(); } catch { /* already closing */ }
				};
				signal.addEventListener("abort", onAbort, { once: true });
			}

			Object.defineProperties(this, {
				_socket: { value: socket },
				_opened: { value: opened },
				_closed: { value: closed },
				_setClosing: { value: () => { closing = true; } },
			});
		}

		get url() { return this._socket.url; }
		get opened() { return this._opened.promise; }
		get closed() { return this._closed.promise; }

		close(closeInfo = undefined) {
			// closeInfo is a dictionary, so a primitive is a TypeError rather than
			// something to coerce: close(true) is a mistake, not a close code.
			if (closeInfo !== undefined && closeInfo !== null
				&& typeof closeInfo !== "object" && typeof closeInfo !== "function") {
				throw new TypeError("close: closeInfo must be an object");
			}
			const info = closeInfo === undefined || closeInfo === null ? {} : closeInfo;
			const reason = info.reason === undefined ? "" : String(info.reason);
			// A reason with no code means the default clean code: the protocol has no
			// way to send a reason without one.
			const code = info.closeCode === undefined
				? (reason === "" ? undefined : 1000)
				: info.closeCode;
			this._setClosing();
			// The validation is the WebSocket's, again by using it.
			this._socket.close(code, reason === "" ? undefined : reason);
		}
	}
	Object.defineProperty(WebSocketStream.prototype, Symbol.toStringTag, {
		value: "WebSocketStream", configurable: true,
	});

	globalThis.WebSocketError = WebSocketError;
	globalThis.WebSocketStream = WebSocketStream;
})();
