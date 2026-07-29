import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
// The only two global stylesheets (docs/web-frontend-spec.md -> Conventions);
// everything else is a CSS Module scoped to its component. Imported here, at
// the entry, so they are in the bundle before any component's module CSS.
import "./styles/reset.css";
import "./styles/tokens.css";

// Entry point, and deliberately nothing more: the providers, the router and
// the route table live in ./App, which a test can render. This file's only
// job — find the container and hand the tree to React — cannot be exercised
// without importing a module whose top level mounts the app, so it is kept
// small enough that there is nothing in it to get wrong.
const container = document.getElementById("root");
if (container === null) {
  throw new Error('main: index.html is missing its <div id="root"> container');
}
createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
