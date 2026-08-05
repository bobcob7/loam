# Loam

This is a centralized code repo with a CLI tool and code-intelligence services.

### Purpose

I find it cumbersome to use GitHub or other centralized code repository systems.
Github has a lot of features that seem more like metadata about the content, not enriching the metadata.

Loam is a single self-hosted server that mirrors enrolled upstream repos and
exposes them to AI agents through one consistent tool. Agents do all their work —
cloning, branching, committing, opening and reviewing changes, querying code — against
this server, never directly against the upstream forge. A human admin enrolls repos and
gives the final sign-off before anything is pushed upstream.

Upstream forges are supported behind a common provider interface. **Forgejo is the MVP
target**; **GitHub is a close-behind secondary**. Provider-specific details live behind that
interface so the agent-facing workflow is identical regardless of forge.

The provider interface only needs to abstract two things: the **access token** (each forge
issues and scopes its own) and the **REST API** used to open upstream PRs. Git transport to
the forge rides the same token over HTTPS, so a single credential per forge host covers
everything Loam does against it (see [`docs/sync-spec.md`](docs/sync-spec.md)).

### Try it

One machine, Docker, no Kubernetes and no build toolchain:

```sh
cp deploy/.env.example deploy/.env   # then fill in four values it explains
docker compose -f deploy/docker-compose.yml up -d
```

The walkthrough from there to an enrolled repo and a working agent identity —
including the one key that cannot be rotated and is not in your database backup
— is [`docs/compose-quickstart.md`](docs/compose-quickstart.md). For the
Kubernetes deployment instead, see [`helm/loam`](helm/loam) and
[`docs/deployment-spec.md`](docs/deployment-spec.md).

### Components at a glance

- **Server** — the single source of truth. Holds the mirrored git repos (on disk) plus a
  Postgres store for metadata, the code graph, and RAG vectors (see
  [`docs/persistence-spec.md`](docs/persistence-spec.md)). It is the only git remote agents
  ever see.
- **CLI tool** — the agent-facing interface. One binary that talks to the server for the
  Loam-native workflow and querying; git itself runs plain against server-bootstrapped
  clones (see [`docs/git-spec.md`](docs/git-spec.md)).
- **Web interface** — the admin-facing interface. Never used by agents. Used to enroll
  repos, manage upstream credentials, and approve proposed upstream PRs.

### CLI Tool

Local CLI tool that bootstraps clones and carries the Loam-native workflow: work branches,
review, verdicts, and code-intelligence queries.

The CLI talks to the central server for everything Loam-native; source control itself is
**plain git**, run against a clone the CLI bootstraps with the server as its only remote —
the server enforces push policy at receive time (see
[`docs/git-spec.md`](docs/git-spec.md)). Every call — RPC and git push alike — carries the
agent's identity and role (below), which the server uses to authorize the requested
operation. The CLI's command surface is specified in [`docs/cli-spec.md`](docs/cli-spec.md).

#### Agent Identity & Roles

Each agent has an assigned name, ID, and role, supplied through environment variables:

- **Name** — a random `<first-name>-<last-name>` combination.
- **Identifier** — `<name>-<id>-<role>`.
- **Role** — determines which operations the agent may perform and what the CLI
  `instructions` command returns to it.

Roles are configurable in the web console but ship with sane defaults: **author**,
**reviewer**, and **orchestrator** — the last a read-only supervisor holding `graph.query`
and `search` and no work-branch capability at all, described in
[`docs/orchestration.md`](docs/orchestration.md). Authorization is role-based and global: a
role grants the same operations across every enrolled repo, with no per-repo or per-branch
scoping in the MVP.

For the MVP there is no authentication — identity and role are trusted exactly as asserted
in the environment, and the server does not verify them. Verified, server-issued
credentials and finer-grained scoping are planned (see Future Work).

One consequence of that, stated here so it moves with the caveat itself rather than drifting
from it: `loam instructions` run with no `LOAM_AGENT_*` configured resolves to a fixed,
well-known identity (`loam-orchestrator-0-orchestrator`) whose role is **orchestrator**, and
makes an ordinary authenticated call like any other. This adds no hole. Under the trust model
above, a caller who wanted `search` and `graph.query` could already have asserted
`LOAM_AGENT_ROLE=reviewer` and been given them; the well-known identity asserts less, not
more. It is not an unauthenticated route — `/healthz` and `/readyz` remain the only ones —
and it hardens exactly when the trust model does.

#### Graph DB

Code is digested upon merge to specific branches. AST is run on the code to find and record both dependencies and ref history.
This information can be queried through the CLI tool.

The ingest step parses each source file into an AST — using **Tree-sitter**, with one
grammar per language, so language coverage is a plugin matrix: adding a language means
dropping in a grammar rather than changing the ingest core. It records symbols (functions,
types, modules), the files they live in, and the dependency edges between them, along with
the commit/ref history for each symbol. Agents query this to answer structural questions
without reading the whole tree: where a symbol is defined, what references it, what it
depends on, and the blast radius of a proposed change. Re-ingested whenever the repo's
indexed branch advances (one designated target branch in the MVP; all target branches is
Future Work).

The MVP ships a starter set of grammars (e.g. TypeScript/JavaScript, Python, Go). At this
layer cross-file symbols and dependency edges are approximate; precise reference resolution
is planned (see Future Work).

#### RAG

Documentation in the code, and the code itself is chunked and ingested into a vector DB upon merge to specific branches.
This information can be queried through the CLI tool.

On merge to the indexed branch, docs and code are split into semantically meaningful chunks
(a doc section, a function, a class), embedded, and stored in a vector DB. Agents run
natural-language queries through the CLI to pull the most relevant chunks as context —
"how is auth handled here", "where do we format review descriptions" — instead of grepping
blind. The graph DB answers *structural* questions; RAG answers *semantic* ones.

#### Work Branches

A work branch is the first-party unit of work; "review" is a state of it, not a separate
object. A work branch has a randomly generated name plus a title and description the agent
sets and refines as work progresses. When ready, the agent **requests review**; other agents
then submit **verdicts** (the first marks it *reviewed*), and only after the admin accepts
does anything reach the upstream forge. The CLI exposes the full loop below. Reviewer
selection and lifecycle are handled by an external orchestrator, not by Loam — agents
discover reviewable work branches awaiting their verdict by polling List.

##### List
Using the CLI tool, agents will be able to list work branches.

Returns work branches with basic status, filterable by repo, target branch, author, state,
and whether a reviewable work branch is awaiting the calling agent's verdict.

##### Get Details
Another command will pull the details of a work branch.

Returns the full work branch: title, description, target branch, current state, and the
comment threads (the diff is fetched separately).

##### Set Title / Description
A command sets or updates a work branch's title and description. Because the branch name is
random, these are its human-facing identity and can be changed at any point as the work
evolves.

##### Request Review
A command (and a UI button) requests review, putting a work branch up for review. It moves a
`draft` branch to `reviewable`, or asks for another round on a `reviewed` one (marking that
round's verdicts stale). Requires a title and description first.

##### Comment
Another command will pull individual comments on a work branch.

Fetches comment threads — the comment body, the file/line it anchors to, author, and whether
the thread is resolved — so an agent can work through feedback item by item.

##### Add Comment
A reviewer command that stages a new comment thread (optionally anchored to a file/line) and
can mark a thread resolved.

Comments are **staged locally on the CLI**, not sent to the server as they are written. A
reviewer accumulates their comments across the diff and nothing is visible until they submit
(see Submit Verdict).

##### Reply
An immediate command to reply to an existing thread (no staging). This is how an author
responds to review feedback; reviewers raise threads via their verdict.

##### Submit Verdict
Publishes a reviewer's staged comments to the server in one atomic action as a verdict,
together with an overall outcome: **approve**, **disapprove**, or **neutral**. This is the
only point at which staged comments become visible; the first verdict marks the work branch
**reviewed**. Verdicts are tracked per unique agent and marked stale when a new review is
requested. A reviewed work branch with at least one non-stale approve verdict becomes a
proposal in the admin's queue.

##### Verdicts
A command lists the verdicts on a work branch — each unique reviewer's outcome, including
ones marked stale by a later review request.

### Git

The CLI tool can be used to clone in a repo from the main server with the main server as it's only remote.

The server keeps a mirror of each enrolled upstream repo. Agents clone from the server and
push back to it; the server is their sole remote. Syncing with the real upstream is the
server's job, not the agent's, so agents work in a closed, controlled loop. Transport and
push policy are specified in [`docs/git-spec.md`](docs/git-spec.md).

Upstream is authoritative. When the server polls upstream and a mirrored branch has
diverged, it always takes upstream — the local copy is reset to match, with no
local-vs-upstream merge or conflict resolution. This is also how merged work flows back:
once an upstream PR is merged, the next sync advances the target branch in the mirror,
which is what triggers graph/RAG ingest. Each target advance is also checked for
mergeability against every open work branch; on a conflict, the work branch falls back to
`draft` until an agent catches it up (see [`docs/git-spec.md`](docs/git-spec.md) → Target
Advances & Catch-Up). The server never authors commits — work branches advance only by
agent pushes.

#### Enrollment

In the web interface. The admin should be able to enter in an access token (Forgejo for MVP, GitHub PAT for GH) that enables cloning repos.
The admin should also be able to enroll repos, which will clone it to the server and periodically keep it up to date.

The admin supplies upstream credentials once, then enrolls repos by name. On enrollment the
server clones the repo locally and thereafter polls upstream on a schedule to keep the
mirror and the enrolled target branches current.

#### Clone

The CLI tool will have a git clone command that clones the repo at a named branch.

Clones an enrolled repo from the server, checking out the named branch — always explicit,
typically the work branch the agent is working or reviewing. The resulting clone has the
server set as its only remote.

#### Work Branches

The CLI tool can be used to start work branches from target branches, set their title and
description, and request review. Requesting review requires a title and description.

Once review is requested, a work branch is visible to other agents via the List/Get Details
commands. Descriptions and comments are free text in the MVP; admin-configurable
communication schemas are Future Work.

Work branches never merge into the server's branches. When the admin accepts a reviewed work
branch (in the web interface), the server opens the corresponding upstream PR against the
target branch; the work branch is marked complete only once that PR merges, and the server's
target branch advances solely by pulling merged changes back from upstream (see mirror sync
above).

### Web Interface

The admin-facing interface. Loam ships as a single binary: one HTTP port serves the CLI
Connect API, the admin Connect API, a generated SPA embedded via `go:embed`, and git
smart HTTP under `/git/*` (see [`docs/git-spec.md`](docs/git-spec.md)). Full detail in
[`docs/web-spec.md`](docs/web-spec.md).

#### Auth

The interface is never intended for agents to access.
The admin will provide username and password on server startup (via the server's
environment — see [`docs/server-spec.md`](docs/server-spec.md)).
These credentials will use basic HTTP Auth. A single admin credential is sufficient for the
MVP; multiple admin accounts are future work.

#### Repos

##### Auth

Admin will be able to set an upstream access token (Forgejo token for MVP, GitHub PAT for GH).

The token is the only upstream credential: it authorizes both the REST calls that open
upstream PRs and git-over-HTTPS transport (clone/fetch/push) to the forge. It must carry
both scopes; enrollment verifies git access against the actual repo (see
[`docs/sync-spec.md`](docs/sync-spec.md) → Upstream Transport).

##### Enroll

The admin should be able to enroll repos, by upstream URL.

Target branches can be specified. These branches are eligible as targets for reviews.

### Workflow

#### Enroll Repo
1. Admin logs into web interface
2. Admin adds an upstream access token (Forgejo token for MVP, GitHub PAT for GH). The token is confirmed to work and to have the appropriate permissions.
3. Admin types in exact repo name <group>/<repo_name>.
4. Server clones the repo locally.
5. Admin specifies target branches and designates one as the indexed branch (pre-filled from upstream's default).
6. Server ingests data from the indexed branch.

#### Work & Review
1. Work is defined by the user.
2. The agent starts a work branch from a target branch. Its name is randomly generated — a work branch's identity is its title and description, not its name.
3. The agent clones the repo at the work branch via the CLI (which bootstraps identity), then does the work with plain git, committing and pushing as it goes.
4. The agent gives the work branch a title and description, refining them as the work takes shape. They belong to the work branch and can be updated at any time.
5. When the work is ready, the agent **requests review** — the signal that puts the work branch up for review. There is no separate PR or review object; "review" is simply a state of the work branch.
6. Other agents review it and submit **verdicts**: staged comments plus an approve / disapprove / neutral outcome. The first verdict marks the work branch **reviewed**.
7. The author reads the comments and iterates — replying to threads and pushing changes; requesting review again starts a fresh round (marking the prior round's verdicts stale).
8. Once a work branch is reviewed with at least one approving verdict, it appears in the admin's queue. There is no author "complete" step.
9. The admin reviews it and either accepts, requests a re-review (sending it back for another round; any feedback goes into the comment threads), or closes it.
10. On acceptance, an upstream PR is created on the forge (Forgejo for MVP, GitHub for GH) with a generated branch name and the work branch's title/description. The work branch is marked **complete** only when that upstream PR merges (or **closed** if it or the work branch is closed).

### Future Work

Post-MVP directions, roughly in priority order. None are required for the MVP to be useful.

- **Precise & cross-repo code intelligence.** Layer per-language symbol resolution on top
  of the Tree-sitter parse to get accurate definitions, references, and blast-radius — and,
  crucially, **cross-repo dependency edges** (global symbol identity + import/coordinate
  resolution, so a usage in one repo links to its definition in another). Standard options:
  **SCIP** (Sourcegraph) with per-language indexers, or **stack-graphs** (GitHub) to stay
  within the Tree-sitter toolchain. Until then, `graph --all` is a per-repo fan-out only.
- **Graph query filters.** Grow `graph` beyond the MVP's `--file`: `--kind`
  (function/type/module), `--lang`, `--path <glob>`, and `--depth <n>` to bound
  `deps`/`dependents` traversal — making ambiguity-as-data progressively more precise
  without introducing a query language.
- **GitHub support (close second forge).** Promote GitHub from secondary to fully
  supported behind the existing provider interface — GitHub PAT credentials and the GitHub
  PR REST API. Git transport already works unchanged.
- **Dedicated persistence stores.** The MVP consolidates metadata, the code graph, and RAG
  vectors in one Postgres (pgvector + recursive-CTE graph). Split the rebuildable stores onto
  dedicated services — a vector DB (e.g. Qdrant) and/or a graph DB — behind the existing
  repository seams as scale demands. See `docs/persistence-spec.md`.
- **Authentication.** Replace the MVP's trusted-environment identity model with
  server-issued agent credentials so identity and role can be verified rather than asserted —
  on git, via a standard credential helper; the MVP trusts the identity headers a clone
  carries.
- **Per-repo / per-branch authorization.** Move beyond global role-based access to scope
  what a role may do on a per-repo and per-branch basis.
- **Reviewer sub-roles.** Split the single reviewer role into specialized types (e.g.
  security, style, architecture), each with its own sub-instructions delivered via the
  `instructions` command. Each sub-role carries two policy flags: whether its review is
  **required** (a work branch cannot complete until that sub-role has reviewed) and whether its
  **approval** is required (that sub-role must return an approve outcome), extending the
  MVP's flat "at least one approval" bar into a per-sub-role matrix.
- **Reviewer orchestration.** Have Loam itself drive reviewers rather than relying
  on an external orchestrator: assign the sub-roles a given review needs, dispatch/notify the
  matching agents, and track which required reviews and approvals are still outstanding
  before a work branch can be marked complete.
- **Communication schemas.** Per-repo JSON Schemas, set by the admin, validating
  work-branch descriptions and review-comment bodies at the server boundary — forcing
  agents into a consistent, machine-checkable communication format that stays flexible
  for the admin to adjust as needed. Dropped from the MVP; free text until then.
- **Collaborative work branches.** Let multiple agents work the same work branch at once by
  cloning it into separate directories and coordinating commits/pushes on the shared branch.
  The MVP assumes a single agent per work branch.
- **Expanded Tree-sitter grammar coverage.** Grow the shipped grammar set beyond the MVP
  starter languages as needed.
- **Releases.** Local release proposals (tag + notes on a target branch) that propagate
  upstream on admin approval, the same way a completed work branch reaches upstream.
- **Multiple admins / users.** Move beyond the single startup admin credential to multiple
  admin accounts and roles in the web interface.

