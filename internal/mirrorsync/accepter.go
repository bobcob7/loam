package mirrorsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/forge"
	"github.com/bobcob7/loam/internal/mirrorpath"
	"github.com/bobcob7/loam/internal/refnames"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// attributionFooter is the PR-body footer docs/sync-spec.md -> Proposal
// Acceptance specifies, verbatim and in full:
//
//	---
//	Proposed via Loam.
//
// It names Loam and nothing else, deliberately: "agent attribution is
// already carried by the commit authors in git history". Nothing about the
// authoring agent, the model behind it, or the admin who accepted reaches
// an upstream PR body through this package, and prBody has no seam through
// which it could.
const attributionFooter = "---\nProposed via Loam."

// errProposalNotReviewed is StoreProposalAccepter's refusal to push a work
// branch that is not in the reviewed state (docs/sync-spec.md -> Proposal
// Acceptance: "preconditions: state reviewed"). Accepting is the one
// operation that makes a work branch visible on the forge under Loam's own
// name, so the state gate is enforced here rather than trusted to the
// caller.
var errProposalNotReviewed = errors.New("mirrorsync: work branch is not in the reviewed state")

// errProposalConflicted is the refusal for a work branch carrying a
// conflict flag (docs/sync-spec.md -> Proposal Acceptance: "not
// conflicted"; the web-spec ripple that added the conflict check to the
// precondition list). A conflicted branch no longer merges into its
// target, so opening a PR from it would put an unmergeable proposal in
// front of the upstream reviewers.
var errProposalConflicted = errors.New("mirrorsync: work branch is flagged conflicted")

// errUnusablePRIdentity is the refusal to record something the forge
// returned that cannot identify a pull request -- a non-positive number,
// an empty URL -- even though the call itself reported success.
//
// This is the "validate before trusting" rule this package has already
// paid for twice: parsePorcelainFetch fabricated RefUpdates out of
// interleaved stderr (fixed in 5aaf563), and git merge-tree reports a
// missing ref with the same exit status as a real conflict
// (internal/gitmergetree). A PR number is worse than either, because it is
// written to a column with a one-shot guard: recording #0 would both
// consume the row's single chance to record the real PR and park the work
// branch in StorePRPoller's poll set forever, failing every tick against a
// PR that does not exist.
var errUnusablePRIdentity = errors.New("mirrorsync: forge reported a pull request with no usable number or URL")

// errPRVanishedAfterDuplicate is the refusal on the one genuinely
// ambiguous forge answer in this path: CreatePR rejected the request with
// forge.ErrDuplicatePR ("a PR already exists for this head/target pair"),
// but the follow-up FindOpenPR found no such open PR.
//
// The two answers cannot both be current, so something changed underneath
// (the PR was closed or merged between the two calls) or the provider is
// inconsistent. The non-destructive reading is the only safe one: record
// nothing, surface a distinguishable error, and let the admin re-run
// accept -- which will now find no duplicate and open a fresh PR.
// Fabricating a number from ErrDuplicatePR's message is specifically NOT
// an option; that message embeds Forgejo's INTERNAL id, not the per-repo
// number this column needs, and the two coincide only on a repo's very
// first PR (see forge.ErrDuplicatePR's godoc).
var errPRVanishedAfterDuplicate = errors.New("mirrorsync: forge rejected the pull request as a duplicate but reports no open pull request for the pair")

// AcceptResult reports what one AcceptProposal call did. CreatedPR
// distinguishes the two legitimate successful outcomes -- a first accept
// that opened the pull request, and a re-accept that only fast-forwarded
// the branch an existing PR already tracks -- so a caller (the
// AcceptProposal RPC) can tell the admin which happened without re-reading
// the row.
type AcceptResult struct {
	UpstreamBranch string
	PRURL          string
	PRNumber       int
	CreatedPR      bool
}

// StoreProposalAccepter is the production proposal-acceptance engine
// (docs/sync-spec.md -> Proposal Acceptance; owned by bead giq.7). Given a
// repo and one of its work branches it does exactly three things, in
// order:
//
//  1. pushes the work-branch tip to the upstream branch loam/<name>, over
//     the upstream transport, as a create-or-fast-forward;
//  2. opens the upstream pull request from that branch into the work
//     branch's recorded target, with its title and its description plus
//     the attribution footer -- UNLESS a PR is already recorded;
//  3. records that PR's URL and number on the work_branches row, alongside
//     the tip just pushed (accepted_tip, loam-cgg) -- or, when step 2 was
//     skipped because a PR is already recorded, refreshes ONLY that tip.
//
// It is the only writer of work_branches.upstream_pr_number in the tree,
// and that column is the entire poll set of StorePRPoller (pr_poller.go).
// Nothing tracks a proposal's PR until this engine records one. It is
// likewise the only writer of accepted_tip, which
// internal/handler/proposal's ListProposals compares against a live
// re-resolve of the same ref to decide docs/web-spec.md's "PR branch is
// behind the work branch" clause.
//
// # Idempotency
//
// Re-accepting is expected, not exceptional: a conflicting target advance
// resets an accepted proposal to draft with its PR left open
// (docs/git-spec.md -> Target Advances & Catch-Up), and the admin accepts
// again once the agent has caught up and the branch has been re-reviewed.
// That second accept must fast-forward the same upstream branch and leave
// the same PR in place, never open a second one.
//
// Step 2 is therefore skipped whenever the row already carries an
// upstream_pr_number -- that null-check is the whole mechanism, with no
// separate accepted-once flag to keep in sync with it. The check is made
// twice, on purpose, at two different layers: here against the row this
// call read, and again inside the guarded UPDATE that records the number
// (workbranchstore.Store.RecordUpstreamPR). The first is what avoids the
// redundant forge call at all; the second is what makes the property
// survive two concurrent accepts, which the first cannot.
//
// # The push is never forced
//
// A work branch's history only ever gains commits -- the mirror's
// work-branch refs are agent-push-only and non-fast-forward pushes into
// them are refused by the pre-receive hook (docs/git-spec.md -> Ref
// Policy) -- so a create-or-fast-forward push is always sufficient, and a
// forced one could only ever destroy upstream work nobody asked to
// destroy.
//
// That is structural here, not a convention. The engine's only push seam
// (upstreamRefPusher) takes a refspec and nothing else, with no force
// parameter to pass; gittransport.Transport.Push behind it never adds
// --force; and the refspec is built by upstreamProposalRefspec from a
// name that has passed safeWorkBranchName, whose character class admits no
// '+' and therefore cannot produce a force refspec. There is no route from
// this type to a forced update.
//
// # A forge that says no is not a forge that failed
//
// CreatePR has three distinguishable outcomes here and each gets its own
// treatment: success (validated, then recorded), forge.ErrDuplicatePR
// (adopted through FindOpenPR, which is a lookup -- never a parse of the
// rejection message, whose embedded id is not this column's number), and
// anything else, including a transport failure or a cancelled context
// (recorded nothing, reported as itself). Every ambiguous path records
// nothing at all and leaves upstream_pr_number NULL, so a retried accept
// re-attempts against the branch the first attempt already pushed.
type StoreProposalAccepter struct {
	dataDir     string
	logger      *slog.Logger
	attribution bool
	repos       repoByNameLookup
	branches    workBranchByNameLookup
	recorder    workBranchPRRecorder
	forge       pullRequestOpener
	upstream    upstreamRefPusher
	tips        workBranchTipResolver
}

// NewStoreProposalAccepter builds a StoreProposalAccepter rooted at
// dataDir (LOAM_DATA_DIR; the same root every other mirror-addressing
// collaborator in this package derives bare-mirror paths from through
// internal/mirrorpath).
//
// attribution is config.Config.PRAttribution (LOAM_PR_ATTRIBUTION, default
// true; docs/server-spec.md). It is captured at construction rather than
// read per call because it is server-wide static configuration, and it is
// a plain bool rather than a whole config struct so this package keeps no
// dependency on internal/config.
//
// repos resolves the repo's forge host and upstream URL (typically
// *reposstore.Store), branches and recorder read and write the
// work_branches row (both typically *workbranchstore.Store), forge opens
// the pull request, and upstream runs the push (typically
// *gittransport.Transport).
//
// forge takes the repo as a per-call ARGUMENT rather than being a
// pre-bound forge.Provider, and that shape is required, not incidental: a
// *forge.Forgejo binds one host and one token at construction, while
// different enrolled repos can live on different forge hosts under
// different credentials, so one shared instance would send one repo's
// token to another repo's forge. The production implementation resolves
// repos.forge_host and that host's stored credential per call
// (cmd/server/sync.go's forgePRTracker, the same conclusion
// repoadmin.ForgeChecker and StorePRPoller's own tracker seam reached).
// tips resolves the work branch's local tip immediately before the push
// (typically *gitref.Creator, the same type registerWorkBranchService
// already wires for ref creation) -- what gets recorded as
// work_branches.accepted_tip (loam-cgg).
func NewStoreProposalAccepter(dataDir string, logger *slog.Logger, attribution bool, repos repoByNameLookup, branches workBranchByNameLookup, recorder workBranchPRRecorder, forge pullRequestOpener, upstream upstreamRefPusher, tips workBranchTipResolver) *StoreProposalAccepter {
	return &StoreProposalAccepter{
		dataDir:     dataDir,
		logger:      logger,
		attribution: attribution,
		repos:       repos,
		branches:    branches,
		recorder:    recorder,
		forge:       forge,
		upstream:    upstream,
		tips:        tips,
	}
}

// AcceptProposal pushes repo's workBranchName to loam/<workBranchName>
// upstream and, unless one is already recorded, opens and records its pull
// request. See the type doc comment for the idempotency, no-force, and
// forge-error rules this method implements.
//
// The preconditions checked here are the two this package can see on the
// row itself: state reviewed and no conflict flag. The third precondition
// docs/sync-spec.md lists -- at least one non-stale approve verdict -- is
// NOT checked here and is deliberately left to the AcceptProposal RPC
// handler (loam-ofg.14): it is a question about the review aggregate
// (review_rounds/verdicts, and the current-round staleness derivation that
// goes with it), which this package neither imports nor should.
func (a *StoreProposalAccepter) AcceptProposal(ctx context.Context, repo RepoID, workBranchName string) (AcceptResult, error) {
	row, err := a.repos.GetRepoByName(ctx, string(repo))
	if err != nil {
		return AcceptResult{}, fmt.Errorf("resolving repo %s for proposal acceptance: %w", repo, err)
	}
	wb, err := a.branches.GetByName(ctx, row.ID, workBranchName)
	if err != nil {
		return AcceptResult{}, fmt.Errorf("resolving work branch %s in repo %s: %w", workBranchName, repo, err)
	}
	if wb.State != workbranchstore.StateReviewed {
		return AcceptResult{}, fmt.Errorf("accepting work branch %s in repo %s (state %s): %w", wb.Name, repo, wb.State, errProposalNotReviewed)
	}
	if wb.Conflict != workbranchstore.ConflictNone {
		return AcceptResult{}, fmt.Errorf("accepting work branch %s in repo %s (conflict %s): %w", wb.Name, repo, wb.Conflict, errProposalConflicted)
	}
	refspec, upstreamBranch, err := upstreamProposalRefspec(wb.Name)
	if err != nil {
		return AcceptResult{}, fmt.Errorf("accepting work branch %s in repo %s: %w", wb.Name, repo, err)
	}
	// Resolved BEFORE the push, from the LOCAL mirror ref that push is
	// about to send upstream unchanged (a create-or-fast-forward push
	// moves no object, it only copies what is already there): this is
	// exactly the tip that lands at upstreamBranch, so recording it after
	// a successful push is recording what was actually accepted, not a
	// value that could have drifted in between (loam-cgg).
	tip, err := a.tips.ResolveWorkBranchRef(ctx, string(repo), wb.Name)
	if err != nil {
		return AcceptResult{}, fmt.Errorf("resolving the tip of work branch %s in repo %s before pushing: %w", wb.Name, repo, err)
	}
	if _, err := a.upstream.Push(ctx, row.ForgeHost, mirrorpath.Dir(a.dataDir, string(repo)), row.UpstreamURL, refspec); err != nil {
		return AcceptResult{}, fmt.Errorf("pushing work branch %s to %s on %s: %w", wb.Name, upstreamBranch, repo, err)
	}
	a.logger.InfoContext(ctx, "pushed proposal branch upstream", "repo", string(repo), "work_branch", wb.Name, "upstream_branch", upstreamBranch, "tip", tip)
	if wb.UpstreamPRNumber != nil {
		if _, err := a.recorder.RecordAcceptedTip(ctx, wb.ID, tip); err != nil {
			return AcceptResult{}, fmt.Errorf("refreshing the accepted tip for work branch %s in repo %s: %w", wb.Name, repo, err)
		}
		a.logger.InfoContext(ctx, "upstream PR already recorded; fast-forwarded it in place", "repo", string(repo), "work_branch", wb.Name, "pr_number", *wb.UpstreamPRNumber, "accepted_tip", tip)
		return AcceptResult{UpstreamBranch: upstreamBranch, PRURL: derefString(wb.UpstreamPRURL), PRNumber: int(*wb.UpstreamPRNumber), CreatedPR: false}, nil
	}
	prURL, prNumber, err := a.openPR(ctx, repo, wb, upstreamBranch)
	if err != nil {
		return AcceptResult{}, err
	}
	if _, err := a.recorder.RecordUpstreamPR(ctx, wb.ID, prURL, int32(prNumber), tip); err != nil {
		if !errors.Is(err, workbranchstore.ErrPRAlreadyRecorded) {
			return AcceptResult{}, fmt.Errorf("recording PR %s#%d for work branch %s: %w", repo, prNumber, wb.Name, err)
		}
		return a.adoptRacedPR(ctx, repo, row.ID, wb.Name, upstreamBranch)
	}
	a.logger.InfoContext(ctx, "opened upstream PR for proposal", "repo", string(repo), "work_branch", wb.Name, "upstream_branch", upstreamBranch, "pr_number", prNumber, "pr_url", prURL)
	return AcceptResult{UpstreamBranch: upstreamBranch, PRURL: prURL, PRNumber: prNumber, CreatedPR: true}, nil
}

// openPR opens the pull request for an accept that found no recorded one,
// or adopts the pull request the forge says already exists. It never
// records anything itself; every path either returns a validated
// (url, number) pair or an error that leaves upstream_pr_number NULL.
func (a *StoreProposalAccepter) openPR(ctx context.Context, repo RepoID, wb workbranchstore.WorkBranch, upstreamBranch string) (string, int, error) {
	title := derefString(wb.Title)
	body := prBody(derefString(wb.Description), a.attribution)
	prURL, prNumber, err := a.forge.CreatePR(ctx, string(repo), upstreamBranch, wb.Target, title, body)
	if err == nil {
		return validatePRIdentity(repo, upstreamBranch, prURL, prNumber)
	}
	if !errors.Is(err, forge.ErrDuplicatePR) {
		return "", 0, fmt.Errorf("opening upstream PR from %s into %s on %s: %w", upstreamBranch, wb.Target, repo, err)
	}
	// A previous accept opened the PR and then failed before recording it
	// (docs/sync-spec.md: "a failure between the steps ... is retried
	// safely by the admin re-running accept"). Recover the number by
	// LOOKUP, never by parsing the 409's message -- that message carries
	// Forgejo's internal id, not the per-repo number, and the two coincide
	// only on a repo's first PR (forge.ErrDuplicatePR's godoc).
	prURL, prNumber, found, findErr := a.forge.FindOpenPR(ctx, string(repo), upstreamBranch, wb.Target)
	if findErr != nil {
		return "", 0, fmt.Errorf("adopting the existing PR from %s into %s on %s after a duplicate rejection: %w", upstreamBranch, wb.Target, repo, findErr)
	}
	if !found {
		return "", 0, fmt.Errorf("adopting the existing PR from %s into %s on %s: %w", upstreamBranch, wb.Target, repo, errPRVanishedAfterDuplicate)
	}
	a.logger.InfoContext(ctx, "adopted an existing upstream PR reported as a duplicate", "repo", string(repo), "work_branch", wb.Name, "upstream_branch", upstreamBranch, "pr_number", prNumber)
	return validatePRIdentity(repo, upstreamBranch, prURL, prNumber)
}

// adoptRacedPR handles the narrow race the store's guarded UPDATE catches:
// this call read a NULL upstream_pr_number, opened a PR, and lost the
// column to a concurrent accept that got there first. Re-reading the row
// is the only way to report the number that actually won -- the one
// StorePRPoller will poll and the one a later re-accept will skip on --
// which is not necessarily the number this call just created.
func (a *StoreProposalAccepter) adoptRacedPR(ctx context.Context, repo RepoID, repoID uuid.UUID, workBranchName, upstreamBranch string) (AcceptResult, error) {
	wb, err := a.branches.GetByName(ctx, repoID, workBranchName)
	if err != nil {
		return AcceptResult{}, fmt.Errorf("re-reading work branch %s in repo %s after a concurrent accept recorded its PR: %w", workBranchName, repo, err)
	}
	if wb.UpstreamPRNumber == nil {
		return AcceptResult{}, fmt.Errorf("work branch %s in repo %s reported an already-recorded PR but carries none", workBranchName, repo)
	}
	a.logger.WarnContext(ctx, "a concurrent accept recorded this proposal's PR first", "repo", string(repo), "work_branch", workBranchName, "pr_number", *wb.UpstreamPRNumber)
	return AcceptResult{UpstreamBranch: upstreamBranch, PRURL: derefString(wb.UpstreamPRURL), PRNumber: int(*wb.UpstreamPRNumber), CreatedPR: false}, nil
}

// upstreamProposalRefspec returns the push refspec and the upstream branch
// name for a work branch: the mirror's own work-branch ref
// (refnames.WorkBranch(name), under Loam's reserved namespace) to
// refs/heads/loam/<name> upstream, exactly the namespace docs/sync-spec.md
// specifies and the same one StorePRPoller is the only deleter of. The
// reserved namespace stops at the mirror -- the UPSTREAM name is unchanged
// by loam-cmq, since upstream is already namespaced under loam/.
//
// The refspec carries no leading '+', and cannot: name is validated by
// safeWorkBranchName, whose character class (letters, digits, '.', '_',
// '-', no leading dash, no "..") admits nothing that could turn either
// side of the colon into a force marker, an extra refspec, or a git flag.
// The absence of '+' here and the absence of --force in
// gittransport.Transport.Push are together the whole no-force guarantee --
// there is no third place a force could be introduced from this package.
func upstreamProposalRefspec(name string) (refspec, upstreamBranch string, err error) {
	ref, err := safeWorkBranchName(name)
	if err != nil {
		return "", "", err
	}
	return refnames.WorkBranch(name) + ":" + ref, strings.TrimPrefix(ref, "refs/heads/"), nil
}

// prBody builds the upstream pull request body from the work branch's
// description (docs/sync-spec.md -> Proposal Acceptance, "PR body").
//
// With attribution off the body is the description alone, byte for byte --
// no trailing newline, no separator, nothing appended. With it on, the
// footer follows after a blank line, and it is attributionFooter verbatim:
// this function has no parameter through which an agent name, a model
// name, or an admin identity could reach the body.
//
// An empty description yields the footer with no leading blank lines. A
// reviewed work branch always has a description (the store's guarded
// transition into reviewable requires a non-empty title AND description),
// so this is a defensive branch rather than a reachable one -- but "\n\n"
// prefixed onto an empty body would be a visible defect upstream if it
// ever were reached.
func prBody(description string, attribution bool) string {
	if !attribution {
		return description
	}
	if description == "" {
		return attributionFooter
	}
	return description + "\n\n" + attributionFooter
}

// validatePRIdentity refuses a (url, number) pair that cannot name a real
// pull request, however the provider reported it. A successful call is not
// the same as a usable answer, and this column pair has no second chance:
// see errUnusablePRIdentity.
func validatePRIdentity(repo RepoID, upstreamBranch, prURL string, prNumber int) (string, int, error) {
	if prNumber <= 0 || prURL == "" {
		return "", 0, fmt.Errorf("pull request for %s on %s reported as %q/#%d: %w", upstreamBranch, repo, prURL, prNumber, errUnusablePRIdentity)
	}
	return prURL, prNumber, nil
}

// derefString reads a nullable text column into a plain string, treating
// SQL NULL as empty.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
