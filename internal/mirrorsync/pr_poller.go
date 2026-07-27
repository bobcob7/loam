package mirrorsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/forge"
	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// The three pull-request states forge.Provider.GetPRState is contracted to
// return (internal/forge/interfaces.go: `"open"`, `"merged"`, or
// `"closed"`). StorePRPoller matches against exactly these and treats every
// other string -- including "" -- as unknown, never as a terminal state.
const (
	prStateOpen   = "open"
	prStateMerged = "merged"
	prStateClosed = "closed"
)

// upstreamBranchPrefix is the ref namespace proposal acceptance pushes work
// branches into on the forge: docs/sync-spec.md -> Proposal Acceptance,
// "Push the work-branch tip to the upstream branch loam/<work-branch-name>".
// It is the ONLY namespace this package ever deletes from.
const upstreamBranchPrefix = "refs/heads/loam/"

// closedUpstreamPRReason is the work_branches.close_reason StorePRPoller
// records when it closes a branch because its upstream PR was closed
// without merging, distinguishing the row from an admin close (which
// records the admin's own reason -- docs/sync-spec.md -> PR State Tracking).
const closedUpstreamPRReason = "upstream pull request was closed without merging"

// errUnknownPRState is StorePRPoller's refusal to act on a pull-request
// state it does not recognize. It exists because the destructive
// interpretation is the cheap one: reading an unrecognized state as
// "closed" would close a live work branch and delete its upstream branch
// on the strength of a string nobody validated. This package has already
// been bitten by that class once (parsePorcelainFetch fabricating
// RefUpdates out of interleaved stderr, fixed in 5aaf563), so an
// unrecognized state transitions nothing, deletes nothing, and surfaces
// loudly as a cycle error instead.
var errUnknownPRState = errors.New("mirrorsync: forge reported an unrecognized pull request state")

// errNoRecordedPR guards pollOne against a work branch whose
// upstream_pr_number is NULL. pollSet already filters those out, so this is
// unreachable in practice -- it is here so that a future edit which widens
// or drops that filter surfaces as a reported error rather than as a nil
// dereference panicking the whole sync cycle.
var errNoRecordedPR = errors.New("mirrorsync: work branch has no recorded upstream PR to poll")

// errUnsafeWorkBranchName is StorePRPoller's refusal to build an upstream
// ref path out of a work-branch name that could escape
// upstreamBranchPrefix. See safeWorkBranchName.
var errUnsafeWorkBranchName = errors.New("mirrorsync: work branch name cannot be used to build an upstream ref")

// workBranchNamePattern is the conservative shape a work-branch name must
// match before it is interpolated into a ref path. Names are server-
// generated ("wb-9c2f1a", docs/cli-spec.md -> start: "The name is randomly
// generated"), so this rejects nothing legitimate; it exists so that a row
// whose name column was somehow written with a slash, a "..", or a leading
// dash cannot turn a scoped delete of refs/heads/loam/<name> into a delete
// of some other ref, or into a git push argument that parses as a flag.
var workBranchNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// StorePRPoller is the production PRPoller (docs/sync-spec.md -> PR State
// Tracking; owned by bead giq.8). Once per cycle, for every work branch of
// the repo that has a recorded upstream PR and has not already reached a
// terminal state, it asks the forge what that PR's state is and applies the
// forge's answer:
//
//   - open -> nothing at all.
//   - merged -> the branch completes (workbranchstore.Store.Complete).
//   - closed (without merging) -> the branch closes, recording
//     closedUpstreamPRReason.
//
// The poll set is chosen on PR PRESENCE, not on work-branch state: a branch
// can be sitting in draft or reviewable after a conflicting target advance
// reset it while its PR is still open and untouched (docs/git-spec.md ->
// Target Advances & Catch-Up leaves the upstream PR alone), and that PR
// merging is still authoritative. Only the two terminal states are
// excluded, and only because there is nothing left to transition them to.
//
// Every transition is a guarded single-statement UPDATE on the store side,
// and a branch that transitions leaves the poll set permanently, so polling
// the same already-merged PR on a later tick is a no-op rather than a
// double transition. A concurrent actor that terminated the branch first
// surfaces as workbranchstore.ErrIllegalTransition, which is reported, not
// re-applied, and does NOT trigger branch cleanup.
//
// Failure isolation is per branch: one branch's forge or store failure is
// collected and the remaining branches are still polled, so a single
// unreachable PR cannot starve every other proposal in the repo. The
// collected errors are joined and returned, which aborts nothing else (step
// 5 is the cycle's last step) but does put the repo in sync_state = error
// so the failure is visible, and the next tick is the retry.
type StorePRPoller struct {
	dataDir     string
	logger      *slog.Logger
	repos       repoByNameLookup
	branches    workBranchNameLister
	transitions workBranchTerminator
	forge       pullRequestTracker
	upstream    upstreamRefDeleter
}

// NewStorePRPoller builds a StorePRPoller rooted at dataDir (LOAM_DATA_DIR;
// the same root MirrorFetcher derives bare-mirror paths from), resolving
// repo rows through repos and work branches through branches (both
// typically the reposstore/workbranchstore Stores), transitioning branches
// through transitions (typically *workbranchstore.Store), reading PR state
// through forge (typically a forge.Provider), and deleting upstream
// branches through upstream (typically *gittransport.Transport).
func NewStorePRPoller(dataDir string, logger *slog.Logger, repos repoByNameLookup, branches workBranchNameLister, transitions workBranchTerminator, forge pullRequestTracker, upstream upstreamRefDeleter) *StorePRPoller {
	return &StorePRPoller{
		dataDir:     dataDir,
		logger:      logger,
		repos:       repos,
		branches:    branches,
		transitions: transitions,
		forge:       forge,
		upstream:    upstream,
	}
}

// PollPRs satisfies PRPoller. See the type doc comment for the poll set and
// what each state does. A failure to resolve the repo or to list its work
// branches aborts the whole step (there is nothing to poll without them);
// past that point every failure is per branch and collected.
func (p *StorePRPoller) PollPRs(ctx context.Context, repo RepoID) error {
	row, err := p.repos.GetRepoByName(ctx, string(repo))
	if err != nil {
		return fmt.Errorf("resolving repo %s for PR polling: %w", repo, err)
	}
	pollable, err := p.pollSet(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("listing work branches with a recorded PR for repo %s: %w", repo, err)
	}
	var errs []error
	for _, wb := range pollable {
		if ctxErr := ctx.Err(); ctxErr != nil {
			errs = append(errs, fmt.Errorf("stopped polling repo %s before work branch %s: %w", repo, wb.Name, ctxErr))
			break
		}
		if err := p.pollOne(ctx, repo, row.ForgeHost, row.UpstreamURL, wb); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// pollOne polls one work branch's recorded PR and applies the forge's
// answer. Branch cleanup runs ONLY after a terminal transition has actually
// committed: a forge lookup that failed, an unrecognized state, and a
// rejected or failed store transition all leave the upstream branch exactly
// where it is. Deleting a branch is the one irreversible thing this poller
// does, so it never happens on the strength of anything less than a
// durable, terminal state change this call itself made.
func (p *StorePRPoller) pollOne(ctx context.Context, repo RepoID, host, upstreamURL string, wb workbranchstore.WorkBranch) error {
	if wb.UpstreamPRNumber == nil {
		return fmt.Errorf("work branch %s in repo %s: %w", wb.Name, repo, errNoRecordedPR)
	}
	prNumber := int(*wb.UpstreamPRNumber)
	state, err := p.forge.GetPRState(ctx, string(repo), prNumber)
	if err != nil {
		return fmt.Errorf("getting state of PR %s#%d for work branch %s: %w", repo, prNumber, wb.Name, err)
	}
	switch state {
	case prStateOpen:
		return nil
	case prStateMerged:
		if _, err := p.transitions.Complete(ctx, wb.ID); err != nil {
			return fmt.Errorf("completing work branch %s on merged PR %s#%d: %w", wb.Name, repo, prNumber, err)
		}
		p.logger.InfoContext(ctx, "work branch completed by upstream PR merge", "repo", string(repo), "work_branch", wb.Name, "pr_number", prNumber)
	case prStateClosed:
		if _, err := p.transitions.Close(ctx, wb.ID, closedUpstreamPRReason); err != nil {
			return fmt.Errorf("closing work branch %s on closed PR %s#%d: %w", wb.Name, repo, prNumber, err)
		}
		p.logger.InfoContext(ctx, "work branch closed by upstream PR close", "repo", string(repo), "work_branch", wb.Name, "pr_number", prNumber)
	default:
		return fmt.Errorf("work branch %s, PR %s#%d reported state %q: %w", wb.Name, repo, prNumber, state, errUnknownPRState)
	}
	p.cleanupUpstreamBranch(ctx, repo, host, upstreamURL, wb.Name)
	return nil
}

// CleanupUpstreamBranch best-effort deletes repo's upstream
// loam/<workBranchName> branch -- the shared terminal-cleanup step
// docs/sync-spec.md -> PR State Tracking describes ("On either terminal
// state, the server best-effort deletes the upstream loam/... branch ...;
// failures are ignored"). It is exported so the admin's
// CloseWorkBranch-with-an-open-PR path (loam-ofg.14, "the branch cleanup
// above follows") calls this rather than reimplementing which ref may be
// deleted and how.
//
// Failures are logged and swallowed, never returned: the ref is routinely
// already gone (forges configured to auto-delete a merged PR's head branch
// make this a pure no-op), and a work branch that has already reached a
// terminal state must not be dragged back into a repo-level sync error by
// a cleanup that had nothing left to clean.
func (p *StorePRPoller) CleanupUpstreamBranch(ctx context.Context, repo RepoID, workBranchName string) {
	row, err := p.repos.GetRepoByName(ctx, string(repo))
	if err != nil {
		p.logger.WarnContext(ctx, "skipping upstream branch cleanup: repo lookup failed", "repo", string(repo), "work_branch", workBranchName, "error", err)
		return
	}
	p.cleanupUpstreamBranch(ctx, repo, row.ForgeHost, row.UpstreamURL, workBranchName)
}

// ClosePRAndCleanup closes an open upstream PR and then runs
// CleanupUpstreamBranch -- the reverse direction docs/sync-spec.md -> PR
// State Tracking describes ("the admin's CloseWorkBranch on a branch with
// an open PR closes the PR via ClosePR (best-effort), and the branch
// cleanup above follows"), for loam-ofg.14's admin CloseWorkBranch path.
//
// The branch cleanup ALWAYS runs, whatever the close did, and its own
// failures are always swallowed. The returned error reports only the close
// half, so a caller that wants to tell the admin "the work branch is
// closed here, but I could not close its PR upstream" can; the work branch
// row is already closed by the time this is called, so treating a non-nil
// return as fatal would be wrong.
//
// A forge.ErrPRAlreadyMerged rejection returns nil, deliberately: PATCH
// state=closed against a MERGED pull request is refused with a 412 and
// leaves the state untouched (verified against Forgejo 9.0.3; see that
// sentinel's godoc), which means the PR is already in a terminal state and
// there is nothing left for this call to do. Reporting it as a close
// failure would invite a retry that re-fails identically, forever, against
// a state the forge will never let anyone change.
func (p *StorePRPoller) ClosePRAndCleanup(ctx context.Context, repo RepoID, workBranchName string, prNumber int) error {
	closeErr := p.forge.ClosePR(ctx, string(repo), prNumber)
	switch {
	case closeErr == nil:
	case errors.Is(closeErr, forge.ErrPRAlreadyMerged):
		p.logger.InfoContext(ctx, "upstream PR was already merged; nothing to close", "repo", string(repo), "work_branch", workBranchName, "pr_number", prNumber)
		closeErr = nil
	default:
		p.logger.WarnContext(ctx, "best-effort upstream PR close failed", "repo", string(repo), "work_branch", workBranchName, "pr_number", prNumber, "error", closeErr)
		closeErr = fmt.Errorf("closing PR %s#%d for work branch %s: %w", repo, prNumber, workBranchName, closeErr)
	}
	p.CleanupUpstreamBranch(ctx, repo, workBranchName)
	return closeErr
}

// cleanupUpstreamBranch is the delete itself, given coordinates the caller
// has already resolved. The ref is always upstreamBranchPrefix + a name
// that passed safeWorkBranchName, so this can only ever remove a branch
// Loam itself pushed under loam/ -- never a target branch, never any other
// upstream ref, and never anything in the local mirror (the mirror's own
// refs/heads/<name> work-branch ref is untouched; docs/git-spec.md -> Ref
// Policy governs pushes INTO the mirror and is not what is being written
// here).
func (p *StorePRPoller) cleanupUpstreamBranch(ctx context.Context, repo RepoID, host, upstreamURL, workBranchName string) {
	ref, err := safeWorkBranchName(workBranchName)
	if err != nil {
		p.logger.ErrorContext(ctx, "skipping upstream branch cleanup: unsafe work branch name", "repo", string(repo), "work_branch", workBranchName, "error", err)
		return
	}
	if _, err := p.upstream.DeleteRemoteRef(ctx, host, mirrorpath.Dir(p.dataDir, string(repo)), upstreamURL, ref); err != nil {
		p.logger.WarnContext(ctx, "best-effort upstream branch cleanup failed", "repo", string(repo), "work_branch", workBranchName, "ref", ref, "error", err)
		return
	}
	p.logger.InfoContext(ctx, "deleted upstream branch", "repo", string(repo), "work_branch", workBranchName, "ref", ref)
}

// pollSet pages through every work_branches row for repoID and returns the
// ones worth polling: a recorded upstream_pr_number and a non-terminal
// state, sorted by name so a repo's branches are polled in a stable order
// regardless of what order the store paged them back in.
func (p *StorePRPoller) pollSet(ctx context.Context, repoID uuid.UUID) ([]workbranchstore.WorkBranch, error) {
	var pollable []workbranchstore.WorkBranch
	var offset int32
	for {
		page, total, err := p.branches.List(ctx, workbranchstore.ListFilter{RepoID: &repoID}, workBranchListPageSize, offset)
		if err != nil {
			return nil, err
		}
		for _, wb := range page {
			if wb.UpstreamPRNumber == nil {
				continue
			}
			if isTerminalWorkBranchState(wb.State) {
				continue
			}
			pollable = append(pollable, wb)
		}
		offset += int32(len(page))
		if len(page) == 0 || int64(offset) >= total {
			break
		}
	}
	sort.Slice(pollable, func(i, j int) bool { return pollable[i].Name < pollable[j].Name })
	return pollable, nil
}

// safeWorkBranchName validates name against workBranchNamePattern and
// returns the full upstream ref path to delete for it. A name that does not
// match, or that contains "..", yields errUnsafeWorkBranchName and no ref
// at all -- the caller deletes nothing.
func safeWorkBranchName(name string) (string, error) {
	if !workBranchNamePattern.MatchString(name) {
		return "", fmt.Errorf("%q: %w", name, errUnsafeWorkBranchName)
	}
	for i := 0; i+1 < len(name); i++ {
		if name[i] == '.' && name[i+1] == '.' {
			return "", fmt.Errorf("%q: %w", name, errUnsafeWorkBranchName)
		}
	}
	return upstreamBranchPrefix + name, nil
}
