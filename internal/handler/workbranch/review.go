package workbranch

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/reviewpublish"
	"github.com/bobcob7/loam/internal/reviewstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// ListComments returns a work branch's PUBLISHED comment threads, each with
// its anchor, resolved flag, the round it was raised in, and its comments
// (docs/cli-spec.md -> "comments"). Gated by CapabilityWorkRead; an admin
// superuser reaches it too (docs/web-spec.md -> ProposalService lists
// ListComments among the operations the admin performs as superuser).
//
// Staged comments are never returned here and cannot be: they are not on
// this server at all until a verdict publishes them (docs/persistence-spec.md
// -> "comments": "Staged comments are not here -- they live locally in the
// CLI's .loam"). `loam work comments --staged` reads the local staging area,
// never this RPC.
func (h *Handler) ListComments(ctx context.Context, req *connect.Request[loamv1.ListCommentsRequest]) (*connect.Response[loamv1.ListCommentsResponse], error) {
	if err := h.capabilities.RequireCapability(ctx, handler.CapabilityWorkRead); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	repo, name := req.Msg.GetRepo(), req.Msg.GetWorkBranch()
	_, wb, err := h.resolveWorkBranch(ctx, repo, name)
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	limit, offset := pageLimitOffset(req.Msg.GetPage())
	threads, total, err := h.threads.List(ctx, wb.ID, limit, offset)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("listing comment threads for work branch %s/%s: %w", repo, name, err))
	}
	result := make([]*loamv1.Thread, len(threads))
	for i, thread := range threads {
		result[i] = threadToProto(thread)
	}
	return connect.NewResponse(&loamv1.ListCommentsResponse{
		Threads:  result,
		PageInfo: &loamv1.PageInfo{Total: uint32(total)},
	}), nil
}

// ListVerdicts returns the work branch's verdicts -- the current round's
// plus stale ones from prior rounds, each flagged (docs/cli-spec.md ->
// "verdicts"). Gated by CapabilityWorkRead.
//
// Staleness is DERIVED, never stored: reviewstore's query computes each
// verdict's Current by comparing its round's number against the branch's
// MAX(number), and `stale` here is exactly that flag's negation. There is no
// stale column and this handler must not synthesize a second mechanism for
// one.
//
// One row per reviewer, not per (reviewer, round): docs/cli-spec.md ->
// "verdicts" says "Returns each reviewer's recorded verdict (unique agent +
// outcome)" and reviewing.feature asks that "each reviewer appears once with
// their latest outcome". A reviewer who voted in several rounds therefore
// collapses to their LATEST vote -- which is also the one whose stale flag
// answers the question a caller is actually asking ("does this agent
// currently approve?"). dedupeLatestPerReviewer relies on VerdictStore.List
// returning newest round first, which its query guarantees
// (ORDER BY r.number DESC).
func (h *Handler) ListVerdicts(ctx context.Context, req *connect.Request[loamv1.ListVerdictsRequest]) (*connect.Response[loamv1.ListVerdictsResponse], error) {
	if err := h.capabilities.RequireCapability(ctx, handler.CapabilityWorkRead); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	repo, name := req.Msg.GetRepo(), req.Msg.GetWorkBranch()
	_, wb, err := h.resolveWorkBranch(ctx, repo, name)
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	records, err := h.verdicts.List(ctx, wb.ID)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("listing verdicts for work branch %s/%s: %w", repo, name, err))
	}
	latest := dedupeLatestPerReviewer(records)
	result := make([]*loamv1.VerdictSummary, len(latest))
	for i, record := range latest {
		result[i] = &loamv1.VerdictSummary{
			Reviewer: record.Reviewer,
			Outcome:  outcomeToProto(record.Outcome),
			Stale:    !record.Current,
			Round:    uint32(record.RoundNumber),
		}
	}
	return connect.NewResponse(&loamv1.ListVerdictsResponse{Verdicts: result}), nil
}

// SubmitVerdict publishes the caller's staged comments atomically as a
// verdict with an outcome (docs/cli-spec.md -> "verdict"). Gated by
// CapabilityWorkVerdict.
//
// This handler validates the request and then delegates the ENTIRE write to
// VerdictPublisher, which performs it in one transaction: the new threads
// and their opening comments, the requested resolutions, the verdict row,
// and the reviewable -> reviewed flip all commit together or not at all.
// Nothing here writes anything itself, deliberately -- an atomicity claim
// that lived in this function's call ordering would be false the moment any
// step failed.
//
// The work-branch state gate (verdict is allowed in reviewable/reviewed,
// rejected in draft -- which has no round yet -- and the terminal states)
// also lives inside that transaction, reading the row there, so this
// handler does not pre-check it: a check here would be both duplicated and
// raceable.
func (h *Handler) SubmitVerdict(ctx context.Context, req *connect.Request[loamv1.SubmitVerdictRequest]) (*connect.Response[loamv1.SubmitVerdictResponse], error) {
	if err := h.capabilities.RequireCapability(ctx, handler.CapabilityWorkVerdict); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	repo, name := req.Msg.GetRepo(), req.Msg.GetWorkBranch()
	_, wb, err := h.resolveWorkBranch(ctx, repo, name)
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	reviewer, err := reviewerIdentifier(ctx)
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	outcome, err := protoToOutcome(req.Msg.GetOutcome())
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	comments, err := verdictComments(req.Msg.GetComments())
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	resolveIDs, err := parseThreadIDs(req.Msg.GetResolveThreadIds())
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	result, err := h.publisher.Publish(ctx, reviewpublish.Request{
		WorkBranchID:     wb.ID,
		Reviewer:         reviewer,
		Outcome:          outcome,
		Comments:         comments,
		ResolveThreadIDs: resolveIDs,
	})
	if err != nil {
		return nil, h.errors.ToConnectErr(mapPublishErr(err, fmt.Sprintf("submitting a verdict on work branch %s/%s", repo, name)))
	}
	return connect.NewResponse(&loamv1.SubmitVerdictResponse{
		Outcome:   outcomeToProto(result.Verdict.Outcome),
		Published: uint32(result.Published),
	}), nil
}

// ReplyToThread posts a reply to an existing thread IMMEDIATELY -- it is
// never staged (docs/cli-spec.md -> "reply"). Gated by CapabilityWorkReply.
//
// The reply is stamped with the branch's CURRENT round, which may be later
// than the round the thread was raised in; the thread's own round is not
// touched (replies.feature -> "A reply records the round it was made in":
// "the thread still shows it was raised in the first round").
//
// A reply changes no work-branch state and casts no verdict: it is a single
// comments row. The only precondition is the state gate -- reply is allowed
// in draft/reviewable/reviewed and rejected in the terminal complete/closed
// (docs/cli-spec.md -> "State gates"). Unlike SubmitVerdict there is no
// transaction to push that check into, because there is only one write, so
// it is enforced here.
func (h *Handler) ReplyToThread(ctx context.Context, req *connect.Request[loamv1.ReplyToThreadRequest]) (*connect.Response[loamv1.ReplyToThreadResponse], error) {
	if err := h.capabilities.RequireCapability(ctx, handler.CapabilityWorkReply); err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	repo, name := req.Msg.GetRepo(), req.Msg.GetWorkBranch()
	_, wb, err := h.resolveWorkBranch(ctx, repo, name)
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	if wb.State == workbranchstore.StateComplete || wb.State == workbranchstore.StateClosed {
		return nil, h.errors.ToConnectErr(fmt.Errorf("work branch %s/%s is %s, a terminal state -- it can no longer be replied to: %w", repo, name, wb.State, handler.ErrFailedPrecondition))
	}
	author, err := replyAuthorIdentifier(ctx)
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	body := req.Msg.GetBody()
	if body == "" {
		return nil, h.errors.ToConnectErr(fmt.Errorf("a reply body is required: %w", handler.ErrInvalidArgument))
	}
	threadID, err := parseThreadID(req.Msg.GetThreadId())
	if err != nil {
		return nil, h.errors.ToConnectErr(err)
	}
	opContext := fmt.Sprintf("replying to thread %s on work branch %s/%s", req.Msg.GetThreadId(), repo, name)
	if _, err := h.threads.Get(ctx, wb.ID, threadID); err != nil {
		return nil, h.errors.ToConnectErr(mapThreadStoreErr(err, opContext))
	}
	round, err := h.rounds.CurrentRound(ctx, wb.ID)
	if err != nil {
		return nil, h.errors.ToConnectErr(mapRoundStoreErr(err, opContext))
	}
	comment, err := h.threads.Reply(ctx, threadID, round.ID, round.Number, author, body)
	if err != nil {
		return nil, h.errors.ToConnectErr(mapThreadStoreErr(err, opContext))
	}
	return connect.NewResponse(&loamv1.ReplyToThreadResponse{Comment: commentToProto(comment)}), nil
}

// dedupeLatestPerReviewer keeps one record per reviewer -- the first one
// seen, which is their highest-numbered round because VerdictStore.List
// orders by round number descending. Input order is otherwise preserved, so
// current-round verdicts still lead the response and stale prior-round ones
// follow.
func dedupeLatestPerReviewer(records []reviewstore.VerdictRecord) []reviewstore.VerdictRecord {
	seen := make(map[string]struct{}, len(records))
	latest := make([]reviewstore.VerdictRecord, 0, len(records))
	for _, record := range records {
		if _, ok := seen[record.Reviewer]; ok {
			continue
		}
		seen[record.Reviewer] = struct{}{}
		latest = append(latest, record)
	}
	return latest
}

// verdictComments converts the request's staged comments to the publisher's
// own type, rejecting an empty body (a comment with no text is a client bug,
// not an outcome-only verdict -- that is expressed by sending no comments at
// all) and an anchor with no file path.
func verdictComments(comments []*loamv1.VerdictComment) ([]reviewpublish.NewComment, error) {
	result := make([]reviewpublish.NewComment, 0, len(comments))
	for i, comment := range comments {
		if comment.GetBody() == "" {
			return nil, fmt.Errorf("comment %d has an empty body: %w", i, handler.ErrInvalidArgument)
		}
		anchor := comment.GetAnchor()
		if anchor != nil && anchor.GetFile() == "" {
			return nil, fmt.Errorf("comment %d has an anchor with no file: %w", i, handler.ErrInvalidArgument)
		}
		result = append(result, reviewpublish.NewComment{File: anchorFile(anchor), Line: anchorLine(anchor), Body: comment.GetBody()})
	}
	return result, nil
}

// anchorFile returns the anchor's file path, or nil for an unanchored
// (top-level) thread -- threads.file is nullable and a top-level thread
// stores SQL NULL there, never the empty string.
func anchorFile(anchor *loamv1.FileLine) *string {
	if anchor == nil {
		return nil
	}
	file := anchor.GetFile()
	return &file
}

// anchorLine returns the anchor's line, or nil when the anchor names a whole
// file (proto's FileLine.line is optional: "Absent = the whole file / no
// specific line") or there is no anchor at all.
func anchorLine(anchor *loamv1.FileLine) *int32 {
	if anchor == nil || anchor.Line == nil {
		return nil
	}
	line := int32(anchor.GetLine())
	return &line
}

// parseThreadIDs parses SubmitVerdict's resolve_thread_ids, rejecting the
// whole request on the first malformed id rather than silently resolving the
// well-formed subset -- a partially applied verdict is exactly what the
// publish transaction exists to prevent.
func parseThreadIDs(ids []string) ([]uuid.UUID, error) {
	parsed := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		threadID, err := parseThreadID(id)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, threadID)
	}
	return parsed, nil
}

// parseThreadID parses a thread id from the wire. A malformed id is an
// invalid argument, not a not-found: the caller sent something that could
// never name a thread.
func parseThreadID(id string) (uuid.UUID, error) {
	if id == "" {
		return uuid.UUID{}, fmt.Errorf("a thread id is required: %w", handler.ErrInvalidArgument)
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("thread id %q is not a valid identifier: %w", id, handler.ErrInvalidArgument)
	}
	return parsed, nil
}

// reviewerIdentifier resolves the caller's agent identity for the verdict's
// reviewer column. Casting a verdict is an agent-only operation: verdicts are
// keyed per unique agent (verdicts_round_id_reviewer_key) and docs/web-spec.md
// -> ProposalService lists GetWorkBranch, GetWorkBranchDiff, ListComments and
// RequestReview -- NOT SubmitVerdict -- as what the admin reaches as
// superuser. A caller with no resolvable agent identity is rejected rather
// than recorded under a placeholder reviewer that would then occupy a real
// agent's one-verdict-per-round slot.
func reviewerIdentifier(ctx context.Context) (string, error) {
	identity, ok := httpauth.IdentityFromContext(ctx)
	if !ok {
		return "", fmt.Errorf("submitting a verdict requires an agent identity: %w", handler.ErrInvalidArgument)
	}
	return identity.Identifier(), nil
}

// replyAuthorIdentifier resolves the caller's agent identity for a reply's
// author column, rejected for the same reason as reviewerIdentifier: a
// comment attributed to nobody is worse than a refused reply.
func replyAuthorIdentifier(ctx context.Context) (string, error) {
	identity, ok := httpauth.IdentityFromContext(ctx)
	if !ok {
		return "", fmt.Errorf("replying to a thread requires an agent identity: %w", handler.ErrInvalidArgument)
	}
	return identity.Identifier(), nil
}

// mapPublishErr maps a VerdictPublisher failure to the handler.Err* sentinel
// ErrorMapper recognizes. Every case here is a caller-fixable precondition
// or argument problem; anything else falls through to CodeInternal-and-log,
// the same "loud failure over silent wrong behavior" choice the rest of this
// package makes. The ErrNotOpenForReview/ErrNoCurrentRound and
// ErrNotThreadAuthor cases wrap err
// alongside handler.ErrFailedPrecondition (Go 1.20+'s multi-%w), rather
// than discarding it: ErrNotOpenForReview's own wrapping ("work branch is
// %s: %w") names the actual state, and dropping it left the message
// terminate in the generic "handler: failed precondition" -- the same
// defect loam-blc, loam-dq0o, and loam-c4ab fixed elsewhere (loam-jv8f).
func mapPublishErr(err error, context string) error {
	switch {
	case errors.Is(err, reviewpublish.ErrNotOpenForReview), errors.Is(err, reviewstore.ErrNoCurrentRound):
		return fmt.Errorf("%s: %w: %w", context, err, handler.ErrFailedPrecondition)
	case errors.Is(err, reviewstore.ErrNotThreadAuthor):
		return fmt.Errorf("%s: %w: %w", context, err, handler.ErrPermissionDenied)
	case errors.Is(err, reviewstore.ErrThreadNotFound):
		return fmt.Errorf("%s: %w", context, handler.ErrNotFound)
	case errors.Is(err, workbranchstore.ErrNotFound):
		return fmt.Errorf("%s: %w", context, handler.ErrNotFound)
	default:
		return fmt.Errorf("%s: %w", context, err)
	}
}

// mapThreadStoreErr maps a ThreadStore error to the handler.Err* sentinel
// ErrorMapper recognizes. A thread belonging to another work branch already
// arrives as ErrThreadNotFound from the store, so it is reported as not
// found here too -- a thread id must not be probeable across work branches.
// ErrNotThreadAuthor wraps err alongside handler.ErrPermissionDenied
// (Go 1.20+'s multi-%w), rather than discarding it: err's own wrapping
// ("thread %s was opened by %s, not %s: %w") names the actual author and
// actor, exactly the diagnostic a caller wants -- the same defect loam-blc,
// loam-dq0o, and loam-c4ab fixed elsewhere (loam-jv8f).
func mapThreadStoreErr(err error, context string) error {
	switch {
	case errors.Is(err, reviewstore.ErrThreadNotFound):
		return fmt.Errorf("%s: %w", context, handler.ErrNotFound)
	case errors.Is(err, reviewstore.ErrNotThreadAuthor):
		return fmt.Errorf("%s: %w: %w", context, err, handler.ErrPermissionDenied)
	default:
		return fmt.Errorf("%s: %w", context, err)
	}
}

// mapRoundStoreErr maps a RoundStore lookup failure. A work branch with no
// round at all cannot carry a thread to reply to, so ErrNoCurrentRound here
// means the branch was never opened for review -- a failed precondition, not
// an internal fault.
func mapRoundStoreErr(err error, context string) error {
	if errors.Is(err, reviewstore.ErrNoCurrentRound) {
		return fmt.Errorf("%s: the work branch has never been opened for review: %w", context, handler.ErrFailedPrecondition)
	}
	return fmt.Errorf("%s: %w", context, err)
}

// outcomeToProto maps a reviewstore.Outcome to its proto enum value.
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

// protoToOutcome maps a proto VerdictOutcome to its reviewstore.Outcome.
// VERDICT_OUTCOME_UNSPECIFIED and any unrecognized value are rejected as an
// invalid argument (docs/cli-spec.md -> "verdict" -> Errors: "exit 2 on a
// missing or invalid outcome") rather than defaulting to any of the three
// real outcomes -- guessing "neutral" for a client that forgot the field
// would record a verdict the reviewer never cast.
func protoToOutcome(o loamv1.VerdictOutcome) (reviewstore.Outcome, error) {
	switch o {
	case loamv1.VerdictOutcome_VERDICT_OUTCOME_APPROVE:
		return reviewstore.OutcomeApprove, nil
	case loamv1.VerdictOutcome_VERDICT_OUTCOME_DISAPPROVE:
		return reviewstore.OutcomeDisapprove, nil
	case loamv1.VerdictOutcome_VERDICT_OUTCOME_NEUTRAL:
		return reviewstore.OutcomeNeutral, nil
	default:
		return "", fmt.Errorf("outcome %s is not a valid verdict outcome: %w", o, handler.ErrInvalidArgument)
	}
}

// threadToProto converts a reviewstore.ThreadWithComments to its proto
// representation, including the round it was raised in and each comment's
// own (possibly later) round.
func threadToProto(thread reviewstore.ThreadWithComments) *loamv1.Thread {
	comments := make([]*loamv1.Comment, len(thread.Comments))
	for i, comment := range thread.Comments {
		comments[i] = commentToProto(comment)
	}
	return &loamv1.Thread{
		Id:       thread.ID.String(),
		Resolved: thread.Resolved,
		Anchor:   anchorToProto(thread.File, thread.Line),
		Comments: comments,
		Round:    uint32(thread.RoundNumber),
	}
}

// commentToProto converts a reviewstore.Comment to its proto representation.
func commentToProto(comment reviewstore.Comment) *loamv1.Comment {
	return &loamv1.Comment{Author: comment.Author, Body: comment.Body, Round: uint32(comment.RoundNumber)}
}

// anchorToProto renders a thread's optional file/line anchor: nil for a
// top-level thread (threads.file is SQL NULL), and a FileLine with no line
// for a whole-file anchor.
func anchorToProto(file *string, line *int32) *loamv1.FileLine {
	if file == nil {
		return nil
	}
	anchor := &loamv1.FileLine{File: *file}
	if line != nil {
		lineNumber := uint32(*line)
		anchor.Line = &lineNumber
	}
	return anchor
}
