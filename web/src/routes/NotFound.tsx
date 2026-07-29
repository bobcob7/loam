import type { ReactElement } from "react";
import { Link } from "wouter";
import { routePatterns } from "./paths";

/**
 * The route table's fallback. It exists because the server's SPA fallback
 * hands `index.html` to *any* path that is not an RPC route or a real asset
 * (docs/web-frontend-spec.md -> Build & Embed), so a mistyped or stale URL
 * reaches the client router rather than producing a 404. Without a terminal
 * `<Route>` here, `<Switch>` would match nothing and the layout would render
 * an empty `<main>` — a blank screen with no way back.
 */
export function NotFound(): ReactElement {
  return (
    <>
      <h1>Not found</h1>
      <p>
        This URL does not match any screen. <Link href={routePatterns.repos}>Go to Repos</Link>
      </p>
    </>
  );
}
