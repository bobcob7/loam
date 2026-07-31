# Project Instructions for AI Agents

This file provides instructions and context for AI coding agents working on this project.

## Development Workflow

When picking up and completing beads, follow the operating loop in
[docs/bead-workflow.md](docs/bead-workflow.md): Sonnet implements in an isolated
worktree, a separate Opus subagent reviews the diff (accuracy, completion, test
coverage, idiomatic practice), fixes loop back up to 5 times, and the review
verdict is recorded in the bead's notes on close.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:6cd5cc61 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.

## Agent Context Profiles

The managed Beads block is task-tracking guidance, not permission to override repository, user, or orchestrator instructions.

- **Conservative (default)**: Use `bd` for task tracking. Do not run git commits, git pushes, or Dolt remote sync unless explicitly asked. At handoff, report changed files, validation, and suggested next commands.
- **Minimal**: Keep tool instruction files as pointers to `bd prime`; use the same conservative git policy unless active instructions say otherwise.
- **Team-maintainer**: Only when the repository explicitly opts in, agents may close beads, run quality gates, commit, and push as part of session close. A current "do not commit" or "do not push" instruction still wins.

## Session Completion

This protocol applies when ending a Beads implementation workflow. It is subordinate to explicit user, repository, and orchestrator instructions.

1. **File issues for remaining work** - Create beads for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **Handle git/sync by active profile**:
   ```bash
   # Conservative/minimal/default: report status and proposed commands; wait for approval.
   git status

   # Team-maintainer opt-in only, unless current instructions forbid it:
   git pull --rebase
   git push
   git status
   ```
5. **Hand off** - Summarize changes, validation, issue status, and any blocked sync/commit/push step

**Critical rules:**
- Explicit user or orchestrator instructions override this Beads block.
- Do not commit or push without clear authority from the active profile or the current user request.
- If a required sync or push is blocked, stop and report the exact command and error.
<!-- END BEADS INTEGRATION -->


## Build & Test

Go 1.26.5. `internal/parser` (and anything importing it) needs
`CGO_ENABLED=1` and a C compiler — see "Go standards" below before your
first build fails on this.

```bash
task generate   # regenerate proto (buf) + moq mocks — do this after editing proto/*.proto or an Iface
task build      # web:build (real SPA -> web/dist) + go build ./... + restore web/dist placeholders (loam-nvb.15)
task build:bin  # build server, loam, and loamhook binaries side by side into bin/ (loam-mce)
task test       # per-PR gate: lint + go test ./... -race + test:integration + test:acceptance (loam-li0.12)
task lint       # gofmt -l . (must be empty) + go tool buf lint
task proto:breaking  # pre-1.0 breaking-change check against the pinned baseline (see Taskfile.yml)

task test:integration        # //go:build integration suite, real Postgres via testcontainers (needs Docker)
task test:acceptance         # godog acceptance suite vs internal/fakeforge (needs Docker)
task test:contract:forgejo   # NIGHTLY-only: provider contract vs a REAL Forgejo (LOAM_TEST_FORGEJO=1, needs Docker)
task test:e2e                # NIGHTLY-only: compose e2e smoke + Playwright admin journeys (needs Docker; see task desc)
task test:e2e:golden         # NIGHTLY-only: compose e2e golden path + conflict/catch-up/re-accept (needs Docker)

task web:install   # npm ci in web/
task web:generate  # go tool buf generate --template web/buf.gen.yaml -- refresh web/src/gen after a proto/ change
task web:build     # tsc --noEmit + vite build -> web/dist (real output; see task build above for what happens to it there)
task web:test      # npm test (vitest run) in web/

task docker:build  # build the server image (loam-ytt2.1) from the repo-root Dockerfile
```

See `docs/deployment-spec.md` for how that image, the `helm/loam` chart, and
the running service are configured, backed up, and rolled back.

CI runs on **Forgejo Actions** at git.bobcob7.com, not GitHub — the repo
moved (loam-ytt2.6) and `.github/workflows/` was deleted, because Forgejo
Actions reads that directory too and its `ubuntu-latest` jobs queued
forever against runners that do not exist.

`.forgejo/workflows/ci.yaml` is the per-PR gate: **one** job on the
`arm64` runner (gofmt, build, vet, `test -race`, `go vet` over both the
`integration` and `acceptance` build tags, `buf lint`, and a `go mod tidy`
drift check that the generated-code check cannot subsume because nothing
it runs rewrites `go.mod`, loam-j09k). `.forgejo/workflows/build.yaml`
builds the container image on native `arm64` and `amd64` runners and
merges them into a multi-arch manifest list.

Three constraints on this instance, learned the hard way and documented at
length in those files' headers: `runs-on` accepts **only** `arm64` and
`amd64`; each runner has capacity 1 and a 2h timeout, so same-arch jobs
serialize rather than parallelize (hence one gate job, not a matrix); and
`actions/checkout` is a JavaScript action, so any job overriding
`container:` with a node-less image (golang, docker) must clone by hand.

NOT yet ported, and tracked separately: the `web` job (SPA
install/typecheck/test/build — see `docs/web-frontend-spec.md`), the
generated-code drift check, and the nightly suites (provider contract vs a
real Forgejo, compose e2e, Playwright — docs/testing-spec.md "CI Stages"),
which are blocked on proving testcontainers works inside a Forgejo job.

## Architecture Overview

_Add a brief overview of your project architecture_

## Conventions & Patterns

### Go standards

- Go 1.26.5 (see `go.mod`). Tool dependencies (`buf`, `protoc-gen-go`,
  `protoc-gen-connect-go`, `matryer/moq`) are pinned in **go.mod's native
  `tool (...)` directive** (Go 1.24+) and invoked as `go tool <name>` — e.g.
  `go tool buf lint`, `go tool moq -out moq_test.go . Iface`. This is
  hermetic: no GOBIN, no globally-installed binaries, no version drift
  between contributors.
- **Do not add `internal/tools/tools.go` with a `//go:build tools` tag.**
  That blank-import idiom predates Go 1.24's first-class tool directive and
  is superseded by it in this repo. If you're tempted to reach for it
  because it's a common convention elsewhere, it's the wrong call here —
  use the `tool (...)` block in `go.mod` instead.
- Every `//go:generate` directive that shells out to a pinned tool uses the
  `go tool` form (e.g. `//go:generate go tool moq -out moq_test.go . Iface`),
  never a bare binary name. `task generate` (`Taskfile.yml`) regenerates
  everything — proto (`go tool buf generate`) and mocks (`go generate
  ./...`) — from a clean checkout with no other setup.
- Interfaces are defined where consumed, all in one `interfaces.go` per
  package. Mocks are moq-generated into `moq_test.go` in the same package —
  never hand-written.
- `internal/parser` (and anything importing it) requires `CGO_ENABLED=1`
  plus a C compiler, because its tree-sitter grammar bindings vendor and
  statically link C sources. This applies to every `./...` invocation,
  since the wildcard sweeps `internal/parser` in — including `go vet`, not
  just `go build`/`go test`. No external tree-sitter library is needed.
  - linux: `gcc` or `clang` + libc headers (`build-essential` on
    Debian/Ubuntu).
  - darwin: Xcode Command Line Tools (`xcode-select --install`); Apple
    clang is sufficient, no separate gcc needed.
  - A missing compiler fails as `cgo: C compiler "..." not found` — install
    the toolchain above rather than disabling CGO.
