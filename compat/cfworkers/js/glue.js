// cfworkers glue: the guest half of the request/response plumbing. Evaluated
// once per worker instance, after compat/web's builtins (Request/Response/
// Headers come from there) and before the user's module is booted.
(() => {
	"use strict";
	const state = { pending: [], result: null };

	// Build the Request the handler sees from host-supplied parts.
	globalThis.__cfw_make_request = (method, url, headerPairs, body) =>
		new Request(url, {
			method,
			headers: headerPairs,
			body: body === null || body === undefined ? undefined : body,
		});

	// Kick the handler; completion lands in state.result via the microtask
	// queue (drained by the host loop).
	globalThis.__cfw_run = (req) => {
		state.result = null;
		const ctx = {
			waitUntil: (p) => { state.pending.push(Promise.resolve(p).catch(() => {})); },
			passThroughOnException: () => {},
		};
		Promise.resolve()
			.then(() => globalThis.__cfw_handler.fetch(req, globalThis.__cfw_env, ctx))
			.then((resp) => { state.result = { ok: true, resp }; })
			.catch((err) => {
				// SpiderMonkey stacks do not include the message line; compose
				// both so the host error is actually diagnosable.
				const msg = err instanceof Error
					? `${err.name}: ${err.message}${err.stack ? "\n" + err.stack : ""}`
					: String(err);
				state.result = { ok: false, error: msg };
			});
	};

	globalThis.__cfw_status = () => (state.result === null ? "pending" : state.result.ok ? "ok" : "error");
	globalThis.__cfw_error = () => String(state.result.error);

	globalThis.__cfw_response_meta = () => {
		const r = state.result.resp;
		if (!r || typeof r !== "object" || typeof r.status !== "number"
			|| !r.headers || typeof r.headers.entries !== "function") {
			throw new TypeError("handler did not return a Response");
		}
		return JSON.stringify({
			status: r.status,
			statusText: String(r.statusText || ""),
			headers: [...r.headers.entries()],
		});
	};

	// The buffered body bytes (Uint8Array) or null.
	globalThis.__cfw_response_body = () => {
		const r = state.result.resp;
		return r._body === null || r._body === undefined ? null : r._body;
	};
})();
