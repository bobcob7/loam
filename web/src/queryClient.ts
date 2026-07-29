import { Code, ConnectError } from "@connectrpc/connect";
import { QueryClient } from "@tanstack/react-query";

/**
 * Two retries after the first attempt (three requests worst case). TanStack's
 * own default is 3 retries; this is lower because every code that is worth
 * retrying here is a transport or availability fault, and if the second retry
 * has not cleared it, a fourth request will not either — it just delays the
 * error state the admin needs to see.
 */
const maxQueryRetries = 2;

/**
 * The Connect codes worth a second attempt. Everything absent from this set
 * is deterministic: retrying it re-runs a call whose answer cannot change.
 *
 * - `Unknown` is what connect-web assigns a failed `fetch` (see
 *   `protocol/run-call.js`: `ConnectError.from(reason)` defaults to Unknown),
 *   so this is the offline/DNS/connection-reset case — the single most
 *   retry-worthy failure a browser client has.
 * - `Unavailable` is what the Connect protocol maps HTTP 429/502/503/504 to:
 *   a server that is restarting or shedding load.
 * - `DeadlineExceeded` and `ResourceExhausted` are the other two transient
 *   server-side conditions.
 *
 * Explicitly NOT retried, all of which a naive "retry everything" default
 * would get wrong:
 * - `Unauthenticated` / `PermissionDenied` — the SPA is behind basic auth and
 *   a 401 carries `WWW-Authenticate`; retrying re-sends the same rejected
 *   credential and can make the browser re-prompt. The admin needs the auth
 *   state (docs/web-frontend-spec.md -> Error mapping), not three more 401s.
 * - `FailedPrecondition` — e.g. `RemoveRepo` blocked by open work branches,
 *   or `AcceptProposal` without an approve verdict. The precondition will not
 *   change between two requests 1s apart, and the typed `RemovalBlocked`
 *   detail is the payload the screen wants.
 * - `InvalidArgument` / `NotFound` — deterministic; the screen renders a form
 *   error or an empty state.
 * - `Canceled` — the query was aborted (unmount, or a superseded fetch).
 *   Retrying a cancelled request is the one case that is actively wrong.
 */
const retryableCodes: ReadonlySet<Code> = new Set<Code>([
  Code.Unknown,
  Code.Unavailable,
  Code.DeadlineExceeded,
  Code.ResourceExhausted,
]);

/**
 * shouldRetryQuery is the `retry` predicate installed on every query. It is
 * exported so it can be tested directly, and so the screens never restate it.
 */
export function shouldRetryQuery(failureCount: number, error: unknown): boolean {
  if (failureCount >= maxQueryRetries) {
    return false;
  }
  // Every error that reaches here through connect-query is a ConnectError:
  // connect-web funnels transport faults through `ConnectError.from`. Anything
  // else is a bug in a queryFn rather than a network condition, and repeating
  // a bug two more times only makes it harder to read in the console.
  if (!(error instanceof ConnectError)) {
    return false;
  }
  return retryableCodes.has(error.code);
}

/**
 * TanStack's default backoff is `min(1000 * 2 ** attempt, 30_000)`. The
 * formula is kept; the ceiling is dropped to 5s. With at most two retries the
 * cap is only reachable via a `retryDelay` override anyway, but a console
 * whose tables can sit visibly "loading" for half a minute reads as hung.
 */
const retryDelay = (attemptIndex: number): number => Math.min(1000 * 2 ** attemptIndex, 5000);

/**
 * createQueryClient builds the single `QueryClient` provided at the root. It
 * is a factory, not a module-level singleton, so tests get an isolated cache
 * per case; `src/App.tsx` calls it exactly once for the app.
 *
 * The defaults are tuned for an admin console watching server-side activity
 * it did not initiate (ingest jobs, mirror sync, upstream PR state), which is
 * a different shape from a CRUD app where the user is the only writer:
 *
 * - `staleTime: 5_000`. Not TanStack's `0`: with zero, every remount refetches,
 *   and this app's navigation (repos list -> repo detail -> back) remounts
 *   constantly. Not minutes either: an ingest job's status changes without any
 *   action from this browser, so cached-forever is actively misleading. Five
 *   seconds dedupes a navigation burst while keeping "go back to the list and
 *   it is current" true.
 * - `refetchOnWindowFocus: true` (TanStack's default, set explicitly because
 *   it is a decision, not an accident). Returning to the tab after watching a
 *   reindex run in a terminal should show the finished job, not the snapshot
 *   from when the tab lost focus.
 * - `refetchOnReconnect: true` for the same reason across a network blip.
 * - No `refetchInterval` here. Polling is per-screen — only the Jobs screen and
 *   a syncing repo's status need it, and making every list poll would put a
 *   permanent request floor under an idle tab. loam-nvb.14 owns that.
 * - `throwOnError` left false: screens render errors inline (`ErrorBanner`,
 *   and the structured blocking-work-branches panel for `RemoveRepo`), so
 *   errors must stay data, not become render-time throws.
 * - Mutations `retry: false`, unconditionally. `EnrollRepo` clones a repo,
 *   `AcceptProposal` opens an upstream PR on a real forge, `SetUpstreamToken`
 *   replaces a credential — none are idempotent, and an automatic second
 *   attempt after a response that was lost rather than never sent is a
 *   duplicate side effect on someone else's system. A failed write is the
 *   admin's decision to repeat.
 */
export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        retry: shouldRetryQuery,
        retryDelay,
        staleTime: 5_000,
        refetchOnWindowFocus: true,
        refetchOnReconnect: true,
      },
      mutations: {
        retry: false,
      },
    },
  });
}
