import type { DescMessage, DescMethodUnary, MessageInitShape, MessageShape } from "@bufbuild/protobuf";
import type { ConnectError } from "@connectrpc/connect";
import { createConnectQueryKey, useMutation, type UseMutationOptions } from "@connectrpc/connect-query";
import { useQueryClient, type UseMutationResult } from "@tanstack/react-query";

/**
 * useQuery/useMutation convention (docs/web-frontend-spec.md -> Data Layer):
 * every screen reads with connect-query's `useQuery` and writes with
 * `useMutation`, called directly against the method descriptors generated
 * into `src/gen` -- there is no wrapped read hook here, because there is
 * nothing to add: reads never invalidate anything and never take a
 * `transport` override. Neither hook is ever passed a `transport` argument,
 * here or in any screen: both resolve it from the single `TransportProvider`
 * at the app root (`src/App.tsx`, `src/transport.ts`); passing one per call
 * would risk silently forking a screen onto a second, uncached transport
 * instance, which is the exact failure `src/App.test.tsx` guards against for
 * the provider itself.
 *
 * `useMutationInvalidating` below is the one write-side addition: a mutation
 * and the reads it affects are declared together at the call site, so
 * `EnrollRepo` invalidating `ListRepos` (or `AcceptProposal` invalidating
 * `ListProposals`) is wired once, in one hook call, rather than repeated -- or
 * forgotten -- as an inline `onSuccess` on every screen that calls the
 * mutation.
 */

/**
 * One read this mutation affects, identified by its method schema alone --
 * deliberately never by `input`. Invalidating without an input matches every
 * cached variant of that query (every page of `ListRepos`, every repo/status
 * filter on `ListIngestJobs`): a write does not know which page or filter the
 * admin has on screen, so it must not narrow the invalidation to just one.
 */
export interface Invalidates {
  readonly schema: DescMethodUnary<DescMessage, DescMessage>;
}

/**
 * `useMutation` (`@connectrpc/connect-query`) plus: on success, invalidate
 * every query listed in `invalidates`. A caller-supplied `options.onSuccess`
 * still runs -- after the invalidation, so a screen reacting to the mutation
 * (closing a dialog, navigating away) sees a cache that is already stale and
 * due for a refetch, not the reverse.
 */
export function useMutationInvalidating<I extends DescMessage, O extends DescMessage, Ctx = unknown>(
  schema: DescMethodUnary<I, O>,
  invalidates: readonly Invalidates[],
  options?: UseMutationOptions<I, O, Ctx>,
): UseMutationResult<MessageShape<O>, ConnectError, MessageInitShape<I>, Ctx> {
  const queryClient = useQueryClient();
  return useMutation(schema, {
    ...options,
    onSuccess: (data, variables, onMutateResult, context) => {
      for (const target of invalidates) {
        void queryClient.invalidateQueries({
          queryKey: createConnectQueryKey({ schema: target.schema, cardinality: undefined }),
        });
      }
      return options?.onSuccess?.(data, variables, onMutateResult, context);
    },
  });
}
