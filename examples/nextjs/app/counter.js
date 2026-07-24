"use client";

import { useState } from "react";

// A Client Component: the "use client" directive marks it as interactive — its
// JavaScript ships to the browser and hydrates there. Its INITIAL state
// (seeded from the Server Component's prop) is server-rendered into the HTML,
// so the count is present before hydration.
export default function Counter({ label, initial }) {
  const [count, setCount] = useState(initial);
  return (
    <div>
      <p id="client-count">
        {label}: {count}
      </p>
      <button id="increment" onClick={() => setCount((c) => c + 1)}>
        increment
      </button>
    </div>
  );
}
