import { create } from "@bufbuild/protobuf";
import { TransportProvider, createConnectQueryKey } from "@connectrpc/connect-query";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactElement, ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { acceptProposal, listProposals } from "../gen/loam/admin/v1/proposal-ProposalService_connectquery";
import { ListProposalsResponseSchema } from "../gen/loam/admin/v1/proposal_pb";
import { enrollRepo, listRepos } from "../gen/loam/admin/v1/repo_admin-RepoAdminService_connectquery";
import { ListReposResponseSchema } from "../gen/loam/admin/v1/repo_admin_pb";
import { transport } from "../transport";
import { useMutationInvalidating } from "./invalidation";

const emptyListReposResponse = () => create(ListReposResponseSchema, {});
const emptyListProposalsResponse = () => create(ListProposalsResponseSchema, {});

/**
 * These drive `useMutationInvalidating` through the real transport (stubbed
 * at `fetch`, exactly as `transport.test.ts` does) rather than a hand-rolled
 * fake `Transport`, so the assertions exercise the actual wiring a screen
 * gets: a real `useMutation` from connect-query, a real `QueryClient`, and
 * the two sample mutations the spec names --
 * `EnrollRepo` -> `ListRepos` and `AcceptProposal` -> `ListProposals`
 * (docs/web-frontend-spec.md -> Data Layer).
 */

const jsonOk = (): Promise<Response> =>
  Promise.resolve(new Response("{}", { status: 200, headers: { "content-type": "application/json" } }));

const stubSuccessfulFetch = (): void => {
  vi.stubGlobal("fetch", () => jsonOk());
};

const stubFailingFetch = (): void => {
  vi.stubGlobal("fetch", () => Promise.reject(new Error("network down")));
};

afterEach(() => {
  vi.unstubAllGlobals();
});

const wrapperFor = (queryClient: QueryClient) => {
  return function Wrapper({ children }: { readonly children: ReactNode }): ReactElement {
    return (
      <TransportProvider transport={transport}>
        <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
      </TransportProvider>
    );
  };
};

const listReposKey = createConnectQueryKey({ schema: listRepos, cardinality: "finite" });
const listProposalsKey = createConnectQueryKey({ schema: listProposals, cardinality: "finite" });

describe("useMutationInvalidating", () => {
  it("invalidates ListRepos when EnrollRepo succeeds", async () => {
    stubSuccessfulFetch();
    const queryClient = new QueryClient();
    // Seed both caches so a helper that invalidates indiscriminately, or not
    // at all, is distinguishable from one that invalidates exactly the
    // declared target.
    queryClient.setQueryData(listReposKey, emptyListReposResponse());
    queryClient.setQueryData(listProposalsKey, emptyListProposalsResponse());
    const { result } = renderHook(
      () => useMutationInvalidating(enrollRepo, [{ schema: listRepos }]),
      { wrapper: wrapperFor(queryClient) },
    );
    result.current.mutate({
      upstreamUrl: "https://example.com/acme/widgets.git",
      targetBranches: ["main"],
      indexedBranch: "main",
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(queryClient.getQueryState(listReposKey)?.isInvalidated).toBe(true);
    // The unrelated ListProposals cache is untouched by an EnrollRepo write.
    expect(queryClient.getQueryState(listProposalsKey)?.isInvalidated).toBe(false);
  });

  it("invalidates every cached page of ListRepos, not just the one with no input", async () => {
    // Invalidates declares a target by schema alone -- an admin might be
    // sitting on page 2 of ListRepos when EnrollRepo succeeds elsewhere, and
    // that cached page must go stale too (docs/web-frontend-spec.md -> Data
    // Layer). A key that pinned a specific `input` would miss this entirely.
    stubSuccessfulFetch();
    const queryClient = new QueryClient();
    const secondPageKey = createConnectQueryKey({
      schema: listRepos,
      cardinality: "finite",
      input: { page: { limit: 25, offset: 25 } },
    });
    queryClient.setQueryData(secondPageKey, emptyListReposResponse());
    const { result } = renderHook(
      () => useMutationInvalidating(enrollRepo, [{ schema: listRepos }]),
      { wrapper: wrapperFor(queryClient) },
    );
    result.current.mutate({
      upstreamUrl: "https://example.com/acme/widgets.git",
      targetBranches: ["main"],
      indexedBranch: "main",
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(queryClient.getQueryState(secondPageKey)?.isInvalidated).toBe(true);
  });

  it("invalidates ListProposals when AcceptProposal succeeds", async () => {
    stubSuccessfulFetch();
    const queryClient = new QueryClient();
    queryClient.setQueryData(listProposalsKey, emptyListProposalsResponse());
    queryClient.setQueryData(listReposKey, emptyListReposResponse());
    const { result } = renderHook(
      () => useMutationInvalidating(acceptProposal, [{ schema: listProposals }]),
      { wrapper: wrapperFor(queryClient) },
    );
    result.current.mutate({ repo: "acme/widgets", workBranch: "wb-9c2f1a" });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(queryClient.getQueryState(listProposalsKey)?.isInvalidated).toBe(true);
    expect(queryClient.getQueryState(listReposKey)?.isInvalidated).toBe(false);
  });

  it("does not invalidate anything when the mutation fails", async () => {
    stubFailingFetch();
    const queryClient = new QueryClient();
    queryClient.setQueryData(listReposKey, emptyListReposResponse());
    const { result } = renderHook(
      () => useMutationInvalidating(enrollRepo, [{ schema: listRepos }]),
      { wrapper: wrapperFor(queryClient) },
    );
    result.current.mutate({
      upstreamUrl: "https://example.com/acme/widgets.git",
      targetBranches: ["main"],
      indexedBranch: "main",
    });
    await waitFor(() => expect(result.current.isError).toBe(true));
    // A failed EnrollRepo did not clone anything -- ListRepos is still
    // accurate, and re-fetching it would be pure waste.
    expect(queryClient.getQueryState(listReposKey)?.isInvalidated).toBe(false);
  });

  it("still calls the caller's own onSuccess, after invalidation has happened", async () => {
    stubSuccessfulFetch();
    const queryClient = new QueryClient();
    queryClient.setQueryData(listReposKey, emptyListReposResponse());
    let wasInvalidatedDuringCallback: boolean | undefined;
    const { result } = renderHook(
      () =>
        useMutationInvalidating(enrollRepo, [{ schema: listRepos }], {
          onSuccess: () => {
            wasInvalidatedDuringCallback = queryClient.getQueryState(listReposKey)?.isInvalidated;
          },
        }),
      { wrapper: wrapperFor(queryClient) },
    );
    result.current.mutate({
      upstreamUrl: "https://example.com/acme/widgets.git",
      targetBranches: ["main"],
      indexedBranch: "main",
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(wasInvalidatedDuringCallback).toBe(true);
  });
});
