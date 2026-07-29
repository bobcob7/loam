/**
 * The route table's patterns and the builders that produce URLs for them.
 *
 * Every path in the app is named here rather than written as a string
 * literal at each `<Route>` and `<Link>`, so a URL shape can only change in
 * one place — and so the tests can round-trip a builder's output through the
 * matching pattern.
 *
 * ## Why `:group/:name` and not `:repo`
 *
 * docs/web-frontend-spec.md -> Routing & Screens writes these routes as
 * `/repos/:repo` and `/proposals/:repo/:workBranch`. The URLs below are
 * exactly the URLs that spec describes — `/repos/acme/widgets`,
 * `/proposals/acme/widgets/wb-9c2f1a` — but the *patterns* cannot be written
 * that way, because an enrolled repo's identifier is `<group>/<repo_name>`
 * (docs/web-spec.md -> RepoAdminService; proto/loam/v1/common.proto:
 * `// Enrolled repo identifier, "<group>/<name>"`). It contains a slash, so
 * it spans two path segments and no single-segment `:repo` parameter can
 * capture it. The two segments are rejoined by `repoFromSegments` at the one
 * place that needs to: the route table. Screens receive the identifier in
 * its wire form, `"acme/widgets"`, and never see the split.
 *
 * The alternative — percent-encoding the identifier into one segment — was
 * rejected: `%2F` survives `decodeURI` but is mangled or rejected by enough
 * proxies and servers to be a liability, and it would make the URLs in the
 * address bar unreadable for no gain.
 *
 * ## Why no encoding
 *
 * The builders interpolate raw. Both halves of a repo identifier come from
 * splitting an upstream git URL's path (docs/sync-spec.md -> "URL parsing to
 * `<group>/<repo_name>` (a path split)"), so they are already URL path
 * segments; a work branch name is server-generated as `wb-<hex>`
 * (docs/git-spec.md). Encoding here would also be *unsafe* to pair with a
 * decode on the way out: wouter runs `decodeURI` over the location before
 * matching (`wouter/src/paths.js`), so a `decodeURIComponent` on a captured
 * parameter would be a second decode, and a literal `%` in a name would
 * round-trip wrong. Raw in, raw out, decoded exactly once by wouter.
 */

/**
 * Pattern strings for `<Route path=…>`. `as const` is load-bearing: wouter
 * derives each route's parameter names from the literal type, so widening
 * these to `string` would degrade `params` to an index signature.
 */
export const routePatterns = {
  repos: "/",
  repoDetail: "/repos/:group/:name",
  credentials: "/credentials",
  roles: "/roles",
  proposals: "/proposals",
  proposalDetail: "/proposals/:group/:name/:workBranch",
  jobs: "/jobs",
} as const;

/** Rejoins the two captured segments into the `<group>/<repo_name>` identifier. */
export function repoFromSegments(group: string, name: string): string {
  return `${group}/${name}`;
}

/** URL for one enrolled repo, e.g. `repoDetailPath("acme/widgets")`. */
export function repoDetailPath(repo: string): string {
  return `/repos/${repo}`;
}

/** URL for one proposal, keyed by the work branch's `(repo, name)` identity. */
export function proposalDetailPath(repo: string, workBranch: string): string {
  return `/proposals/${repo}/${workBranch}`;
}
