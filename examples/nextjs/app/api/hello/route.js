// A Route Handler (App Router's API): exports HTTP-method functions returning
// a web Response.
export async function GET(request) {
  return Response.json({ hello: "from route handler", method: "GET" });
}
