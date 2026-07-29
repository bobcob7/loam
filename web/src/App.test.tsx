import { useTransport } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import type { ReactElement } from "react";
import { describe, expect, it } from "vitest";
import { App, AppProviders } from "./App";
import { createQueryClient, shouldRetryQuery } from "./queryClient";
import { transport } from "./transport";

/**
 * "The provider renders its children" is not a test — it passes for a
 * provider wired to the wrong value, and connect-query in particular fails
 * *silently* when its provider is missing: `useTransport` falls back to an
 * internal `fallbackTransport` and the failure only surfaces later, inside
 * some screen's query. So the probe below asserts identity: the transport the
 * tree sees is the shared one, and the client it sees is a configured one.
 */

interface Seen {
  transport?: unknown;
  retry?: unknown;
}

const Probe = ({ seen }: { readonly seen: Seen }): ReactElement => {
  seen.transport = useTransport();
  seen.retry = useQueryClient().getDefaultOptions().queries?.retry;
  return <span>probe</span>;
};

describe("AppProviders", () => {
  it("gives the tree the shared transport, not connect-query's fallback", () => {
    const seen: Seen = {};
    render(
      <AppProviders>
        <Probe seen={seen} />
      </AppProviders>,
    );
    expect(seen.transport).toBe(transport);
  });

  it("gives the tree a QueryClient carrying the configured retry policy", () => {
    const seen: Seen = {};
    render(
      <AppProviders>
        <Probe seen={seen} />
      </AppProviders>,
    );
    // A bare `new QueryClient()` would satisfy every render-based assertion
    // and silently restore TanStack's retry-everything-three-times default.
    expect(seen.retry).toBe(shouldRetryQuery);
  });

  it("creates one client per mounted tree, so two apps do not share a cache", () => {
    expect(createQueryClient()).not.toBe(createQueryClient());
  });
});

describe("App", () => {
  it("mounts the shell, the nav and the default screen at /", () => {
    // jsdom's location is "/", so this exercises the real browser-location
    // hook rather than a memory one: providers, router, layout and route
    // table composed exactly as main.tsx composes them.
    render(<App />);
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Repos");
    expect(screen.getByRole("navigation", { name: "Main" })).toBeInTheDocument();
  });
});
