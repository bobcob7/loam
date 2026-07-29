import type { Transport } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";

/**
 * The one transport the whole SPA shares (docs/web-frontend-spec.md -> Data
 * Layer: "One shared connect-web transport (baseUrl: '/', Connect
 * protocol)"). It serves BOTH proto packages: `loam.admin.v1` and, because
 * the admin is a superuser, `loam.v1` (`WorkBranchService`, on the proposal
 * detail screen) — docs/web-spec.md -> Hosting & Routing. Connect method
 * URLs are `<baseUrl><package>.<Service>/<Method>`, and the package name is
 * part of the path, so one same-origin transport reaches both groups with
 * no per-package configuration.
 *
 * `baseUrl: "/"` (not an absolute URL) is what makes it same-origin in both
 * deployments: served from the Go binary the SPA and the RPCs share an
 * origin by construction, and under `vite dev` the dev server proxies
 * `/loam.v1.*` and `/loam.admin.v1.*` to the backend (vite.config.ts), so
 * the browser still only ever sees its own origin. No CORS, no configurable
 * API host, and nothing for the SPA to configure at runtime
 * (docs/web-frontend-spec.md -> Conventions: "No client-side secrets or
 * config").
 *
 * AUTH is entirely the browser's (docs/web-frontend-spec.md -> Auth,
 * docs/web-spec.md -> Auth): every path the SPA is served from is behind
 * `httpauth.Auth.AdminOnly`, so the document request itself draws the
 * native basic-auth prompt with a `WWW-Authenticate: Basic realm="loam"`
 * challenge before any of this code runs. The browser then attaches the
 * cached `Authorization` header to same-origin subresource requests,
 * including these `fetch` calls. There is no token to store, no header to
 * set, and no login screen. `credentials: "same-origin"` below is the fetch
 * default, but connect-web does not set it at all, so stating it here is
 * what pins the behaviour this depends on rather than inheriting it — and
 * it is the line a test can hold.
 *
 * The `loam.v1` calls need no special handling either: `httpauth.Auth.CLI`
 * accepts admin basic auth ahead of the `Loam-Agent-*` identity headers and
 * marks the request context admin, which is exactly how the web UI gets
 * superuser access to the CLI services.
 *
 * Deliberately NO `defaultTimeoutMs`: `EnrollRepo` clones an upstream repo
 * and `ReindexRepo` rebuilds a repo's indexes, so any blanket deadline
 * short enough to be useful on a list call would break the slow writes.
 * Timeouts are a per-call decision for the screen that makes the call.
 */
const sameOriginFetch: typeof globalThis.fetch = (input, init) =>
  globalThis.fetch(input, { ...init, credentials: "same-origin" });

export const transport: Transport = createConnectTransport({
  baseUrl: "/",
  fetch: sameOriginFetch,
});
