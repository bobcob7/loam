# CLI Spec

Specification for the Loam CLI — the agent-facing tool that talks to the server (see the
root `README.md`). The CLI carries the Loam-native workflow: work branches, review,
verdicts, and code-intelligence queries. Source control itself is **plain git**, run
against a clone that `loam clone` bootstraps; the server enforces push policy at receive
time (see `docs/git-spec.md`). This document specifies the CLI's command surface.

Status: **draft.** The command surface below is specified; remaining spec-level open
questions are tracked at the end.

## Conventions

The binary is invoked as `loam`. Configuration is entirely through environment variables
(see below) — the CLI has **no global flags**.

### Workspace

The CLI operates within a **workspace** — the directory it is run from. The workspace holds
a `.loam/` directory for staged comments, config, and cache, and it is where `clone` places
repos (each at `./<repo_name>`). A typical layout:

```
<workspace>/
  .loam/          # staged comments, config, cache
  doc-server/     # a clone; repo_name inferred as "doc-server"
  other-repo/
```

Commands are normally run from the workspace root. When run from inside a repo directory
(`<workspace>/<repo_name>`), the `repo` and work-branch arguments are inferred — repo from
the directory name, work branch from the current git branch — so they may be omitted from
**any** command that takes them.

### Output

Output is **JSON by default** — the CLI is agent-first, so structured output is the norm
and the format agents should parse. `LOAM_OUTPUT_FORMAT` (see Environment Variables) selects the
format: `json` (default), `yaml`, `xml`, or `human` for interactive use. An unrecognized
value falls back to `json`. This applies globally to every command.

### Environment Variables

The complete set of environment variables the CLI reads. All are **required except
`LOAM_OUTPUT_FORMAT`**. Identity & role feed the authorization model described in README → Agent
Identity & Roles. Names are provisional.

| Variable | Purpose | Required | Default |
| --- | --- | --- | --- |
| `LOAM_SERVER_URL` | Base URL of the Loam server — the Connect APIs and the git smart-HTTP endpoint (`clone` composes `<LOAM_SERVER_URL>/git/<group>/<repo>.git`). A URL (not host/port) so future transports like local sockets can be expressed via scheme. | yes | — |
| `LOAM_AGENT_NAME` | Agent name, a `<first-name>-<last-name>` combination. | yes | — |
| `LOAM_AGENT_ID` | Agent ID; combined into the identifier `<name>-<id>-<role>`. | yes | — |
| `LOAM_AGENT_ROLE` | Agent role; determines allowed operations and `instructions` output. | yes | — |
| `LOAM_OUTPUT_FORMAT` | Output format: `json`, `yaml`, `xml`, or `human`. Unknown values fall back to `json`. | no | `json` |

On the wire, the `LOAM_AGENT_*` values travel as the request headers `Loam-Agent-Name`,
`Loam-Agent-Id`, and `Loam-Agent-Role` — attached by the CLI to every RPC, and written
into a clone's git config by `clone` so plain git carries them too (see
`docs/git-spec.md`).

### Exit Codes & Errors

All output — success and error alike — is written to **stdout**. Because the CLI is used
almost entirely by agents, exit codes are kept deliberately coarse:

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Unexpected internal error |
| 2 | Usage error, authorization denied, conflict, or precondition failed |
| 3 | Not found |

On failure the CLI writes a structured error in the active `LOAM_OUTPUT_FORMAT` format (JSON
shown):

```json
{ "error": { "code": "not_found", "message": "work branch wb-9c2f1a not found in repo acme/web" } }
```

The `code` string names the specific failure within its exit-code class — e.g. `usage`,
`unauthorized`, `conflict`, and `precondition_failed` all exit `2`. In `human` output mode
the CLI prints a plain message instead of the structured object.

## Commands

### instructions

Returns the role-specific instructions for the calling agent, plus a general explanation of
how to use the CLI. Intended as the first command an agent runs to orient itself.

**Synopsis:** `loam instructions [command]`

**Arguments:**

- `command` *(optional)* — return help for just that command instead of the full
  orientation. Lets an agent fetch a single command's usage without the whole payload.

**Behavior:** Merges a static usage guide built into the binary (overall usage + the
conventions above) with the role-specific instructions fetched from the server (configured
per role in the web console). The command list is filtered to what the caller's role may
do.

**Output** (JSON; minimal by design — identity is deliberately excluded, see `whoami`):

```json
{
  "usage": "…overall usage and conventions…",
  "commands": [ { "name": "work list", "summary": "List work branches" } ],
  "role_instructions": "…configured for this role…"
}
```

**Errors:** exit `1` if the server is unreachable while fetching role instructions.

### whoami

Reports the calling agent's identity and role as resolved from the environment. Split out
of `instructions` so identity can be fetched on its own without the larger payload.

**Synopsis:** `loam whoami`

**Arguments:** none.

**Behavior:** Reads the `LOAM_AGENT_*` environment variables and returns the resolved
identity. Local only — no server call.

**Output** (JSON):

```json
{ "name": "ada-lovelace", "id": "7", "role": "reviewer", "identifier": "ada-lovelace-7-reviewer" }
```

**Errors:** exit `2` if a required identity variable is missing or malformed.

### Git

#### clone
Clone an enrolled repo from the server (server as sole remote) at a branch, and
bootstrap it so plain git works from then on.

**Synopsis:** `loam clone <repo> <branch>`

**Arguments:**

- `repo` *(required)* — the enrolled repo identifier, `<group>/<repo_name>` (e.g.
  `bobcob7/doc-server`).
- `branch` *(required)* — branch to check out: typically a work branch created via
  `work start`, or a target branch for exploration. Always explicit — there is no
  default.

**Behavior:** Clones the repo over smart HTTP from
`<LOAM_SERVER_URL>/git/<group>/<repo>.git` into `./<repo_name>` — always the final path
segment of the identifier, with no override (e.g. `bobcob7/doc-server` → `./doc-server`).
The clone's only remote is that endpoint. Checks out `branch` as a single-branch clone —
a convenient default shape, not an enforcement. `clone` then bootstraps
the clone for plain git: it sets the git author (`user.name` / `user.email`) to the agent
identity so commits are attributed, and writes the agent identity headers into the
clone's git config so every subsequent git operation carries them.

After the clone, **source control is plain git** — commit, push, fetch, merge, pull.
There are no `loam commit` or `loam push` commands and no client-side hook guard. The
server authorizes each push at receive time from the identity in the clone's config and
its ref policy: pushes land only on the caller's own, non-terminal work branch; target
branches are read-only; force pushes and deletions are rejected (see `docs/git-spec.md` →
Ref Policy).

**Output** (JSON):

```json
{ "repo": "bobcob7/doc-server", "path": "./doc-server", "branch": "wb-9c2f1a" }
```

**Errors:** exit `3` if the repo is not enrolled; exit `2` if `branch` does not exist.

### Work Branches

A **work branch** is the unit of work and the first-party entity. It is identified by its
`repo` and `name` — the name is randomly generated and carries no meaning, so a work
branch's title and description (set via `set`, editable at any time) are its human-facing
identity. "Review" is simply activity on a work branch, not a separate object.

Its lifecycle: `draft` → (**request-review**) → `reviewable` → (**first verdict**) →
`reviewed`. A re-review returns it to `reviewable` (marking the prior round's verdicts
stale), requested by the author (`request-review`) or by the admin. Each transition into
`reviewable` opens a numbered **review round**; threads, comments, and verdicts are
recorded against their round. The terminal states are
`complete` (set by the server when the upstream PR merges) and `closed` (admin-only, or when
the upstream PR is closed) — neither is an agent action. A conflicting target advance
resets a `reviewable`/`reviewed` branch to `draft` (marking its verdicts stale); a push
that catches the branch up to its target returns it to `reviewable` automatically, with no
`request-review` (see `docs/git-spec.md` → Target Advances & Catch-Up).

Commands that act on an existing work branch take `<repo>` and `<work-branch>` (its name) as
positional arguments; both are optional when run from inside the repo directory (inferred
per the Workspace rules above) and required otherwise.

A reviewer contributes a **verdict**: a batch of new-thread comments plus an outcome
(`approve` / `disapprove` / `neutral`), published atomically by `verdict`. Verdicts are
tracked per unique agent and marked **stale** when a new review is requested; only non-stale
verdicts count. Reviewers raise threads through their verdict; anyone (typically the author)
responds to a thread with `reply`, which posts immediately.

#### start
Start a work branch from a target branch. The name is randomly generated.

**Synopsis:** `loam work start <repo> <from>`

**Arguments:**

- `repo` *(required)* — the enrolled repo identifier, `<group>/<repo_name>`.
- `from` *(required)* — target branch to base off. Always explicit — there is no default
  base branch; the orchestrator tells the agent its target.

**Behavior:** Creates a **randomly named** work branch on the server from `from`, in state
`draft`, and returns it. This is a **server-side** ref creation — no local checkout — after
which the agent `clone`s the repo at the returned work branch. The name carries no meaning;
a work branch's identity is its title and description, not its name.

**Output** (JSON):

```json
{ "repo": "bobcob7/doc-server", "name": "wb-9c2f1a", "target": "main", "state": "draft" }
```

**Errors:** exit `2` if `from` is not a valid target branch; exit `3` if the repo is not
enrolled.

#### set
Set or update a work branch's title and/or description. Editable at any point.

**Synopsis:** `loam work set [repo] [work-branch] [--title <title>]` (optional description read from stdin)

**Input:**

- `repo`, `work-branch` positional — identify the work branch (see the convention above).
- `--title <title>` *(optional)* — new title. Omit to leave the title unchanged.
- **stdin** *(optional)* — new description (free text). Omit (empty stdin) to leave the
  description unchanged.

At least one of `--title` or a non-empty stdin must be provided.

**Behavior:** Replaces the work branch's title and/or description; nothing else changes.
Both must be present before the work branch can be made `reviewable`.

**Output** (JSON) — the updated work branch:

```json
{ "repo": "bobcob7/doc-server", "name": "wb-9c2f1a", "target": "main", "title": "Add login", "state": "draft" }
```

**Errors:** exit `2` if neither title nor description is provided; exit `3` if the work
branch does not exist.

#### request-review
Request review of a work branch — the signal that puts it up for review, or asks for another
round.

**Synopsis:** `loam work request-review [repo] [work-branch]`

**Input:** `repo`, `work-branch` positional — identify the work branch (see the convention
above). There is no request comment — feedback and discussion live in comment threads and
replies, each recorded against its review round.

**Behavior:** Transitions the work branch to `reviewable` — from `draft` (first review) or
from `reviewed` (a re-review, which opens a fresh round and marks the prior round's
verdicts stale) — making it visible to reviewers via `list`. Every transition into
`reviewable` opens a numbered **review round**; threads, comments, and verdicts record the
round they belong to. Requires a title and description to already be set (via `set`).
This is a single operation with two callers: the author (this command) and the admin
sending a reviewed branch back (a button in the web UI, reaching the same RPC as a
superuser — see `docs/web-spec.md`).

**Output** (JSON) — the work branch:

```json
{ "repo": "bobcob7/doc-server", "name": "wb-9c2f1a", "target": "main", "title": "Add login", "state": "reviewable" }
```

**Errors:** exit `2` if the work branch has no title or description, or is in a terminal
state (precondition failed); exit `3` if it does not exist.

#### list
List work branches across all enrolled repos.

**Synopsis:** `loam work list [--repo <repo>] [--author <id>] [--target <branch>] [--awaiting-review] [--state <state>]`

**Input:**

- `--repo <repo>` *(optional)* — limit to one enrolled repo.
- `--author <id>` *(optional)* — limit to work branches authored by this agent identifier.
- `--target <branch>` *(optional)* — limit to work branches targeting this branch.
- `--awaiting-review` *(optional)* — limit to reviewable work branches awaiting the calling
  agent's verdict.
- `--state <state>` *(optional)* — `draft` / `reviewable` / `reviewed` / `complete` /
  `closed`; defaults to `reviewable`.

With no flags, lists all reviewable work branches across all enrolled repos.

**Behavior:** Returns matching work branches, each identified by its `repo` and `name`.

**Output** (JSON array):

```json
[ { "repo": "bobcob7/doc-server", "name": "wb-9c2f1a", "target": "main", "title": "Add login", "author": "grace-hopper-3-author", "state": "reviewable" } ]
```

**Errors:** exit `2` on a bad filter value. An empty result is a normal exit `0`.

#### show (details)
Return a work branch's metadata, title, description, and state. The diff and comment threads
are fetched separately via `diff` and `comments`, to keep each response small.

**Synopsis:** `loam work show [repo] [work-branch]`

**Input:** `repo` and `work-branch` positional arguments identify the work branch (see the
convention above).

**Output** (JSON):

```json
{ "repo": "bobcob7/doc-server", "name": "wb-9c2f1a", "target": "main", "title": "Add login",
  "description": "…", "state": "reviewable", "author": "grace-hopper-3-author",
  "round": { "number": 2, "requested_by": "grace-hopper-3-author" } }
```

**Errors:** exit `3` if the work branch does not exist; exit `2` if the identifier cannot be
resolved (not in a clone and arguments omitted).

#### diff
Return the work branch's diff against its target, separately from `show` to keep both small.

**Synopsis:** `loam work diff [repo] [work-branch]`

**Input:** `repo` and `work-branch` positional arguments identify the work branch (see the
convention above).

**Output:** the unified diff of the work branch against its target branch, as a field in the
active `LOAM_OUTPUT_FORMAT` (e.g. `{ "diff": "…" }` for JSON).

**Errors:** exit `3` if the work branch does not exist; exit `2` if the identifier cannot be
resolved.

#### comments (get)
Fetch the comment threads on a work branch, or the caller's own staged comments.

**Synopsis:** `loam work comments [repo] [work-branch] [--staged]`

**Input:**

- `repo`, `work-branch` positional — identify the work branch (see the convention above).
- `--staged` *(optional)* — return the caller's locally staged (unpublished) comments from
  `.loam` instead of the published threads.

**Behavior:** By default returns the work branch's published threads — each with its resolved
state, optional file/line anchor, and the comments within it. Published threads only; the
caller's staged comments are excluded until submitted. With `--staged`, returns those staged
items instead (the shape produced by `comment`), so an agent can review what it is about to
submit.

**Output** (JSON array) — published threads by default:

```json
[ { "id": "t1", "resolved": false, "file": "auth.go", "line": 42, "round": 1,
    "comments": [ { "author": "ada-lovelace-7-reviewer", "body": "…", "round": 1 } ] } ]
```

**Errors:** exit `3` if the work branch does not exist; exit `2` if the identifier cannot be
resolved.

#### verdicts
List the verdicts on a work branch — the current round plus stale ones from prior rounds.

**Synopsis:** `loam work verdicts [repo] [work-branch]`

**Input:** `repo`, `work-branch` positional — identify the work branch (see the convention
above).

**Behavior:** Returns each reviewer's recorded verdict (unique agent + outcome), including
those marked **stale** by a later review request. Only non-stale verdicts count toward the
approval bar.

**Output** (JSON array):

```json
[ { "reviewer": "ada-lovelace-7-reviewer", "outcome": "approve", "round": 2, "stale": false } ]
```

**Errors:** exit `3` if the work branch does not exist; exit `2` if the identifier cannot be
resolved.

#### comment (add)
Stage a review comment on a work branch locally. Nothing is published until `verdict`
submits. Reviewer tooling — comments open *new* threads; replying to an existing thread is
immediate, via `reply`.

**Synopsis:** `loam work comment [repo] [work-branch] [--file <path> --line <n>] [--resolve <thread-id>] [--edit <staged-id>] [--discard <staged-id>]` (body read from stdin)

**Input:**

- `repo`, `work-branch` positional — identify the work branch (see the convention above).
- **stdin** — the comment body. Required unless only `--resolve` or `--discard` is given.
- `--file <path>` + `--line <n>` *(optional)* — anchor the new thread to a line.
- `--resolve <thread-id>` *(optional)* — mark a thread resolved. Only the thread's original
  author may resolve it. May carry a body or be used alone.
- `--edit <staged-id>` *(optional)* — replace the body of a previously staged comment (new
  body from stdin), before it is submitted.
- `--discard <staged-id>` *(optional)* — remove a staged comment from the staging area.

The modes are mutually exclusive: a single invocation either opens a new thread (top-level or
`--file`/`--line`-anchored), `--edit`s a staged comment, or `--discard`s one. `--resolve` may
accompany a new comment or stand alone.

**Behavior:** Operates on the caller's **local staging area** for this work branch (in
`.loam`). New-thread comments and resolves append to it; `--edit` and `--discard` modify or
remove an already-staged item. Staged items accumulate across invocations and stay invisible
to everyone else until `verdict` publishes them.

**Staging location:** the workspace's `.loam/` directory (see Workspace above), keyed by
repo, work branch, and agent — outside any clone, so reviewers who never clone can still
stage.

**Output** (JSON) — the staged item with a local staging id:

```json
{ "staged": true, "id": "s3", "file": "auth.go", "line": 42, "body": "…" }
```

**Errors:** exit `2` on conflicting modes, a missing body when one is required, or attempting
to resolve a thread the caller did not author; exit `3` if the work branch, referenced
thread, or referenced staged comment does not exist.

#### reply
Reply to an existing comment thread. Immediate — the reply posts right away, not staged.

**Synopsis:** `loam work reply [repo] [work-branch] --thread <thread-id>` (body read from stdin)

**Input:**

- `repo`, `work-branch` positional — identify the work branch (see the convention above).
- `--thread <thread-id>` *(required)* — the thread to reply to.
- **stdin** *(required)* — the reply body.

**Behavior:** Posts a reply to the thread immediately (no staging). This is how an author
responds to review feedback; reviewers raise threads via `verdict`, not here.

**Output** (JSON) — the posted reply:

```json
{ "author": "grace-hopper-3-author", "body": "…" }
```

**Errors:** exit `3` if the work branch or thread does not exist; exit `2` if the identifier
cannot be resolved.

#### verdict
Publish the caller's staged comments on a work branch atomically, as a verdict with an
outcome.

**Synopsis:** `loam work verdict [repo] [work-branch] --outcome <approve|disapprove|neutral>`

**Input:**

- `repo`, `work-branch` positional — identify the work branch (see the convention above).
- `--outcome <approve|disapprove|neutral>` *(required)* — the verdict's overall outcome.

**Behavior:** Publishes all of the caller's locally staged comments for this work branch in
one atomic action as a verdict, attaches the outcome, and clears the local staging area.
This is the only point at which staged comments become visible. The **first** non-stale
verdict of a round flips the work branch `reviewable` → `reviewed`; verdicts are tracked per
unique agent and are marked **stale** when a review is requested. Only non-stale verdicts
count, so the admin can create the upstream PR once there is ≥1 non-stale approve. Submitting
with no staged comments is allowed (an outcome-only verdict). Re-submitting replaces that
agent's verdict for the round.

**Output** (JSON):

```json
{ "repo": "bobcob7/doc-server", "work_branch": "wb-9c2f1a", "outcome": "approve", "published": 3 }
```

**Errors:** exit `2` on a missing or invalid outcome; exit `3` if the work branch does not
exist.

Once a work branch is `reviewed` with at least one approve verdict, it becomes a proposal in
the admin's queue (see `docs/web-spec.md` → ProposalService). There is no agent `complete`
command — the admin creates the upstream PR, and the work branch flips to `complete` only
when that PR merges.

### Graph DB queries

Structural queries over the Tree-sitter graph (see README → Graph DB). A fixed set of
subqueries rather than a query language.

**Synopsis:** `loam graph <subquery> <target> [--repo <repo>] [--all]`

**Subqueries:**

- `def <symbol>` — where the symbol is defined.
- `refs <symbol>` — everywhere the symbol is referenced.
- `deps <file|symbol>` — what the target depends on.
- `dependents <file|symbol>` — what depends on the target (blast radius).
- `history <symbol>` — the symbol's commit/ref history.

**Scope:** defaults to the repo inferred from the current directory; `--repo <repo>` targets
a specific enrolled repo; `--all` runs the query across all enrolled repos. If run outside a
repo directory with neither flag, exit `2`.

`--all` is a **fan-out**: it queries each repo's graph independently and unions the results.
For the MVP it does **not** resolve cross-repo dependency edges — each repo's graph is built
in isolation, so a usage in one repo is not linked to a definition in another. True
cross-repo dependency resolution (global symbol identity + import resolution) is Future Work
(see README).

**Output** (JSON) — `results` shape depends on the subquery (e.g. `refs` below). Every
response carries `ingested`: the commit each queried repo's index was built from
(`docs/ingestion-spec.md`), so an agent can tell how stale an answer is relative to the
branch tip:

```json
{ "ingested": [ { "repo": "bobcob7/doc-server", "target": "main", "ref": "a1b2c3d", "at": "2026-07-25T12:00:00Z" } ],
  "results": [ { "repo": "bobcob7/doc-server", "file": "auth.go", "line": 42, "symbol": "Login" } ] }
```

**Errors:** exit `2` on an unknown subquery or unresolvable scope; exit `3` if the target
symbol/file is not found.

### RAG queries (search)

Natural-language semantic search over ingested docs/code (see README → RAG).

**Synopsis:** `loam search <query> [--repo <repo>] [--all] [--limit <n>]`

**Input:**

- `query` *(required)* — the natural-language query.
- `--limit <n>` *(optional)* — maximum number of chunks to return; defaults to `10`.
- Scope: same as `graph` — the repo inferred from the current directory by default,
  `--repo <repo>` to target a specific enrolled repo, `--all` for all enrolled repos. If run
  outside a repo directory with neither flag, exit `2`. Unlike `graph`, `search --all`
  genuinely spans repos: semantic matches surface relevant chunks from any enrolled repo
  (cross-repo *discovery*, though still not dependency edges).

**Behavior:** Embeds `query` and returns the most relevant ingested doc/code chunks with
provenance.

**Output** (JSON) — the same `ingested` envelope as `graph`, so staleness is visible per
queried repo:

```json
{ "ingested": [ { "repo": "bobcob7/doc-server", "target": "main", "ref": "a1b2c3d", "at": "2026-07-25T12:00:00Z" } ],
  "results": [ { "repo": "bobcob7/doc-server", "file": "auth.go", "lines": [40, 58], "score": 0.82, "snippet": "…" } ] }
```

**Errors:** exit `2` on bad arguments or unresolvable scope.

## Open Questions

None currently open.
