// Package proposal implements loam.admin.v1.ProposalService
// (docs/web-spec.md -> "ProposalService"): the admin's proposal queue
// (ListProposals), the accept decision (AcceptProposal), and the close
// decision (CloseWorkBranch).
//
// Everything a proposal decision SHARES with an ordinary review -- viewing
// the branch, its diff, its comments, and sending it back for another round
// -- is deliberately not here: that is the CLI's loam.v1.WorkBranchService,
// which the admin reaches as a superuser (docs/web-spec.md ->
// ProposalService, first paragraph). In particular there is no
// RequestReview here; "Requesting a re-review sends the work branch back"
// (features/admin-proposals.feature) is WorkBranchService.RequestReview,
// already implemented in internal/handler/workbranch.
//
// # What this package does NOT do
//
// It performs no git and no forge work of its own. Accepting delegates
// wholesale to *mirrorsync.StoreProposalAccepter (loam-giq.7: push
// loam/<name>, idempotent CreatePR, record upstream_pr_url/number) and
// closing delegates its upstream half to *mirrorsync.StorePRPoller's
// exported ClosePRAndCleanup (loam-giq.8). Neither the loam/ ref namespace,
// nor the no-force push rule, nor the "skip CreatePR when a PR is already
// recorded" idempotency guard is re-expressed here -- a second copy of any
// of them is a second thing to keep in sync, and the one in mirrorsync is
// the one with the guarded UPDATE behind it.
//
// # Where the preconditions live
//
// docs/sync-spec.md -> Proposal Acceptance lists three: state reviewed, no
// conflict flag, and >= 1 non-stale approve verdict. The accepter enforces
// the first two itself, on the row it reads. The THIRD is this package's,
// by explicit division (see StoreProposalAccepter.AcceptProposal's own doc
// comment): it is a question about review_rounds/verdicts and the
// current-round staleness derivation, which internal/mirrorsync neither
// imports nor should. See Handler.AcceptProposal for the exact rule and
// currentRoundApprovals for why it is a store COUNT rather than anything
// computed here.
package proposal

import (
	"context"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/mirrorsync"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/reviewstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

//go:generate go tool moq -out moq_test.go . workBranchStore repoStore verdictStore proposalAccepter upstreamPRCloser

// workBranchStore is the internal/workbranchstore.Store surface this
// package needs, defined here at the consumer per repo convention.
// *workbranchstore.Store satisfies it structurally.
//
// Note what is absent: RecordUpstreamPR. This handler never writes
// upstream_pr_url/upstream_pr_number -- *mirrorsync.StoreProposalAccepter
// is the only writer of those columns in the tree, and the whole point of
// delegating to it is that its guarded UPDATE, not a handler, arbitrates
// two concurrent accepts.
type workBranchStore interface {
	// GetByName resolves the (repoID, name) identity AcceptProposal and
	// CloseWorkBranch are keyed on.
	GetByName(ctx context.Context, repoID uuid.UUID, name string) (workbranchstore.WorkBranch, error)
	// List pages the proposal-queue candidate set: every work branch in
	// state reviewed, across all repos.
	List(ctx context.Context, filter workbranchstore.ListFilter, limit, offset int32) ([]workbranchstore.WorkBranch, int64, error)
	// Close is the admin-only terminal transition, recording close_reason
	// in the same guarded statement.
	Close(ctx context.Context, id uuid.UUID, reason string) (workbranchstore.WorkBranch, error)
}

// repoStore resolves a repo name to the id work-branch lookups key on, and
// back again for the queue (whose rows span every enrolled repo and must
// each report their own repo name). Defined here at the consumer;
// *reposstore.Store satisfies it structurally.
type repoStore interface {
	GetRepoByName(ctx context.Context, name string) (reposstore.Repo, error)
	GetRepoByID(ctx context.Context, id uuid.UUID) (reposstore.Repo, error)
}

// verdictStore is the internal/reviewstore.VerdictStore surface this
// package needs, defined here at the consumer. *reviewstore.VerdictStore
// satisfies it structurally.
//
// Both methods derive current-round membership from MAX(review_rounds
// .number) inside their own SQL (internal/db/queries/review_rounds.sql) --
// there is no stored stale column anywhere in this system
// (docs/web-spec.md -> ProposalService: "`stale` is derived"), and this
// package must not synthesize a second mechanism for one. That is
// precisely why the approve precondition is CurrentRoundApproveCount and
// not a count this handler computes by walking List's records itself: one
// rule, one place, expressed once in SQL.
type verdictStore interface {
	// List returns every verdict across every round for the work branch,
	// newest round first, each decorated with whether its round is the
	// branch's current one.
	List(ctx context.Context, workBranchID uuid.UUID) ([]reviewstore.VerdictRecord, error)
	// CurrentRoundApproveCount counts approve-outcome verdicts in the
	// branch's current round only. A branch with no rounds at all counts
	// 0 rather than failing -- the proposal queue must not break just
	// because some branch has never been reviewed.
	CurrentRoundApproveCount(ctx context.Context, workBranchID uuid.UUID) (int64, error)
}

// proposalAccepter is the acceptance engine AcceptProposal delegates the
// entire git-and-forge half of the operation to, defined here at the
// consumer. *mirrorsync.StoreProposalAccepter satisfies it in production
// (wired by cmd/server/sync.go's buildProposalAccepter).
//
// One method, taking the two identifiers the RPC itself carries: that
// narrowness is what keeps this handler from acquiring a git transport, a
// forge provider, or a mirror path of its own.
type proposalAccepter interface {
	AcceptProposal(ctx context.Context, repo mirrorsync.RepoID, workBranchName string) (mirrorsync.AcceptResult, error)
}

// upstreamPRCloser is the best-effort upstream half of CloseWorkBranch,
// defined here at the consumer. *mirrorsync.StorePRPoller satisfies it in
// production; ClosePRAndCleanup is exported by that type specifically for
// this call site (see its doc comment, which names this bead).
//
// The work branch row is already CLOSED by the time this runs, so a
// non-nil return is reported to the admin as a warning in the log and
// never as a failed RPC: docs/web-spec.md says Loam closes the upstream PR
// "best-effort", and failing the RPC would tell the admin the close did not
// happen when it demonstrably did.
type upstreamPRCloser interface {
	ClosePRAndCleanup(ctx context.Context, repo mirrorsync.RepoID, workBranchName string, prNumber int) error
}
