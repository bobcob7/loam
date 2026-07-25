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
is forge-agnostic; only the REST calls differ per forge. **Forgejo is the MVP
implementation; GitHub is the close-behind second.**

Operations (Go interface, one implementation per forge):

- `ValidateToken(host, token) → error` — backs `CredentialService.SetUpstreamToken`;
  confirms the token works and has the scopes needed to open PRs.
- `CheckRepo(upstream_url) → error` — backs `EnrollRepo`; confirms the repo exists
  and is accessible with the host's credential before cloning.
- `CreatePR(repo, head_branch, target_branch, title, description) →
  { pr_url, pr_number }` — opens the upstream PR.
- `GetPRState(repo, pr_number) → open | merged | closed` — backs completion and
  closure detection.
- `GitCredentials(token) → { username, password }` — the forge's convention for
  token-authenticated HTTPS git (e.g. GitHub uses `x-access-token` as the
  username; Forgejo takes the token as the password with any username).

Everything else — URL parsing to `<group>/<repo_name>` (a path split, uniform
across forges), git fetch/push to the upstream, branch naming — lives in the
core, outside the interface.

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

1. **Fetch** all upstream refs into the mirror, forced, with pruning —
   upstream-wins, always: a diverged or force-pushed upstream ref simply
   replaces the mirror's copy, and a branch deleted upstream is pruned.
   **Registered work-branch refs are excluded from the fetch refspec** — they
   are Loam's own refs and must never be clobbered even if upstream grows a
   branch with a colliding name.
2. **Detect target advances** by comparing each target branch's SHA before and
   after the fetch.
3. **Mergeability check** (below) for every advanced target.
4. **Enqueue ingest** for advanced indexed branches (`docs/ingestion-spec.md`).
5. **Poll PR states** for work branches with an open recorded PR (below).

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
code**. Work-branch refs advance only by agent pushes. On a target advance, the
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
Catch-Up). If the target has advanced again since the reset, the flag simply
stays until a push catches up to the newer tip.

## Proposal Acceptance

`AcceptProposal(repo, work_branch)` — preconditions: state `reviewed`, ≥1
non-stale approve verdict, **not `conflicted`** (web-spec ripple: the
precondition list gains the conflict check). Then:

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

## Future Work

- **Webhooks.** Replace or supplement polling with forge webhooks for near-instant
  advance and PR-state detection; polling remains the fallback.
- **Per-repo sync intervals.** A single server-wide interval is enough for the
  MVP.

## Open Questions

None currently open.
