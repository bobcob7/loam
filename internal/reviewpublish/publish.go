// Package reviewpublish holds the one operation in the review lifecycle
// that spans three store packages and must be all-or-nothing: publishing a
// reviewer's batch of staged comments as a verdict (docs/cli-spec.md ->
// "verdict": "Publishes all of the caller's locally staged comments for
// this work branch in ONE ATOMIC ACTION as a verdict").
//
// Atomicity here is a transaction, not a convention. Publish opens a single
// pgx.Tx and binds internal/reviewstore's ThreadStore and VerdictStore and
// internal/workbranchstore's Store to it via their NewInTx constructors, so
// every row the publish writes -- new threads, their opening comments,
// thread resolutions, the verdict itself, and the reviewable -> reviewed
// flip -- commits together or not at all. A concurrent reader on any other
// connection sees NONE of it until that commit lands (the MVCC snapshot
// property internal/storetx's integration test demonstrates directly), and
// any failure part-way through -- a resolve of a thread the caller did not
// open, a lost connection, a cancelled request -- leaves the work branch
// exactly as it was, with no half-published comments visible to anyone.
//
// That is what makes "staged comments are not visible until submitted"
// (reviewing.feature) true at the SERVER boundary. The other half is the
// CLI's: staged comments live in .loam and never reach this server at all
// until `loam work verdict` sends them (docs/persistence-spec.md ->
// "comments": "Staged comments are not here").
//
// This package deliberately owns no rows of its own. It exists because the
// property it enforces spans reviewstore and workbranchstore and belongs to
// neither: putting the reviewable -> reviewed flip inside reviewstore would
// make a store package reach across aggregates, and leaving it in the
// handler would put it outside the transaction, which is precisely the bug.
package reviewpublish

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bobcob7/loam/internal/reviewstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// ErrNotOpenForReview is returned when the work branch's state does not
// admit a verdict (docs/cli-spec.md -> "State gates": `verdict` is allowed
// in reviewable and reviewed, rejected in draft -- which has no round yet
// -- and in the terminal complete/closed). The check runs INSIDE the
// publish transaction, reading the row there, so it cannot be raced by a
// concurrent transition between a handler's earlier read and this write.
var ErrNotOpenForReview = errors.New("work branch is not open for review")

// NewComment is one staged comment being published: an optional file/line
// anchor and the body. Each one opens a NEW thread -- reviewers raise
// threads through their verdict and reply through ReplyToThread, never the
// other way round (docs/cli-spec.md -> "comment", "reply").
type NewComment struct {
	File *string
	Line *int32
	Body string
}

// Request is one atomic publish: reviewer's outcome on WorkBranchID,
// together with the comments to publish and the threads to resolve.
type Request struct {
	WorkBranchID     uuid.UUID
	Reviewer         string
	Outcome          reviewstore.Outcome
	Comments         []NewComment
	ResolveThreadIDs []uuid.UUID
}

// Result reports what the committed transaction did: the verdict row, the
// round it landed in, how many comments were published, and the work
// branch's state AFTER the publish (reviewed, once the round's first
// verdict has flipped it).
type Result struct {
	Verdict   reviewstore.Verdict
	Round     reviewstore.Round
	Published int
	State     workbranchstore.State
}

// Publisher performs the atomic verdict publish over a connection pool.
type Publisher struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// New builds a Publisher over pool. Every Publish call opens and owns its
// own transaction on that pool; Publisher holds no state between calls.
func New(pool *pgxpool.Pool, logger *slog.Logger) *Publisher {
	return &Publisher{pool: pool, logger: logger}
}

// Publish executes req as one transaction. The steps run in a deliberate
// order so that a failure at ANY of them discards everything before it:
//
//  1. read the work branch and reject a state that admits no verdict;
//  2. resolve the current round (the highest-numbered one -- staleness is
//     derived from round numbers, never a stored flag);
//  3. open a thread + opening comment per staged comment;
//  4. apply the requested thread resolutions, author-only;
//  5. write the verdict (replacing this reviewer's prior one for the round);
//  6. flip reviewable -> reviewed if this is the round's first verdict.
//
// Step 3 preceding steps 4 and 5 is load-bearing, not incidental: it is
// what makes "a rejected resolve publishes nothing" observable rather than
// merely claimed -- if the comments were written outside this transaction
// they would already be visible by the time step 4 failed.
func (p *Publisher) Publish(ctx context.Context, req Request) (Result, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("beginning verdict publish transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := p.publishInTx(ctx, tx, req)
	if err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("committing verdict publish for work branch %s: %w", req.WorkBranchID, err)
	}
	p.logger.InfoContext(ctx, "published verdict", "work_branch_id", req.WorkBranchID, "reviewer", req.Reviewer, "outcome", req.Outcome, "published", result.Published, "round", result.Round.Number, "state", result.State)
	return result, nil
}

// publishInTx is Publish's body, every write bound to tx. It never commits
// or rolls back -- that is Publish's job -- so an error returned from here
// always leaves the caller's deferred Rollback to discard the whole batch.
func (p *Publisher) publishInTx(ctx context.Context, tx pgx.Tx, req Request) (Result, error) {
	workBranches := workbranchstore.NewInTx(tx, p.logger)
	threads := reviewstore.NewThreadStoreInTx(tx, p.logger)
	verdicts := reviewstore.NewVerdictStoreInTx(tx, p.logger)
	rounds := reviewstore.NewRoundStoreInTx(tx, p.logger)
	wb, err := workBranches.Get(ctx, req.WorkBranchID)
	if err != nil {
		return Result{}, fmt.Errorf("reading work branch %s for verdict publish: %w", req.WorkBranchID, err)
	}
	if wb.State != workbranchstore.StateReviewable && wb.State != workbranchstore.StateReviewed {
		return Result{}, fmt.Errorf("work branch is %s: %w", wb.State, ErrNotOpenForReview)
	}
	round, err := rounds.CurrentRound(ctx, req.WorkBranchID)
	if err != nil {
		return Result{}, err
	}
	for _, comment := range req.Comments {
		if _, err := threads.OpenThread(ctx, req.WorkBranchID, round.ID, round.Number, req.Reviewer, comment.File, comment.Line, comment.Body); err != nil {
			return Result{}, err
		}
	}
	for _, threadID := range req.ResolveThreadIDs {
		if _, err := threads.Resolve(ctx, req.WorkBranchID, threadID, req.Reviewer); err != nil {
			return Result{}, err
		}
	}
	verdict, err := verdicts.Submit(ctx, round.ID, req.Reviewer, req.Outcome)
	if err != nil {
		return Result{}, err
	}
	state := wb.State
	if wb.State == workbranchstore.StateReviewable {
		updated, err := workBranches.UpdateState(ctx, req.WorkBranchID, workbranchstore.StateReviewed)
		if err != nil {
			return Result{}, fmt.Errorf("marking work branch %s reviewed on its first verdict: %w", req.WorkBranchID, err)
		}
		state = updated.State
	}
	return Result{Verdict: verdict, Round: round, Published: len(req.Comments), State: state}, nil
}
