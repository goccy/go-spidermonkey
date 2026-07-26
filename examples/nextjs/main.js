// A standard Next.js custom server: prepare the App Router production build and
// serve it over HTTP. It listens on process.env.PORT (default 3000).
const { createServer } = require("http");
const next = require("next");

const port = process.env.PORT ? Number(process.env.PORT) : 3000;
const app = next({ dev: false, dir: "/" });
const handle = app.getRequestHandler();

app.prepare().then(() => {
	createServer((req, res) => {
		Promise.resolve(handle(req, res)).catch((e) => {
			if (!res.headersSent) res.statusCode = 500;
			res.end("handler error: " + (e && e.message));
		});
	}).listen(port, () => console.log("listening on " + port));
}).catch((e) => {
	console.error("next boot failed:", (e && e.stack) || e);
});
