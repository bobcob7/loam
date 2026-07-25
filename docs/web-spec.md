# Web Interface Spec

Specification for the Loam web interface — the admin-facing surface (see the root
`README.md`). Never used by agents. Covers hosting/routing, auth, the admin API, and the
screens.

Status: **draft / in progress.** The architecture and surface below are settled;
message-level detail is filled in iteratively. Sections marked TODO are not yet specified.

## Hosting & Routing

Loam is a single self-contained binary. One HTTP listener serves four things, dispatched by
path.

| Path | Handler | Auth |
| --- | --- | --- |
| `/loam.v1.*` | CLI Connect services (WorkBranch, Repo, Graph, Search, Meta) | agent identity headers (MVP: trusted) **or** admin basic auth |
| `/loam.admin.v1.*` | admin-only Connect services | admin basic auth |
| `/git/*` | git smart HTTP — clone/fetch/push (`docs/git-spec.md`) | agent identity headers **only** (MVP: trusted) |
| everything else (`/`, assets) | embedded static SPA (fallback to `index.html`) | admin basic auth |

The admin is a **superuser**: admin basic auth is accepted on `/loam.v1.*` too, so the web UI
reuses the CLI's work-branch services for shared operations (view, diff, comments,
request-review) rather than duplicating them. Agents (identity headers only) can reach
`/loam.v1.*` but never the admin paths.

- **Static content** is a generated single-page app, built at compile time and embedded in
  the binary via Go `go:embed`. There is no separate web server and no runtime file
  dependency; the frontend framework is an implementation detail left open here.
- **Git transport** (clone/fetch/push between agents and the server) runs on this same
  port as git smart HTTP under `/git/<group>/<repo>.git` — there is no separate git port
  or URL; the CLI composes the git URL from `LOAM_SERVER_URL`. See `docs/git-spec.md`.

## Auth

- **Admin** (web + `/loam.admin.v1.*`): HTTP basic auth. The admin username and password are
  provided on server startup (see README → Web Interface → Auth). A single admin credential
  is sufficient for the MVP. Basic-auth middleware wraps the admin RPC paths and all static
  content.
- **Agents** (`/loam.v1.*`): identity via `LOAM_AGENT_*` headers, trusted in the MVP (no
  authentication). These paths accept agent identity headers; they also accept admin basic
  auth so the web UI (a superuser) can call them.
- The two regimes are split purely by path prefix: the web is unreachable to an agent that
  only carries identity headers, and the CLI API never prompts for basic auth.
- **Git (`/git/*`)**: the same trusted agent identity headers as `/loam.v1.*`, written
  into each clone's config by `loam clone` so plain git carries them; the server's ref
  policy authorizes pushes at receive time (`docs/git-spec.md`). Admin basic auth is
  **not** accepted here — the admin administers the process, not the code. Verified
  per-agent git credentials (a standard credential helper) are deferred with
  authentication (see README → Future Work).

## Admin API

Admin-facing Connect services, package `loam.admin.v1` (defined in `proto/loam/admin/v1/`),
served under `/loam.admin.v1.*` and gated by admin basic auth. The SPA consumes them with a
connect-web client.

### RepoAdminService
Enroll and manage repos. The admin's repo view is richer than the CLI's read-only
`RepoService.GetRepo`.

- `EnrollRepo(string upstream_url, string[] target_branches, string indexed_branch) →
  { EnrolledRepo }` — enroll by upstream URL; the server derives the `<group>/<repo_name>`
  identifier, clones the repo, and begins periodic sync + ingest of the indexed branch.
  `indexed_branch` must be one of `target_branches`; the enrollment form pre-fills it from
  upstream's `HEAD` (free from the `ls-remote` access probe). Uses the credential for the
  URL's forge host (see CredentialService).
- `ListRepos(Page) → { EnrolledRepo[], PageInfo }` — enrolled repos with status.
- `GetRepo(repo) → { EnrolledRepo }` — one repo with full status.
- `RemoveRepo(repo) → { }` — unenroll and drop the local mirror, graph, and vector data.
- `SetTargetBranches(repo, string[] target_branches, string indexed_branch) →
  { EnrolledRepo }` — replace the branches eligible as work-branch targets and designate
  which one is indexed. Changing `indexed_branch` triggers a full ingest of the new
  branch. Removing a target branch only affects *eligibility* — existing work branches
  keep their recorded target and full lifecycle.
- `ReindexRepo(repo) → { IngestJob }` — force a full rebuild of the repo's derived indexes.
- `ListIngestJobs(Page, optional string repo, optional string status) → { IngestJob[], PageInfo }`
  — recent and active ingest jobs across repos, for the Jobs view (see `docs/ingestion-spec.md`).

`EnrolledRepo` is `{ repo, upstream_url, target_branches[], indexed_branch, SyncStatus sync,
string ingested_ref }` — `ingested_ref` is the indexed branch's last ingested commit,
empty until first ingest. `SyncStatus` is `{ SyncState state, string last_synced_at,
string error }`, with `state` one of `idle` / `syncing` / `error`. `IngestJob` is
`{ repo, target_branch, kind, status, attempts, error, queued_at, started_at, finished_at }`.

### CredentialService
Manage the credentials the server uses to reach each upstream forge. Credentials are keyed by
**forge host** (e.g. `github.com`, `forgejo.example.com`) and shared by all repos on that
host. One credential per host: a **token** (Forgejo token / GitHub PAT) covering both the
REST calls that open upstream PRs and git-over-HTTPS transport to the upstream (see
`docs/sync-spec.md` → Upstream Transport).

- `SetUpstreamToken(string host, string token) → { CredentialStatus }` — set/replace the
  forge token; the server validates the REST side immediately (git access is proven per
  repo at enrollment).
- `GetCredentialStatus(string host) → { CredentialStatus }` / `ListCredentials() →
  { CredentialStatus[] }` — presence and validation state per host.

`CredentialStatus` is `{ string host, bool has_token, bool validated }`.

### RoleService
Configure agent roles (see README → Agent Identity & Roles). A role grants a set of
operations and carries the instruction text returned by `MetaService.GetInstructions`.

- `ListRoles() → { Role[] }` / `GetRole(name) → { Role }`
- `CreateRole(Role) → { Role }` / `UpdateRole(Role) → { Role }` — set the granted operations
  and instructions.
- `DeleteRole(name) → { }` — built-in roles cannot be deleted.

`Role` is `{ string name, string[] operations, string instructions, bool builtin }`.
Operations are a fixed capability vocabulary at the command-group level: `work.start`,
`work.set`, `work.request_review`, `work.reply`, `work.verdict`, `work.read`, `git.clone`,
`git.push`, `graph.query`, `search`. (`instructions` and `whoami` are always available and
ungated.) `work.verdict` covers publishing staged new-thread comments,
outcomes, and thread resolutions; `work.reply` covers immediate replies. Loam ships built-in
defaults:

- **author** — `work.start`, `work.set`, `work.request_review`, `work.reply`, `git.clone`,
  `git.push`, `work.read`, `graph.query`, `search`; cannot submit verdicts or open review
  threads.
- **reviewer** — `work.read`, `work.reply`, `work.verdict`, `git.clone`, `graph.query`,
  `search`; cannot start work branches or push. Clone access lets a reviewer run their own
  tools against the branch under review (see `docs/git-spec.md`).

### ProposalService
Only the genuinely admin-exclusive actions on work branches. Everything shared — viewing
(`GetWorkBranch`), diff (`GetWorkBranchDiff`), comments (`ListComments`), and sending a
branch back (`RequestReview`, with a comment) — is the CLI's `WorkBranchService` in
`loam.v1`, which the admin reaches as a superuser. Admin protos reuse `WorkBranch`, `Page`,
and `PageInfo` from `loam.v1` (they import `loam/v1/common.proto`).

A proposal is a **REVIEWED** work branch with ≥1 non-stale approve verdict awaiting an
admin decision — either it has no upstream PR yet, or its existing PR's branch is behind
the work branch (a conflict catch-up that has been re-reviewed; see `docs/git-spec.md` →
Target Advances & Catch-Up).

- `ListProposals(Page) → { Proposal[] proposals, PageInfo }` — proposals awaiting an admin
  decision, across all repos, paginated. Each `Proposal` is `{ WorkBranch, VerdictSummary[]
  verdicts }` so the admin sees who approved without a second call.
- `AcceptProposal(repo, work_branch) → { string pr_url, string upstream_branch }` — creates
  the upstream PR on the forge with a generated branch name and the work branch's
  title/description, and records `pr_url` on the work branch. On a re-accept after a
  conflict catch-up, the existing PR's branch is fast-forwarded instead — the PR updates
  in place and no new PR is created. The work branch **stays REVIEWED**; it flips to COMPLETE only when the server's sync sees the upstream PR merge (or
  to CLOSED if the upstream PR is closed). Requires ≥1 non-stale approve verdict and no
  outstanding conflict flag — a branch behind a conflicting target must be caught up and
  re-reviewed first (`docs/sync-spec.md` → Mergeability Check).
- `CloseWorkBranch(repo, work_branch, string body) → { WorkBranch }` — closes a work branch
  (→ CLOSED). Admin-only; the server also closes a work branch on sync when its upstream PR
  is closed.

To send a reviewed branch back for another round, the admin calls
`WorkBranchService.RequestReview` with a comment — the same operation an author uses — which
returns it to REVIEWABLE, marking the prior round's verdicts stale.

`VerdictSummary` (defined in `loam.v1`, also returned by `WorkBranchService.ListVerdicts`) is
`{ reviewer, outcome, stale }` — each unique reviewer's recorded `SubmitVerdict`; a verdict
becomes `stale` once a later review is requested.

## Screens

The SPA the admin uses. All behind basic auth. The SPA's front-end architecture (stack,
codegen, routing, build/embed) is designed in [`docs/web-frontend-spec.md`](web-frontend-spec.md).

- **Login** — the browser's basic-auth prompt; no dedicated page.
- **Repos** — list enrolled repos with sync status; enroll form (upstream URL + target
  branches); per-repo settings (target branches, credentials).
- **Credentials** — set and validate the upstream token per forge host.
- **Roles** — edit agent roles: granted operations, instructions, defaults.
- **Proposals** — the queue of reviewed work branches: view one (title/description/diff/
  verdicts), then accept (create the upstream PR), request a re-review, or close.
- **Jobs** — ingest job activity across repos (queued/running/succeeded/failed, timings,
  errors), and a per-repo reindex action.

TODO — per-screen detail, navigation, and states.

## Open Questions

None currently open.
