# Web Interface Spec

Specification for the Loam web interface — the admin-facing surface (see the root
`README.md`). Never used by agents. Covers hosting/routing, auth, the admin API, and the
screens.

Status: **draft / in progress.** The architecture and surface below are settled;
message-level detail is filled in iteratively. Sections marked TODO are not yet specified.

## Hosting & Routing

Loam is a single self-contained binary. One HTTP listener serves three things, dispatched by
path; git transport is separate (SSH).

| Path | Handler | Auth |
| --- | --- | --- |
| `/loam.v1.*` | CLI Connect services (WorkBranch, Repo, Graph, Search, Meta) | agent identity headers (MVP: trusted) **or** admin basic auth |
| `/loam.admin.v1.*` | admin-only Connect services | admin basic auth |
| everything else (`/`, assets) | embedded static SPA (fallback to `index.html`) | admin basic auth |

The admin is a **superuser**: admin basic auth is accepted on `/loam.v1.*` too, so the web UI
reuses the CLI's work-branch services for shared operations (view, diff, comments,
request-review) rather than duplicating them. Agents (identity headers only) can reach
`/loam.v1.*` but never the admin paths.

- **Static content** is a generated single-page app, built at compile time and embedded in
  the binary via Go `go:embed`. There is no separate web server and no runtime file
  dependency; the frontend framework is an implementation detail left open here.
- **Git transport** (clone/fetch/push between agents and the server) is **not** on this
  port — it runs over SSH on its own port (`ssh://git@<host>/<group>/<repo>.git`). The CLI is
  pointed at it by `LOAM_GIT_URL`, separate from the HTTP `LOAM_SERVER_URL`. The HTTP port
  carries only RPC + web + static.

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
- **Git (SSH)**: agents authenticate to the git endpoint with an SSH key from their
  environment. In the MVP this is a **shared key granting transport access only** — it does
  not identify the agent; attribution comes from the commit author (set by `loam clone`) and
  the RPC identity headers. Per-agent git auth is deferred with authentication (see README →
  Future Work).

## Admin API

Admin-facing Connect services, package `loam.admin.v1` (defined in `proto/loam/admin/v1/`),
served under `/loam.admin.v1.*` and gated by admin basic auth. The SPA consumes them with a
connect-web client.

### RepoAdminService
Enroll and manage repos. The admin's repo view is richer than the CLI's read-only
`RepoService.GetRepo`.

- `EnrollRepo(string upstream_url, string[] target_branches) → { EnrolledRepo }` — enroll by
  upstream URL; the server derives the `<group>/<repo_name>` identifier, clones the repo, and
  begins periodic sync + ingest of the target branches. Uses the credential for the URL's
  forge host (see CredentialService).
- `ListRepos(Page) → { EnrolledRepo[], PageInfo }` — enrolled repos with status.
- `GetRepo(repo) → { EnrolledRepo }` — one repo with full status.
- `RemoveRepo(repo) → { }` — unenroll and drop the local mirror, graph, and vector data.
- `SetTargetBranches(repo, string[] target_branches) → { EnrolledRepo }` — replace the
  branches eligible as work-branch targets.
- `SetDescriptionSchema(repo, string schema) → { EnrolledRepo }` — set/replace the JSON
  Schema (as a JSON string) that validates work-branch descriptions and comment/response
  formats; empty clears it.

`EnrolledRepo` is `{ repo, upstream_url, target_branches[], default_target, SyncStatus sync,
bool has_description_schema }`. `SyncStatus` is `{ SyncState state, string last_synced_at,
string error }`, with `state` one of `idle` / `syncing` / `error`.

### CredentialService
Manage the credentials the server uses to reach each upstream forge. Credentials are keyed by
**forge host** (e.g. `github.com`, `forgejo.example.com`) and shared by all repos on that
host. Two kinds may be set per host: a REST **token** (opens upstream PRs) and an **SSH key**
(git transport to upstream) — see README's provider-interface note.

- `SetUpstreamToken(string host, string token) → { CredentialStatus }` — set/replace the
  forge token (Forgejo token / GitHub PAT); the server validates it.
- `GenerateSSHKeyPair(string host) → { string public_key }` — generate a keypair server-side
  and return the public key for the admin to install on the forge. The private key never
  leaves the server.
- `GetCredentialStatus(string host) → { CredentialStatus }` / `ListCredentials() →
  { CredentialStatus[] }` — presence and validation state per host.

`CredentialStatus` is `{ string host, bool has_token, bool has_ssh_key, bool validated }`.

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
ungated; `commit` is local.) `work.verdict` covers publishing staged new-thread comments,
outcomes, and thread resolutions; `work.reply` covers immediate replies. Loam ships built-in
defaults:

- **author** — `work.start`, `work.set`, `work.request_review`, `work.reply`, `git.clone`,
  `git.push`, `work.read`, `graph.query`, `search`; cannot submit verdicts or open review
  threads.
- **reviewer** — `work.read`, `work.reply`, `work.verdict`, `graph.query`, `search`; cannot
  start work branches or push.

### ProposalService
Only the genuinely admin-exclusive actions on work branches. Everything shared — viewing
(`GetWorkBranch`), diff (`GetWorkBranchDiff`), comments (`ListComments`), and sending a
branch back (`RequestReview`, with a comment) — is the CLI's `WorkBranchService` in
`loam.v1`, which the admin reaches as a superuser. Admin protos reuse `WorkBranch`, `Page`,
and `PageInfo` from `loam.v1` (they import `loam/v1/common.proto`).

A proposal is a **REVIEWED** work branch with ≥1 non-stale approve verdict and no upstream PR
yet.

- `ListProposals(Page) → { Proposal[] proposals, PageInfo }` — proposals awaiting an admin
  decision, across all repos, paginated. Each `Proposal` is `{ WorkBranch, VerdictSummary[]
  verdicts }` so the admin sees who approved without a second call.
- `AcceptProposal(repo, work_branch) → { string pr_url, string upstream_branch }` — creates
  the upstream PR on the forge with a generated branch name and the work branch's
  title/description, and records `pr_url` on the work branch. The work branch **stays
  REVIEWED**; it flips to COMPLETE only when the server's sync sees the upstream PR merge (or
  to CLOSED if the upstream PR is closed). Requires ≥1 non-stale approve verdict.
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
  branches); per-repo settings (target branches, description schema, credentials).
- **Credentials** — set the upstream token, or generate and show an SSH public key to
  install upstream.
- **Roles** — edit agent roles: granted operations, instructions, defaults.
- **Proposals** — the review queue: list completed work branches, view a proposed upstream
  PR (title/description/diff), accept or comment.

TODO — per-screen detail, navigation, and states.

## Open Questions

None currently open.
