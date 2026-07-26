import Counter from "./counter";

// Render this Server Component on the server for every request (dynamic SSR)
// rather than prerendering it at build time, so the production server actually
// executes it inside go-spidermonkey on each hit.
export const dynamic = "force-dynamic";

// A Server Component (the App Router default): it runs ONLY on the server, its
// code never ships to the client, and it may be async and touch server-only
// values. Here it computes data server-side and passes it into a Client
// Component as props.
async function getServerData() {
  return { engine: "spidermonkey", answer: 42, node: process.version };
}

export default async function Home() {
  const data = await getServerData();
  return (
    <main>
      <h1>Hello from Next.js App Router on go-spidermonkey!</h1>
      <p id="server-data">
        {data.engine}:{data.answer}
      </p>
      <p id="server-only">rendered by node {data.node}</p>
      <Counter label="Server value" initial={data.answer} />
    </main>
  );
}
