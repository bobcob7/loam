import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";
import { mapConnectError } from "./mapConnectError";

// Each Connect code the spec calls out gets its own outcome `kind` -- that is
// the one thing a screen branches on -- and, where the outcome carries a
// message, both the "server sent one" and "server sent none" paths, since a
// fallback that is never reached is a fallback nobody has tested.

const connectError = (code: Code, message = ""): ConnectError => new ConnectError(message, code);

describe("mapConnectError", () => {
  it("maps Unauthenticated to an auth-required outcome with no message of its own", () => {
    const outcome = mapConnectError(connectError(Code.Unauthenticated, "invalid credentials"));
    expect(outcome.kind).toBe("auth-required");
    // auth-required is a UI *state* (docs/web-frontend-spec.md -> Auth: a
    // refresh re-triggers the browser prompt) -- it deliberately carries no
    // `message` field, so there is nothing here to source from the server.
    expect("message" in outcome).toBe(false);
  });

  it("maps PermissionDenied to a not-allowed outcome carrying the server's message", () => {
    const outcome = mapConnectError(connectError(Code.PermissionDenied, "role lacks capability X"));
    expect(outcome.kind).toBe("not-allowed");
    if (outcome.kind !== "not-allowed") throw new Error("unreachable: kind asserted above");
    expect(outcome.message).toBe("role lacks capability X");
  });

  it("falls back to a generic not-allowed message when the server sent none", () => {
    const outcome = mapConnectError(connectError(Code.PermissionDenied));
    if (outcome.kind !== "not-allowed") throw new Error("unreachable: kind asserted above");
    expect(outcome.message).toBe("You do not have permission to do this.");
  });

  it("maps InvalidArgument to an invalid-argument outcome for inline field errors", () => {
    const outcome = mapConnectError(connectError(Code.InvalidArgument, "empty upstream_url"));
    expect(outcome.kind).toBe("invalid-argument");
    if (outcome.kind !== "invalid-argument") throw new Error("unreachable: kind asserted above");
    expect(outcome.message).toBe("empty upstream_url");
  });

  it("maps FailedPrecondition to a failed-precondition outcome without flattening the cause", () => {
    const cause = connectError(Code.FailedPrecondition, "2 open work branches block removal");
    const outcome = mapConnectError(cause);
    expect(outcome.kind).toBe("failed-precondition");
    if (outcome.kind !== "failed-precondition") throw new Error("unreachable: kind asserted above");
    expect(outcome.message).toBe("2 open work branches block removal");
    // The screen for RemoveRepo reads RemovalBlocked off this directly via
    // `cause.findDetails(RemovalBlockedSchema)` -- never by parsing
    // `message` -- so the original ConnectError must survive, not just its
    // text.
    expect(outcome.cause).toBe(cause);
  });

  it("maps NotFound to a not-found outcome for an empty/404 state", () => {
    const outcome = mapConnectError(connectError(Code.NotFound, "repo acme/widgets"));
    expect(outcome.kind).toBe("not-found");
    if (outcome.kind !== "not-found") throw new Error("unreachable: kind asserted above");
    expect(outcome.message).toBe("repo acme/widgets");
  });

  it.each([
    ["a transient server fault", Code.Unavailable],
    ["an unexpected server error", Code.Internal],
    ["a client-triggered cancellation", Code.Canceled],
    ["a code with no dedicated mapping", Code.AlreadyExists],
  ])("maps %s to the unexpected/ErrorBanner outcome", (_description, code) => {
    expect(mapConnectError(connectError(code)).kind).toBe("unexpected");
  });

  it("normalises a non-ConnectError value via ConnectError.from rather than throwing", () => {
    // Everything connect-web raises is already a ConnectError
    // (queryClient.ts), so this is the defensive path for a bug elsewhere --
    // it must still resolve to a valid outcome, not throw a second error on
    // top of the first.
    const outcome = mapConnectError(new TypeError("undefined is not a function"));
    expect(outcome.kind).toBe("unexpected");
    if (outcome.kind !== "unexpected") throw new Error("unreachable: kind asserted above");
    expect(outcome.message).toBe("undefined is not a function");
  });

  it("passes an already-normalised ConnectError through unchanged as the cause", () => {
    const cause = connectError(Code.NotFound, "gone");
    const outcome = mapConnectError(cause);
    if (outcome.kind !== "not-found") throw new Error("unreachable: kind asserted above");
    expect(outcome.cause).toBe(cause);
  });
});
