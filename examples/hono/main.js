// An ESM Cloudflare-Workers module: it imports the real Hono framework,
// defines a couple of routes, and exports the app as the default handler.
// c.env.GREETING reads the value bound from Go via the pool's Env.
import { Hono } from "hono";

const app = new Hono();

app.get("/", (c) => c.text("Hello Hono!"));
app.get("/api/items/:id", (c) =>
	c.json({
		id: c.req.param("id"),
		v: c.req.query("v") ?? null,
		greeting: c.env.GREETING ?? null,
	}));

export default app;
