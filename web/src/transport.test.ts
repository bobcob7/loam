import { createClient } from "@connectrpc/connect";
import { afterEach, describe, expect, it, vi } from "vitest";
import { RepoAdminService } from "./gen/loam/admin/v1/repo_admin_pb";
import { WorkBranchService } from "./gen/loam/v1/workbranch_pb";
import { transport } from "./transport";

/**
 * These cases drive the real transport through a real generated client and
 * inspect the `fetch` it makes. Asserting the exported object "is defined",
 * or that it has a `unary` method, would pass for any transport at any base
 * URL — the things that can actually be wrong here are the URL it targets,
 * the protocol it speaks, and whether it lets the browser attach the cached
 * basic-auth credential.
 */

/** One captured `fetch` call. */
interface Call {
  readonly url: string;
  readonly init: RequestInit;
}

/**
 * Stubs global fetch with a minimal valid Connect unary response and returns
 * the recorded calls. Connect's response validation requires HTTP 200 and
 * `content-type: application/json`; the body is the response message as JSON,
 * and `{}` is a valid instance of every message used below.
 */
const captureFetch = (): Call[] => {
  const calls: Call[] = [];
  vi.stubGlobal("fetch", (input: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ url: String(input), init: init ?? {} });
    return Promise.resolve(
      new Response("{}", { status: 200, headers: { "content-type": "application/json" } }),
    );
  });
  return calls;
};

const onlyCall = (calls: readonly Call[]): Call => {
  expect(calls).toHaveLength(1);
  const call = calls[0];
  if (call === undefined) throw new Error("unreachable: length asserted above");
  return call;
};

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("the shared transport", () => {
  it("posts an admin RPC to a root-relative, same-origin Connect path", async () => {
    const calls = captureFetch();
    await createClient(RepoAdminService, transport).listRepos({});
    const call = onlyCall(calls);
    // Root-relative, not absolute: this is what "same origin, no CORS, no
    // configurable API host" means concretely. An absolute baseUrl, or a
    // "/api" prefix, or a missing leading slash all fail here.
    expect(call.url).toBe("/loam.admin.v1.RepoAdminService/ListRepos");
    expect(call.init.method).toBe("POST");
  });

  it("reaches loam.v1 through the same transport, since the admin is a superuser", async () => {
    const calls = captureFetch();
    await createClient(WorkBranchService, transport).getWorkBranch({});
    // docs/web-frontend-spec.md -> Routing & Screens: the proposal detail
    // screen calls WorkBranchService directly. One transport must serve both
    // proto packages; a per-package baseUrl would break this call.
    expect(onlyCall(calls).url).toBe("/loam.v1.WorkBranchService/GetWorkBranch");
  });

  it("speaks the Connect protocol, not gRPC-Web", async () => {
    const calls = captureFetch();
    await createClient(RepoAdminService, transport).listRepos({});
    // createGrpcWebTransport would send application/grpc-web+proto here, and
    // would still produce identical URLs — this is the assertion that tells
    // the two apart. The Go server registers Connect handlers.
    expect(new Headers(onlyCall(calls).init.headers).get("content-type")).toBe("application/json");
  });

  it("lets the browser attach the cached basic-auth credential", async () => {
    const calls = captureFetch();
    await createClient(RepoAdminService, transport).listRepos({});
    const call = onlyCall(calls);
    // docs/web-frontend-spec.md -> Auth. connect-web does not set
    // `credentials` at all, so without the transport's fetch wrapper this is
    // undefined and the app depends on an implicit fetch default for its
    // entire auth story.
    expect(call.init.credentials).toBe("same-origin");
    // ... and the SPA itself must never construct an Authorization header:
    // "no login page, no token storage".
    expect(new Headers(call.init.headers).has("authorization")).toBe(false);
  });
});
