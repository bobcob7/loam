# Testing Spec

The test strategy above the unit level: acceptance (feature), integration, and
end-to-end. Unit tests are governed by the repo's existing Go/TS standards and are out
of scope here. The goal is that meaningful tests grow **alongside** the MVP — every spec
behavior lands with its validation — and that "the MVP is complete" is a checkable
statement, not a feeling.

Status: **draft.** The layers and harness decisions are settled; suite contents grow
with implementation.

## Principles

- **The feature files are the spec of record.** The Gherkin scenarios in `features/`
  run as acceptance tests (godog) against the real CLI and server. MVP completion is
  defined by this suite passing — see Completion Criteria.
- **Real infrastructure by default.** Postgres (pgvector image), the git binary,
  Tree-sitter grammars, and the compiled `loam` CLI are always real
  (`testcontainers-go` per `docs/persistence-spec.md`). Fakes are permitted only at the
  two external boundaries where determinism demands them — the upstream forge and the
  embedder — and each fake is kept honest by a contract suite that runs the same
  operations against the real thing.
- **Deterministic time.** Nothing sleeps and nothing polls wall-clock intervals. The
  harness drives the sync scheduler and ingest workers by explicit ticks; a step like
  "when the next sync runs" is one tick, run to completion. No `Eventually` loops
  around timing guesses.
- **One behavior, one home.** A behavior is validated at the lowest layer that can
  observe it honestly. Acceptance owns workflow behavior; integration owns boundary
  mechanics (SQL, git plumbing, parsing); e2e owns "the shipped artifacts actually
  compose."

## The Three Test Doubles

The only fakes in the entire strategy:

1. **Fake forge** — one in-process Go server playing the upstream: bare git repos
   served over token-authenticated smart HTTP, plus the provider REST surface
   (`ValidateToken`, `CheckRepo`, `CreatePR`, `GetPRState`, `ClosePR`) and a **test
   control API** for scripting upstream events: advance a branch, force-push, delete a
   branch, merge or close a PR, create a colliding `wb-*` branch.
2. **Deterministic embedder** — implements the `Embedder` interface with a
   bag-of-words projection into the vector space, so cosine similarity tracks keyword
   overlap. Search tests assert plumbing and ranking mechanics ("the auth chunk ranks
   first for an auth query"), never model quality — that is explicitly untested.
3. **Manual scheduler** — the sync scheduler and ingest worker pool driven by
   harness-controlled ticks instead of timers (the components already take their
   trigger as an input; tests just own it).

## Layer 1 — Acceptance (godog, per-PR)

Runs every scenario in `features/` against a real assembled system.

**Topology.** Postgres via testcontainers; the **server in-process** (wired through the
same constructor graph as `main()`, pointed at temp data dirs, fake forge, fake
embedder, manual scheduler); the **CLI as the real compiled binary**. In-process
hosting is what makes ticks, sockets, and teardown controllable; the CLI staying a real
binary keeps the agent surface honest end to end — env vars, JSON output, exit codes,
git config bootstrap.

**Actors.** Each Gherkin actor maps to a driver:

| Actor | Driver |
| --- | --- |
| Author / Reviewer agent | `loam` binary, per-actor workspace tmpdir + `LOAM_AGENT_*` env; plain `git` in clones |
| Admin | connect-go client with basic auth (the SPA is covered in Layer 3, not here) |
| Upstream forge | fake-forge control API |
| Time / sync | manual scheduler ticks |

**Step vocabulary.** Every domain phrase resolves to exactly one driver call, so steps
stay implementation-stable:

| Step | Resolution |
| --- | --- |
| "I request review" | `loam work request-review` (parse JSON, assert exit code) |
| "I commit and push" | `git add/commit/push` inside the actor's clone |
| "the upstream PR merges" | fake-forge `mergePR` + one sync tick |
| "the next sync runs" | one sync tick, run to completion |
| "after ingestion" | drain the ingest queue |
| "I accept it" | `ProposalService.AcceptProposal` as admin |

**Conventions.** Scenarios for unimplemented behavior are tagged `@wip` and excluded
from the gate; landing a feature means removing its tag. New spec behavior may not
merge without a scenario (the spec docs and `features/` move together — this is already
the working practice).

**`@wip` as bead acceptance criteria.** Implementation beads name the scenarios they
will green (`bd create --acceptance="un-@wip: <feature>: <scenario>, …"`), and closing
a bead requires exactly those tags removed with the gate green. "Done" becomes a tag
diff plus a green run — checkable, not a judgment call — and `git log` over the
feature files doubles as the MVP burn-down.

**Step definitions are distributed the same way.** The harness work owns the actor
drivers and the core step vocabulary (the tables above); beyond that, the bead that
removes a scenario's tag also contributes whatever step definitions that scenario still
needs. Test code grows with the feature it validates — there is no separate
test-writing phase and no bead that owns "make all the scenarios pass."

## Layer 2 — Integration (Go test packages, per-PR)

Focused suites where a real boundary is crossed; testify + table-driven per repo
standards.

- **Store** (vs. pgvector container): migrations apply cleanly up and down and are
  idempotent; sqlc queries against the real schema; `UNIQUE (round_id, reviewer)` and
  derived staleness; `conflict` column transitions; HNSW ordering with seeded vectors;
  the `dependents` recursive CTE — including **cycle safety** (mutually recursive
  symbols must terminate, not loop).
- **Git transport & policy** (vs. real git binaries, temp mirrors): the smart-HTTP
  handler and pre-receive path as a push matrix — target ref, unknown ref, non-author,
  terminal state, force push, deletion, missing identity headers, wrong role — each
  rejected with its documented reason; single-branch clone bootstrap (config, author,
  headers); startup reconciliation rewrites hooks and `receive.deny*` config; and the
  fail-closed property: **policy socket down ⇒ push rejected**, never silently
  accepted.
- **Ingest pipeline** (real Tree-sitter, fixture repos): golden symbol/reference/edge
  sets for the polyglot fixture; deleted/renamed file handling; grammarless files
  chunked but not parsed; and the key property test — after any scripted sequence of
  changes, **incremental ingest ≡ full rebuild** (same symbols, edges, chunks).
- **Provider contract** (fake forge *and* real Forgejo container, same suite): token
  validation (valid, invalid, missing scopes), `CheckRepo` read/write probes, PR
  create/state/close, `GitCredentials` actually authenticating a clone and push. This
  suite is what licenses the acceptance layer to trust the fake. Runs per-PR against
  the fake; nightly against real Forgejo.

## Layer 3 — End-to-End (nightly / pre-release)

Everything real, no doubles, compose-style stack: the server **binary** (embedded SPA),
Postgres, a seeded Forgejo, and optionally real Ollama.

- **Golden-path smoke** (scripted CLI + admin API): enroll a Forgejo repo → agent
  starts, works, pushes → reviewer clones, verdicts → admin accepts → PR appears on
  Forgejo → merge it there → sync flips the branch `complete` → graph/search reflect
  the merged code and report the new ingested ref. One conflict variant: upstream
  advance conflicts → reset to draft → catch-up → re-review → re-accept fast-forwards
  the same PR.
- **Admin SPA (Playwright)**: the four critical admin journeys against the real
  server — enroll (form → repo listed, syncing), credentials (set token → validated),
  proposal decision (view diff/verdicts → accept → PR URL shown), jobs (reindex →
  job runs visible). Component-level frontend tests stay under the repo's Vitest
  standards, outside this plan.

E2E exists to prove the shipped artifacts compose; behavioral coverage lives in Layers
1–2 and is not duplicated here.

## CI Stages

| Stage | Suites | Budget |
| --- | --- | --- |
| Per-PR gate | lint, unit (existing standards), integration, acceptance (fake forge) | minutes |
| Nightly | provider contract vs. real Forgejo, e2e smoke, Playwright | tens of minutes |
| Pre-release | full nightly set, green required | — |

Taskfile targets mirror the stages: `task test:integration`, `task test:acceptance`,
`task test:e2e` (and `task test` = the PR gate).

## Fixtures

- **`fixture-polyglot`** — a small seeded repo (Go + TypeScript + Python + markdown)
  with a *known* symbol graph: cross-file references, an ambiguous symbol name defined
  in two files, a mutual-recursion cycle, a doc section per top-level concept. Golden
  files derive from it; upstream mutations (advance, conflict, force-push, rename) are
  scripted helpers over it.
- Fake-forge repos are seeded from fixtures at harness start; every test gets isolated
  temp dirs and containers — no shared state between scenarios.

## Completion Criteria

The MVP is **done** when, on one commit:

1. Zero `@wip` tags remain in `features/` and the acceptance suite is green.
2. Integration suites are green, including the provider contract against **real
   Forgejo**.
3. The e2e golden path and conflict variant pass against the shipped binary.
4. The Playwright admin journeys pass against the embedded SPA.

## Out of Scope

Unit tests (repo standards govern them), performance/load testing, security testing,
embedding-model quality, multi-replica behavior, and browser-matrix coverage. Each can
get its own plan post-MVP if warranted.
