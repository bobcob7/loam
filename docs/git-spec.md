# Git Transport Spec

Specification for git transport between agents and the Loam server — how clones and pushes
travel, how agent identity reaches git operations, and what the server enforces on its
refs. Server↔upstream git (mirror sync, pushing an accepted proposal's branch to the forge)
is **out of scope** here; it belongs to the upstream sync spec.

Status: **draft — transport and policy settled.** This spec adopts **git smart HTTP**
(no agent-facing SSH endpoint) with agents using **plain git** against a clone that
`loam clone` bootstraps — there are no CLI git wrappers and no client-side hook guard.
The root `README.md`, `docs/cli-spec.md`, `docs/web-spec.md`, `docs/persistence-spec.md`,
and the `features/` files are aligned with this document. The server↔upstream SSH key
pair managed by `CredentialService` is unaffected (it covers transport to the forge, not
to agents). Implementation details (socket protocol, hook stub) firm up during build.

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

- **Mirrored refs** — target branches and every other upstream ref. **Read-only to
  agents**; owned by upstream sync (upstream-wins). Without this rule, anything writable
  here would silently corrupt the mirror until the next sync clobbered it.
- **Work-branch refs** — `refs/heads/<name>` where `<name>` is a registered work branch
  of this repo. Created **server-side by `work start` only**, never by push.

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

- On every target-branch advance, the server **attempts to merge the new target into
  each open (non-terminal) work branch** on the mirror.
  - **Clean merge** → a server-authored merge commit lands on the work branch; the
    branch's state is untouched.
  - **Conflict** → nothing is committed. A `reviewable` or `reviewed` branch (including
    an accepted proposal with an open PR) is **reset to `draft`** and flagged as
    conflicted; its verdicts go stale, since they judged content the target has
    invalidated. A `draft` branch just gains the flag.
- A flagged branch recovers **by push**: when an agent pushes commits that bring the
  branch up to date (its history contains the current target tip), the server clears the
  flag, and a conflict-reset branch flips **directly back to `reviewable`** — no
  `request-review` needed; the round was interrupted, not abandoned.
- Catching up is **ordinary git**: `git pull`, then `git fetch origin <target>` and merge
  it into the work branch, resolve the conflicts, commit, push. No Loam verb is involved;
  conflict resolution is the thing git is best at.
- Server-authored merge commits mean a branch can advance **under** an agent's clone; the
  agent's next push is rejected as non-fast-forward, and the remedy is `git pull` —
  plain git again, no special handling.
- **The upstream PR is never touched** during this cycle — not closed, not updated.
  Because history only gains commits, when the admin re-accepts the caught-up branch,
  refreshing the existing PR's branch is a plain fast-forward push and the PR updates in
  place (mechanics owned by the upstream sync spec).

The merge scheduling, flag storage, and PR-refresh flow belong to the upstream sync spec;
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

## The CLI's Role

Loam's client-side git surface shrinks to one bootstrap verb:

- **`loam clone <repo> [branch]`** — composes the URL, clones (default shape:
  `--branch <b> --single-branch`, a convenience rather than an enforcement), sets
  `user.name` / `user.email` to the agent identity, and writes the identity headers into
  the clone's config. After that, agents use **plain git**: commit, push, fetch, merge,
  pull.
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
