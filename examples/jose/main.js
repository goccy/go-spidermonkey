// A pure-ESM module: it imports the real jose package, signs a JWT with HS256
// over a fixed secret and fixed claims (deterministic setIssuedAt), then
// verifies it. It prints only the verified fields — not the token string,
// which is stable here but not part of the asserted contract.
import { SignJWT, jwtVerify } from "jose";

await (async () => {
	const secret = new TextEncoder().encode("a-string-secret-at-least-256-bits-long!!");
	const jwt = await new SignJWT({ role: "admin" })
		.setProtectedHeader({ alg: "HS256", typ: "JWT" })
		.setSubject("alice")
		.setIssuedAt(1720000000)
		.sign(secret);
	const { payload, protectedHeader } = await jwtVerify(jwt, secret);
	console.log(JSON.stringify({
		sub: payload.sub,
		role: payload.role,
		iat: payload.iat,
		alg: protectedHeader.alg,
		verified: true,
	}));
})();
