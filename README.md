# Loam

This is a centralized code repo with a CLI tool and code-intelligence services.

### Purpose

I find it cumbersome to use GitHub or other centralized code repository systems.
Github has a lot of features that seem more like metadata about the content, not enriching the metadata.

Loam is a single self-hosted server that mirrors enrolled upstream repos and
exposes them to AI agents through one consistent tool. Agents do all their work —
cloning, branching, committing, opening and reviewing PRs, querying code — against
this server, never directly against the upstream forge. A human admin enrolls repos and
gives the final sign-off before anything is pushed upstream.

Upstream forges are supported behind a common provider interface. **Forgejo is the MVP
target**; **GitHub is a close-behind secondary**. Provider-specific details live behind that
interface so the agent-facing workflow is identical regardless of forge.

The provider interface only needs to abstract two things: the **access token** (each forge
issues and scopes its own) and the **REST API** used to open upstream PRs. Git transport
itself is forge-agnostic — SSH key auth and clone/fetch/push work the same everywhere — so
it sits outside the provider interface.

### Components at a glance

- **Server** — the single source of truth. Holds the mirrored git repos, the graph DB,
  the vector DB, and local PR state. It is the only git remote agents ever see.
- **CLI tool** — the agent-facing interface. One binary that talks to the server; all
  workflow, querying, and authorization flow through it.
- **Web interface** — the admin-facing interface. Never used by agents. Used to enroll
  repos, manage upstream credentials, and approve proposed upstream PRs.

### CLI Tool

Local CLI tool that sets up the Git repo and enables consistent workflow and querying and enforces authorization.

A single tool talks to the central server and is the only path agents use to reach it,
which keeps the workflow (clone → branch → commit → PR → review) consistent and auditable.
Every call carries the agent's identity and role (below), which the server uses to
authorize the requested operation. Its command surface is specified in
[`docs/cli-spec.md`](docs/cli-spec.md).

#### Agent Identity & Roles

Each agent has an assigned name, ID, and role, supplied through environment variables:

- **Name** — a random `<first-name>-<last-name>` combination.
- **Identifier** — `<name>-<id>-<role>`.
- **Role** — determines which operations the agent may perform and what the CLI
  `instructions` command returns to it.

Roles are configurable in the web console but ship with sane defaults. Authorization is
role-based and global: a role grants the same operations across every enrolled repo, with
no per-repo or per-branch scoping in the MVP.

For the MVP there is no authentication — identity and role are trusted exactly as asserted
in the environment, and the server does not verify them. Verified, server-issued
credentials and finer-grained scoping are planned (see Future Work).

#### Graph DB

Code is digested upon merge to specific branches. AST is run on the code to find and record both dependencies and ref history.
This information can be queried through the CLI tool.

The ingest step parses each source file into an AST — using **Tree-sitter**, with one
grammar per language, so language coverage is a plugin matrix: adding a language means
dropping in a grammar rather than changing the ingest core. It records symbols (functions,
types, modules), the files they live in, and the dependency edges between them, along with
the commit/ref history for each symbol. Agents query this to answer structural questions
without reading the whole tree: where a symbol is defined, what references it, what it
depends on, and the blast radius of a proposed change. Re-ingested whenever a target branch
advances.

The MVP ships a starter set of grammars (e.g. TypeScript/JavaScript, Python, Go). At this
layer cross-file symbols and dependency edges are approximate; precise reference resolution
is planned (see Future Work).

#### RAG

Documentation in the code, and the code itself is chunked and ingested into a vector DB upon merge to specific branches.
This information can be queried through the CLI tool.

On merge to a target branch, docs and code are split into semantically meaningful chunks
(a doc section, a function, a class), embedded, and stored in a vector DB. Agents run
natural-language queries through the CLI to pull the most relevant chunks as context —
"how is auth handled here", "where do we format PR descriptions" — instead of grepping
blind. The graph DB answers *structural* questions; RAG answers *semantic* ones.

#### Local PRs

PRs live on the Loam server, not upstream. Agents open them, other agents review
them, and only after the admin approves does anything reach the upstream forge. The CLI
exposes the full review loop below. Reviewer selection and lifecycle are handled by an
external orchestrator, not by Loam — agents discover PRs awaiting their review by
polling List.

##### List
Using the CLI tool, agents will be able to list reviewable PRs.

Returns open PRs with basic status, filterable by repo, target branch, author, and
whether the PR is awaiting the calling agent's review.

##### Get Details
Another command will pull the details of the PR.

Returns the full PR: title, description, source/target branches, the diff, current review
state, and the comment threads.

##### Comment
Another command will pull individual PR comments.

Fetches comment threads for a PR — the comment body, the file/line it anchors to, author,
and whether the thread is resolved — so an agent can work through review feedback item by
item.

##### Add Comment
Another command will either reply to a PR comment, or create a new one.

Creates a new comment (optionally anchored to a file/line) or replies to an existing
thread, and can mark a thread resolved. Comment and response shape can be constrained by
the admin's JSON schema (see Git → Local PRs).

Comments are **staged locally on the CLI**, not sent to the server as they are written. A
reviewer accumulates their comments across the diff and nothing is visible on the PR until
they submit (see Submit Review).

##### Submit Review
Publishes a reviewer's staged comments to the server in one atomic action, together with an
overall outcome: **approve**, **disapprove**, or **neutral**. This is the only point at
which staged comments become visible on the PR. Approvals gate completion: a PR needs **at
least one approval** before it can be marked complete and surfaced to the admin as a
proposed upstream PR.

### Git

The CLI tool can be used to clone in a repo from the main server with the main server as it's only remote.

The server keeps a mirror of each enrolled upstream repo. Agents clone from the server and
push back to it; the server is their sole remote. Syncing with the real upstream is the
server's job, not the agent's, so agents work in a closed, controlled loop.

Upstream is authoritative. When the server polls upstream and a mirrored branch has
diverged, it always takes upstream — the local copy is reset to match, with no
local-vs-upstream merge or conflict resolution. This is also how merged work flows back:
once an upstream PR is merged, the next sync advances the target branch in the mirror,
which is what triggers graph/RAG ingest.

#### Enrollment

In the web interface. The admin should be able to enter in an access token (Forgejo for MVP, GitHub PAT for GH) that enables cloning repos.
The admin should also be able to enroll repos, which will clone it to the server and periodically keep it up to date.

The admin supplies upstream credentials once, then enrolls repos by name. On enrollment the
server clones the repo locally and thereafter polls upstream on a schedule to keep the
mirror and the enrolled target branches current.

#### Clone

The CLI tool will have a git clone command that will clone in the repo at specific branch if specified.

Clones an enrolled repo from the server, checking out a given branch when one is specified
and the target branch otherwise. The resulting clone has the server set as its only remote.

#### Local PRs

The CLI tool can be used to create PRs from specific branches to target branches.
When a PR is opened, a title, and description is required.
Upon opening a PR, open PRs will show up on the CLI tool
A JSON schema can be setup by the admin from the web interface to enforce PR description format and comment/response formats.

Creating a PR requires a title and description; the PR then becomes visible to other agents
for review via the List/Get Details commands. The admin can register a JSON schema per repo
that the server validates PR descriptions and comments/responses against, so every PR and
review follows a consistent, machine-checkable format.

Local PRs never merge into the server's branches. Accepting a completed PR only opens the
corresponding upstream PR against the target branch; the server's target branch advances
solely by pulling merged changes back from upstream (see mirror sync above).

### Web Interface

#### Auth

The interface is never intended for agents to access.
The admin will provide username and password on server startup.
These credentials will use basic HTTP Auth. A single admin credential is sufficient for the
MVP; multiple admin accounts are future work.

#### Repos

##### Auth

Admin will be able to set an upstream access token (Forgejo token for MVP, GitHub PAT for GH) or generate an SSH key pair and give a public key to install upstream.

The token is what varies by forge and is the only credential the provider interface cares
about — it authorizes the REST calls that open upstream PRs. The SSH key pair is
forge-agnostic and only covers git transport (clone/fetch/push); it is not part of the
provider interface.

##### Enroll

The admin should be able to enroll repos, by upstream URL.

Target branches can be specified. These branches are eligable for PR creation.

### Workflow

#### Enroll Repo
1. Admin logs into web interface
2. Admin adds an upstream access token (Forgejo token for MVP, GitHub PAT for GH). The token is confirmed to work and to have the appropriate permissions.
3. Admin types in exact repo name <group>/<repo_name>.
4. Server clones the repo locally.
5. Admin specifies target branches.
6. Server ingests data from the target branches.

#### Feature Work
1. Feature is defined by user.
2. Agent will use CLI to start a feature branch from target branch.
3. Agent will use CLI Clone the repo with the feature branch.
4. Agent will do work and make commits and push them.
5. Agent will use the CLI to open a PR and request reviews.
6. Other agents review the PR and submit reviews (staged comments plus an approve/disapprove/neutral outcome).
7. Agent will read PR comments and iterate on them. Comments will be resolved once the agent thinks that they've been.
8. Once the PR has at least one approval, it is marked complete, and the web interface shows the proposed upstream PR title and description.
9. Admin will review the proposed upstream PR in the web interface. Admin can either accept or leave a comment.
10. Upon acceptance, an upstream PR is created on the forge (Forgejo for MVP, GitHub for GH) with the proposed title and description.

### Future Work

Post-MVP directions, roughly in priority order. None are required for the MVP to be useful.

- **Precise cross-file code intelligence.** Layer per-language symbol resolution on top of
  the Tree-sitter parse to get accurate definitions, references, and blast-radius. Standard
  options: **SCIP** (Sourcegraph) with per-language indexers, or **stack-graphs** (GitHub)
  to stay within the Tree-sitter toolchain. This upgrades the Graph DB from approximate to
  exact reference edges.
- **GitHub support (close second forge).** Promote GitHub from secondary to fully
  supported behind the existing provider interface — GitHub PAT credentials and the GitHub
  PR REST API. Git transport already works unchanged.
- **Authentication.** Replace the MVP's trusted-environment identity model with
  server-issued agent credentials so identity and role can be verified rather than asserted.
- **Per-repo / per-branch authorization.** Move beyond global role-based access to scope
  what a role may do on a per-repo and per-branch basis.
- **Reviewer sub-roles.** Split the single reviewer role into specialized types (e.g.
  security, style, architecture), each with its own sub-instructions delivered via the
  `instructions` command. Each sub-role carries two policy flags: whether its review is
  **required** (a PR cannot complete until that sub-role has reviewed) and whether its
  **approval** is required (that sub-role must return an approve outcome), extending the
  MVP's flat "at least one approval" bar into a per-sub-role matrix.
- **Reviewer orchestration.** Have Loam itself drive reviewers rather than relying
  on an external orchestrator: assign the sub-roles a given PR needs, dispatch/notify the
  matching agents, and track which required reviews and approvals are still outstanding
  before a PR can be marked complete.
- **Expanded Tree-sitter grammar coverage.** Grow the shipped grammar set beyond the MVP
  starter languages as needed.
- **Releases.** Local release proposals (tag + notes on a target branch) that propagate
  upstream on admin approval, mirroring the PR flow.
- **Multiple admins / users.** Move beyond the single startup admin credential to multiple
  admin accounts and roles in the web interface.

