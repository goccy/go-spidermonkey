// A static Server Component route: no dynamic data, so Next prerenders it at
// build time.
export default function About() {
  return <p id="static-page">Statically generated About (App Router)</p>;
}
