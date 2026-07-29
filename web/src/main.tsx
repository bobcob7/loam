import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { Placeholder } from "./components/Placeholder";
// The only two global stylesheets (docs/web-frontend-spec.md -> Conventions);
// everything else is a CSS Module scoped to its component. Imported here, at
// the entry, so they are in the bundle before any component's module CSS.
import "./styles/reset.css";
import "./styles/tokens.css";

// Entry point. loam-nvb.3 replaces the render below with the real shell --
// QueryClientProvider, the connect-web transport, and the wouter router
// (docs/web-frontend-spec.md -> Project Layout); the container lookup stays.
const container = document.getElementById("root");
if (container === null) {
  throw new Error('main: index.html is missing its <div id="root"> container');
}
createRoot(container).render(
  <StrictMode>
    <Placeholder message="Loam admin" />
  </StrictMode>,
);
