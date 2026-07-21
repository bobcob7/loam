# CLI Spec

Specification for the Loam CLI — the single agent-facing tool that talks to the
server (see the root `README.md`). This document specifies its command surface.

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
(`<workspace>/<repo_name>`), the `repo` and `branch` arguments are inferred — repo from the
directory name, branch from the current git branch — so they may be omitted from **any**
command that takes them.

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
| `LOAM_SERVER_URL` | URL of the Loam server the CLI talks to. A URL (rather than host/port) so future transports like local sockets can be expressed via scheme. | yes | — |
| `LOAM_AGENT_NAME` | Agent name, a `<first-name>-<last-name>` combination. | yes | — |
| `LOAM_AGENT_ID` | Agent ID; combined into the identifier `<name>-<id>-<role>`. | yes | — |
| `LOAM_AGENT_ROLE` | Agent role; determines allowed operations and `instructions` output. | yes | — |
| `LOAM_OUTPUT_FORMAT` | Output format: `json`, `yaml`, `xml`, or `human`. Unknown values fall back to `json`. | no | `json` |

The CLI also sets `LOAM_INTERNAL` itself when it invokes git (read by the clone-installed
git hooks to tell loam-invoked git from direct git); it is not an operator-configured
variable.

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
{ "error": { "code": "not_found", "message": "PR #42 not found in repo acme/web" } }
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
  "commands": [ { "name": "pr list", "summary": "List reviewable PRs" } ],
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
Clone an enrolled repo from the server (server as sole remote), optionally at a branch.

**Synopsis:** `loam clone <repo> [branch]`

**Arguments:**

- `repo` *(required)* — the enrolled repo identifier, `<group>/<repo_name>` (e.g.
  `bobcob7/doc-server`).
- `branch` *(optional)* — branch to check out; defaults to the repo's target branch.

**Behavior:** Clones the repo from the Loam server into `./<repo_name>` — always the final
path segment of the identifier, with no override (e.g. `bobcob7/doc-server` → `./doc-server`).
The clone's only remote is the Loam server, and its git author (`user.name` / `user.email`)
is set to the agent identity so commits are attributed to the agent. Checks out `branch` if
given, and **pins** the clone to that branch: `commit` and `push` operate only on it, and
switching feature branches means cloning again.

To steer agents through the CLI, `clone` also installs `pre-commit` and `pre-push` git hooks
in the clone that reject direct `git commit` / `git push`. `loam commit` and `loam push` set
a sentinel environment variable (`LOAM_INTERNAL`) when they invoke git; the hooks succeed
only when it is present, so plain git is blocked while loam-invoked git passes.

This is a **soft guard for cooperative agents, not a security boundary** — it can be
bypassed with `git … --no-verify`, by removing the hooks, or by repointing
`core.hooksPath`. Hard enforcement is server-side (a pre-receive check that rejects pushes
not originating from `loam`) and is deferred alongside authentication (see README → Future
Work).

**Output** (JSON):

```json
{ "repo": "bobcob7/doc-server", "path": "./doc-server", "branch": "main" }
```

**Errors:** exit `3` if the repo is not enrolled; exit `2` if `branch` does not exist.

#### branch (start feature branch)
Start a feature branch from a target branch.

**Synopsis:** `loam branch <repo> <name> [from]`

**Arguments:**

- `repo` *(required)* — the enrolled repo identifier, `<group>/<repo_name>`.
- `name` *(required)* — the new feature branch name. Free-form (no enforced prefix).
- `from` *(optional)* — target branch to base off; defaults to the repo's target branch.

**Behavior:** Creates feature branch `name` on the server from `from`. This is a
**server-side** ref creation — no local checkout — after which the agent `clone`s the repo
at `name`. Errors if `name` already exists.

**Output** (JSON):

```json
{ "repo": "bobcob7/doc-server", "branch": "feat/login", "from": "main" }
```

**Errors:** exit `2` if `name` already exists or `from` is not a valid target branch; exit
`3` if the repo is not enrolled.

#### commit
Commit the working copy's changes on the clone's feature branch.

**Synopsis:** `loam commit` (commit message read from stdin)

**Behavior:** Commits all changes in the working copy on the clone's feature branch,
authored as the agent identity (configured by `clone`). Operates **only** on the feature
branch the clone was created for — if HEAD is on any other branch, it errors. Switching to a
different feature branch requires a fresh `loam clone`.

**Output** (JSON):

```json
{ "repo": "bobcob7/doc-server", "branch": "feat/login", "commit": "a1b2c3d" }
```

**Errors:** exit `2` if HEAD is not on the clone's feature branch or there is nothing to
commit; exit `3` if not run inside a clone.

#### push
Push the feature branch's commits to the server.

**Synopsis:** `loam push`

**Behavior:** Pushes the clone's feature branch to the Loam server (its only remote),
attaching the agent identity/role so the server can authorize the push (role-level in the
MVP — see README → Agent Identity & Roles). Operates **only** on the clone's feature branch;
if HEAD is on any other branch, it errors. Switching branches requires a fresh `loam clone`.

**Output** (JSON):

```json
{ "repo": "bobcob7/doc-server", "branch": "feat/login", "pushed": 3 }
```

**Errors:** exit `2` if HEAD is not on the clone's feature branch or the push is not
authorized for the caller's role; exit `3` if not run inside a clone.

### Local PRs

A PR is identified by its `repo` and feature `branch` — there is no numeric ID. Commands
that act on an existing PR take `<repo>` and `<branch>` as positional arguments; both are
optional when run from inside the repo directory (inferred per the Workspace rules above)
and required otherwise.

#### create
Open a PR from the current feature branch to its target branch.

**Synopsis:** `loam pr create --title <title>` (description read from stdin)

**Input:**

- `--title <title>` *(required)* — the PR title.
- **stdin** *(required)* — the PR description. Free text, or a JSON document when the repo
  has a description schema configured (see below).
- `repo`, source branch, and target branch are **not** arguments: repo and source branch
  are inferred from the current working copy, and the target is the branch the feature
  branch was created from (via `branch`). There is no `--to` / `--from`.

**Behavior:** Opens a PR from the current feature branch to its target branch. If the repo
has a description JSON schema configured, the stdin body is validated against it server-side
before the PR is created. Once created, the PR is visible to reviewers via `pr list`.

**Output** (JSON):

```json
{ "repo": "bobcob7/doc-server", "branch": "feat/login", "target": "main", "title": "Add login", "state": "open" }
```

**Errors:** exit `2` on missing title, failed schema validation, or precondition failure
(e.g. the feature branch has no commits / has not been pushed); exit `3` if the repo is not
enrolled.

#### list
List PRs across all enrolled repos.

**Synopsis:** `loam pr list [--repo <repo>] [--author <id>] [--target <branch>] [--awaiting-review] [--state <state>]`

**Input:**

- `--repo <repo>` *(optional)* — limit to one enrolled repo.
- `--author <id>` *(optional)* — limit to PRs opened by this agent identifier.
- `--target <branch>` *(optional)* — limit to PRs targeting this branch.
- `--awaiting-review` *(optional)* — limit to PRs awaiting the calling agent's review.
- `--state <state>` *(optional)* — defaults to `open`.

With no flags, lists all open PRs across all enrolled repos.

**Behavior:** Returns matching PRs, each identified by its `repo` and feature `branch`.

**Output** (JSON array):

```json
[ { "repo": "bobcob7/doc-server", "branch": "feat/login", "target": "main", "title": "Add login", "author": "grace-hopper-3-author", "state": "open" } ]
```

**Errors:** exit `2` on a bad filter value. An empty result is a normal exit `0`.

#### get (details)
Return a PR's metadata, description, and review state. The diff and comment threads are
fetched separately via `pr diff` and `pr comments`, to keep each response small.

**Synopsis:** `loam pr get [repo] [branch]`

**Input:** `repo` and `branch` positional arguments identify the PR (see the identifier
convention above).

**Output** (JSON):

```json
{ "repo": "bobcob7/doc-server", "branch": "feat/login", "target": "main", "title": "Add login",
  "description": "…", "state": "open", "author": "grace-hopper-3-author" }
```

**Errors:** exit `3` if the PR does not exist; exit `2` if the identifier cannot be resolved
(not in a clone and arguments omitted).

#### diff
Return the PR's diff, separately from `pr get` to keep both responses small.

**Synopsis:** `loam pr diff [repo] [branch]`

**Input:** `repo` and `branch` positional arguments identify the PR (see the identifier
convention above).

**Output:** the unified diff of the feature branch against its target branch. Rendered as a
field in the active `LOAM_OUTPUT_FORMAT` (e.g. `{ "diff": "…" }` for JSON) so output stays
consistent with every other command.

**Errors:** exit `3` if the PR does not exist; exit `2` if the identifier cannot be resolved.

#### comments (get)
Fetch the comment threads for a PR, or the caller's own staged comments.

**Synopsis:** `loam pr comments [repo] [branch] [--staged]`

**Input:**

- `repo`, `branch` positional — identify the PR (see the identifier convention above).
- `--staged` *(optional)* — return the caller's locally staged (unpublished) comments for
  this PR from `.loam` instead of the published threads.

**Behavior:** By default returns the PR's published threads — each with its resolved state,
optional file/line anchor, and the comments within it. Published threads only; the caller's
staged comments are excluded until submitted. With `--staged`, returns those staged items
instead (the shape produced by `comment`), so an agent can review what it is about to
submit.

**Output** (JSON array) — published threads by default:

```json
[ { "id": "t1", "resolved": false, "file": "auth.go", "line": 42,
    "comments": [ { "author": "ada-lovelace-7-reviewer", "body": "…" } ] } ]
```

**Errors:** exit `3` if the PR does not exist; exit `2` if the identifier cannot be resolved.

#### comment (add)
Stage a comment on a PR locally. Nothing is published until `pr review` submits.

**Synopsis:** `loam pr comment [repo] [branch] [--file <path> --line <n>] [--reply <thread-id>] [--resolve <thread-id>] [--edit <staged-id>] [--discard <staged-id>]` (body read from stdin)

**Input:**

- `repo`, `branch` positional — identify the PR (see the identifier convention above).
- **stdin** — the comment body. Required unless only `--resolve` or `--discard` is given.
- `--file <path>` + `--line <n>` *(optional)* — anchor a new comment to a line.
- `--reply <thread-id>` *(optional)* — add the comment to an existing thread rather than
  starting a new one.
- `--resolve <thread-id>` *(optional)* — mark a thread resolved. Only the thread's original
  author may resolve it. May carry a reply body or be used alone.
- `--edit <staged-id>` *(optional)* — replace the body of a previously staged comment (new
  body from stdin), before it is submitted.
- `--discard <staged-id>` *(optional)* — remove a staged comment from the staging area.

The modes are mutually exclusive: a single invocation either opens a new thread
(top-level or `--file`/`--line`-anchored), `--reply`s to one, `--edit`s a staged comment,
or `--discard`s one. `--resolve` may accompany a new comment or reply, or stand alone.

**Behavior:** Operates on the caller's **local staging area** for this PR (in `.loam`). New
comments, replies, and resolves append to it; `--edit` and `--discard` modify or remove an
already-staged item. Staged items accumulate across invocations and stay invisible to
everyone else until `pr review` publishes them.

**Staging location:** the workspace's `.loam/` directory (see Workspace above), keyed by
repo, branch, and agent — outside any clone, so reviewers who never clone can still stage.

**Output** (JSON) — the staged item with a local staging id:

```json
{ "staged": true, "id": "s3", "file": "auth.go", "line": 42, "body": "…" }
```

**Errors:** exit `2` on conflicting modes (e.g. `--file` with `--reply`), a missing body
when one is required, or attempting to resolve a thread the caller did not author; exit `3`
if the PR, referenced thread, or referenced staged comment does not exist.

#### review
Publish the caller's staged comments for a PR atomically, with an overall outcome.

**Synopsis:** `loam pr review [repo] [branch] --outcome <approve|disapprove|neutral>`

**Input:**

- `repo`, `branch` positional — identify the PR (see the identifier convention above).
- `--outcome <approve|disapprove|neutral>` *(required)* — the overall review outcome.

**Behavior:** Publishes all of the caller's locally staged comments for this PR in one
atomic action, attaches the outcome, and clears the local staging area. This is the only
point at which staged comments become visible on the PR. An `approve` outcome counts toward
the completion bar (≥1 approval). Submitting with no staged comments is allowed (an
outcome-only review). Re-submitting records a fresh review and outcome from that reviewer.

**Output** (JSON):

```json
{ "repo": "bobcob7/doc-server", "branch": "feat/login", "outcome": "approve", "published": 3 }
```

**Errors:** exit `2` on a missing or invalid outcome; exit `3` if the PR does not exist.

#### complete
Mark a PR complete and surface it to the admin as a proposed upstream PR.

**Synopsis:** `loam pr complete [repo] [branch] [--title <title>]` (optional description read from stdin)

**Input:**

- `repo`, `branch` positional — identify the PR (see the identifier convention above).
- `--title <title>` *(optional)* — proposed upstream PR title; defaults to the PR's title.
- **stdin** *(optional)* — proposed upstream PR description; defaults to the PR's
  description.

**Behavior:** Marks the PR complete, which requires **at least one approval**. Records the
proposed upstream title/description (the PR's own unless overridden) and surfaces the PR in
the web interface for the admin to accept or comment on. Completion does **not** itself open
the upstream PR — that happens only on admin acceptance (see README → Workflow).

**Output** (JSON):

```json
{ "repo": "bobcob7/doc-server", "branch": "feat/login", "state": "complete" }
```

**Errors:** exit `2` if the PR has fewer than one approval (precondition failed); exit `3`
if the PR does not exist.

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

**Output** (JSON array) — shape depends on the subquery; e.g. `refs`:

```json
[ { "repo": "bobcob7/doc-server", "file": "auth.go", "line": 42, "symbol": "Login" } ]
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

**Output** (JSON array):

```json
[ { "repo": "bobcob7/doc-server", "path": "auth.go", "lines": [40, 58], "score": 0.82, "snippet": "…" } ]
```

**Errors:** exit `2` on bad arguments or unresolvable scope.

## Open Questions

None currently open.
