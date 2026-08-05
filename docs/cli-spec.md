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

### Argument Ordering

Flags may appear **anywhere** on the command line relative to positional arguments — before
them, after them, or interspersed. `loam work set acme/repo wb-1 --title "New title"` and
`loam work set --title "New title" acme/repo wb-1` are equivalent. This is the guarantee
implied but never stated by synopses elsewhere in this document that show flags trailing
positionals (e.g. `work set [repo] [work-branch] [--title <title>]`); it holds for every
command, not just the ones whose synopsis happens to put flags last.

A recognized flag that requires a value but has none following it (e.g. a bare trailing
`--title` with nothing after it) is a usage error (exit `2`) — it is never silently
reinterpreted as consuming the next positional argument as that flag's value. `--` ends
flag parsing: every token after it is a literal positional, even one that starts with `-`.

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

### Help

`loam help`, `loam --help`/`-h`, and `loam <command> [<subcommand>] --help`/`-h` print
usage and exit `0`. Unlike every other route through the CLI, **help never requires any
`LOAM_*` environment variable** — it is what an agent runs precisely because it does not
know the configuration yet, so it cannot be gated behind config the way every other
command legitimately is.

- `loam help` / `loam --help` / `loam -h` (no command given, or a bare help token as the
  first argument) print a static, unfiltered listing of every top-level command (and, for
  a group like `work` or `graph`, its subcommands), one line each with its summary. This
  listing is **not** filtered to the caller's role — it cannot be, since it runs before
  any identity is resolved — so it explicitly names `loam instructions` as the authority
  on which of these a given role may actually use.
- `loam <command> [<subcommand>] --help`/`-h` (a help token anywhere among that command's
  own arguments — flags may appear anywhere, see Argument Ordering above) prints that one
  command's real usage line, in this order: positional arguments, a trailing `[flags]`
  **only** when the command actually registers at least one flag, and — last, after flags —
  a stdin note where one applies. That order matches this document's own synopsis lines
  (the parenthetical stdin note always trails the whole shape, flags included; see e.g.
  `work set` below), so a copied usage line reads the same way this document's does. The
  full command listing — summary and registered flags — follows, the flags rendered from the
  same `pflag.FlagSet` the real command parses with. The positional/stdin portion comes from
  `internal/cmdspec`, the same shared tables `loam instructions <command>`'s `synopsis` field
  is built from, so the CLI's own `--help` and the server's answer cannot disagree on the
  underlying shape. The suggested `loam instructions <command>` line it prints is always
  runnable verbatim, including quoting: a subcommand's name contains a space (e.g.
  `work start`), so it is printed pre-quoted (`loam instructions "work start"`) rather than
  left for the reader to work out.
- `loam <group> --help`/`-h`/`help` (e.g. `loam work --help`) lists that group's
  subcommands the same way the top-level listing does. A bare `loam work` with no
  subcommand and no help token is unaffected by any of this — it is still the usual usage
  error (exit `2`, "work requires a subcommand"), not an implicit help listing.

Help output is always plain text on stdout, regardless of `LOAM_OUTPUT_FORMAT` — it is
usage guidance, not a data payload for an agent to parse in a chosen structured format.

### Environment Variables

The complete set of environment variables the CLI reads. All are **required except
`LOAM_OUTPUT_FORMAT`**, with two further exceptions, one per direction: `whoami` without
`--verify` does not require `LOAM_SERVER_URL` (see `whoami` below — "Local only, no server
call" means exactly that), and `instructions` does not require the three `LOAM_AGENT_*`
variables, falling back to the well-known orchestrator identity when *none* of them is set
(see `instructions` below). Identity & role feed the authorization model described in
README → Agent Identity & Roles. Names are provisional.

| Variable | Purpose | Required | Default |
| --- | --- | --- | --- |
| `LOAM_SERVER_URL` | Base URL of the Loam server — the Connect APIs and the git smart-HTTP endpoint (`clone` composes `<LOAM_SERVER_URL>/git/<group>/<repo>.git`). A URL (not host/port) so future transports like local sockets can be expressed via scheme. | yes, except bare `whoami` | — |
| `LOAM_AGENT_NAME` | Agent name, a `<first-name>-<last-name>` combination. | yes, except `instructions` (all three, or none) | — |
| `LOAM_AGENT_ID` | Agent ID; combined into the identifier `<name>-<id>-<role>`. | yes, except `instructions` (all three, or none) | — |
| `LOAM_AGENT_ROLE` | Agent role; determines allowed operations and `instructions` output. | yes, except `instructions` (all three, or none) | — |
| `LOAM_OUTPUT_FORMAT` | Output format: `json`, `yaml`, `xml`, or `human`. Unknown values fall back to `json`. | no | `json` |

On the wire, the `LOAM_AGENT_*` values travel as the request headers `Loam-Agent-Name`,
`Loam-Agent-Id`, and `Loam-Agent-Role` — attached by the CLI to every RPC, and written
into a clone's git config by `clone` so plain git carries them too (see
`docs/git-spec.md`).

Every applicable missing or malformed required variable is reported together in one run,
not one per run: the CLI validates all of them before failing, so an operator setting up a
fresh workspace with nothing configured at all learns everything wrong from a single
invocation.

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

**With no identity configured**, this is the one command that still answers. When none of
`LOAM_AGENT_NAME`, `LOAM_AGENT_ID` or `LOAM_AGENT_ROLE` is set, the CLI resolves a fixed,
well-known identity — name `loam-orchestrator`, id `0`, role `orchestrator`, identifier
`loam-orchestrator-0-orchestrator` — and makes the same authenticated call it always makes,
returning the built-in **orchestrator** role's instructions and its capability-filtered
command list (`graph.query` and `search`, plus the ungated `instructions`/`whoami`; see
`docs/web-spec.md` → RoleService and [`orchestration.md`](orchestration.md)). Three things
this deliberately is not:

- **Not an unauthenticated route.** The request carries `Loam-Agent-*` headers and travels
  the ordinary authenticated path; `/healthz` and `/readyz` remain the server's only
  unauthenticated endpoints.
- **Not an unfiltered command list.** The response is one narrow role's own commands. An
  agent cannot read it as a listing of everything the CLI can do — that is `loam help`,
  which needs no environment at all.
- **Not a relaxation of `LOAM_SERVER_URL`.** It stays required: the CLI cannot invent where
  the server is, and this command makes a real RPC. With it unset the command still fails
  as a usage error naming *only* `LOAM_SERVER_URL`, not the identity variables it no longer
  needs.

The identity values are fixed in the binary and not configurable — what varies between
deployments is the orchestrator role's text and granted operations, and those are an
ordinary editable role row. Setting `LOAM_AGENT_*` is itself the escape hatch: with any of
the three set, all three are required and the command behaves exactly as it always has,
answering for that role. A *partial* identity is therefore a configuration error, not an
orchestrator, and is reported as one. `whoami` is deliberately unchanged: it reports the
identity an operator configured, and still fails when none is.

**Output** (JSON; minimal by design — identity is deliberately excluded, see `whoami`):

```json
{
  "usage": "…overall usage and conventions…",
  "commands": [
    { "name": "work start", "summary": "Start a work branch from a target branch.", "synopsis": "<repo> <from>" }
  ],
  "role_instructions": "…configured for this role…"
}
```

`synopsis` is each command's positional-argument shape (docs/cli-spec.md's own `<...>`/`[...]`
convention below), plus a trailing note when the command also reads a body from stdin (see
`work set`, `work comment`, `work reply` below), in that order — positional shape, then the
stdin note last, matching this document's own synopsis lines (e.g. below: `` `loam work set
[repo] [work-branch] [--title <title>]` (optional description read from stdin) ``). It is
sourced from `internal/cmdspec`'s shared tables, the same ones `loam <command> --help` (see
Help below) builds its usage line from, so the two cannot disagree on the underlying shape —
`--help`'s usage line additionally inserts a `[flags]` token between the positional shape and
the stdin note when the command registers any flags, which this JSON field has no equivalent
for (it carries no flag detail at all, gated or not). `synopsis` is empty for a command with
neither positional arguments nor a stdin note (e.g. `work list`).

**Errors:** exit `1` if the server is unreachable while fetching role instructions. Exit `2`
if `LOAM_SERVER_URL` is missing or malformed — the only variable this command requires when
no identity is configured.

### whoami

Reports the calling agent's identity and role as resolved from the environment. Split out
of `instructions` so identity can be fetched on its own without the larger payload.

**Synopsis:** `loam whoami [--verify]`

**Arguments:** none.

**Flags:**

- `--verify` *(optional)* — additionally confirm the configured role actually resolves on
  the server, over the same call `instructions` already makes (no separate RPC exists for
  this). Opt-in only: without it, `whoami` makes no server call at all.

**Behavior:** Reads the `LOAM_AGENT_*` environment variables and returns the resolved
identity. By default this is local only — no server call — which is what lets `whoami`
still answer when every other command is failing (e.g. a misconfigured role makes every
gated command answer "unauthorized"/"internal error", but `whoami` alone still reports who
the agent is configured as, which is often the fastest way to isolate the cause). `--verify`
trades that guarantee, on demand, for confirmation the role is actually valid.

**Output** (JSON):

```json
{ "name": "ada-lovelace", "id": "7", "role": "reviewer", "identifier": "ada-lovelace-7-reviewer" }
```

With `--verify` and a role that resolves, the response additionally carries `"verified":
true`; without `--verify`, or when it fails, the field is absent — never `"verified":
false`, since a failed verification is a non-zero exit and no output at all, not a "checked
and failed" flag alongside the identity:

```json
{ "name": "ada-lovelace", "id": "7", "role": "reviewer", "identifier": "ada-lovelace-7-reviewer", "verified": true }
```

**Errors:**

- exit `2` if a required identity variable is missing or malformed.
- With `--verify`: exit `2` (`unauthorized`) if the configured role does not resolve on the
  server — a denial, not a "not found", so the response cannot be used to enumerate valid
  role names. Exit `2` (`usage`) if `--verify` is given but `LOAM_SERVER_URL` is not
  configured. Exit `1` if the server is unreachable — deliberately a different code than a
  rejected role, so the two failure modes stay distinguishable instead of both collapsing
  into an opaque failure.

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
identity so commits are attributed, writes the agent identity headers into the
clone's git config so every subsequent git operation carries them, and writes the
`remote.origin.push` / `remote.origin.fetch` refspecs that map a work branch's bare name
onto its server-owned ref path (`docs/git-spec.md` → The CLI's Role). Unlike the rest of
the bootstrap, **those refspecs are load-bearing**: without them plain `git push` from
the clone is rejected, so a hand-rolled `git clone` no longer works for pushing.

A `branch` the repo does not report as a target branch is taken to be a work branch and
cloned from its server-owned ref; `clone` renames the resulting local branch back to the
bare name, so the checked-out branch is `wb-9c2f1a`, as every other command expects.

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
resets a `reviewable`/`reviewed` branch to `draft` and flags it as conflicted; a push
that catches the branch up to its target returns it to `reviewable` automatically, with no
`request-review` — and it is that restore, which opens a fresh round, that marks the
prior round's verdicts stale (staleness is derived from the round number, so nothing
marks them at demotion time) (see `docs/git-spec.md` → Target Advances & Catch-Up).

Commands that act on an existing work branch take `<repo>` and `<work-branch>` (its name) as
positional arguments; both are optional when run from inside the repo directory (inferred
per the Workspace rules above) and required otherwise.

#### State gates

Reads always work; writes require a state where the write means something:

| Command | Allowed states | Rejected (exit `2`, `precondition_failed`) |
| --- | --- | --- |
| `set` | `draft`, `reviewable`, `reviewed` | `complete`, `closed` |
| `request-review` | `draft`, `reviewed` | `reviewable`, `complete`, `closed` |
| `verdict` | `reviewable`, `reviewed` | `draft` (no round exists yet), `complete`, `closed` |
| `reply` | `draft`, `reviewable`, `reviewed` | `complete`, `closed` |
| `show`, `diff`, `comments`, `verdicts`, `list` | any state | — |

Two supporting rules:

- `comment` stages **locally** and is state-ungated: the CLI checks that the work branch
  exists, nothing more — the state gate lives at the only server boundary, `verdict`.
- Staged comments are inert local data and **survive round changes** (anchors may drift,
  as in any review tool; review them with `comments --staged` before submitting). Against
  a terminal branch, `verdict` fails with the precondition error and the staged items
  remain until `--discard`ed — no automatic cleanup.

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

**Errors:** exit `2` if neither title nor description is provided, or the work branch is
in a terminal state; exit `3` if it does not exist.

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

**Synopsis:** `loam work list [--repo <repo>] [--author <id>] [--target <branch>] [--awaiting-review] [--state <state>] [--limit <n>]`

**Input:**

- `--repo <repo>` *(optional)* — limit to one enrolled repo.
- `--author <id>` *(optional)* — limit to work branches authored by this agent identifier.
- `--target <branch>` *(optional)* — limit to work branches targeting this branch.
- `--awaiting-review` *(optional)* — limit to reviewable work branches awaiting the calling
  agent's verdict.
- `--state <state>` *(optional)* — `draft` / `reviewable` / `reviewed` / `complete` /
  `closed`; defaults to `reviewable`.
- `--limit <n>` *(optional)* — maximum results; defaults to `100`.

With no flags, lists all reviewable work branches across all enrolled repos.

**Behavior:** Returns matching work branches, each identified by its `repo` and `name`.

**Output** (JSON) — `truncated` is set when `--limit` capped the results:

```json
{ "truncated": false,
  "results": [ { "repo": "bobcob7/doc-server", "name": "wb-9c2f1a", "target": "main", "title": "Add login", "author": "grace-hopper-3-author", "state": "reviewable" } ] }
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
  "round": { "number": 2, "requested_by": "grace-hopper-3-author" },
  "latest_verdict": { "outcome": "disapprove", "reviewer": "ada-lovelace-7-reviewer", "round": 2, "stale": false } }
```

`round` is omitted entirely (not `{ "number": 0 }`) for a work branch with no review round
yet — e.g. still `draft`, before the first `request-review`.

`latest_verdict` is the single most recent verdict overall — across all reviewers and rounds,
including stale ones, and when several reviewers voted in the same round, the most recently
cast of them — NOT whether the branch was approved: `state` reports workflow position (a round
closed and a verdict landed), the same `"reviewed"` after an approve or a disapprove, and
`latest_verdict` is what distinguishes the two. It carries `outcome`, `reviewer`, `round`, and
`stale` together, and is omitted entirely (not a zeroed object) for a work branch with no
verdicts yet, matching `round`'s convention. This costs one extra RPC (`ListVerdicts`) beyond
the metadata fetch.

**Errors:** exit `3` if the work branch does not exist; exit `2` if the identifier cannot be
resolved (not in a clone and arguments omitted).

#### diff
Return the work branch's diff against its target, separately from `show` to keep both small.

**Synopsis:** `loam work diff [repo] [work-branch]`

**Input:** `repo` and `work-branch` positional arguments identify the work branch (see the
convention above).

**Output:** the unified diff of the work branch against its target branch, as a field in the
active `LOAM_OUTPUT_FORMAT` (e.g. `{ "diff": "…" }` for JSON). In `human` mode this is the
one exception to that wrapping: the diff is written verbatim, with no field wrapper and no
added or stripped trailing newline, so it can be read directly or piped to a pager instead
of requiring the caller to unwrap the JSON first.

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
- **stdin** — the comment body. Required unless only `--resolve` or `--discard` is given. A
  lone `--resolve` or a lone `--discard` (see below) never reads stdin at all — not even to
  check whether one was piped — so it cannot hang on an interactive or un-redirected stdin,
  and a body piped alongside either is silently ignored rather than attached or rejected.
- `--file <path>` + `--line <n>` *(optional)* — anchor the new thread to a line.
- `--resolve <thread-id>` *(optional)* — mark a thread resolved. Only the thread's original
  author may resolve it. Used alone (no `--file`/`--line`), it never reads stdin, so it
  cannot carry a body — any body piped alongside a lone `--resolve` is silently ignored.
  Combined with `--file`/`--line` it is a new anchored comment and still reads and attaches
  the body as usual.
- `--edit <staged-id>` *(optional)* — replace the body of a previously staged comment (new
  body from stdin), before it is submitted.
- `--discard <staged-id>` *(optional)* — remove a staged comment from the staging area. Never
  reads stdin; any body piped alongside it is silently ignored, not rejected.

The modes are mutually exclusive: a single invocation either opens a new thread (top-level or
`--file`/`--line`-anchored), `--edit`s a staged comment, or `--discard`s one. `--resolve` may
accompany a new anchored comment or stand alone.

**Behavior:** Operates on the caller's **local staging area** for this work branch (in
`.loam`). New-thread comments and resolves append to it; `--edit` and `--discard` modify or
remove an already-staged item. Staged items accumulate across invocations and stay invisible
to everyone else until `verdict` publishes them.

**Staging location:** the workspace's `.loam/` directory (see Workspace above), keyed by
repo, work branch, and agent — outside any clone, so reviewers who never clone can still
stage.

Every staging read, write, and directory creation is confined to that directory at the
syscall level (`os.Root`), so a symlinked component anywhere in `.loam/staging/…` — planted
before or after the key was validated — fails the operation instead of relocating it. The
repo/work-branch key checks are lexical and cannot see symlinks, so they are not what
provides this; they exist to reject a malformed key with a precise usage error (exit `2`).
The CLI never exposes a staging path string to write to directly.

**Staging format:** one `staged.json` document per `(repo, work-branch, agent)` staging
directory — written by `comment`, read by `comments --staged`, published and cleared by
`verdict`:

```json
{ "version": 1, "next_id": 4,
  "items": [ { "id": "s3", "file": "auth.go", "line": 42, "body": "…", "resolve": "t1" } ] }
```

Every field of an item except `id` is optional: a top-level comment has no `file`/`line`, a
resolve-only item has no `body`, and a plain comment has no `resolve`. An item carries **no
round** — staged items are inert local data that survive round changes, and the round is
assigned only when `verdict` publishes them. `next_id` is persisted rather than derived from
the items, so an id freed by `--discard` is never handed out again and a `--edit s3` in a
later invocation always addresses the same comment the agent read earlier.

**Output** (JSON) — the staged item with a local staging id:

```json
{ "staged": true, "id": "s3", "file": "auth.go", "line": 42, "body": "…" }
```

`staged` is `false` for a `--discard`, which reports the item it removed.

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

**Errors:** exit `3` if the work branch or thread does not exist; exit `2` if the
identifier cannot be resolved or the work branch is in a terminal state.

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

**Errors:** exit `2` on a missing or invalid outcome, if the work branch is not open
for review (`draft` or a terminal state — see State gates), or if any staged comment's
`--file`/`--line` anchor is invalid **at the work branch's current tip** — a nonpositive
line, a line beyond the file's actual length (the error names that length), or a file not
present there at all. This check runs server-side, against the mirror, not against
whatever the CLI staged against: a comment staged when the anchor was valid can still be
rejected here if the author pushed a shrinking change during review. A rejected anchor
fails the **whole** verdict — nothing in the batch publishes, and every staged item
(including the ones with valid anchors) stays in the local staging area for the reviewer
to fix or drop and resubmit. Exit `3` if the work branch does not exist.

Once a work branch is `reviewed` with at least one approve verdict, it becomes a proposal in
the admin's queue (see `docs/web-spec.md` → ProposalService). There is no agent `complete`
command — the admin creates the upstream PR, and the work branch flips to `complete` only
when that PR merges.

### Graph DB queries

Structural queries over the Tree-sitter graph (see README → Graph DB). A fixed set of
subqueries rather than a query language.

**Synopsis:** `loam graph <subquery> <target> [--repo <repo>] [--all] [--file <path>] [--limit <n>]`

Each subquery below is its own runnable command and its own `loam instructions` catalog
entry (`graph def`, `graph refs`, `graph deps`, `graph dependents`, `graph history`) — this
section covers all five together since they share everything but `<subquery>` itself.

**Subqueries:**

- `def <symbol>` — where the symbol is defined.
- `refs <symbol>` — everywhere the symbol is referenced.
- `deps <file|symbol>` — what the target depends on.
- `dependents <file|symbol>` — what depends on the target (blast radius).
- `history <symbol>` — the symbol's commit/ref history.

**Ambiguity:** symbol targets are name-based and approximate by design, so an ambiguous
target (three `Login`s in three files) is **data, not an error**: the query operates on
every matching symbol and returns the union, each result row naming its match in `of`
(`{ symbol, file, kind }`, present only when the target was ambiguous). `--file <path>`
narrows the target to the definition in one file. Richer filters are Future Work
(see README).

**Limit:** `--limit <n>` caps the result rows (default `50`). A capped response sets
`truncated: true` in the envelope, so an agent never mistakes a partial answer for a
complete one.

**Result shapes** — the `results` rows per subquery:

| Subquery | Row |
| --- | --- |
| `def` | `{ repo, file, line, symbol, kind }` — one per matching definition |
| `refs` | `{ repo, file, line, symbol }` |
| `deps` | `{ repo, symbol, file, line, kind }` — what the target depends on |
| `dependents` | `{ repo, symbol, file, line, kind }` — the transitive blast radius |
| `history` | `{ repo, symbol, file, commit, ref, message }` |

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
  "truncated": false,
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

**Output** (JSON) — the same envelope as `graph` (`ingested` for staleness per queried
repo, `truncated` when `--limit` capped the results):

```json
{ "ingested": [ { "repo": "bobcob7/doc-server", "target": "main", "ref": "a1b2c3d", "at": "2026-07-25T12:00:00Z" } ],
  "truncated": false,
  "results": [ { "repo": "bobcob7/doc-server", "file": "auth.go", "lines": [40, 58], "score": 0.82, "snippet": "…" } ] }
```

**Errors:** exit `2` on bad arguments or unresolvable scope.

## Open Questions

None currently open.
