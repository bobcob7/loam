# Orchestrating loam

loam models two roles that *do* work — author and reviewer — and one that
*supervises* it. This document is for the third. It is written from roughly
twenty real author/review cycles run against this repository, and nearly every
rule below exists because something went wrong without it.

An orchestrator is a loam identity, but a narrow one. It holds `search` and
`graph.query` and no work-branch capability: it can read code but cannot start
a branch, push, comment or cast a verdict. It reads the task, dispatches agents,
carries findings between them, and records outcomes.

`loam instructions` needs no `LOAM_AGENT_*` configuration: those three variables
default to the well-known orchestrator identity, and the command returns that
role's guidance over the ordinary authenticated path, carrying a real identity
like every other call. Set them and it returns your own role's guidance, exactly
as before.

## The loop

1. **Verify the task's claims before dispatching.** Whatever the source — a
   tracker issue, a one-off instruction, a paragraph someone pasted — check its
   factual assertions about the code before an agent acts on them. A spec
   written from memory is often wrong in a way that costs a whole cycle. This
   is what `search` and `graph.query` are for: an orchestrator can check
   whether a thing already exists, or where it lives, without a clone. One task
   this project ran turned out to be fully implemented already; another named
   six call sites when there were seven, and the two it missed were the ones
   that made the design hard.
2. **Dispatch an author** with its own identity, exclusive file territory, and
   every decision the task leaves open stated as a decision it must make and
   record.
3. **Dispatch a separate reviewer** under a different identity. Tell it to
   mutate the code and confirm tests fail. A reviewer that only re-runs the
   suite adds nothing. Name the diff command it should use — see *Naming the
   diff* below; briefs in this project told reviewers to "diff against
   `origin/main`" while a clone had no such ref, and every reviewer worked
   around it silently rather than reporting it.
4. **Carry findings between them.** loam has no round-over-round view, so the
   mapping from a reviewer's finding to the commit answering it exists only in
   the orchestrator's context. This is the least automatable part of the job.
5. **Record the outcome and its reasoning** — including the arguments that
   were rejected. Six months later the rejected argument is the one someone
   will re-propose. If you have a tracker, it goes there; if you do not, the
   work branch's description and the published review threads are the durable
   record, and they live in loam whether or not anything else does.

## Identity

The identifier is `<name>-<id>-<role>`, built from three separate variables:

    LOAM_AGENT_NAME=ada-lovelace     # bare name
    LOAM_AGENT_ID=7                  # bare id
    LOAM_AGENT_ROLE=author

Setting `NAME` to the whole string produces identifiers like
`ada-lovelace-7-author-ada-lovelace-7-author-author` in permanent review
records. This is easy to do and impossible to fix afterwards without rewriting
review history.

The three default **together or not at all**: leave all of them unset and you
get the well-known orchestrator identity, `loam-orchestrator-0-orchestrator`,
which is synthetic on its face wherever it is recorded; set any one of them and
all three are required. A forgotten `LOAM_AGENT_ROLE` is a usage error, never a
silent promotion to orchestrator — a per-variable default would write the wrong
role into permanent review records for exactly the reason above.

Author and reviewer must be **different identities**. Roles gate capabilities:
a reviewer holds `git.clone` but not `git.push` and genuinely cannot push.

## What agents get wrong without being told

* **Every shell invocation is fresh.** Exports do not persist between them.
* **Commit early, even if broken.** Uncommitted work in a scratch directory is
  lost when an agent dies, and agents die — on API errors, on stalls, on
  timeouts.
* **Never background a gate.** Ending a turn does not wait for a background
  command; it stops. Run long gates in the foreground with a raised timeout.
* **`request-review` does not create a pull request.** Only admin acceptance
  does. An agent therefore cannot observe CI on its own branch, and any task
  whose acceptance depends on observing CI is unachievable by an author alone.
* **Push, then verify.** `git rev-list --count @{u}..HEAD` must be 0. A branch
  can reach `reviewable` with its commits still local, and everything
  downstream will look healthy.

## Verification hazards

Every one of these is a check that reports success without performing the
check. All were found in practice, and each cost at least one cycle.

| Hazard | Why it lies |
|---|---|
| `go vet -tags=X ./...` | Type-checks the tagged tree; runs no tests |
| `go test ... \| grep ...; echo $?` | `$?` is grep's status, not the test's |
| `${PIPESTATUS[0]}` | bash-only; expands to empty under zsh, so the check never fires |
| `go test -race` in a container | The sanitizer can abort (`SanitizerTool: CHECK failed`) while `go test` still prints `ok`. Needs `--security-opt seccomp=unconfined` |
| `docker ps -aq --filter label=org.testcontainers.reap=true` | That label is only written when ryuk is **enabled**; with it disabled the sweep always finds nothing |
| `docker ps -a` as a cleanup check | Shows the whole shared daemon. In a multi-agent session it reports other agents' containers, and clearing it destroys their work |

The general rule: **when a step exists to catch a failure, ask what it would
look like if the step itself were broken.** Prefer checks you can deliberately
make fail.

## Reviewing

The findings that mattered came from making things fail, not from reading:

* **Mutate, then confirm the test dies by an assertion** — not by a panic, a
  nil dereference, or a compile error.
* **Ask what the fixtures make indistinguishable.** Tests whose seed values let
  two different code paths produce identical output pass a rigorous-looking
  mutation battery while the bug ships. This was the single most common defect
  class found, in several forms: values chosen so an `ORDER BY` component never
  decides anything; a mock returning one boolean for a question with three
  real-world answers; every seeded row using a distinct file so the column
  under test never breaks a tie.
* **Read prose as prose.** Comments and docs that assert a mechanism which does
  not exist were the second most common defect. A diff will not reveal them —
  a false sentence is often only visible when the whole paragraph is read
  against itself. If a comment says "git does X", run it.
* **When a justification has been wrong twice, delete it** rather than
  rewriting it a third time. The replacement sentence, written under pressure
  to close a review, is where the next error lands.

Block on prose that asserts a mechanism which does not exist, because a reader
ends up with a false model. Do not block on prose that is merely loose where a
neighbouring sentence fixes the scope — that is a standard no sentence can
satisfy.

### Naming the diff

The most expensive review defect this project hit was not a missed bug. It was
reviewing the **wrong range**, confidently, because every way of getting a diff
wrong still produces a well-formed diff:

* guessing the base with `git diff` → wrong **range**
* `loam work diff` before pushing → wrong **tip**
* neither one printing a SHA → **no signal** in either case

Any of the following now works, and all of them name the commits involved, so a
reviewer's diff can be checked against what the author said it pushed:

```
loam work diff <repo> <work-branch> --stat   # which files, and by how much
loam work diff <repo> <work-branch>          # the full patch
git diff origin/<target>...HEAD              # from inside the clone
```

Two habits follow from this. **Give the reviewer the head SHA the author
pushed**, which `loam work show` reports as `head_sha` — round-over-round
scoping has no other home, since loam carries no round-to-round view. And do
not accept a review artifact that names no commit: an unidentified diff cannot
be contradicted, which is the same reason this project reports *which*
mutations died rather than how many, and *which* comment ids were staged rather
than how many.

## Territory and concurrency

Agents running in parallel must not share files. Before dispatching a batch,
map each task to the files it will touch and check for overlap. Two specific
traps:

* **Generated output is shared territory.** Two agents running `task generate`,
  or one running `sqlc generate` while another runs `buf generate`, will
  conflict in `internal/db/gen` or `internal/gen`. Assign one owner per
  generator per batch.
* **Containers are shared.** Postgres via testcontainers is not free; limit how
  many agents need one concurrently, and tell each to tear down only what it
  started — by project name or label, never by clearing the daemon.

## The task specification

loam does not care where work comes from. It has no tracker integration and no
opinion about one: a work branch carries a **title and description**, set with
`loam work set`, and that is the specification an author and a reviewer both
read. Whether it originates in an issue tracker, a one-off instruction, or a
paragraph you wrote yourself, the same properties make it work — and the same
absences make it fail.

* State what is **already true**, with file and line, so nobody re-derives it.
* State what will **bite**, especially anything that fails silently.
* Name the **decisions the implementer owns**, and require the argument — not
  just the outcome. A task answered without its reasoning gets reopened.
* Say what is explicitly **out of scope**, so a proposal does not grow.
* When the task turns out to be already done, or wrong, record that with
  evidence rather than building something anyway. Two specs this project ran
  were stale planning stubs that had outlived their own completion.

A specification that only describes a symptom produces an implementation that
solves the symptom.

**For one-off work**, you are the author of the spec as well as its supervisor,
which removes a check rather than a step: nobody else's claims are being
verified, so verify your own. Write the description into the work branch before
requesting review. An author agent that gets its task only through a chat
prompt leaves the reviewer reading a diff with no statement of intent, and the
reviewer cannot tell an omission from a decision.

**With a tracker**, keep the tracker authoritative for *why* and the work
branch authoritative for *what changed*, and put enough of the former in the
latter that a reviewer never needs both. This repository uses `bd` (beads) and
mandates it for its own work — see CLAUDE.md — but nothing in loam requires it,
and the orchestrator role's default instructions deliberately name no tracker.
