export default function Home({ engine, answer }) {
  return (
    <main>
      <h1>Hello from Next.js on go-spidermonkey!</h1>
      <p id="ssr-prop">{engine}:{answer}</p>
    </main>
  );
}

export async function getServerSideProps() {
  return { props: { engine: "spidermonkey", answer: 42 } };
}
