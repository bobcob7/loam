import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClientProvider } from "@tanstack/react-query";
import { useState, type ReactElement, type ReactNode } from "react";
import { Router } from "wouter";
import { createQueryClient } from "./queryClient";
import { AppLayout } from "./routes/AppLayout";
import { AppRoutes } from "./routes/AppRoutes";
import { transport } from "./transport";

export interface AppProvidersProps {
  readonly children: ReactNode;
}

/**
 * The two providers the whole app runs inside, split out from `App` so a
 * test — or a screen bead's test — can mount a component with the real
 * transport and a real query cache without also mounting the router and the
 * chrome.
 *
 * `TransportProvider` is not optional in the way it looks: with no provider
 * above it, connect-query's `useTransport` silently returns its own
 * `fallbackTransport`, so a missing provider surfaces as a confusing runtime
 * error inside a query rather than as a render failure. That is why the test
 * for this file asserts transport *identity*, not that children rendered.
 *
 * The `QueryClient` is created per mounted tree via `useState`'s lazy
 * initialiser rather than as a module-level singleton: the app mounts once,
 * so it still gets exactly one client, while each test gets a cache that
 * cannot leak into the next one.
 */
export function AppProviders({ children }: AppProvidersProps): ReactElement {
  const [queryClient] = useState(createQueryClient);
  return (
    <TransportProvider transport={transport}>
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    </TransportProvider>
  );
}

/**
 * The whole SPA: providers, the router, the chrome, and the route table.
 *
 * `<Router>` carries no props, so it inherits wouter's default
 * browser-location hook. It is written out rather than left implicit because
 * it is the single place a `base` would go if the SPA were ever mounted under
 * a path prefix, and because a test can override the location by rendering
 * its own `<Router hook=…>` — a nested prop-less Router defers to the parent.
 */
export function App(): ReactElement {
  return (
    <AppProviders>
      <Router>
        <AppLayout>
          <AppRoutes />
        </AppLayout>
      </Router>
    </AppProviders>
  );
}
