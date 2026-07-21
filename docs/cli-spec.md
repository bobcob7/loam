# CLI Spec

Specification for the Loam CLI — the single agent-facing tool that talks to the
server (see the root `README.md`). This document specifies its command surface.

Status: **draft / in progress.** Sections marked TODO are not yet specified.

## Conventions

The binary is invoked as `loam`. Configuration is entirely through environment variables
(see below) — the CLI has **no global flags**.

### Output

Output is **JSON by default** — the CLI is agent-first, so structured output is the norm
and the format agents should parse. `LOAM_OUTPUT_FORMAT` (see Environment Variables) selects the
format: `json` (default), `yaml`, `xml`, or `human` for interactive use. This applies
globally to every command.

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
| `LOAM_OUTPUT_FORMAT` | Output format: `json`, `yaml`, `xml`, or `human`. | no | `json` |

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

Returns the role-specific instructions for the calling agent, **plus a general explanation
of how to use the CLI** — overall usage, the conventions above, and the set of commands
available to the caller's role. Intended as the first command an agent runs to orient
itself.

TODO — args, output shape, how the general-usage portion is sourced vs. the role-specific
portion (which comes from role config in the web console).

### Git

#### clone
Clone an enrolled repo from the server (server as sole remote), optionally at a branch.

TODO — args (repo, branch), behavior, output.

#### branch (start feature branch)
Start a feature branch from a target branch.

TODO — args, naming rules, whether this is server-side, local, or both.

#### commit / push
Work, commit, and push feature-branch changes back to the server.

TODO — decide how much is thin git passthrough vs. dedicated commands.

### Local PRs

#### create
Open a PR from a feature branch to a target branch. Requires title + description;
validated against the repo's JSON schema if configured.

TODO — args, schema-validation behavior, output.

#### list
List reviewable PRs, filterable by repo, target branch, author, and awaiting-my-review.

TODO — flags, output columns/shape.

#### get (details)
Return full PR: title, description, source/target branches, diff, review state, threads.

TODO — args, output shape.

#### comments (get)
Fetch comment threads for a PR (body, file/line anchor, author, resolved state).

TODO — args, output shape.

#### comment (add)
Create a new comment (optionally anchored to file/line) or reply to a thread; may mark a
thread resolved. Staged locally until `review submit`.

TODO — args, staging model, local storage location.

#### review submit
Publish staged comments atomically with an outcome: approve / disapprove / neutral.

TODO — args, what happens to staged state, completion-bar interaction.

#### complete
Mark a PR complete once it has ≥1 approval; surfaces the proposed upstream PR to the admin.

TODO — confirm this is a distinct command vs. implicit, args, preconditions.

### Graph DB queries

Query the structural graph (symbols, references, dependencies, blast radius).

TODO — command shape (subcommands vs. query language), the query set, output.

### RAG queries

Natural-language semantic search over ingested docs/code.

TODO — command shape, args, output (chunks + provenance).

## Open Questions

TODO — collect spec-level open questions here as they come up.
