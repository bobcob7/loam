# Git Transport Spec

Specification for git transport between agents and the Loam server — how clones and pushes
travel, how agent identity reaches git operations, and what the server enforces on its
refs. Server↔upstream git (mirror sync, pushing an accepted proposal's branch to the forge)
is **out of scope** here; it belongs to the upstream sync spec.

Status: **draft — transport and policy settled.** This spec adopts **git smart HTTP**
(no agent-facing SSH endpoint) with agents using **plain git** against a clone that
`loam clone` bootstraps — there are no CLI git wrappers and no client-side hook guard.
The root `README.md`, `docs/cli-spec.md`, `docs/web-spec.md`, `docs/persistence-spec.md`,
and the `features/` files are aligned with this document. Upstream transport to the forge
is likewise token-authenticated HTTPS (`docs/sync-spec.md` → Upstream Transport) — there
is no SSH anywhere in Loam. Implementation details (socket protocol, hook stub) firm up
during build.

## Division of Labor: git's Plumbing, Loam's Policy

Git already solves transport, refs, merges, fast-forward enforcement, and conflict
resolution; Loam does not re-solve any of it. What git deliberately does **not** solve is
*who may write which ref under what state* — that is why pre-receive hooks exist, and it
is exactly how forges implement protected branches. Loam's enforcement therefore lives in
one place: **server-side, at receive time**. The client side is not a control point —
`loam clone` bootstraps a clone (URL, identity, config) and gets out of the way; from
then on agents use **plain git**, the tool they already know.

## Decision: smart HTTP, not SSH

The MVP serves git over the **smart HTTP protocol** on the **same HTTP listener** as the
Connect APIs, under a dedicated path prefix. There is no agent-facing SSH endpoint.

The SSH design had a contradiction: push authorization was specified by role, but the SSH
endpoint authenticated with a **shared key that does not identify the agent** — identity
had no channel on git operations, so role rules like "a reviewer may not push" had no
enforcement point. HTTP closes that, and simplifies everything around it:

- **One identity model.** Git requests carry the same identity headers as the Connect
  RPCs; a single server interceptor authorizes both surfaces.
- **One port, one URL.** Git is dispatched by path on the existing listener;
  `LOAM_GIT_URL` disappears and the CLI composes clone URLs from `LOAM_SERVER_URL`.
- **No key machinery.** No SSH host keys, no shared private key to distribute into every
  agent environment, no known-hosts friction.
- **Plain git works.** Identity is written into the clone's config at bootstrap, so
  stock `git push` / `git fetch` carry it natively — no wrapper commands, no client-side
  hook guard.
- **Future auth lands uniformly.** Server-issued credentials (README → Future Work)
  become a git **credential helper** plus an `Authorization` header on RPC — the stock
  mechanism every forge uses for authenticated HTTP git.

## Endpoint & Protocol

- Path prefix: **`/git/<group>/<repo_name>.git`** — the standard smart-HTTP surface
  (`GET …/info/refs?service=…`, `POST …/git-upload-pack`, `POST …/git-receive-pack`).
- **Smart protocol only** (v2 for fetch); the dumb HTTP protocol is not served.
- The web-spec routing table gains one row:

| Path | Handler | Auth |
| --- | --- | --- |
| `/git/*` | git smart HTTP (upload-pack / receive-pack) | agent identity headers (MVP: trusted) |

- The CLI composes the remote URL as `<LOAM_SERVER_URL>/git/<group>/<repo_name>.git`.
  `LOAM_GIT_URL` is removed from the environment table.

## Identity on Git Operations

Identity travels as three request headers — the same ones the Connect interceptor reads,
now named concretely (to be recorded in `docs/cli-spec.md` as the wire form of
`LOAM_AGENT_*`):

| Header | Source |
| --- | --- |
| `Loam-Agent-Name` | `LOAM_AGENT_NAME` |
| `Loam-Agent-Id` | `LOAM_AGENT_ID` |
| `Loam-Agent-Role` | `LOAM_AGENT_ROLE` |

`loam clone` writes these into the clone's git config as `http.extraHeader` entries and
sets `user.name` / `user.email` to the agent identity. From then on **plain git carries
identity** on every fetch and push; there is no wrapper to invoke.

- Persisting the headers in the clone changes nothing about trust: MVP identity is
  asserted, not verified, on every surface — the config copy is the same
  environment-sourced assertion, one step along. Anyone with access to the clone could
  assert the same identity by setting three env vars; the boundary is unchanged.
- When authentication lands, the header entries are replaced by a credential helper
  (`loam` itself can serve as one), and the server starts verifying instead of trusting.
  The transport is unchanged by that upgrade.
- The server responds `403` (not `401`) to missing or malformed identity, so
  unconfigured git clients fail fast instead of prompting for credentials.

## Operations & Role Gates

Middleware authorizes the operation before any git process runs:

| Git operation | Requests | Required capability |
| --- | --- | --- |
| clone / fetch | `info/refs?service=git-upload-pack`, `git-upload-pack` | `git.clone` |
| push | `info/refs?service=git-receive-pack`, `git-receive-pack` | `git.push` |

- Missing/malformed identity → `403`. Role lacking the capability → `403`. Repo not
  enrolled → `404`. These surface as ordinary git HTTP errors — there is no CLI layer in
  between.
- **Repo enrolled but no mirror on disk yet → `503`, not `404`** (loam-1gq). The window
  between `EnrollRepo` writing the `repos` row and the first successful clone/sync
  landing is normally narrow — `EnrollRepo` runs the clone synchronously and returns
  only once `sync_state` is back to `idle` — but it stays open indefinitely if that
  first clone **fails** (`docs/sync-spec.md`: `sync_state` goes to `error`, not back to
  unenrolled), which is exactly when an operator most needs an honest signal. `404`
  would read as "no such repo" to an agent that just watched enrollment succeed; `503`
  says what is actually true — this repo is known, it is just not ready yet — mirroring
  the `503`/"not ready: `<reason>`" convention `internal/health`'s `/readyz` already
  establishes elsewhere in this codebase, rather than inventing a second one. All three
  smart-HTTP requests (`info/refs`, `git-upload-pack`, `git-receive-pack`) share this
  check, run once before any response byte is written.
- `upload-pack` serves the **whole mirror** — roles gate *whether* an agent may fetch,
  not which refs. Mirrors are not secret from enrolled agents.
- **Both built-in roles carry `git.clone`.** Reviewers clone so they can bring their own
  tools to the branch under review — run the tests, build it, open it in an analyzer —
  rather than judging from `work diff` alone. Only the author role carries `git.push`; a
  reviewer's stray local commit is additionally stopped by the ref policy's author rule.
  (The built-in reviewer role in `docs/web-spec.md` gains `git.clone`.)
- **Admin basic auth is not accepted on `/git/*`** — unlike `/loam.v1.*`, where the admin
  is a superuser. The admin administers the process, not the code; their lever is the
  proposal queue, and nothing in the admin workflow needs a mirror clone. Git speaks
  agent identity only.

## Ref Policy (push)

The mirror's refs fall into two classes:

- **Mirrored refs** — target branches and tags. **Read-only to
  agents**; owned by upstream sync (upstream-wins). Without this rule, anything writable
  here would silently corrupt the mirror until the next sync clobbered it.
- **Work-branch refs** — `refs/heads/loam-reserved/<name>` where `<name>` is a registered
  work branch of this repo. Created **server-side by `work start` only**, never by push.

### The `refs/heads/loam-reserved/` namespace is server-owned

`refs/heads/loam-reserved/` is **reserved in the mirror**: everything below it is written
by Loam itself, and **upstream refs under that path are consequently not mirrored** — the
mirror fetch excludes the whole subtree structurally, so an upstream branch that happened
to be named `loam-reserved/…` is simply never carried in. That is the one carve-out from
"target branches and tags" above.

The namespace exists because the mirror fetch is a **pruning** fetch of
`+refs/heads/*:refs/heads/*` and `+refs/tags/*:refs/tags/*` (`loam-5f3` narrowed this from
the `git clone --mirror`-equivalent `+refs/*:refs/*`, which also pulled in `refs/pull/*`,
`refs/notes/*`, and `refs/replace/*` — nothing Loam reads, and the last of which would
otherwise silently alter object visibility) whose argv — including one negative exclusion
per *currently registered* work branch — is fixed **before** the network operation begins. A work branch created at any point during
that fetch is absent from the enumerated exclusions, and its brand-new, purely-local ref
is therefore a prune candidate; no colliding upstream name is needed. The loss is
unrecoverable: `work_branches` carries no SHA column and a bare mirror has no reflog, so
the row survives pointing at a ref that no longer exists. A whole reserved path segment,
excluded by glob, closes that window for every work-branch ref that will ever exist. The
enumerated per-branch exclusions remain the semantic rule; the glob is a structural
backstop.

**Only the mirror's ref path carries the namespace.** The work branch's *name* is
unchanged — still `wb-9c2f1a` as a CLI argument, still `loam/wb-9c2f1a` upstream:

| | |
| --- | --- |
| name | `wb-9c2f1a` |
| mirror ref | `refs/heads/loam-reserved/wb-9c2f1a` |
| upstream proposal branch | `refs/heads/loam/wb-9c2f1a` |

A push aimed at `refs/heads/<name>` for a name that *is* a registered work branch is
rejected with a reason naming the ref that would have worked — see the table below. It is
a reachable shape, not a hypothetical: `git push origin HEAD` resolves its destination by
name and bypasses `remote.origin.push`, as does any push from a clone that `loam clone`
never bootstrapped.

Stock git enforces the mechanical rules: every mirror carries
`receive.denyNonFastForwards` and `receive.denyDeletes`, so force pushes and ref
deletions are rejected by git itself, with git's own messages. The sanctioned workflow
never needs a rewrite — history only ever gains commits, and any future
refresh-from-target feature should **merge the target into the work branch** rather than
rebase, which keeps that permanent.

Loam's pre-receive hook evaluates only the rules git cannot know, because they live in
Loam's database. A push is accepted only if **every** ref update in it satisfies all of:

1. The ref names a registered work branch of the repo (creates of unknown refs, and any
   update outside `refs/heads/`, are rejected).
2. The caller is that work branch's **author** — the MVP assumes a single agent per work
   branch (collaborative branches are Future Work). This is an entity-ownership rule like
   "only a thread's author may resolve it," not per-branch role scoping.
3. The work branch is in a **non-terminal state** (`draft` / `reviewable` / `reviewed`).
   An existing upstream PR does **not** lock the branch — pushes keep flowing so a
   conflicted branch can be caught up (see Target Advances & Catch-Up).

Rejections are surfaced through receive-pack's per-ref status with a `loam:`-prefixed
reason:

| Reason | Example |
| --- | --- |
| read-only ref | `loam: refs/heads/main is read-only (target branch)` |
| unknown ref | `loam: refs/heads/foo is not a work branch; create one with 'work start'` |
| wrong ref path | `loam: wb-9c2f1a must be pushed to refs/heads/loam-reserved/wb-9c2f1a; re-run 'loam clone' to configure the push refspec, then push by branch name` |
| not the author | `loam: wb-9c2f1a belongs to grace-hopper-3-author` |
| terminal state | `loam: wb-9c2f1a is closed` |

Policy evaluation is **atomic**: one bad ref update rejects the whole push (pre-receive
semantics), so a push never half-lands.

This pulls the "server-side pre-receive enforcement" item forward from Future Work into
the MVP for **ref policy**; only identity *verification* stays deferred.

## Target Advances & Catch-Up

Work branches race their target: upstream keeps moving while a branch is written,
reviewed, and decided. Conflicts are inevitable, and the server — whose sync sees the
advance — notices before any agent does. The behavior, as it shapes transport:

- On every target-branch advance, the server **tests each open (non-terminal) work
  branch against the new tip** — a mergeability check only. The server is a broker and a
  store: it never authors commits, and work-branch refs advance **only by agent pushes**.
  - **Merges cleanly** → nothing happens. Behind-but-mergeable is a normal state; the
    *forge* performs the actual merge when the accepted PR merges.
  - **Conflict** → a `reviewable` or `reviewed` branch (including an accepted proposal
    with an open PR) is **reset to `draft`** and flagged as conflicted. Its verdicts
    judged content the target has since invalidated, and they go **stale when the
    branch returns to review** — the catch-up restore opens a new round, and staleness
    is derived from the round number (see "A flagged branch recovers by push" below).
    They are not marked stale at the moment of demotion: no round opens there, so
    there is nothing for the derivation to compare against, and opening one would
    invent a review round for a branch nobody has asked anyone to review. Nothing can
    act on the branch in the meantime regardless — acceptance gates independently on
    `state = reviewed` **and** `conflict = none`, and a demoted branch fails both.
    A `draft` branch just gains the flag. Resolved as loam-di9q.
- A flagged branch recovers **by push**: when an agent pushes commits that bring the
  branch up to date (its history contains the current target tip), the server clears the
  flag, and a conflict-reset branch flips **directly back to `reviewable`** — no
  `request-review` needed. The *review* was interrupted rather than abandoned, but the
  restore is still a transition into `reviewable`, so it **opens a new numbered round**
  like every other such transition (`docs/cli-spec.md` → "Each transition into
  `reviewable` opens a numbered review round"; `internal/db/queries/review_rounds.sql`
  names catch-up auto-restore as one of `OpenReviewRound`'s three callers). "No
  `request-review` needed" is a statement about the *agent verb* — the author does not
  have to ask — not about round numbering.

  The round opens **only when the branch actually transitions into `reviewable`**, which
  is not every catch-up. `workbranchstore.ClearConflict` already distinguishes the two
  cases and the distinction is the whole rule: a branch that was **demoted** (`conflict`
  = `reset`, so it had been `reviewable`/`reviewed` before the conflict) flips back to
  `reviewable` and gets a new round; a branch that was **merely flagged** and stayed
  `draft` throughout just loses the flag, its state untouched — no transition, therefore
  **no new round**. Opening one for the flagged-draft case would invent a review round
  for a branch nobody has asked anyone to review.

  This is load-bearing, not bookkeeping. Staleness in this codebase is **derived** from
  `MAX(review_rounds.number)`; there is no stored stale flag. If the catch-up resumed the
  same round, the pre-conflict verdicts would keep deriving as CURRENT — so the branch
  would drop out of the awaiting-verdict filter and its approvals of content the target
  has already invalidated would still count toward the approval bar. That is precisely
  the staleness this spec and `docs/cli-spec.md` both say a conflict demotion must
  produce, and the derived mechanism cannot express "interrupted round" without a new
  number. Resolved as loam-lb6.
- Catching up is **ordinary git**: `git fetch origin <target>`, merge it into the work
  branch, resolve the conflicts, commit, push. No Loam verb is involved; conflict
  resolution is the thing git is best at. An agent may do the same merge at any time on a
  cleanly-mergeable branch, on its own schedule — nothing forces it.
- **The upstream PR is never touched** during this cycle — not closed, not updated.
  Because history only gains commits, when the admin re-accepts the caught-up branch,
  refreshing the existing PR's branch is a plain fast-forward push and the PR updates in
  place (mechanics owned by the upstream sync spec).

The check scheduling, flag storage, and PR-refresh flow belong to the upstream sync spec;
they appear here because they define the push rules above.

## Enforcement Mechanics

- The handler resolves the repo, runs the middleware gates, then **shells out to stock
  git** (`git receive-pack --stateless-rpc` / `git upload-pack --stateless-rpc`) against
  the bare mirror. The server already shells out to git for sync, diffs, and ingest, so
  this adds no new dependency — and pack handling (thin packs, connectivity checks,
  protocol edge cases) is bought, not built.
- Ref policy runs as a **pre-receive hook**: a trivial stub that forwards the proposed
  ref updates, plus the identity the handler placed in the hook environment, to the
  server over a **unix socket**, and passes or fails on the answer. The server evaluates
  rules 1–3 against Postgres (the work-branch registry: name, author, state) in one
  place; pre-receive semantics keep the decision atomic.
- The server writes the hook and the `receive.denyNonFastForwards` /
  `receive.denyDeletes` config **idempotently at enrollment and on startup**, so upgrades
  never chase stale mirror state and the mirror stays a plain bare repo. The policy
  itself lives in one Go function with the rest of the domain logic.

**One path bypasses all of the above, by construction.** Everything in this
section applies to pushes that arrive through `/git/*`. A commit pushed
directly to a `loam/<work-branch>` branch *on the forge* reaches Loam through
the mirror fetch instead, so no hook runs and no policy applies to it. Loam
adopts such a commit only when it is a clean fast-forward, and reopens the
review round when it does, so the code cannot reach a further upstream push
unreviewed — see `docs/sync-spec.md` → Upstream Drift for the full rule and
why divergence is escalated to the admin rather than resolved.

## The CLI's Role

Loam's client-side git surface shrinks to one bootstrap verb:

- **`loam clone <repo> [branch]`** — composes the URL, clones (default shape:
  `--branch <b> --single-branch`, a convenience rather than an enforcement), sets
  `user.name` / `user.email` to the agent identity, writes the identity headers into
  the clone's config, and writes the two refspecs below. After that, agents use **plain
  git**: commit, push, fetch, merge, pull.

  | Key | Value | Written with |
  | --- | --- | --- |
  | `remote.origin.push` | `refs/heads/wb-*:refs/heads/loam-reserved/wb-*` | `git config` (single-valued) |
  | `remote.origin.fetch` | `+refs/heads/loam-reserved/*:refs/remotes/origin/*` | `git config --add` (appended — the `--single-branch` clone's own refspec must survive) |

  These map a work branch's bare name to its reserved ref path in both directions, so
  `git push origin wb-9c2f1a` lands on `refs/heads/loam-reserved/wb-9c2f1a` and
  `git fetch origin` brings work branches down as `origin/wb-9c2f1a`. git resolves a
  command-line refspec with no `:<dst>` through `remote.origin.push`, which is what makes
  the plain form work.

  **This makes the clone bootstrap load-bearing for pushes**, unlike every other thing
  `loam clone` writes. A hand-rolled `git clone <url>` still fetches and commits, but its
  pushes aim at `refs/heads/<name>` and are rejected (see the ref-policy table's *wrong
  ref path* row). `git push origin HEAD` bypasses `remote.origin.push` too, for the same
  reason, and is rejected the same way — push by branch name, or plain `git push`.
  The `wb-*` scoping of the push refspec is deliberate: an unscoped `refs/heads/*` would
  sweep the clone's own `main` into every bare `git push`, and pre-receive atomicity would
  then reject the work branch along with it.
- **`loam commit`, `loam push`, the `pre-commit`/`pre-push` hook guard, and the
  `LOAM_INTERNAL` sentinel are removed.** They existed to inject identity per invocation;
  with identity in the clone's config they gate nothing the server does not already
  enforce.
- **Branch pinning is demoted from enforcement to default.** A stray local checkout is
  harmless — the ref policy decides what lands, not the working copy's HEAD. Fetching
  the target for catch-up works from a single-branch clone by naming the ref
  (`git fetch origin <target>`).
- `work start` remains a server-side RPC: work-branch refs are only ever created by it,
  never by push.

## Open Questions

None currently open.
