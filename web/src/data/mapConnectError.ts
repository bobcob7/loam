import { Code, ConnectError } from "@connectrpc/connect";

/**
 * The five UI outcomes a Connect error can resolve to
 * (docs/web-frontend-spec.md -> Error mapping), plus a catch-all. `cause` is
 * always the normalised `ConnectError`, never discarded: `failed-precondition`
 * in particular carries typed details a screen must read itself --
 * `RemoveRepo`'s `RemovalBlocked` renders as a structured blocking-work-branch
 * panel via `cause.findDetails(RemovalBlockedSchema)`, never by parsing
 * `message` (docs/web-frontend-spec.md -> Error mapping: "never message
 * parsing"). `mapConnectError` only classifies; it never inspects details
 * itself, since it has no way to know which RPC-specific detail type (if any)
 * a given failed-precondition carries.
 */
export type ErrorOutcome =
  | { readonly kind: "auth-required"; readonly cause: ConnectError }
  | { readonly kind: "not-allowed"; readonly message: string; readonly cause: ConnectError }
  | { readonly kind: "invalid-argument"; readonly message: string; readonly cause: ConnectError }
  | { readonly kind: "failed-precondition"; readonly message: string; readonly cause: ConnectError }
  | { readonly kind: "not-found"; readonly message: string; readonly cause: ConnectError }
  | { readonly kind: "unexpected"; readonly message: string; readonly cause: ConnectError };

/** Shown when the server sent no message of its own. */
const fallbackMessages: Record<Exclude<ErrorOutcome["kind"], "auth-required">, string> = {
  "not-allowed": "You do not have permission to do this.",
  "invalid-argument": "Check the highlighted fields and try again.",
  "failed-precondition": "This action isn't possible right now.",
  "not-found": "Not found.",
  unexpected: "Something went wrong. Please try again.",
};

/**
 * Maps a Connect failure to the UI outcome that renders it
 * (docs/web-frontend-spec.md -> Error mapping): `unauthenticated` to an
 * auth-required state (the SPA has no login page -- a browser refresh
 * re-triggers the native basic-auth prompt, docs/web-frontend-spec.md ->
 * Auth); `permission_denied` to "not allowed"; `invalid_argument` to an
 * inline form error (there is no typed field-violation detail anywhere in
 * proto/ -- every InvalidArgument in this codebase is a plain wrapped error,
 * internal/handler/errors.go -- so "inline" means the caller renders this
 * next to the form rather than in the page-level `ErrorBanner`, not that a
 * specific input is singled out); `failed_precondition` to an action-specific
 * message; `not_found` to an empty/404 state; everything else to a generic
 * `ErrorBanner` message.
 *
 * Accepts `unknown`, not `ConnectError`, because a query's `error` is only
 * `ConnectError`-typed by convention (connect-query's hooks pin the type
 * parameter but nothing prevents a thrown non-Connect value from reaching
 * here). `ConnectError.from` is the same normalisation `shouldRetryQuery`
 * relies on: passed an existing `ConnectError` it returns it unchanged, so
 * this never loses information calling code already had.
 */
export function mapConnectError(error: unknown): ErrorOutcome {
  const cause = ConnectError.from(error);
  const message = (fallback: string): string => (cause.rawMessage.length > 0 ? cause.rawMessage : fallback);
  switch (cause.code) {
    case Code.Unauthenticated:
      return { kind: "auth-required", cause };
    case Code.PermissionDenied:
      return { kind: "not-allowed", message: message(fallbackMessages["not-allowed"]), cause };
    case Code.InvalidArgument:
      return { kind: "invalid-argument", message: message(fallbackMessages["invalid-argument"]), cause };
    case Code.FailedPrecondition:
      return {
        kind: "failed-precondition",
        message: message(fallbackMessages["failed-precondition"]),
        cause,
      };
    case Code.NotFound:
      return { kind: "not-found", message: message(fallbackMessages["not-found"]), cause };
    default:
      return { kind: "unexpected", message: message(fallbackMessages.unexpected), cause };
  }
}
