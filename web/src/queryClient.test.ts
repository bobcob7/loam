import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, it, vi } from "vitest";
import { createQueryClient, shouldRetryQuery } from "./queryClient";

/**
 * The retry policy is the only part of the QueryClient configuration that is
 * a decision rather than a value, so it is the part worth testing — and the
 * behavioural cases below go through a real `QueryClient`, not just the
 * predicate, because a predicate that is never installed on the client is
 * exactly the failure a unit test of the predicate alone would miss.
 *
 * `staleTime` / `refetchOnWindowFocus` are asserted only where they change
 * observable behaviour (the de-duplication case). Reading them back off
 * `getDefaultOptions()` would restate the source file.
 */

const connectError = (code: Code): ConnectError => new ConnectError("boom", code);

describe("shouldRetryQuery", () => {
  it.each([
    ["a failed fetch (offline), which connect-web reports as Unknown", Code.Unknown],
    ["a restarting or overloaded server", Code.Unavailable],
    ["a deadline overrun", Code.DeadlineExceeded],
    ["backpressure", Code.ResourceExhausted],
  ])("retries %s", (_description, code) => {
    expect(shouldRetryQuery(0, connectError(code))).toBe(true);
  });

  it.each([
    ["basic auth was rejected — retrying re-sends the same credential", Code.Unauthenticated],
    ["the admin is not allowed to do this", Code.PermissionDenied],
    ["a precondition that cannot change between attempts", Code.FailedPrecondition],
    ["a malformed request", Code.InvalidArgument],
    ["a missing resource", Code.NotFound],
    ["a request the client itself aborted", Code.Canceled],
  ])("does not retry %s", (_description, code) => {
    expect(shouldRetryQuery(0, connectError(code))).toBe(false);
  });

  it("gives up after two retries even on a retryable code", () => {
    expect(shouldRetryQuery(1, connectError(Code.Unavailable))).toBe(true);
    expect(shouldRetryQuery(2, connectError(Code.Unavailable))).toBe(false);
  });

  it("does not retry an error that is not a ConnectError", () => {
    // Everything connect-web raises is a ConnectError, so this is a bug in a
    // queryFn rather than a network condition. Repeating it just triples the
    // noise in the console.
    expect(shouldRetryQuery(0, new TypeError("undefined is not a function"))).toBe(false);
  });
});

describe("the configured QueryClient", () => {
  it("installs the retry policy on queries: a transient failure is attempted three times", async () => {
    const queryFn = vi.fn(() => Promise.reject(connectError(Code.Unavailable)));
    await expect(
      // retryDelay is overridden only to keep the test instant; `retry` comes
      // from the client's own defaults, which is what is under test.
      createQueryClient().fetchQuery({ queryKey: ["unavailable"], queryFn, retryDelay: 0 }),
    ).rejects.toThrow(ConnectError);
    expect(queryFn).toHaveBeenCalledTimes(3);
  });

  it("installs the retry policy on queries: a rejected credential is attempted once", async () => {
    const queryFn = vi.fn(() => Promise.reject(connectError(Code.Unauthenticated)));
    await expect(
      createQueryClient().fetchQuery({ queryKey: ["unauthenticated"], queryFn, retryDelay: 0 }),
    ).rejects.toThrow(ConnectError);
    expect(queryFn).toHaveBeenCalledTimes(1);
  });

  it("never retries a mutation, however transient the failure looks", async () => {
    const mutationFn = vi.fn(() => Promise.reject(connectError(Code.Unavailable)));
    const client = createQueryClient();
    await expect(
      client.getMutationCache().build(client, { mutationFn }).execute(undefined),
    ).rejects.toThrow(ConnectError);
    // AcceptProposal opens a PR on a real forge; EnrollRepo clones a repo.
    // An automatic second attempt is a duplicate side effect on someone
    // else's system.
    expect(mutationFn).toHaveBeenCalledTimes(1);
  });

  it("serves a second read of fresh data from cache rather than refetching", async () => {
    const queryFn = vi.fn(() => Promise.resolve("repos"));
    const client = createQueryClient();
    await client.fetchQuery({ queryKey: ["repos"], queryFn });
    await client.fetchQuery({ queryKey: ["repos"], queryFn });
    // This is the whole point of a non-zero staleTime: navigating list ->
    // detail -> back remounts the list query, and with TanStack's default of
    // 0 that is a second round trip every time.
    expect(queryFn).toHaveBeenCalledTimes(1);
  });
});
