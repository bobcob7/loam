package proposal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/forge"
	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/gen/loam/admin/v1/adminv1connect"
	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/mirrorsync"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/reviewstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// defaultListLimit is the page size ListProposals uses when the request's
// Page.limit is unset (0), matching every other paginated list surface in
// this tree (docs/cli-spec.md -> "list": "--limit <n> ... defaults to 100").
const defaultListLimit = 100

// candidateScanPageSize is how many work_branches rows ListProposals pulls
// per store round trip while scanning for proposals. It is NOT the response
// page size: the proposal predicate spans three tables (state and conflict
// on work_branches, the approve count on verdicts joined to the branch's
// current review_rounds row) and no single store query expresses it, so the
// candidate set -- state=reviewed, every repo -- is scanned in full and the
// caller's limit/offset is applied to the FILTERED result.
//
// Applying limit/offset to the unfiltered scan instead would be materially
// wrong, not merely approximate: a page of 100 reviewed branches of which 3
// are proposals would return 3 rows and a next-page offset of 100, so the
// admin would page through mostly-empty pages and PageInfo.total would
// report reviewed branches rather than proposals. Scanning is affordable
// because the candidate set is bounded by "reviewed, or demoted out of
// reviewed by a conflicting advance, and not yet decided" (queueCandidates),
// which is exactly the queue the admin is looking at; the same page-
// everything-then-filter shape mirrorsync.StorePRPoller's pollSet uses, and
// for the same reason.
const candidateScanPageSize = 200

// Handler implements adminv1connect.ProposalServiceHandler.
type Handler struct {
	workBranches workBranchStore
	repos        repoStore
	verdicts     verdictStore
	accepter     proposalAccepter
	prCloser     upstreamPRCloser
	tips         workBranchTipResolver
	errors       *handler.ErrorMapper
	logger       *slog.Logger
}

// compile-time assertion that Handler satisfies the generated interface.
var _ adminv1connect.ProposalServiceHandler = (*Handler)(nil)

// New builds a Handler over the given seams. accepter is the proposal
// acceptance engine (in production *mirrorsync.StoreProposalAccepter) and
// prCloser the upstream PR close + branch cleanup (in production
// *mirrorsync.StorePRPoller); both are constructed at the composition root,
// where the per-repo forge binding and the mirror root live. tips resolves
// a work branch's live tip against the local mirror (in production
// *gitref.Creator) -- ListProposals's loam-cgg comparison (see
// proposalUpToDate).
func New(
	workBranches workBranchStore,
	repos repoStore,
	verdicts verdictStore,
	accepter proposalAccepter,
	prCloser upstreamPRCloser,
	tips workBranchTipResolver,
	errors *handler.ErrorMapper,
	logger *slog.Logger,
) *Handler {
	return &Handler{
		workBranches: workBranches,
		repos:        repos,
		verdicts:     verdicts,
		accepter:     accepter,
		prCloser:     prCloser,
		tips:         tips,
		errors:       errors,
		logger:       logger,
	}
}

// ListProposals returns the proposal queue: every work branch, across every
// enrolled repo, that carries at least one non-stale approve verdict and is
// therefore awaiting an admin decision -- each with that round's verdicts so
// the admin sees who approved without a second call (docs/web-spec.md ->
// ProposalService), and each marked `acceptable` or not.
//
// # Blocked branches are listed and marked, never dropped (loam-u84g)
//
// The queue's membership test is "does this branch hold a live approve",
// which is a fact about the REVIEW. Whether it can be merged right now is a
// separate fact about the TARGET and the forge, and it now travels as
// Proposal.acceptable rather than as absence from the list. The three things
// that make a branch unacceptable -- a conflict flag, upstream drift, and the
// demotion out of REVIEWED that a conflicting target advance performs -- used
// to remove it from this response entirely.
//
// That was silent and it was expensive. An operator working this queue merges
// what it offers; a branch that is absent is not skipped, not shown blocked,
// and not distinguishable from one that was never approved. On
// forgejo_admin/loam, wb-88c455 held a non-stale approve on an already-merged
// P1 fix while sitting in draft (its target had advanced into a conflict), and
// it missed the v0.0.8 release because no surface in the system named it. The
// demotion itself is correct and specified (docs/git-spec.md -> "Target
// Advances & Catch-Up", loam-di9q); making it invisible was not.
//
// Nothing this change lists becomes acceptable. AcceptProposal re-checks all
// three preconditions itself and refuses exactly as before -- acceptableNow is
// the same predicate, so the queue and the accept gate cannot disagree about
// which branches the button is for. What loam-giq.11 wanted from excluding
// drift ("listing it would offer the admin a button that cannot work") is
// preserved by the flag, and its stated goal -- that conflict and drift
// "reach the admin on WorkBranch itself ... [on] the proposal queue" -- becomes
// true for the first time, since a filtered queue could only ever carry NONE
// in both fields.
//
// # The predicate, made exact (loam-cgg)
//
// docs/web-spec.md defines a proposal as a reviewed branch with >= 1
// non-stale approve "awaiting an admin decision -- either it has no
// upstream PR yet, or its existing PR's branch is behind the work branch (a
// conflict catch-up that has been re-reviewed)". The first three conditions
// are evaluated here exactly, as before.
//
// A branch carrying upstream drift is marked unacceptable alongside a
// conflicted one (loam-giq.11, docs/web-spec.md -> "Upstream drift is
// surfaced, not listed" -- surfaced is now literal: it is on the row, with
// the button suppressed, rather than absent). AcceptProposal refuses it, so
// offering the admin a button would be offering one that cannot work. What
// the admin needs about it -- that it is diverged, and how that differs
// from a conflict -- travels on the WorkBranch message itself
// (workBranchToProto), which is the surface every other view of a branch
// already reads. The final disjunction now is too:
// mirrorsync.StoreProposalAccepter records the tip it pushes as
// work_branches.accepted_tip on every accept (both a first accept and a
// re-accept fast-forward), so "the PR's branch is behind" reduces to a live
// tip resolve (proposalUpToDate) compared against that recorded value --
// equality, never ancestry, since accepted_tip already IS what was pushed.
//
// The one case that STILL over-includes, deliberately, is a row with a
// recorded PR but no recorded accepted_tip: every work branch accepted
// before this column existed. NULL is read as "cannot prove this is caught
// up", not as "up to date" -- a migration that made a historical row
// silently vanish from the queue would be a data-loss-shaped bug even
// though no row is deleted, and it would strand a branch loam-ofg.14's own
// over-inclusion was written to keep visible. Over-inclusion for that one
// case still costs only a redundant row the admin can ignore, or an accept
// that idempotently fast-forwards; the option this bead explicitly did NOT
// take -- excluding every branch with a PR -- was rejected for the reason
// loam-ofg.14 already gives: it would hide the re-accept-after-catch-up
// case the spec names.
//
// Pagination applies to the filtered result; see candidateScanPageSize.
func (h *Handler) ListProposals(ctx context.Context, req *connect.Request[adminv1.ListProposalsRequest]) (*connect.Response[adminv1.ListProposalsResponse], error) {
	if err := requireAdmin(ctx, "listing proposals"); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	candidates, err := h.queueCandidates(ctx)
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	repoNames := make(map[uuid.UUID]string, len(candidates))
	proposals := make([]*adminv1.Proposal, 0, len(candidates))
	for _, wb := range candidates {
		approvals, err := h.verdicts.CurrentRoundApproveCount(ctx, wb.ID)
		if err != nil {
			return nil, h.errors.ToConnectErr(fmt.Errorf("counting current-round approvals for work branch %s: %w", wb.Name, err))
		}
		if approvals < 1 {
			continue
		}
		repoName, err := h.repoNameFor(ctx, wb.RepoID, repoNames)
		if err != nil {
			return nil, h.errors.ToConnectErr(err)
		}
		canAccept := acceptableNow(wb)
		if canAccept {
			upToDate, err := h.proposalUpToDate(ctx, repoName, wb)
			if err != nil {
				return nil, h.errors.ToConnectErr(err)
			}
			if upToDate {
				continue
			}
		}
		records, err := h.verdicts.List(ctx, wb.ID)
		if err != nil {
			return nil, h.errors.ToConnectErr(fmt.Errorf("listing verdicts for work branch %s: %w", wb.Name, err))
		}
		proposals = append(proposals, &adminv1.Proposal{
			WorkBranch: workBranchToProto(repoName, wb),
			Verdicts:   currentRoundVerdicts(records),
			Acceptable: canAccept,
		})
	}
	limit, offset := pageLimitOffset(req.Msg.GetPage())
	page := paginate(proposals, limit, offset)
	return connect.NewResponse(&adminv1.ListProposalsResponse{
		Proposals: page,
		PageInfo:  &loamv1.PageInfo{Total: uint32(len(proposals))},
	}), nil
}

// AcceptProposal pushes the work branch upstream as loam/<name> and opens
// (or, on a re-accept, fast-forwards) its upstream pull request, recording
// the PR on the work_branches row. The branch STAYS REVIEWED: only sync's
// PR poller flips it to complete on merge or closed on an upstream close
// (docs/web-spec.md -> ProposalService, and the bead's own CRITICAL RULE).
//
// # The approve precondition -- this handler's, and exactly what it means
//
// docs/sync-spec.md -> Proposal Acceptance requires ">= 1 non-stale approve
// verdict", and that is implemented literally: at least one verdict with
// outcome 'approve' cast in the branch's CURRENT review round, where
// "current" is MAX(review_rounds.number) for the branch and staleness is
// that comparison's negation -- derived, never stored (docs/web-spec.md ->
// ProposalService: "`stale` is derived"). Three consequences are worth
// stating because each is a rule someone could reasonably assume instead:
//
//   - An approve from an EARLIER round does not count. Requesting a
//     re-review opens a new round, which is precisely how prior verdicts go
//     stale (features/admin-proposals.feature -> "Requesting a re-review
//     sends the work branch back": "the prior verdicts are marked stale").
//   - A disapprove does NOT veto. Nothing in docs/web-spec.md,
//     docs/sync-spec.md, docs/cli-spec.md, or the feature file expresses a
//     "no outstanding disapprove" rule; the queue scenario excludes a branch
//     with ONLY a disapprove, which the >= 1 approve rule already excludes
//     on its own. A branch with one approve and one disapprove is therefore
//     a proposal and is acceptable -- the admin is the decision-maker and
//     can see both verdicts in the queue. Adding a veto here would be
//     inventing policy the specs do not state.
//   - A neutral verdict is not an approve, so a branch with only neutrals is
//     refused ("Accepting requires an approval").
//
// The count comes from reviewstore.VerdictStore.CurrentRoundApproveCount --
// a query built for this ("backs the proposal queue / approval bar") whose
// current-round join is the same MAX(number) subquery every other staleness
// derivation in this system uses -- rather than from anything counted in Go
// here, so the accept gate and the queue predicate cannot drift apart.
//
// # Idempotency
//
// This handler deliberately does NOT refuse a branch that already has a
// recorded upstream_pr_number. Re-accepting after a conflict catch-up is
// the documented flow ("Re-accepting a caught-up work branch updates the
// existing PR"), and the accepter's two-layer guard -- the null check on
// the row it reads, plus the guarded UPDATE in
// workbranchstore.RecordUpstreamPR -- is what keeps it from opening a
// second PR. A pre-check here would not add safety; it would DEFEAT that
// flow by rejecting the second accept outright.
//
// # Preconditions checked here versus in the accepter
//
// State, conflict, and upstream drift are re-checked here, ahead of the
// delegation, even though the accepter checks all three too. That is not redundancy for its own
// sake: the accepter's sentinels are unexported, so an error surfacing from
// there cannot be classified at this boundary and would collapse to
// CodeInternal, whereas "this branch is not reviewed" and "this branch is
// conflicted" are failed preconditions the admin can act on. Checking here
// is what makes the RPC answer correctly; the accepter's own check remains
// the authority against a concurrent transition between these two reads.
func (h *Handler) AcceptProposal(ctx context.Context, req *connect.Request[adminv1.AcceptProposalRequest]) (*connect.Response[adminv1.AcceptProposalResponse], error) {
	if err := requireAdmin(ctx, "accepting a proposal"); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	repo, name := req.Msg.GetRepo(), req.Msg.GetWorkBranch()
	repoRow, wb, err := h.resolveWorkBranch(ctx, repo, name)
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	if wb.State != workbranchstore.StateReviewed {
		return nil, h.errors.ToConnectErr(fmt.Errorf("work branch %s/%s is %s, not reviewed -- only a reviewed branch can be accepted: %w", repo, name, wb.State, handler.ErrFailedPrecondition))
	}
	if wb.Conflict != workbranchstore.ConflictNone {
		return nil, h.errors.ToConnectErr(fmt.Errorf("work branch %s/%s is flagged %s against its target -- it must be caught up and re-reviewed before it can be accepted: %w", repo, name, wb.Conflict, handler.ErrFailedPrecondition))
	}
	if wb.UpstreamDrift != workbranchstore.DriftNone {
		return nil, h.errors.ToConnectErr(fmt.Errorf("work branch %s/%s has upstream drift %s -- its loam/%s branch on the forge was moved somewhere this branch cannot fast-forward into, and no push Loam is willing to make reconciles it; reconcile the upstream branch by hand first: %w", repo, name, wb.UpstreamDrift, name, handler.ErrFailedPrecondition))
	}
	approvals, err := h.verdicts.CurrentRoundApproveCount(ctx, wb.ID)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("counting current-round approvals for work branch %s/%s: %w", repo, name, err))
	}
	if approvals < 1 {
		return nil, h.errors.ToConnectErr(fmt.Errorf("work branch %s/%s has no approve verdict in its current review round: %w", repo, name, handler.ErrFailedPrecondition))
	}
	result, err := h.accepter.AcceptProposal(ctx, mirrorsync.RepoID(repoRow.Name), wb.Name)
	if err != nil {
		return nil, h.errors.ToConnectErr(mapAcceptErr(err, fmt.Sprintf("accepting work branch %s/%s", repo, name)))
	}
	h.logger.InfoContext(ctx, "admin accepted proposal", "repo", repoRow.Name, "work_branch", wb.Name, "upstream_branch", result.UpstreamBranch, "pr_url", result.PRURL, "created_pr", result.CreatedPR)
	return connect.NewResponse(&adminv1.AcceptProposalResponse{
		PrUrl:          result.PRURL,
		UpstreamBranch: result.UpstreamBranch,
	}), nil
}

// CloseWorkBranch closes a work branch (-> CLOSED), recording body as its
// close reason, and -- if Loam ever opened one for it -- best-effort closes
// the upstream pull request and deletes the loam/ branch behind it
// (docs/web-spec.md -> ProposalService: "Loam opened it, Loam closes it").
//
// The order is deliberate and is the one mirrorsync.ClosePRAndCleanup's
// contract assumes: the work_branches row is closed FIRST, by a guarded
// single-statement UPDATE, and only then is the forge touched. A close that
// succeeded locally is therefore never reported as a failure because the
// forge was unreachable, and the closed row immediately leaves
// StorePRPoller's poll set, so the poller does not race this call to
// re-close the same PR.
//
// The upstream half is best-effort BY SPECIFICATION, so its failure is
// logged and the RPC still succeeds -- returning an error here would tell
// the admin the branch is not closed when it is, and invite a retry that
// would answer ErrIllegalTransition on the already-closed row. An upstream
// PR that is already merged is not a failure at all and is swallowed inside
// ClosePRAndCleanup (a forge refuses PATCH state=closed on a merged PR).
//
// Nothing upstream is touched when no PR was ever recorded. That leaves one
// known, narrow gap: an accept whose push succeeded and whose CreatePR then
// failed leaves a loam/<name> branch upstream with no recorded PR, and
// closing the branch will not reap it. Reaping it would mean a remote
// delete on EVERY close, including the overwhelmingly common case of a
// draft branch that was never accepted at all -- a network round trip and a
// warn-level log line per close, to clean up after a failure mode whose own
// retry (re-running accept) already resolves it.
func (h *Handler) CloseWorkBranch(ctx context.Context, req *connect.Request[adminv1.CloseWorkBranchRequest]) (*connect.Response[adminv1.CloseWorkBranchResponse], error) {
	if err := requireAdmin(ctx, "closing a work branch"); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	repo, name := req.Msg.GetRepo(), req.Msg.GetWorkBranch()
	repoRow, wb, err := h.resolveWorkBranch(ctx, repo, name)
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	body := req.Msg.GetBody()
	if body == "" {
		return nil, h.errors.ToConnectErr(fmt.Errorf("a close reason is required: %w", handler.ErrInvalidArgument))
	}
	closed, err := h.workBranches.Close(ctx, wb.ID, body)
	if err != nil {
		return nil, h.errors.ToConnectErr(mapWorkBranchStoreErr(err, fmt.Sprintf("closing work branch %s/%s", repo, name)))
	}
	h.logger.InfoContext(ctx, "admin closed work branch", "repo", repoRow.Name, "work_branch", wb.Name)
	if wb.UpstreamPRNumber != nil {
		if err := h.prCloser.ClosePRAndCleanup(ctx, mirrorsync.RepoID(repoRow.Name), wb.Name, int(*wb.UpstreamPRNumber)); err != nil {
			h.logger.WarnContext(ctx, "work branch closed, but its upstream PR could not be closed", "repo", repoRow.Name, "work_branch", wb.Name, "pr_number", *wb.UpstreamPRNumber, "error", err)
		}
	}
	return connect.NewResponse(&adminv1.CloseWorkBranchResponse{
		WorkBranch: workBranchToProto(repoRow.Name, closed),
	}), nil
}

// acceptableNow reports whether AcceptProposal's own preconditions hold for
// wb right now -- the same three the RPC checks, in the same order, and
// deliberately NOT a second, looser opinion about them. A branch this
// returns false for is still listed, marked unacceptable; see
// queueCandidates for why listing it is the point.
//
// The approve count is not part of this: a branch with no live approve is
// not in the queue at all, so every branch reaching this predicate has
// already cleared that bar.
func acceptableNow(wb workbranchstore.WorkBranch) bool {
	return wb.State == workbranchstore.StateReviewed &&
		wb.Conflict == workbranchstore.ConflictNone &&
		wb.UpstreamDrift == workbranchstore.DriftNone
}

// queueCandidates pages the whole candidate set the proposal queue is
// computed over, across every enrolled repo: every state=reviewed branch,
// plus every state=draft branch a conflicting target advance DEMOTED out of
// review (conflict = 'reset').
//
// # Why draft is scanned at all (loam-u84g)
//
// This used to scan state=reviewed alone, and ListProposals then dropped any
// candidate carrying a conflict or upstream drift. Both halves of that hid a
// branch the operator needed to see, and the first hid it beyond recovery:
// docs/git-spec.md -> "Target Advances & Catch-Up" resets a reviewed branch
// to draft on a conflicting advance, and deliberately does NOT stale its
// verdicts at that moment ("no round opens there"). So a branch could -- and
// on forgejo_admin/loam wb-88c455 did -- hold a non-stale approve while
// sitting in draft, which is a state no listing this system offers looks at:
// the proposal queue scanned only reviewed, and `work list` defaults to
// --state reviewable. An approved P1 fix missed a release inside that gap
// and nothing anywhere reported it as blocked, skipped, or even present.
//
// conflict = 'reset' is exactly and only the demoted set. MarkWorkBranchConflicted
// (internal/db/queries/work_branches.sql) writes 'reset' when and only when it
// moves a reviewable/reviewed branch to draft; a branch that was already draft
// gains 'flagged' instead. So this scan cannot pick up an ordinary in-progress
// draft, and the approve-count filter in ListProposals would exclude one anyway
// -- a branch that never reached review has no verdicts to count.
//
// # Why not a store-level filter
//
// ListFilter has no conflict column, and adding one would mean a new sqlc
// query for a predicate this is the only caller of. The draft page is scanned
// and filtered in Go instead, the same page-everything-then-filter shape
// candidateScanPageSize's doc comment already justifies for the reviewed set.
// The cost is one extra paged scan per queue load, bounded by the number of
// open drafts.
func (h *Handler) queueCandidates(ctx context.Context) ([]workbranchstore.WorkBranch, error) {
	candidates, err := h.scanState(ctx, workbranchstore.StateReviewed)
	if err != nil {
		return nil, err
	}
	demoted, err := h.scanState(ctx, workbranchstore.StateDraft)
	if err != nil {
		return nil, err
	}
	for _, wb := range demoted {
		if wb.Conflict == workbranchstore.ConflictReset {
			candidates = append(candidates, wb)
		}
	}
	return candidates, nil
}

// scanState pages the whole state=<state> set across every enrolled repo. It
// stops on an empty page as well as on the total, so a store whose total and
// page contents ever disagree cannot spin here forever -- the same
// two-condition loop guard mirrorsync's pollSet uses.
func (h *Handler) scanState(ctx context.Context, state workbranchstore.State) ([]workbranchstore.WorkBranch, error) {
	var all []workbranchstore.WorkBranch
	var offset int32
	for {
		page, total, err := h.workBranches.List(ctx, workbranchstore.ListFilter{State: state}, candidateScanPageSize, offset)
		if err != nil {
			return nil, fmt.Errorf("listing %s work branches for the proposal queue: %w", state, err)
		}
		all = append(all, page...)
		offset += int32(len(page))
		if len(page) == 0 || int64(offset) >= total {
			return all, nil
		}
	}
}

// proposalUpToDate reports whether wb's already-recorded upstream PR is
// already caught up with the work branch (loam-cgg), so ListProposals can
// exclude it: docs/web-spec.md's disjunction only lists a branch with a
// recorded PR when "its existing PR's branch is behind the work branch".
//
// Two of the three states this reads never call ResolveWorkBranchRef at
// all: no recorded PR (UpstreamPRNumber nil) is never up to date -- there
// is nothing for the admin to have already decided -- and a recorded PR
// with no recorded accepted_tip (every row from before this column
// existed) is ALSO never treated as up to date, on purpose. A NULL here
// must not be read as "caught up": that would silently drop every
// historical accepted-and-still-open proposal out of the queue the moment
// this column's migration runs, which is a data-loss-shaped bug even
// though no row is deleted. Only the third state -- a recorded PR AND a
// recorded tip -- resolves the branch's CURRENT tip live from the mirror
// (the same "never cached, always read from git" rule every other SHA in
// this codebase follows) and compares it against accepted_tip: equal means
// caught up, anything else means behind. That is a plain identity check,
// not ancestry -- accepted_tip already IS the commit that was pushed, so
// there is no history to walk, only two SHAs to compare.
func (h *Handler) proposalUpToDate(ctx context.Context, repoName string, wb workbranchstore.WorkBranch) (bool, error) {
	if wb.UpstreamPRNumber == nil || wb.AcceptedTip == nil {
		return false, nil
	}
	tip, err := h.tips.ResolveWorkBranchRef(ctx, repoName, wb.Name)
	if err != nil {
		return false, fmt.Errorf("resolving work branch %s/%s's tip to check proposal freshness: %w", repoName, wb.Name, err)
	}
	return tip == *wb.AcceptedTip, nil
}

// repoNameFor resolves a work branch's repo name, memoized in cache: the
// queue spans every enrolled repo, so an uncached lookup would be one store
// round trip per row rather than one per distinct repo.
func (h *Handler) repoNameFor(ctx context.Context, repoID uuid.UUID, cache map[uuid.UUID]string) (string, error) {
	if name, ok := cache[repoID]; ok {
		return name, nil
	}
	row, err := h.repos.GetRepoByID(ctx, repoID)
	if err != nil {
		return "", fmt.Errorf("resolving repo %s for the proposal queue: %w", repoID, err)
	}
	cache[repoID] = row.Name
	return row.Name, nil
}

// resolveWorkBranch resolves the (repo, work branch) name pair every
// decision RPC is keyed on, mapping a missing repo or branch to
// handler.ErrNotFound and a missing field to handler.ErrInvalidArgument.
func (h *Handler) resolveWorkBranch(ctx context.Context, repo, name string) (reposstore.Repo, workbranchstore.WorkBranch, error) {
	if repo == "" || name == "" {
		return reposstore.Repo{}, workbranchstore.WorkBranch{}, fmt.Errorf("repo and work branch are required: %w", handler.ErrInvalidArgument)
	}
	repoRow, err := h.repos.GetRepoByName(ctx, repo)
	if err != nil {
		return reposstore.Repo{}, workbranchstore.WorkBranch{}, mapRepoStoreErr(err, fmt.Sprintf("repo %s", repo))
	}
	wb, err := h.workBranches.GetByName(ctx, repoRow.ID, name)
	if err != nil {
		return reposstore.Repo{}, workbranchstore.WorkBranch{}, mapWorkBranchStoreErr(err, fmt.Sprintf("work branch %s/%s", repo, name))
	}
	return repoRow, wb, nil
}

// requireAdmin is defence in depth on top of the routing-level gate, not a
// replacement for it: the whole /loam.admin.v1.* path group is already
// wrapped in httpauth.Auth.AdminOnly before any request reaches a handler
// (docs/web-spec.md -> Auth), which is exactly why internal/handler/repoadmin
// documents having no per-RPC gate of its own.
//
// This package differs from that sibling on purpose. Its two mutating RPCs
// are the only ones in the system that push Loam's own name onto a third-
// party forge and that terminate a work branch, and both are irreversible
// from Loam's side. httpauth.IsAdmin reads the flag AdminOnly itself sets,
// so this costs one context read and makes "only an admin can accept or
// close" a property asserted by this package's own tests rather than one
// inherited from a wiring line in cmd/server that no test in this package
// can see.
func requireAdmin(ctx context.Context, operation string) error {
	if httpauth.IsAdmin(ctx) {
		return nil
	}
	return fmt.Errorf("%s requires the admin superuser: %w", operation, handler.ErrPermissionDenied)
}

// currentRoundVerdicts renders the branch's CURRENT-round verdicts as
// proto, dropping every stale one. The proto field's own comment is the
// contract ("This round's verdicts (unique reviewer + outcome), so the
// admin sees who approved"), and the verdicts_round_id_reviewer_key unique
// constraint means current-round records are already one per reviewer, so
// no de-duplication step is needed or performed.
//
// This is a narrower answer than WorkBranchService.ListVerdicts, which
// returns each reviewer's LATEST verdict across all rounds with a stale
// flag: that is a history view, this is a decision view. Stale is set
// anyway, from the record and not from a literal, so a record that ever
// reached here mislabelled would show as such rather than being silently
// rewritten to false.
func currentRoundVerdicts(records []reviewstore.VerdictRecord) []*loamv1.VerdictSummary {
	summaries := make([]*loamv1.VerdictSummary, 0, len(records))
	for _, record := range records {
		if !record.Current {
			continue
		}
		summaries = append(summaries, &loamv1.VerdictSummary{
			Reviewer: record.Reviewer,
			Outcome:  outcomeToProto(record.Outcome),
			Stale:    !record.Current,
			Round:    uint32(record.RoundNumber),
		})
	}
	return summaries
}

// paginate applies the request's limit/offset to the already-filtered
// proposal list. An offset past the end yields an empty page rather than an
// error -- the same thing a store-side OFFSET past the last row does.
func paginate(proposals []*adminv1.Proposal, limit, offset int32) []*adminv1.Proposal {
	if offset >= int32(len(proposals)) {
		return nil
	}
	end := offset + limit
	if end > int32(len(proposals)) {
		end = int32(len(proposals))
	}
	return proposals[offset:end]
}

// pageLimitOffset reads a request Page into the (limit, offset) pair, with
// defaultListLimit for an unset or non-positive limit. page may be nil;
// protoc-gen-go's getters are nil-safe, so no separate branch is needed.
func pageLimitOffset(page *loamv1.Page) (int32, int32) {
	limit := int32(defaultListLimit)
	if page.GetLimit() > 0 {
		limit = int32(page.GetLimit())
	}
	offset := int32(page.GetOffset())
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// mapAcceptErr classifies a failure from the acceptance engine.
//
// The distinction it draws is "the forge REFUSED" versus "the call FAILED",
// a conflation this repo has already paid for twice (parsePorcelainFetch
// fabricating RefUpdates out of interleaved stderr; git merge-tree
// reporting a missing ref with the same exit status as a real conflict). A
// refusal the forge is entitled to make -- an invalid or under-scoped
// token, a token with no git write access, a repo the forge does not have
// -- is a failed precondition the admin can fix from the Credentials or
// Repos screen, and it is reported as one. Everything else, including a
// transport failure, a cancelled context, and the accepter's own
// unexported refusals (a state or conflict change that raced the checks in
// AcceptProposal above), falls through to CodeInternal-and-log: an
// unrecognized failure must be loud, never quietly recast as the admin's
// fault.
//
// err is formatted with %w, not %v: the earlier %v left the text intact
// but broke the chain -- errors.Is(mapped, forge.ErrInvalidToken) was
// false -- the same defect loam-blc, loam-dq0o, and loam-c4ab fixed
// elsewhere, here in its %v variant rather than a dropped argument
// (loam-jv8f).
func mapAcceptErr(err error, context string) error {
	switch {
	case errors.Is(err, forge.ErrInvalidToken), errors.Is(err, forge.ErrInsufficientScope),
		errors.Is(err, forge.ErrNoWriteAccess), errors.Is(err, forge.ErrRepoNotFound):
		return fmt.Errorf("%s: the forge refused the request (%w): %w", context, err, handler.ErrFailedPrecondition)
	case errors.Is(err, workbranchstore.ErrNotFound):
		return fmt.Errorf("%s: %w", context, handler.ErrNotFound)
	default:
		return fmt.Errorf("%s: %w", context, err)
	}
}

// mapRepoStoreErr maps a repo lookup failure: an unknown repo name is a
// not-found, anything else is unclassified.
func mapRepoStoreErr(err error, context string) error {
	if errors.Is(err, reposstore.ErrNotFound) {
		return fmt.Errorf("%s: %w", context, handler.ErrNotFound)
	}
	return fmt.Errorf("%s: %w", context, err)
}

// mapWorkBranchStoreErr maps a work-branch lookup or transition failure.
// ErrIllegalTransition on the close path means the branch is already
// terminal (complete or closed) -- a failed precondition, not an internal
// fault: the admin asked to close something that can no longer be closed.
// err is wrapped alongside handler.ErrFailedPrecondition (Go 1.20+'s
// multi-%w), rather than discarded: it used to substitute a hand-written
// "the work branch has already reached a terminal state" message for err
// entirely, which read fine but meant errors.Is(mapped,
// workbranchstore.ErrIllegalTransition) was false -- the same sentinel-drop
// loam-blc fixed for mapDiffComputerErr and loam-dq0o fixed for this
// function's own namesake in internal/handler/workbranch (loam-c4ab is the
// third instance of that shape). errors.Is(mapped,
// workbranchstore.ErrIllegalTransition) and errors.Is(mapped,
// handler.ErrFailedPrecondition) both now hold.
//
// ErrNotFound deliberately keeps the single-%w shape and still discards
// err: this is the choice already made (and left alone) when workbranch.go's
// mapWorkBranchStoreErr was fixed for ErrIllegalTransition above, and it is
// the right one here too. Unlike ErrIllegalTransition, err's own wrapping
// for a not-found (GetByName's "getting work branch %s/%s: %w" or the
// guarded transition's "%s work branch %s: %w" in store.go) names the
// row's internal UUID or repo ID, not the repo/branch name pair context
// already carries -- so preserving it would not add a diagnosable cause,
// only an internal id with no meaning to the caller. There is nothing more
// useful to say than "not found" once context has named what was looked
// up, so err is left out on purpose here, not by accident.
func mapWorkBranchStoreErr(err error, context string) error {
	switch {
	case errors.Is(err, workbranchstore.ErrNotFound):
		return fmt.Errorf("%s: %w", context, handler.ErrNotFound)
	case errors.Is(err, workbranchstore.ErrIllegalTransition):
		return fmt.Errorf("%s: %w: %w", context, err, handler.ErrFailedPrecondition)
	default:
		return fmt.Errorf("%s: %w", context, err)
	}
}

// outcomeToProto maps a reviewstore.Outcome to its proto enum value. This
// is a second copy of internal/handler/workbranch's identical function
// rather than a shared import, for the reason that package's own
// repoSegmentPattern duplication documents: the function is unexported
// there and exporting a proto-conversion helper purely to share three
// switch arms would widen that package's surface for no caller other than
// this one.
func outcomeToProto(o reviewstore.Outcome) loamv1.VerdictOutcome {
	switch o {
	case reviewstore.OutcomeApprove:
		return loamv1.VerdictOutcome_VERDICT_OUTCOME_APPROVE
	case reviewstore.OutcomeDisapprove:
		return loamv1.VerdictOutcome_VERDICT_OUTCOME_DISAPPROVE
	case reviewstore.OutcomeNeutral:
		return loamv1.VerdictOutcome_VERDICT_OUTCOME_NEUTRAL
	default:
		return loamv1.VerdictOutcome_VERDICT_OUTCOME_UNSPECIFIED
	}
}

// stateToProto maps a workbranchstore.State to its proto enum value, a
// second copy for the same reason as outcomeToProto.
func stateToProto(s workbranchstore.State) loamv1.WorkBranchState {
	switch s {
	case workbranchstore.StateDraft:
		return loamv1.WorkBranchState_WORK_BRANCH_STATE_DRAFT
	case workbranchstore.StateReviewable:
		return loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE
	case workbranchstore.StateReviewed:
		return loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWED
	case workbranchstore.StateComplete:
		return loamv1.WorkBranchState_WORK_BRANCH_STATE_COMPLETE
	case workbranchstore.StateClosed:
		return loamv1.WorkBranchState_WORK_BRANCH_STATE_CLOSED
	default:
		return loamv1.WorkBranchState_WORK_BRANCH_STATE_UNSPECIFIED
	}
}

// conflictToProto maps a workbranchstore.Conflict to its proto enum value,
// a second copy for the same reason as outcomeToProto. An unrecognized
// value becomes UNSPECIFIED, never NONE: NONE is a positive claim that the
// branch merges cleanly into its target, and this admin surface is exactly
// where such a claim must not be invented.
func conflictToProto(c workbranchstore.Conflict) loamv1.WorkBranchConflict {
	switch c {
	case workbranchstore.ConflictNone:
		return loamv1.WorkBranchConflict_WORK_BRANCH_CONFLICT_NONE
	case workbranchstore.ConflictFlagged:
		return loamv1.WorkBranchConflict_WORK_BRANCH_CONFLICT_FLAGGED
	case workbranchstore.ConflictReset:
		return loamv1.WorkBranchConflict_WORK_BRANCH_CONFLICT_RESET
	default:
		return loamv1.WorkBranchConflict_WORK_BRANCH_CONFLICT_UNSPECIFIED
	}
}

// driftToProto maps a workbranchstore.UpstreamDrift to its proto enum
// value, with the same treatment of an unrecognized value as
// conflictToProto and for the same reason.
func driftToProto(d workbranchstore.UpstreamDrift) loamv1.UpstreamDrift {
	switch d {
	case workbranchstore.DriftNone:
		return loamv1.UpstreamDrift_UPSTREAM_DRIFT_NONE
	case workbranchstore.DriftDiverged:
		return loamv1.UpstreamDrift_UPSTREAM_DRIFT_DIVERGED
	default:
		return loamv1.UpstreamDrift_UPSTREAM_DRIFT_UNSPECIFIED
	}
}

// workBranchToProto renders a work_branches row as the shared loam.v1
// WorkBranch the admin protos reuse (docs/web-spec.md: "Admin protos reuse
// WorkBranch, Page, and PageInfo from loam.v1").
//
// conflict and upstream_drift ride along (loam-giq.11). This is the surface
// the second one exists for: a diverged branch is deliberately NOT listed
// as a proposal (see ListProposals), so the console learns about it from
// the work branch itself, and it must be able to tell "the target moved,
// catch up" apart from "someone rewrote the branch Loam pushed" -- two
// different remedies -- rather than collapsing them into one badge.
func workBranchToProto(repoName string, wb workbranchstore.WorkBranch) *loamv1.WorkBranch {
	return &loamv1.WorkBranch{
		Repo:          repoName,
		Name:          wb.Name,
		Target:        wb.Target,
		Title:         derefOr(wb.Title),
		Description:   derefOr(wb.Description),
		State:         stateToProto(wb.State),
		Author:        wb.Author,
		UpstreamPrUrl: wb.UpstreamPRURL,
		Conflict:      conflictToProto(wb.Conflict),
		UpstreamDrift: driftToProto(wb.UpstreamDrift),
	}
}

// derefOr reads a nullable text column into a plain string, treating SQL
// NULL as empty.
func derefOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
