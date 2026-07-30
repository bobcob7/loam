# Bead Development Workflow

How agents pick up and complete beads in this repo. This is the operating loop —
it sits on top of the Beads commands (`bd prime`) and the testing spec
(`docs/testing-spec.md`), which define *what* to track and *how done is measured*.
This doc defines *the loop that gets a bead from ready to closed*.

## Model & authority

- **Implementation runs on Sonnet**, in an isolated git worktree subagent (one
  writer per worktree — no two agents edit the same files).
- **Review runs on Opus**, in a separate subagent that only reads the diff and
  routes findings. The reviewer is advisory; it never edits code itself.
- Git authority is **team-maintainer**: agents may commit, push, run
  `bd dolt push`, and close beads as part of session close. An explicit user,
  orchestrator, or repository "do not commit/push" instruction still wins.

## The loop

### 1. Intake

- `bd show <id>` on each bead. Read the description, design, and especially the
  `--acceptance` line — the named `@wip` scenarios in `features/*.feature` are
  the definition of done (see `docs/testing-spec.md` → "`@wip` as bead
  acceptance criteria").
- Build a dependency picture (`bd ready` + each bead's blockers). Only beads with
  no open blockers are eligible; the rest queue.
- Partition the eligible set into **independent** units (no shared files or
  packages). Independent units fan out in parallel; coupled beads are grouped
  onto a single shared branch.
- **Parallelism is bounded by real independence.** If two "independent" beads
  turn out to touch the same package, collapse them to sequential on one branch
  rather than fight merge conflicts.

### 2. Implement — worktree subagent (Sonnet)

1. `bd update <id> --claim`.
2. Branch off `main` in an isolated worktree (one bead, or one coupled group).
3. Locate the named `@wip` scenarios; confirm they are currently `@wip`. That is
   the target.
4. Implement per the repo's Go standards: interfaces in `interfaces.go` at the
   consumer, moq mocks in `moq_test.go`, `t.Parallel()` / `t.Context()` /
   `io.Discard` loggers, no blank lines inside function bodies, early returns,
   unexported by default.
5. Remove **only** the `@wip` tags this bead owns.
6. Run the gates green: `go build ./...`, `go test ./...`, `go tool buf lint`
   (+ `go tool buf generate` if protos changed), and the godog acceptance
   scenarios once that harness exists (`loam-li0` epic). Un-@wip'd scenarios
   pass; nothing else regresses.
7. **Typecheck the build-tagged trees**, even when you are not running their
   suites: `go vet -tags=integration ./...` and `go vet -tags=acceptance ./...`.
   Neither starts a container, so both are affordable under the resource
   constraints that normally keep a subagent off the tagged suites — and an
   untagged `go build ./...` cannot see these files at all. Any change to an
   exported signature a `_test.go` under a build tag calls (`loam-cgg` widened
   `workbranchstore.RecordUpstreamPR` and broke four call sites in
   `integration_test.go`) is otherwise invisible until a full tagged run.

### 3. Review — separate subagent (Opus)

Once the implementation subagent reports gates green, a fresh **Opus** subagent
reviews *that bead's diff only* against four axes:

- **Accuracy** — does it do what the bead specifies; any bugs or wrong behavior.
- **Completion** — full bead scope and named `@wip` scenarios satisfied; nothing
  stubbed or half-done.
- **Test coverage** — the un-@wip'd scenarios genuinely exercise the behavior;
  edge/error paths covered; no vacuously-passing tests.
- **Idiomatic practice** — conforms to the Go standards above.

**Disposition of findings:**

- **Immediate + in scope** (bug, missing assertion, style violation, tightly
  scoped gap) → handed back to the same Sonnet worktree subagent to fix in
  place, then re-gate. Loop **up to 5 times** before stopping and surfacing to a
  human.
- **Comprehensive / out of scope** (larger refactor, adjacent feature,
  cross-cutting concern) → first `bd search` for an existing bead. If one exists,
  note the linkage. If not, `bd create` a follow-up bead scoped to it,
  prioritized to be picked up next (`bd dep add` where it should block/follow
  related work). This does **not** hold up the current bead's integration.

The reviewer only orders in-place fixes for work **within the bead's stated
scope**. Scope creep — even small — becomes a follow-up bead, so a review can
never silently expand what a bead does.

### 4. Close

- Write the structured verdict to the bead's `--notes` (see below).
- `bd close <id>` (batch if multiple), then `bd close <id> --suggest-next` to
  surface newly-unblocked work.
- A bead closes only on a clean review, or clean-modulo-filed-followups.

### 5. Session close (team-maintainer)

Run gates, commit each branch (imperative mood, conventional prefix,
`Co-Authored-By` trailer), merge/PR back resolving any inter-branch conflicts,
`git push`, `bd dolt push`, and hand off with a summary of changed files,
validation, and issue status.

## Structured review verdict

Every closed bead carries the Opus reviewer's verdict in its `--notes`, in this
shape:

```
REVIEW (opus): PASS after 2 fix cycles
- accuracy: ok
- completion: ok — greened @wip: work-review: reviewer approves round
- test coverage: ok — added error-path case for expired token
- idiomatic: fixed 1 (blank line in fn body); 1 style nit deferred
Follow-ups filed: loam-xyz (extract retry policy — out of scope)
```

Verdict is one of `PASS` or `PASS-WITH-FOLLOWUPS`.

## Guardrails

- A bead with behavior-level scope but **no `--acceptance` scenarios** is a
  stop-and-flag, not a guess. Raise it (or `bd human <id>`) before starting
  rather than inventing a definition of done.
- One writer per worktree. The Opus reviewer reports and routes; only the Sonnet
  worktree subagent edits code.
- The review↔fix loop is bounded at **5 cycles**. If a bead has not converged by
  then, stop and surface it to a human instead of churning.
