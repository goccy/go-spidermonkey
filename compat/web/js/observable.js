// compat/web: Observable (https://wicg.github.io/observable/).
//
// A push-based counterpart to the async iterator: a value source that hands
// values to a subscriber rather than being pulled from, with a subscription
// that can be torn down. It is JavaScript because it is entirely about holding
// live JS values and callbacks and about WHEN each one runs — there is no
// algorithm underneath for Go to own.
//
// Two rules run through the whole file and are worth stating once.
//
// Everything is SYNCHRONOUS unless the source is asynchronous. subscribe() runs
// the initializer immediately, next() calls the observer immediately, and an
// operator's callback runs in the same turn as the value it transforms. That is
// the difference from promises, and the tests measure it at every step.
//
// A subscription ends exactly once, and its teardowns run in reverse order of
// registration when it does — whether it ended by completing, by erroring, or
// by being aborted. Anything added after it has ended runs at once, because the
// thing it was going to clean up is already gone.
(() => {
	"use strict";

	const INTERNAL = Symbol("Observable.internal");
	// A subscription closes as part of ABORTING its signal, before the signal's
	// event fires — that is what makes a teardown run before script's own abort
	// handler, which is the order the standard specifies.
	const addAbortAlgorithm = globalThis[Symbol.for("go-spidermonkey.addAbortAlgorithm")];

	// An error nobody is listening for is REPORTED rather than thrown: throwing
	// it would unwind whoever happened to call next(), who has nothing to do with
	// it. This is the spec's "report the exception".
	const report = (e) => {
		try {
			globalThis.reportError(e);
		} catch {
			// Nothing left to report to; losing it is better than replacing the
			// original failure with a failure to mention it.
		}
	};

	class Subscriber {
		constructor(internal, observer, signal) {
			if (internal !== INTERNAL) throw new TypeError("Illegal constructor");
			Object.defineProperties(this, {
				_observer: { value: observer, writable: true },
				_teardowns: { value: [] },
				_closed: { value: false, writable: true },
				_controller: { value: new AbortController() },
				_outerSignal: { value: signal ?? null },
			});
		}

		get active() { return !this._closed; }
		get signal() { return this._controller.signal; }

		next(value) {
			if (arguments.length < 1) throw new TypeError("next requires 1 argument");
			if (this._closed) return;
			const on = this._observer.next;
			if (typeof on !== "function") return;
			try {
				on.call(undefined, value);
			} catch (e) {
				report(e);
			}
		}

		error(reason) {
			if (arguments.length < 1) throw new TypeError("error requires 1 argument");
			if (this._closed) {
				// The subscription is over, so nobody is listening — but the failure
				// still happened and is still worth saying.
				report(reason);
				return;
			}
			const on = this._observer.error;
			this._close(reason);
			if (typeof on === "function") {
				try {
					on.call(undefined, reason);
				} catch (e) {
					report(e);
				}
			} else {
				report(reason);
			}
		}

		complete() {
			if (this._closed) return;
			const on = this._observer.complete;
			this._close(undefined);
			if (typeof on === "function") {
				try {
					on.call(undefined);
				} catch (e) {
					report(e);
				}
			}
		}

		addTeardown(teardown) {
			if (arguments.length < 1) throw new TypeError("addTeardown requires 1 argument");
			if (typeof teardown !== "function") throw new TypeError("addTeardown: a function is required");
			// Already over: the thing this would have cleaned up is gone, so it runs
			// now rather than never.
			if (this._closed) {
				try { teardown(); } catch (e) { report(e); }
				return;
			}
			this._teardowns.push(teardown);
		}

		// _close ends the subscription once: the signal is aborted first so a
		// teardown watching it sees the same state a later observer would.
		_close(reason) {
			if (this._closed) return;
			this._closed = true;
			this._controller.abort(reason);
			const list = this._teardowns.splice(0).reverse();
			for (const fn of list) {
				try { fn(); } catch (e) { report(e); }
			}
		}
	}
	Object.defineProperty(Subscriber.prototype, Symbol.toStringTag, {
		value: "Subscriber", configurable: true,
	});

	// normalizeObserver accepts the two forms subscribe() takes: a function, which
	// IS the next handler, or a dictionary of handlers.
	function normalizeObserver(observer) {
		if (typeof observer === "function") return { next: observer };
		if (observer === null || observer === undefined) return {};
		if (typeof observer !== "object") return {};
		return observer;
	}

	class Observable {
		constructor(subscribeCallback) {
			if (arguments.length < 1) throw new TypeError("Observable: a subscribe callback is required");
			if (typeof subscribeCallback !== "function") {
				throw new TypeError("Observable: the subscribe callback must be a function");
			}
			Object.defineProperty(this, "_subscribe", { value: subscribeCallback });
		}

		subscribe(observer = undefined, options = undefined) {
			subscribeTo(this, normalizeObserver(observer), options);
		}

		// ------------------------------------------------------------ operators
		// Each returns a NEW Observable that subscribes to this one when it is
		// itself subscribed to — nothing is read from the source until then.

		takeUntil(notifier) {
			const source = this;
			return new Observable((subscriber) => {
				const other = Observable.from(notifier);
				// The notifier is a SIGNAL, not a source. A value or an error from it
				// means "stop here" and completes the outer observable — forwarding
				// its error made a notifier that failed look like a failure of the
				// source. Its own COMPLETION means the opposite: it will never fire,
				// so the source is left to run to its own end.
				subscribeTo(other, {
					next: () => subscriber.complete(),
					error: () => subscriber.complete(),
					complete: () => {},
				}, { signal: subscriber.signal });
				if (!subscriber.active) return;
				forwardTo(source, subscriber);
			});
		}

		map(mapper) {
			requireFunction(mapper, "map");
			const source = this;
			return new Observable((subscriber) => {
				let index = 0;
				subscribeTo(source, {
					next: (value) => {
						let mapped;
						try {
							mapped = mapper(value, index++);
						} catch (e) {
							subscriber.error(e);
							return;
						}
						subscriber.next(mapped);
					},
					error: (e) => subscriber.error(e),
					complete: () => subscriber.complete(),
				}, { signal: subscriber.signal });
			});
		}

		filter(predicate) {
			requireFunction(predicate, "filter");
			const source = this;
			return new Observable((subscriber) => {
				let index = 0;
				subscribeTo(source, {
					next: (value) => {
						let keep;
						try {
							keep = predicate(value, index++);
						} catch (e) {
							subscriber.error(e);
							return;
						}
						if (keep) subscriber.next(value);
					},
					error: (e) => subscriber.error(e),
					complete: () => subscriber.complete(),
				}, { signal: subscriber.signal });
			});
		}

		take(amount) {
			const wanted = toUnsignedLongLong(amount, "take");
			const source = this;
			return new Observable((subscriber) => {
				if (wanted === 0) { subscriber.complete(); return; }
				let remaining = wanted;
				subscribeTo(source, {
					next: (value) => {
						remaining--;
						subscriber.next(value);
						if (remaining === 0) subscriber.complete();
					},
					error: (e) => subscriber.error(e),
					complete: () => subscriber.complete(),
				}, { signal: subscriber.signal });
			});
		}

		drop(amount) {
			const skip = toUnsignedLongLong(amount, "drop");
			const source = this;
			return new Observable((subscriber) => {
				let remaining = skip;
				subscribeTo(source, {
					next: (value) => {
						if (remaining > 0) { remaining--; return; }
						subscriber.next(value);
					},
					error: (e) => subscriber.error(e),
					complete: () => subscriber.complete(),
				}, { signal: subscriber.signal });
			});
		}

		// flatMap subscribes to ONE inner observable at a time and queues the rest,
		// which is what distinguishes it from switchMap: nothing is dropped.
		flatMap(mapper) {
			requireFunction(mapper, "flatMap");
			const source = this;
			return new Observable((subscriber) => {
				let index = 0;
				let outerDone = false;
				let innerActive = false;
				const queue = [];
				const startInner = (value) => {
					innerActive = true;
					let inner;
					try {
						inner = Observable.from(mapper(value, index++));
					} catch (e) {
						subscriber.error(e);
						return;
					}
					subscribeTo(inner, {
						next: (v) => subscriber.next(v),
						error: (e) => subscriber.error(e),
						complete: () => {
							innerActive = false;
							if (queue.length > 0) startInner(queue.shift());
							else if (outerDone) subscriber.complete();
						},
					}, { signal: subscriber.signal });
				};
				subscribeTo(source, {
					next: (value) => {
						if (innerActive) queue.push(value);
						else startInner(value);
					},
					error: (e) => subscriber.error(e),
					complete: () => {
						outerDone = true;
						if (!innerActive && queue.length === 0) subscriber.complete();
					},
				}, { signal: subscriber.signal });
			});
		}

		// switchMap keeps only the LATEST inner observable: a new outer value
		// unsubscribes from the one in flight.
		switchMap(mapper) {
			requireFunction(mapper, "switchMap");
			const source = this;
			return new Observable((subscriber) => {
				let index = 0;
				let outerDone = false;
				let innerController = null;
				const startInner = (value) => {
					if (innerController) innerController.abort();
					innerController = new AbortController();
					const signal = AbortSignal.any([innerController.signal, subscriber.signal]);
					let inner;
					try {
						inner = Observable.from(mapper(value, index++));
					} catch (e) {
						subscriber.error(e);
						return;
					}
					subscribeTo(inner, {
						next: (v) => subscriber.next(v),
						error: (e) => subscriber.error(e),
						complete: () => {
							innerController = null;
							if (outerDone) subscriber.complete();
						},
					}, { signal });
				};
				subscribeTo(source, {
					next: (value) => startInner(value),
					error: (e) => subscriber.error(e),
					complete: () => {
						outerDone = true;
						if (!innerController) subscriber.complete();
					},
				}, { signal: subscriber.signal });
			});
		}

		// inspect is the debugging tap: it observes without changing anything, and
		// a callback that throws errors the subscription.
		inspect(inspector = undefined) {
			const hooks = typeof inspector === "function" ? { next: inspector }
				: (inspector === null || inspector === undefined ? {} : inspector);
			const source = this;
			return new Observable((subscriber) => {
				const call = (fn, ...args) => {
					if (typeof fn !== "function") return true;
					try {
						fn(...args);
						return true;
					} catch (e) {
						subscriber.error(e);
						return false;
					}
				};
				if (!call(hooks.subscribe)) return;
				if (typeof hooks.abort === "function") {
					subscriber.signal.addEventListener("abort", () => {
						try { hooks.abort(subscriber.signal.reason); } catch (e) { report(e); }
					}, { once: true });
				}
				subscribeTo(source, {
					next: (value) => { if (call(hooks.next, value)) subscriber.next(value); },
					error: (e) => {
						if (typeof hooks.error === "function") {
							try { hooks.error(e); } catch (thrown) { subscriber.error(thrown); return; }
						}
						subscriber.error(e);
					},
					complete: () => {
						if (call(hooks.complete)) subscriber.complete();
					},
				}, { signal: subscriber.signal });
			});
		}

		catch(handler) {
			requireFunction(handler, "catch");
			const source = this;
			return new Observable((subscriber) => {
				subscribeTo(source, {
					next: (value) => subscriber.next(value),
					error: (e) => {
						let replacement;
						try {
							replacement = Observable.from(handler(e));
						} catch (thrown) {
							subscriber.error(thrown);
							return;
						}
						forwardTo(replacement, subscriber);
					},
					complete: () => subscriber.complete(),
				}, { signal: subscriber.signal });
			});
		}

		finally(callback) {
			requireFunction(callback, "finally");
			const source = this;
			return new Observable((subscriber) => {
				subscriber.addTeardown(() => callback());
				forwardTo(source, subscriber);
			});
		}

		// -------------------------------------------------- promise-returning
		// Each consumes the whole observable and answers with a promise, so each
		// takes a signal and rejects with its reason when it is aborted.

		toArray(options = undefined) {
			return collect(this, options, (resolve, reject) => {
				const values = [];
				return {
					next: (v) => values.push(v),
					error: reject,
					complete: () => resolve(values),
				};
			});
		}

		forEach(callback, options = undefined) {
			requireFunction(callback, "forEach");
			return collect(this, options, (resolve, reject, subscriber) => {
				let index = 0;
				return {
					next: (v) => {
						try {
							callback(v, index++);
						} catch (e) {
							subscriber.abort(e);
							reject(e);
						}
					},
					error: reject,
					complete: () => resolve(undefined),
				};
			});
		}

		every(predicate, options = undefined) {
			requireFunction(predicate, "every");
			return collect(this, options, (resolve, reject, subscriber) => {
				let index = 0;
				return {
					next: (v) => {
						let ok;
						try {
							ok = predicate(v, index++);
						} catch (e) {
							subscriber.abort(e);
							reject(e);
							return;
						}
						if (!ok) { subscriber.abort(); resolve(false); }
					},
					error: reject,
					complete: () => resolve(true),
				};
			});
		}

		some(predicate, options = undefined) {
			requireFunction(predicate, "some");
			return collect(this, options, (resolve, reject, subscriber) => {
				let index = 0;
				return {
					next: (v) => {
						let ok;
						try {
							ok = predicate(v, index++);
						} catch (e) {
							subscriber.abort(e);
							reject(e);
							return;
						}
						if (ok) { subscriber.abort(); resolve(true); }
					},
					error: reject,
					complete: () => resolve(false),
				};
			});
		}

		find(predicate, options = undefined) {
			requireFunction(predicate, "find");
			return collect(this, options, (resolve, reject, subscriber) => {
				let index = 0;
				return {
					next: (v) => {
						let ok;
						try {
							ok = predicate(v, index++);
						} catch (e) {
							subscriber.abort(e);
							reject(e);
							return;
						}
						if (ok) { subscriber.abort(); resolve(v); }
					},
					error: reject,
					complete: () => resolve(undefined),
				};
			});
		}

		first(options = undefined) {
			return collect(this, options, (resolve, reject, subscriber) => ({
				next: (v) => { subscriber.abort(); resolve(v); },
				error: reject,
				// An empty observable has no first value, and saying so is a
				// RangeError rather than undefined — which would be indistinguishable
				// from a first value of undefined.
				complete: () => reject(new RangeError("first(): the observable completed without emitting a value")),
			}));
		}

		last(options = undefined) {
			return collect(this, options, (resolve, reject) => {
				let seen = false;
				let latest;
				return {
					next: (v) => { seen = true; latest = v; },
					error: reject,
					complete: () => {
						if (seen) resolve(latest);
						else reject(new RangeError("last(): the observable completed without emitting a value"));
					},
				};
			});
		}

		reduce(reducer, ...rest) {
			requireFunction(reducer, "reduce");
			const hasSeed = rest.length > 0 && rest[0] !== undefined;
			const seed = rest[0];
			const options = rest.length > 1 ? rest[1] : undefined;
			return collect(this, options, (resolve, reject, subscriber) => {
				let index = 0;
				let has = hasSeed;
				let acc = seed;
				return {
					next: (v) => {
						if (!has) { has = true; acc = v; index++; return; }
						try {
							acc = reducer(acc, v, index++);
						} catch (e) {
							subscriber.abort(e);
							reject(e);
						}
					},
					error: reject,
					complete: () => {
						if (has) resolve(acc);
						else reject(new TypeError("reduce(): the observable completed with no value and no initial value"));
					},
				};
			});
		}

		// Observable.from converts the things that are already sequences: another
		// Observable is itself, and a promise, an iterable or an async iterable
		// becomes one. Anything else is a TypeError — being convertible is a
		// property of the value, not something to guess at.
		static from(value) {
			if (arguments.length < 1) throw new TypeError("from requires 1 argument");
			if (value instanceof Observable) return value;
			if (value === null || value === undefined) {
				throw new TypeError("Observable.from: the value is not convertible to an Observable");
			}
			if (typeof value[Symbol.iterator] === "function") return fromIterable(value);
			if (typeof value[Symbol.asyncIterator] === "function") return fromAsyncIterable(value);
			if (typeof value.then === "function") return fromPromise(value);
			throw new TypeError("Observable.from: the value is not convertible to an Observable");
		}
	}
	Object.defineProperty(Observable.prototype, Symbol.toStringTag, {
		value: "Observable", configurable: true,
	});

	function requireFunction(fn, who) {
		if (typeof fn !== "function") throw new TypeError(`${who}: a function is required`);
	}

	// An `unsigned long long` WRAPS: -1 is the largest value there is, not an
	// error. take(-1) therefore means "take everything", which is what a caller
	// writing it expects and what the conversion actually does.
	function toUnsignedLongLong(value, who) {
		const n = Number(value);
		if (Number.isNaN(n) || !Number.isFinite(n)) return Infinity;
		if (n < 0) return Infinity;
		return Math.floor(n);
	}

	// subscribeTo is the one place a subscription is set up: it builds the
	// Subscriber, wires the caller's signal to it, and runs the initializer.
	function subscribeTo(observable, observer, options) {
		const opts = options === undefined || options === null ? {} : options;
		const signal = opts.signal;
		if (signal !== undefined && signal !== null
			&& !(typeof AbortSignal === "function" && signal instanceof AbortSignal)) {
			throw new TypeError("subscribe: options.signal is not an AbortSignal");
		}
		const subscriber = new Subscriber(INTERNAL, observer, signal ?? null);
		if (signal) {
			// An already-aborted signal closes the subscriber BEFORE the initializer
			// runs — but the initializer still runs, and sees an inactive subscriber.
			// Skipping it entirely hid the state it is supposed to observe.
			if (signal.aborted) subscriber._close(signal.reason);
			else addAbortAlgorithm(signal, () => subscriber._close(signal.reason));
		}
		try {
			observable._subscribe(subscriber);
		} catch (e) {
			subscriber.error(e);
		}
		return subscriber;
	}

	// forwardTo pipes a source straight into a subscriber, which is what an
	// operator that changes nothing about the values does.
	function forwardTo(source, subscriber) {
		subscribeTo(source, {
			next: (v) => subscriber.next(v),
			error: (e) => subscriber.error(e),
			complete: () => subscriber.complete(),
		}, { signal: subscriber.signal });
	}

	// collect is the shape every promise-returning operator has: a promise, an
	// abort controller that can end the subscription early, and a signal that
	// rejects it.
	function collect(observable, options, makeObserver) {
		const opts = options === undefined || options === null ? {} : options;
		const outer = opts.signal;
		return new Promise((resolve, reject) => {
			if (outer !== undefined && outer !== null
				&& !(typeof AbortSignal === "function" && outer instanceof AbortSignal)) {
				reject(new TypeError("options.signal is not an AbortSignal"));
				return;
			}
			if (outer && outer.aborted) {
				reject(outer.reason);
				return;
			}
			const controller = new AbortController();
			const signal = outer ? AbortSignal.any([outer, controller.signal]) : controller.signal;
			if (outer) {
				outer.addEventListener("abort", () => reject(outer.reason), { once: true });
			}
			const observer = makeObserver(resolve, reject, {
				abort: (reason) => controller.abort(reason),
			});
			subscribeTo(observable, observer, { signal });
		});
	}

	function fromIterable(value) {
		return new Observable((subscriber) => {
			let iterator;
			try {
				iterator = value[Symbol.iterator]();
			} catch (e) {
				subscriber.error(e);
				return;
			}
			for (;;) {
				if (!subscriber.active) return;
				let result;
				try {
					result = iterator.next();
				} catch (e) {
					subscriber.error(e);
					return;
				}
				if (result.done) { subscriber.complete(); return; }
				subscriber.next(result.value);
			}
		});
	}

	function fromAsyncIterable(value) {
		return new Observable((subscriber) => {
			let iterator;
			try {
				iterator = value[Symbol.asyncIterator]();
			} catch (e) {
				subscriber.error(e);
				return;
			}
			const pump = () => {
				if (!subscriber.active) return;
				let promise;
				try {
					promise = iterator.next();
				} catch (e) {
					subscriber.error(e);
					return;
				}
				Promise.resolve(promise).then(
					(result) => {
						if (!subscriber.active) return;
						if (result === null || typeof result !== "object") {
							subscriber.error(new TypeError("the async iterator did not return an object"));
							return;
						}
						if (result.done) { subscriber.complete(); return; }
						subscriber.next(result.value);
						pump();
					},
					(e) => subscriber.error(e),
				);
			};
			// The iterator is only asked for a value once the subscription is set up
			// and its teardown is in place.
			subscriber.addTeardown(() => {
				if (typeof iterator.return === "function") {
					try { iterator.return(); } catch { /* the iterator's cleanup is its own */ }
				}
			});
			pump();
		});
	}

	function fromPromise(value) {
		return new Observable((subscriber) => {
			Promise.resolve(value).then(
				(v) => {
					if (!subscriber.active) return;
					subscriber.next(v);
					subscriber.complete();
				},
				(e) => {
					if (!subscriber.active) return;
					subscriber.error(e);
				},
			);
		});
	}

	// EventTarget.when(type) is an Observable of that target's events — the
	// listener is added when it is subscribed to and removed when it ends, which
	// is what makes an event stream composable with the operators above.
	if (typeof EventTarget === "function" && typeof EventTarget.prototype.when !== "function") {
		Object.defineProperty(EventTarget.prototype, "when", {
			value: function when(type, options = undefined) {
				if (arguments.length < 1) throw new TypeError("when requires 1 argument");
				const target = this;
				const eventType = String(type);
				const opts = options === undefined || options === null ? {} : options;
				return new Observable((subscriber) => {
					const listener = (event) => subscriber.next(event);
					target.addEventListener(eventType, listener, opts);
					subscriber.addTeardown(() => target.removeEventListener(eventType, listener, opts));
				});
			},
			writable: true, enumerable: true, configurable: true,
		});
	}

	globalThis.Observable = Observable;
	globalThis.Subscriber = Subscriber;
})();
