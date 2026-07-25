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

_Add your build and test commands here_

```bash
# Example:
# npm install
# npm test
```

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
