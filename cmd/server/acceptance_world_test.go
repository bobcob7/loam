//go:build acceptance

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cucumber/godog"
	"github.com/google/uuid"
)

// acceptanceWorldKey is the context.Value key one scenario's *acceptanceWorld
// is stored under, godog's own documented pattern for per-scenario state
// (godog.ScenarioContext.Before returns a new ctx that every step function
// for that scenario receives).
type acceptanceWorldKey struct{}

// acceptanceScenarioCounter hands out a monotonically increasing suffix so
// every scenario gets its own uniquely named repo/work-branch, even though
// every scenario in this suite shares one Postgres database and one
// in-process server for the whole run (see TestFeatures' own doc comment
// for why). atomic, not a plain int, purely as a defensive habit: godog
// scenarios run sequentially by default in this suite (Options.Concurrency
// is left at its zero value), but nothing here depends on that remaining
// true.
var acceptanceScenarioCounter atomic.Int64

// acceptanceWorld is one scenario's own mutable fixture state: the repo/
// branch names this scenario seeded, the author identity it acts as, its
// own workspace tmpdir (the Author/Reviewer actor's per-actor workspace,
// testing-spec Layer 1's own requirement), and the outcome of whichever
// driver call a When step most recently made, for a later Then step to
// assert on.
type acceptanceWorld struct {
	workspace         string
	repoGroup         string
	repoName          string
	repoID            uuid.UUID
	targetBranch      string
	workBranch        string
	mirrorDir         string
	agentName         string
	agentID           string
	agentRole         string
	clonePath         string
	lastCLI           loamCLIResult
	lastGitOutput     string
	lastGitErr        error
	upstreamPRNumber  int
	lastProposalPRURL string
}

// repo returns this scenario's full "<group>/<repo_name>" identifier.
func (w *acceptanceWorld) repo() string { return w.repoGroup + "/" + w.repoName }

// writeCommitAndPush writes filename with content into this scenario's
// clone, commits it with message, and pushes to refspec -- plain git, no
// loam involvement, exactly what the core vocabulary row "I commit and
// push" (docs/testing-spec.md Layer 1) resolves to. The commit sets an
// explicit committer identity (via the clone's own user.name/user.email,
// already configured by `loam clone`'s bootstrapCloneIdentity) rather than
// relying on any ambient gitconfig, per this repo's own constraint that
// CI carries no global one.
func (w *acceptanceWorld) writeCommitAndPush(filename, content, message, refspec string) error {
	if err := os.WriteFile(filepath.Join(w.clonePath, filename), []byte(content+"\n"), 0o644); err != nil {
		w.lastGitErr = fmt.Errorf("writing %s: %w", filename, err)
		return nil
	}
	if out, err := runPlainGit(w.clonePath, "add", filename); err != nil {
		w.lastGitOutput, w.lastGitErr = out, fmt.Errorf("git add: %w", err)
		return nil
	}
	if out, err := runPlainGit(w.clonePath, "commit", "--quiet", "-m", message); err != nil {
		w.lastGitOutput, w.lastGitErr = out, fmt.Errorf("git commit: %w", err)
		return nil
	}
	out, err := runPlainGit(w.clonePath, "push", "origin", refspec)
	w.lastGitOutput, w.lastGitErr = out, err
	return nil
}

// newAcceptanceWorld builds a fresh acceptanceWorld for one scenario, with
// a uniquely suffixed repo/work-branch/agent identity so concurrent or
// merely sequential scenarios sharing this suite's one database never
// collide, and its own workspace tmpdir for the CLI actor driver to clone
// into.
func newAcceptanceWorld(t *testing.T) *acceptanceWorld {
	n := acceptanceScenarioCounter.Add(1)
	return &acceptanceWorld{
		workspace:    t.TempDir(),
		repoGroup:    "acceptance",
		repoName:     fmt.Sprintf("repo-%d", n),
		targetBranch: "main",
		workBranch:   fmt.Sprintf("wb-%d", n),
		agentName:    fmt.Sprintf("acceptance-author-%d", n),
		agentID:      fmt.Sprintf("%d", n),
		agentRole:    "author",
	}
}

// worldFrom retrieves the current scenario's *acceptanceWorld from ctx,
// panicking if none is set -- every step in this suite runs inside a
// scenario beforeScenario has already initialized, so a missing world
// means a step was invoked outside godog's own lifecycle, a programming
// error worth failing loudly on rather than a nil-pointer step further on.
func worldFrom(ctx context.Context) *acceptanceWorld {
	w, ok := ctx.Value(acceptanceWorldKey{}).(*acceptanceWorld)
	if !ok {
		panic("acceptance harness: no scenario world in context")
	}
	return w
}

// beforeScenario is godog's ScenarioContext.Before hook: it builds a fresh
// acceptanceWorld and stores it on ctx for every step in this scenario to
// retrieve via worldFrom. h.t.TempDir() (the whole suite's *testing.T) is
// what actually creates and schedules cleanup of the workspace directory;
// per-scenario subtests are not used here since godog itself, not `go
// test`, drives scenario iteration.
func (h *acceptanceHarness) beforeScenario(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
	world := newAcceptanceWorld(h.t)
	return context.WithValue(ctx, acceptanceWorldKey{}, world), nil
}

// afterScenario removes this scenario's fixtures that this suite's single
// shared server/database would otherwise keep for the whole run:
//
//   - the bare mirror on disk (lives under the shared server's own
//     LOAM_DATA_DIR, not under the scenario's own workspace tmpdir h.t's
//     TempDir cleanup already reaches).
//   - the repos row itself (cascading to repo_target_branches and
//     work_branches via their ON DELETE CASCADE foreign keys). This is
//     the fixture-isolation seam clone-and-push.feature's own Background
//     needs: every scenario in that file names the SAME literal repo
//     ("bobcob7/doc-server"), by design (they are exercising different
//     properties of what reads, in the Gherkin, as one conceptual repo),
//     so nothing in newAcceptanceWorld's own uniqueness suffix ever
//     applies to it -- world.repoGroup/repoName are overwritten by
//     stepRepoIsEnrolled with the scenario's own literal text. Without
//     this delete, the second scenario naming that repo would collide on
//     repos_name_key (observed directly while building this harness).
func (h *acceptanceHarness) afterScenario(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
	world := worldFrom(ctx)
	if world.mirrorDir != "" {
		_ = os.RemoveAll(world.mirrorDir)
	}
	if world.repoID != (uuid.UUID{}) {
		_, _ = h.server.pool.Exec(context.Background(), `DELETE FROM repos WHERE id = $1`, world.repoID)
	}
	return ctx, nil
}
