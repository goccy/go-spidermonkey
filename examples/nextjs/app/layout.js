// Root layout — a Server Component, required by the App Router. It wraps every
// route and renders the <html>/<body> shell.
export const metadata = {
  title: "go-spidermonkey × Next.js App Router",
  description: "Server + Client Components running on go-spidermonkey",
};

export default function RootLayout({ children }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
