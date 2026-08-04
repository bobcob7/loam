# Upstream Sync Spec

Everything between the Loam server and the upstream forge: mirror sync (poll,
upstream-wins), the mergeability check of work branches against target advances
(whose agent-facing behavior is specified in `docs/git-spec.md` → Target
Advances & Catch-Up), proposal acceptance (pushing the branch upstream and
opening the PR), PR state tracking, and the **provider interface** that
abstracts the forge. Agent↔server transport is out of scope
(`docs/git-spec.md`).

Status: **settled for the MVP.** Upstream transport is HTTPS with the forge
token (no SSH anywhere in Loam), the server never authors commits, and the PR
footer is configurable. `docs/persistence-spec.md`, `docs/web-spec.md`, and the
`features/` files are aligned with this document.

## Provider Interface

The forge-specific surface is deliberately small. Git transport to the upstream
is forge-agnostic; only the REST calls differ per forge. **Two providers exist:
Forgejo and GitHub** (`internal/forge/forgejo.go`, `internal/forge/github.go`).
A repo's forge is resolved from its host, never configured or guessed
per-request — see "Selecting a provider" below.

Operations (Go interface, one implementation per forge —
`internal/forge.Provider`):

- `ValidateToken(host, token) → error` — backs `CredentialService.SetUpstreamToken`;
  confirms the token works and has the scopes needed to open PRs.
- `CheckRepo(upstream_url) → error` — backs `EnrollRepo`; confirms the repo exists
  and is accessible with the host's credential before cloning.
- `CreatePR(repo, head_branch, target_branch, title, description) →
  { pr_url, pr_number }` — opens the upstream PR.
- `GetPRState(repo, pr_number) → open | merged | closed` — backs completion and
  closure detection.
- `ClosePR(repo, pr_number) → error` — backs the admin's `CloseWorkBranch` on a
  branch with an open PR (best-effort; Loam opened the PR, Loam closes it).
- `GitCredentials(token) → { username, password }` — the forge's convention for
  token-authenticated HTTPS git.
- `FindOpenPR(repo, head_branch, target_branch) → { pr_url, pr_number, found }` —
  looks up (by listing and filtering, never by parsing a rejection message) the
  PR a duplicate-PR rejection from `CreatePR` reported as already existing.

Everything else — URL parsing to `<group>/<repo_name>` (a path split, uniform
across forges), git fetch/push to the upstream, branch naming — lives in the
core, outside the interface.

### Selecting a provider

`internal/forge.KindForHost(host)` maps a host to a `Kind` (`KindForgejo` or
`KindGitHub`), and `NewProvider(host, token, ...)` builds the matching
implementation. The rule: an **exact** match on `github.com` or its REST-API
alias `api.github.com` is GitHub; everything else is Forgejo — self-hosted
forges live at arbitrary domains, so there is no positive signal to require
before trusting one, and requiring one would break every already-enrolled
repo. A host **containing the substring "github"** but not one of the two
exact aliases is rejected outright, naming the host, rather than silently
treated as Forgejo — this is a narrow heuristic aimed at vendor-named GitHub
Enterprise Server hosts, not general GHE detection; see "Limits" below for
exactly what it does and does not catch.

Two call shapes reach this seam:

- **Repo-bound** (a `repos` row exists): the repo's `forge_host` resolves the
  Kind, e.g. the PR poller and proposal acceptance
  (`cmd/server/sync.go`'s `forgePRTracker`).
- **Pre-repo** (no `repos` row yet): `CredentialService.SetUpstreamToken`
  (validating a candidate token before it is stored) and
  `RepoAdminService.EnrollRepo`'s upstream check both take a host explicitly
  and resolve through the same function — there is no separate code path for
  "before enrollment."

`internal/forgehost.Canonicalize` folds `api.github.com` to `github.com`
*before* either call shape sees the host, so a credential entered under
either spelling is the same `credentials.host` row a repo enrolled against
either spelling would look up.

### What a third provider would have to supply

Everything `internal/forge.Provider` lists above, plus:

- A `Kind` constant and a case in `KindForHost`/`NewProvider` (`resolve.go`) —
  by exact host match, the same discipline GitHub's alias handling follows,
  not a substring guess that could silently steal a Forgejo host.
- Its own base-URL derivation (see below — this is the first thing that
  differs, and the property that decides how a test double for it gets
  addressed).
- Sentinel mapping for every failure `internal/forge/errors.go` names:
  `ErrInvalidToken`, `ErrInsufficientScope`, `ErrRepoNotFound`,
  `ErrNoWriteAccess`, `ErrDuplicatePR`, `ErrPRAlreadyMerged`. The wire shape
  each maps from is provider-specific (see below); the sentinels themselves
  are not negotiable.
- A test double bounded to exactly what `Provider`'s seven methods call
  (`internal/fakeforge`'s existing two dialects, `forgejoapi.go` and
  `githubapi.go`, are the pattern: reuse the shared state — repos on disk, the
  PR registry, the token registry — and add only the new wire encoding), and
  an `internal/forgesuite.Harness` implementation so the *same* contract
  assertions run against it unchanged.
- A decision on rate limiting (see below) and on which token kind(s) it
  supports, recorded the way GitHub's provider records classic-PAT-only.

### Differences between Forgejo and GitHub that cost real time to discover

These are exactly the traps `loam-tmds`'s own planning notes warned about
before either provider existed; recorded here because each would otherwise be
rediscovered per provider, once.

- **Base URL derivation.** Forgejo's REST API lives at `<host>/api/v1` — the
  same host the repo's git URL uses, just with a path suffix, so a Forgejo
  `Provider` is bound to whatever host `repos.forge_host` says. GitHub's REST
  API is the fixed `https://api.github.com`, independent of the fact that
  every GitHub repo's git/web host is `github.com` — a *different* host
  entirely, not a path suffix on the same one. GitHub Enterprise Server adds a
  third shape, `https://<host>/api/v3`, which this implementation does not
  derive at all (see "Limits" below).
- **Duplicate-PR status code.** A second `CreatePR` for a head/target pair
  that already has an open PR: Forgejo answers `409 Conflict` with a message
  embedding the existing PR's *internal* id (not the per-repo number
  `FindOpenPR` returns — never parse it). GitHub answers `422 Unprocessable
  Entity` with a structured `errors[]` array; this implementation matches on
  the message text "pull request already exists" rather than a specific
  `errors[].code`, because GitHub's own troubleshooting docs document the
  `errors[].code` vocabulary in general but not which value this specific
  case uses — flagged in `github.go`, not guessed.
- **"Merged" is a Loam-level state; both wire formats only carry a boolean.**
  `GetPRState` reports Loam's own three-value `open`/`merged`/`closed`, but
  *neither* forge's actual pull-request object has a three-valued state field
  — both Forgejo's and GitHub's carry a `state` that is only `open`/`closed`,
  plus a separate `merged` boolean. Both providers therefore do the identical
  fold on read (`state=="closed" && merged` → `"merged"`), and getting that
  fold wrong — reporting a merged PR as merely closed — is the single most
  damaging thing either implementation of this method could get wrong, since
  it would make Loam treat a merged proposal as abandoned. This is not a
  place the two providers actually diverge; it is listed here because a
  third provider's author, seeing "open/merged/closed" in the interface doc,
  could otherwise assume their forge exposes that three-way split natively
  and skip writing the fold.
- **Rate limiting is real for one, not the other.** GitHub enforces primary
  rate limits (`403`/`429` with `x-ratelimit-remaining: 0`, honoring
  `Retry-After`/`x-ratelimit-reset`) and secondary rate limits (`403`/`429`
  with no guaranteed header, only a message). `github.go`'s
  `githubRateLimitError` classifies both *before* the generic 401/403 fold,
  specifically so a throttled request can never present as an invalid
  credential. Self-hosted Forgejo does not meaningfully rate-limit, and its
  provider does nothing analogous — a polling cadence harmless against
  Forgejo can get a GitHub-backed account throttled.
- **Token kinds and git-over-HTTPS conventions.** Forgejo issues one token
  kind. GitHub has three usable for git operations — classic PATs,
  fine-grained PATs, and GitHub App installation tokens — with different scope
  models and different git-over-HTTPS username conventions. This
  implementation supports **classic PATs only** (see "Limits" below); its
  git-over-HTTPS convention happens to coincide with Forgejo's (any username,
  token as password — verified against GitHub's own docs), which is why
  `internal/gittransport`'s shared, host-agnostic credential converter needs
  no per-Kind branch today. That would stop being true for an App
  installation token, which conventionally uses `x-access-token` as the
  username.

### Limits (operator-facing)

An operator should learn these from this document, not from a failure:

- **GitHub Enterprise Server is not supported, and detection of it is a
  heuristic, not a guarantee.** A host containing the substring "github" but
  not matching `github.com`/`api.github.com` exactly is refused at
  credential/enrollment time with an explicit error naming the host, rather
  than silently treated as Forgejo. **This only catches GHE installs an
  operator happened to name after the vendor** (`github.example.com`,
  `github-internal.acme.com`). GitHub Enterprise Server installs at whatever
  hostname the customer chooses, and most don't mention GitHub at all —
  `git.acme.com`, `source.corp.io`, `scm.example.net` are all ordinary GHE
  hostnames, and every one of them resolves silently to Forgejo, unmitigated
  (`internal/forge.TestKindForHost_NonGitHubNamedEnterpriseHostsResolveToForgejoUnmitigated`
  demonstrates this directly). If you run GitHub Enterprise Server, do not
  rely on this check to catch a misconfiguration — Loam has no way to tell
  your GHE host apart from a self-hosted Forgejo at the same shape of
  hostname, and will send your token to a Forgejo-shaped API URL that fails
  in a way that looks like a bad credential, not "unsupported forge."
  **This is also a narrow, deliberate behaviour change**: a self-hosted
  Forgejo whose own hostname happens to contain "github" (e.g.
  `github-mirror.internal.corp`) previously resolved fine and now fails to
  resolve at all, refused by the same heuristic — see `KindForHost`'s own
  doc comment (`internal/forge/resolve.go`) for why that tradeoff was
  accepted rather than avoided.
- **Only classic personal-access tokens are supported for GitHub.**
  Fine-grained PATs and GitHub App installation tokens are not — see
  `github.go`'s own doc comment for the reasoning (no generic scope-listing
  header for fine-grained PATs; a different lifecycle and git convention for
  App tokens).
- **`ValidateToken` requires GitHub's `repo` scope unconditionally.** A token
  scoped only to `public_repo` authenticates but is rejected, because
  `ValidateToken` has no way to know in advance whether the repos it will be
  used against are private.
- **GitHub rate limits are honored defensively, not adaptively.** This
  provider classifies and reports rate-limit rejections distinctly (see
  above) but does not itself back off or retry; a caller polling on a fixed
  interval against a GitHub-backed repo should budget for GitHub's documented
  limits.

## Upstream Transport

All git traffic to the forge — mirror fetch, proposal branch push, branch
deletion — runs over **HTTPS, authenticated with the same token that backs the
REST calls**. There is no upstream SSH: one credential per forge host covers
everything Loam does against it, mirroring the agent-side decision in
`docs/git-spec.md`.

- The token is **injected per git invocation** from the decrypted credential
  (askpass-style); it is never written into the mirror's git config or
  anywhere on disk.
- A token's REST scopes don't prove its git scopes, so validation is
  **operational**: `ValidateToken` confirms the REST side when the credential
  is set, and `CheckRepo` at enrollment proves git access against the actual
  repo — an authenticated `ls-remote` for read, a receive-pack probe (dry-run
  push) for write — before the clone starts. A token that can open PRs but not
  push fails at enrollment, not at first accept.

The provider needs the PR **number** to poll state, so `work_branches` carries
`upstream_pr_number` alongside the display-only `upstream_pr_url`
(`docs/persistence-spec.md`).

## Mirror Sync

Each enrolled repo is polled on a fixed interval (`LOAM_SYNC_INTERVAL`,
server-wide, default `60s`; see `docs/server-spec.md`). Syncs are
**serialized per repo** — the same pattern as
ingest jobs — and one sync cycle runs, in order:

1. **Fetch** upstream branches and tags into the mirror, forced, with pruning —
   upstream-wins, always: a diverged or force-pushed upstream ref simply
   replaces the mirror's copy, and a branch deleted upstream is pruned.
   Nothing outside those two namespaces is fetched — no `refs/pull/*`,
   `refs/notes/*`, `refs/replace/*`, or any other upstream ref — since nothing
   in Loam reads them, and `refs/replace/*` would silently alter object
   visibility if it were (`loam-5f3`).
   **Registered work-branch refs are excluded from the fetch refspec** — they
   are Loam's own refs and must never be clobbered even if upstream grows a
   branch with a colliding name.
2. **Detect advances** by comparing SHAs before and after the fetch — for every
   listed target branch, plus any branch that is the recorded target of an open
   work branch, so conflict detection keeps running for targets de-listed while
   work was in flight.
3. **Mergeability check** (below) for every advanced target.
4. **Enqueue ingest** for advanced indexed branches (`docs/ingestion-spec.md`).
5. **Poll PR states** for work branches with an open recorded PR (below).
6. **Reconcile upstream drift** on `loam/<work-branch>` (below) for every work
   branch with a recorded PR and a recorded `accepted_tip`. It runs *after*
   step 5 so a PR that merged this tick has already taken its branch terminal,
   and out of this step's set, before its upstream branch is reaped.

`repos.sync_state` reflects the cycle: `syncing` while running, `idle` on
success (with `last_synced_at`), `error` with the message on failure. A failed
cycle is retried on the next tick — the poll interval is the backoff. If a
**target branch disappears upstream**, the repo goes to `sync_state = error`
naming the branch; the admin resolves it via `SetTargetBranches`. Work branches
targeting the missing branch are left untouched in the meantime.

Enrollment is the degenerate first cycle: `EnrollRepo` records the repo, and the
initial bare-mirror clone runs as its first sync (`syncing` until it completes,
`error` if the clone fails).

## Mergeability Check

The server is a **broker and a store — it never authors commits or contributes
code**. Work-branch refs advance by agent pushes, and by nothing else here — the one
exception in the whole system is drift adoption, below. On a target advance, the
server *tests* each **open (non-terminal) work branch targeting that branch**
against the new tip with `git merge-tree` — no worktree, no writes to any ref:

- **Merges cleanly** → nothing happens. The branch is behind its target but
  mergeable — a normal state; the *forge* performs the actual merge when the
  accepted PR merges. An agent that wants the target merged in does it with
  plain git (`git fetch origin <target>` + merge) on its own schedule.
- **Conflict** → the branch is marked `conflicted`; if it was `reviewable` or
  `reviewed` it is reset to `draft` with `conflict_reset` recorded and its
  verdicts marked stale. A branch already in `draft` is only flagged.

`conflicted` and `conflict_reset` are distinct for a reason: an ordinary draft
that hits a conflict just clears its flag when caught up and **stays draft**;
only a branch that was *demoted from review* auto-returns to `reviewable`.
(Persisted as `work_branches.conflict`: `none`/`flagged`/`reset` —
`docs/persistence-spec.md`.)

**Catch-up detection** runs on every accepted push to a flagged branch: if the
pushed history now contains the current target tip, `conflicted` clears, and a
`conflict_reset` branch flips to `reviewable` (git-spec → Target Advances &
Catch-Up) — opening a fresh review round with `requested_by` = the server
(`docs/persistence-spec.md` → review_rounds). If the target has advanced again since the reset, the flag simply
stays until a push catches up to the newer tip.

## Proposal Acceptance

`AcceptProposal(repo, work_branch)` — preconditions: state `reviewed`, ≥1
non-stale approve verdict, **not `conflicted`** (web-spec ripple: the
precondition list gains the conflict check), and **no `upstream_drift`**
(below). Then:

1. **Push** the work-branch tip to the upstream branch `loam/<work-branch-name>`
   (e.g. `loam/wb-9c2f1a`) over the upstream transport. The prefix namespaces
   Loam's branches on the forge and makes them recognizable. First accept
   creates the branch; a re-accept after a catch-up is a fast-forward push —
   history only ever gains commits, so force is never needed.
2. **Open the PR** via the provider (`CreatePR`) with the work branch's title
   and description, based against the target branch; record `upstream_pr_url`
   and `upstream_pr_number`. On a re-accept with an open recorded PR, this step
   is skipped — the branch push in step 1 already updated the PR in place.

**PR body**: the work branch's description verbatim, followed by an
**attribution footer** identifying Loam as the origin — and nothing else;
agent attribution is already carried by the commit authors in git history:

```
---
Proposed via Loam.
```

The footer is on by default and disabled with server config
(`LOAM_PR_ATTRIBUTION=false`; see `docs/server-spec.md`). When disabled, the
body is the description alone.

The RPC is synchronous and **idempotent by construction**: the push is
fast-forward-or-noop, and step 2 is skipped whenever a PR is already recorded —
so a failure between the steps (branch pushed, PR creation failed) is retried
safely by the admin re-running accept. Errors surface on the RPC for the
proposal screen.

## Upstream Drift on `loam/<work-branch>`

Loam owns the `loam/…` branches it pushes, but it does not control them: the
forge lets anyone with write access push to `loam/<work-branch-name>`
directly, and that has happened in practice. The branch is mirrored back by
the ordinary fetch (`branchesRefspec` is `+refs/heads/*:refs/heads/*`; only
the reserved namespace is excluded), so the divergence is already visible in
the mirror — what follows is the reconciliation.

Each sync cycle, for every work branch with a recorded PR, compare the
mirrored `loam/<name>` tip against `accepted_tip`. Equal is the normal case
and does nothing. Otherwise, classify by ancestry against the work branch's
own tip:

- **Fast-forward** — the work-branch tip is an ancestor of the upstream tip.
  Loam **adopts** it: the work branch advances to the upstream commit, and a
  **new review round opens**, which makes every prior verdict stale (staleness
  is derived from the round number, so no verdict is rewritten). `requested_by`
  is `server`, the same attribution a catch-up round uses. `accepted_tip`
  becomes the adopted commit.

  A `reviewed` branch also returns to `reviewable`, because the round alone
  would leave it invisible: a reviewer's queue is "reviewable branches with no
  live verdict from me", so a branch left in `reviewed` would carry a state
  meaning *decided* while awaiting a decision nobody could see was needed. A
  `reviewable` branch needs only the round. A `draft` branch gets neither — it
  has no live review to invalidate and cannot be accepted from `draft` anyway,
  and `request-review` opens its own round when it is ready (the same rule
  catch-up detection applies, `docs/git-spec.md` → Target Advances & Catch-Up).

  The work-branch ref is moved as a **compare-and-swap** against the tip Loam
  read: an agent push landing mid-cycle is never overwritten, it simply
  refuses the swap and the next cycle re-derives everything.

  Adopting is not blessing. The commit arrived without review, and resetting
  the approvals is what keeps the gate honest: it is now in the work branch,
  and it cannot reach a *further* upstream push until someone approves it.

- **Diverged** — the work-branch tip is **not** an ancestor of the upstream
  tip. Loam changes nothing, and records `upstream_drift = diverged` for the
  admin console (`docs/web-spec.md` → ProposalService). Loam does not merge,
  rebase, or force: it cannot know which side is intended, and the push that
  would reconcile it is precisely the destructive one Proposal Acceptance
  refuses to make.

  That covers two shapes, not one. The obvious shape is genuine divergence —
  neither tip contains the other, because the branch and its upstream copy each
  gained different commits. The other is a **rewind**: someone force-pushed
  `loam/<name>` *back*, so the upstream tip is a strict ancestor of the work
  branch. That is not a fast-forward (adopting it would move the work branch
  backwards, discarding reviewed commits) and it is not nothing (`accepted_tip`
  would permanently misdescribe upstream, and `ListProposals` compares
  `accepted_tip` against the *work branch*, never against upstream, so nothing
  would ever re-list the branch and put the dropped commits back). Both are
  "someone rewrote the branch Loam pushed", which is what `diverged` means.

An upstream branch that is **absent** from the mirror is none of the three: it
is skipped, changing neither `accepted_tip` nor `upstream_drift`. A forge
configured to delete a merged PR's head branch removes it seconds before the
poller flips the branch to `complete`, so absence is routine and carries no
third SHA to classify against.

**Clearing is level-triggered, unlike `conflict`.** A conflict is cleared by a
push, which Loam sees; upstream drift is fixed on the *forge* — force-push
`loam/<name>` back, or merge the work branch's commits into it — which Loam
never sees. So each cycle re-derives the value and writes whichever it
observes, `none` included: a diverged branch recovers on the next tick with no
command to run, and a divergence the operator resolves by merging the work
branch in is adopted (which clears the flag) rather than merely unflagged.
There is deliberately no "clear drift" RPC.

**None of it is transactional across git and Postgres**, so an adoption writes
in the order **review round → ref → `accepted_tip`**, the last of which commits
the reconciliation.

The round is first because it is the only one whose absence is unrecoverable.
Move the ref first and crash before the round, and the branch holds the adopted,
unreviewed commit while its pre-adoption approvals are still live and
`upstream_drift` is still `none` — so an `AcceptProposal` arriving in that window
passes, pushes, and writes `accepted_tip` itself, after which the next cycle sees
upstream equal to `accepted_tip` and never opens the round at all. The commit is
not un-reset for one cycle; it is permanently blessed, and the gate this whole
mechanism exists to keep honest has been bypassed silently. Round-first makes the
window harmless: approvals are already stale, the accept is refused, and
everything after the round is retried next cycle (the work-branch tip now equals
the upstream tip, and a commit is its own ancestor, so the classification is
still *fast-forward*).

The cost, stated rather than buried: the reset can fire for an adoption that then
does not happen — a compare-and-swap lost to an agent push, or a failed ref
advance, sends a branch back for a re-review it would not otherwise have had,
since an ordinary agent push does not reset approvals. A repeatedly failing
adoption likewise opens a round per cycle, each after the first semantically a
no-op against already-stale verdicts, with the repo in `sync_state = error`
throughout. Both costs are re-review; the cost they replace is an unreviewed
commit reaching upstream with approvals that were never re-earned. A duplicated
round is the cheap error; a skipped one is not.

Within the round step the same rule applies again: the **round is opened before**
the `reviewed → reviewable` move, and a failure of that move is logged rather
than propagated. The round *is* the reset (only current-round verdicts count
toward the accept bar); the state move is reviewer visibility. `UpdateState`'s
`reviewed → reviewable` arm requires a non-empty title and description, so
ordering it first would let a branch that cannot satisfy that guard suppress its
own reset indefinitely.

`upstream_drift` is its **own column**, deliberately not a fourth `conflict`
value. The two describe independent facts that can hold simultaneously — a
target can advance into a merge conflict while the `loam/` branch is
separately rewritten — and a single field would let whichever happened second
overwrite the first, leaving the operator to fix one problem while never
learning about the other. They also want different sentences: `flagged`/`reset`
mean *the target moved, catch up*; `diverged` means *someone rewrote the branch
Loam pushed*.

**`AcceptProposal` must refuse on either field.** Its existing precondition
rejects a non-`none` `conflict`; it gains an equivalent check on
`upstream_drift`, with its own message. A branch that is diverged upstream
cannot be accepted, because the push that acceptance performs is exactly the
non-forced push that would fail.

**Both fields are on the wire.** They were not before this feature:
`conflict` was a server-internal value (`workbranchstore.Conflict`, backed by
the column's CHECK constraint) consumed only by `ListProposals`' exclusion and
`AcceptProposal`'s precondition, so a branch Loam had demoted looked, to every
client, exactly like one it had not. `WorkBranch.conflict` and
`WorkBranch.upstream_drift` (`loam/v1/common.proto`) now travel with every work
branch any surface returns, read-only — nothing in any request message sets
either. Surfacing drift to the console required that, so it was part of this
feature rather than an assumed given.

**Why adoption is safe to automate and divergence is not.** A fast-forward
loses nothing — the work branch gains commits and history is preserved. A
diverged pair can only be reconciled by discarding work on one side or
writing a merge nobody reviewed, both of which are decisions, not mechanics.

**A note that belongs beside the implementation.** An adopted commit reached
the mirror through the forge, not through `/git/*`, so it never passed the
pre-receive hook and none of the push policy applied to it — not the author
check, not the reserved-namespace guard, not force-push rejection
(`docs/git-spec.md` → Enforcement Mechanics). This is the one path by which
Loam takes in code it did not gate, and it is defensible **only** because the
approvals reset. Anything that weakens the reset breaks that argument.

Without this reconciliation the failure is deferred rather than avoided: the
next accept attempts a non-forced push to a branch that has moved, and the
operator meets a non-fast-forward rejection several layers from its cause.

## PR State Tracking

Each sync cycle polls `GetPRState` for every work branch with a recorded,
still-open PR (a handful of REST calls at most):

- **merged** → the work branch flips to `complete`. The merge itself lands in
  the mirror as an ordinary target advance on a subsequent fetch, triggering
  ingest — completion and ingestion stay independent.
- **closed** without merging → the work branch flips to `closed`.
- On either terminal state, the server **best-effort deletes** the upstream
  `loam/…` branch (a push of an empty ref); failures are ignored — forges with
  auto-delete-on-merge make this a no-op.
- The reverse direction also holds: the admin's `CloseWorkBranch` on a branch
  with an open PR closes the PR via `ClosePR` (best-effort), and the branch
  cleanup above follows.

## Future Work

- **Webhooks.** Replace or supplement polling with forge webhooks for near-instant
  advance and PR-state detection; polling remains the fallback.
- **Per-repo sync intervals.** A single server-wide interval is enough for the
  MVP.

## Open Questions

None currently open. Two were settled when this section was written:

- **Where upstream drift is recorded** — its own `upstream_drift` column, not
  a fourth `conflict` value, so that a target-advance conflict and an upstream
  rewrite can both be true without one erasing the other.
- **What `accepted_tip` holds after an adoption** — the adopted commit. It
  reads "the upstream tip Loam last pushed *or* adopted", which adds no new
  state and keeps `ListProposals` exact: nothing remains to push, so the branch
  is not re-listed, and the reopened round is what demands re-review. The
  alternative considered and rejected was a strictly push-only column beside an
  observed upstream tip; it bought an audit record rather than any safety,
  since the reopened round already restores the review gate and an accept
  cannot un-push a commit that is already upstream.
