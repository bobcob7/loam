# User Stories

Behavioral specifications for Loam, written as **Gherkin** `.feature` files. They capture
what each actor needs and the acceptance criteria that prove it — a bridge between the design
docs (`README.md`, `docs/*-spec.md`) and eventual tests.

## Format

Each `.feature` file describes one capability area. Within it:

- The **`Feature:`** description holds the user story:
  *As a `<actor>`, I want `<goal>`, so that `<benefit>`.*
- Each **`Scenario:`** is an acceptance criterion, written **Given / When / Then**.
- A **`Background:`** sets up shared context for the file's scenarios.

Scenarios are written in **domain language**, not implementation detail — "I request review",
not "I run `loam work request-review`". This keeps them stable when the CLI or UI changes, and
lets a step-definition layer map the domain verbs onto the CLI or RPCs later.

These are executable-ready: the intent is to run them as acceptance tests with
[godog](https://github.com/cucumber/godog) (Cucumber for Go) against the real CLI + server.
They live in the top-level `features/` directory — godog's default search path — so a
step-definition suite picks them up without extra configuration. For now they are the
specification of record.

## Actors (ubiquitous language)

- **Author** — an agent doing work on a work branch (opens it for review, iterates).
- **Reviewer** — an agent reviewing a work branch (comments, submits verdicts).
- **Admin** — the human operating the web interface (enrolls repos, decides on proposals).
- **Orchestrator** — the external system that runs agents and assigns review work; mostly
  outside Loam's boundary, referenced only where behavior depends on it.

Key terms — **work branch**, **review** (a state, not an object), **verdict**, **proposal**,
**target branch**, **upstream PR** — are defined in the root `README.md`.

## Files

Each file covers one area (see the specs for detail):

- `work-branch-lifecycle.feature` — start → reviewable → reviewed → complete/closed, verdict
  staleness, conflict resets and catch-up (author + state transitions).
- `reviewing.feature` — reviewer stages comments, submits a verdict, resolves own threads,
  lists verdicts.
- `replies.feature` — author replies to review threads (immediate).
- `admin-proposals.feature` — admin accepts / requests re-review / closes; the proposal queue.
- `enrollment.feature` — admin enrolls repos, sets target branches and the indexed
  branch, removes repos.
- `credentials.feature` — admin sets upstream tokens per forge host; one token covers
  REST and git.
- `roles.feature` — role definitions, defaults, and authorization (author vs reviewer).
- `code-intelligence.feature` — graph queries and RAG search, including `--all` fan-out.
- `ingestion.feature` — indexing on enrollment and target-branch advance, edge freshness,
  admin reindex, the Jobs view, and failure handling.
- `sync.feature` — the mirror follows upstream (upstream-wins, pruning, work-branch refs
  untouched), sync errors and retry, accept pushes the `loam/` branch, PR attribution.
- `clone-and-push.feature` — clone bootstrap for plain git; server-side push policy
  (read-only targets, author-only, terminal states, force pushes, identity).
- `instructions.feature` — agent orientation (`instructions` / `whoami`).
