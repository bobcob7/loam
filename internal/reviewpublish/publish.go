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
//
// # Anchor validation (loam-hi5o.15) and the design decision it required
//
// Every staged comment's file/line anchor is validated against the work
// branch's OWN tip -- via AnchorChecker, reading the bare mirror -- before
// Publish opens a transaction at all (validateAnchors, called from Publish
// itself). This is the fix for `loam work comment --line 270` against a
// ~100-line file being accepted with no validation anywhere: the comment
// was published anchored past the end of the file, durable and unremarked
// by either party.
//
// Anchor validation runs BEFORE pool.Begin rather than inside publishInTx
// deliberately: it writes nothing, and it reads only the bare git mirror,
// never a row this transaction's own MVCC snapshot could make stale, so
// there is no atomicity reason to hold it inside the transaction. What
// there IS a reason to avoid is the transaction's own lock window: each
// unique file in a batch costs AnchorChecker up to three git subprocess
// launches (gitanchor.Checker.FileLineCount's own doc comment), and
// TestPublish_InvisibleToConcurrentReadersUntilCommit exists precisely
// because this transaction's writes ARE visible-lock-blocking to other
// callers while it is open -- there is no reason to make that window
// longer than the actual Postgres writes need.
//
// DECISION: an invalid anchor fails the WHOLE Publish call -- nothing in
// req is written, exactly like every other validateAnchors-adjacent
// failure this transaction already treats that way (a resolve of a thread
// the caller does not own, per TestPublish_RejectedResolve_PublishesNothing).
// This applies uniformly whether the anchor was wrong from the moment it
// was staged, or drifted out of range because the author pushed a shrinking
// change between staging and publish -- the server cannot and need not tell
// the two apart, since it never saw the comment before this call (staging
// is entirely local to the CLI's .loam; see this package's own "That is
// what makes ... true at the SERVER boundary" paragraph above) and
// publishing either shape of invalid anchor is the same bug.
//
// The alternative this bead's own instructions raised -- publish the
// comment anyway with its anchor marked "stale" -- was considered and
// rejected. It would need a new persisted flag (this table group has none
// today; reviewstore's own doc comments are emphatic that staleness here is
// ALWAYS derived, never stored, and an anchor-staleness column would be a
// second, unrelated meaning fighting the same word) and a CLI surface to
// show it, neither of which exists yet, so a "stale" comment would be just
// as silently misleading as the bug this fixes -- an agent reading
// `loam work comments` has no more reason to notice a stale flag than it
// had to notice the anchor was wrong in the first place.
//
// Failing the whole call is not the data-loss "discard a reviewer's entire
// batch" outcome that alternative was weighed against, either: the CLI's
// `work verdict` (internal/cli/commands_work_verdict.go's publishVerdict)
// clears its local staging area ONLY after a successful publish, so a
// rejected call leaves every staged item exactly where it was. The reviewer
// fixes or drops the one bad anchor (loam-hi5o.7, tracked separately, is
// what makes "fixing" that cheap) and reissues `work verdict` -- nothing
// staged is lost, only delayed.
package reviewpublish

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/gitanchor"
	"github.com/bobcob7/loam/internal/reviewstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// ErrNotOpenForReview is returned when the work branch's state does not
// admit a verdict (docs/cli-spec.md -> "State gates": `verdict` is allowed
// in reviewable and reviewed, rejected in draft -- which has no round yet
// -- and in the terminal complete/closed). notOpenForReview's check runs
// TWICE: once in Publish, on the row read before the transaction opens (a
// fast-fail, and what keeps this error's precedence over an anchor
// rejection the same as before validateAnchors moved out of the
// transaction), and again, authoritatively, in publishInTx, reading the
// row a second time INSIDE the transaction -- that second check is the one
// that cannot be raced by a concurrent transition between Publish's own
// earlier read and this write, and is the one an error here always traces
// back to.
var ErrNotOpenForReview = errors.New("work branch is not open for review")

// ErrAnchorLineInvalid is returned when a comment's anchor names a line
// that could never be valid regardless of the file's content: zero, or
// negative (reachable not from a well-behaved caller -- proto's FileLine.
// line is a uint32 -- but from anchorLine's uint32->int32 conversion
// overflowing for a value above math.MaxInt32, which a malformed or
// malicious caller can still send on the wire).
var ErrAnchorLineInvalid = errors.New("comment anchor line must be positive")

// ErrAnchorFileNotFound is returned when a comment's anchor names a file
// that is not a blob at the work branch's tip -- never committed there at
// all, a directory, or a submodule gitlink (gitanchor.ErrFileNotFound).
var ErrAnchorFileNotFound = errors.New("commented file not found at the work branch tip")

// ErrAnchorLineOutOfRange is returned when a comment's anchor names a line
// beyond the file's actual length at the work branch tip -- the bug
// loam-hi5o.15 exists to fix: `loam work comment --line 270` against a
// ~100-line file used to be accepted with no validation anywhere,
// publishing a comment anchored past the end of the file.
var ErrAnchorLineOutOfRange = errors.New("comment anchor line exceeds the file's length at the work branch tip")

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
	pool         *pgxpool.Pool
	workBranches *workbranchstore.Store
	anchors      AnchorChecker
	logger       *slog.Logger
}

// New builds a Publisher over pool, validating every staged comment's
// file/line anchor through anchors before publishing (loam-hi5o.15). Every
// Publish call opens and owns its own transaction on pool; Publisher holds
// no state between calls. workBranches is a plain, non-transactional store
// over the same pool (workbranchstore.New, not NewInTx) -- Publish's own
// preliminary work-branch read runs before any transaction exists at all,
// see Publish's own doc comment for why.
func New(pool *pgxpool.Pool, anchors AnchorChecker, logger *slog.Logger) *Publisher {
	return &Publisher{pool: pool, workBranches: workbranchstore.New(gen.New(pool), logger), anchors: anchors, logger: logger}
}

// Publish reads the work branch, validates every staged comment's anchor
// against it, and only THEN opens and executes the rest as one
// transaction. The steps run in a deliberate order so that a failure at
// ANY of them discards everything before it:
//
//  1. [outside any transaction] read the work branch and reject a state
//     that admits no verdict (notOpenForReview) -- a fast preliminary
//     check, superseded by step 3's authoritative one, but kept here so an
//     invalid state still outranks an invalid anchor in a batch broken
//     both ways, the same precedence this had before anchor validation
//     moved out of the transaction (see this package's own doc comment for
//     why it moved);
//  2. [outside any transaction] validate every staged comment's file/line
//     anchor against the work branch's OWN tip (see this package's own doc
//     comment for why an invalid anchor fails the whole call rather than
//     degrading quietly, and for why this runs before Begin);
//  3. [inside the transaction] re-read the work branch and re-check its
//     state -- THIS is the check that cannot be raced by a concurrent
//     transition between step 1's read and this write;
//  4. resolve the current round (the highest-numbered one -- staleness is
//     derived from round numbers, never a stored flag);
//  5. open a thread + opening comment per staged comment;
//  6. apply the requested thread resolutions, author-only;
//  7. write the verdict (replacing this reviewer's prior one for the round);
//  8. flip reviewable -> reviewed if this is the round's first verdict.
//
// Step 5 preceding steps 6 and 7 is load-bearing, not incidental: it is
// what makes "a rejected resolve publishes nothing" observable rather than
// merely claimed -- if the comments were written outside this transaction
// they would already be visible by the time step 6 failed.
func (p *Publisher) Publish(ctx context.Context, req Request) (Result, error) {
	wb, err := p.workBranches.Get(ctx, req.WorkBranchID)
	if err != nil {
		return Result{}, fmt.Errorf("reading work branch %s for verdict publish: %w", req.WorkBranchID, err)
	}
	if err := notOpenForReview(wb); err != nil {
		return Result{}, err
	}
	if err := p.validateAnchors(ctx, wb, req.Comments); err != nil {
		return Result{}, err
	}
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

// notOpenForReview reports ErrNotOpenForReview if wb's state does not admit
// a verdict. Shared between Publish's preliminary check (run once, outside
// any transaction, before validateAnchors -- see Publish's own doc comment
// for why this ordering is preserved) and publishInTx's authoritative one,
// so the two can never drift apart on what "open for review" means.
func notOpenForReview(wb workbranchstore.WorkBranch) error {
	if wb.State != workbranchstore.StateReviewable && wb.State != workbranchstore.StateReviewed {
		return fmt.Errorf("work branch is %s: %w", wb.State, ErrNotOpenForReview)
	}
	return nil
}

// publishInTx is Publish's body, every write bound to tx. It never commits
// or rolls back -- that is Publish's job -- so an error returned from here
// always leaves the caller's deferred Rollback to discard the whole batch.
// Anchor validation does NOT run here -- Publish already ran it, before tx
// existed at all (see Publish's own doc comment) -- only the work branch's
// state is re-read and re-checked, which is what makes that check
// authoritative rather than merely a repeat of Publish's own.
func (p *Publisher) publishInTx(ctx context.Context, tx pgx.Tx, req Request) (Result, error) {
	workBranches := workbranchstore.NewInTx(tx, p.logger)
	threads := reviewstore.NewThreadStoreInTx(tx, p.logger)
	verdicts := reviewstore.NewVerdictStoreInTx(tx, p.logger)
	rounds := reviewstore.NewRoundStoreInTx(tx, p.logger)
	wb, err := workBranches.Get(ctx, req.WorkBranchID)
	if err != nil {
		return Result{}, fmt.Errorf("reading work branch %s for verdict publish: %w", req.WorkBranchID, err)
	}
	if err := notOpenForReview(wb); err != nil {
		return Result{}, err
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

// validateAnchors checks EVERY comment's file/line anchor against wb's
// current tip, via p.anchors (see this package's own doc comment for why an
// invalid anchor fails the whole call). A comment with no File is a
// top-level (unanchored) thread and is skipped entirely; a File with no
// Line is a whole-file anchor, which still needs the file itself to exist
// but has no line to bound.
//
// A bad anchor does not stop the loop: every comment is checked, and every
// failure is collected into errs, joined into one error at the end
// (errors.Join, nil if errs is empty). This is deliberate -- a reviewer
// whose batch has several wrong anchors would otherwise learn about them
// one `work verdict` round trip at a time, fixing and resubmitting for
// each; errors.Join's Unwrap() []error means errors.Is against any single
// sentinel (ErrAnchorLineInvalid, ErrAnchorFileNotFound,
// ErrAnchorLineOutOfRange) still matches if ANY comment in the batch failed
// that way, and Error() concatenates every message, so mapPublishErr and
// every existing assertion on a single bad anchor's message keep working
// unchanged.
//
// Each unique file's line count is looked up at most once per call,
// cached in lineCounts, so a batch commenting on the same file several
// times (the common case for a real review) costs one mirror read per
// file, not one per comment. A file whose lookup FAILED is not cached (an
// error is never written into lineCounts), so a later comment on that same
// file retries the lookup rather than reusing the earlier failure -- the
// same behavior this had before batching every failure together.
func (p *Publisher) validateAnchors(ctx context.Context, wb workbranchstore.WorkBranch, comments []NewComment) error {
	lineCounts := make(map[string]int, len(comments))
	var errs []error
	for _, comment := range comments {
		if comment.File == nil {
			continue
		}
		file := *comment.File
		if comment.Line != nil && *comment.Line <= 0 {
			errs = append(errs, fmt.Errorf("line %d for %s: %w", *comment.Line, file, ErrAnchorLineInvalid))
			continue
		}
		lines, ok := lineCounts[file]
		if !ok {
			var err error
			lines, err = p.anchors.FileLineCount(ctx, wb, file)
			if err != nil {
				errs = append(errs, p.classifyAnchorErr(file, err))
				continue
			}
			lineCounts[file] = lines
		}
		if comment.Line != nil && int(*comment.Line) > lines {
			errs = append(errs, fmt.Errorf("line %d for %s exceeds its %d line(s) at the work branch tip: %w", *comment.Line, file, lines, ErrAnchorLineOutOfRange))
		}
	}
	return errors.Join(errs...)
}

// classifyAnchorErr turns an AnchorChecker failure for file into this
// package's own vocabulary. gitanchor.ErrFileNotFound is the caller's own
// mistake (an anchor naming a path the diff never touched, or a path that
// disappeared under a later push) and is reported as ErrAnchorFileNotFound;
// anything else -- gitanchor.ErrRefMissing, gitanchor.ErrMirrorMissing, or a
// genuine subprocess failure -- is neither this package's nor the caller's
// vocabulary to rename, so it is wrapped with file for context and returned
// as-is, letting the handler's own mapping (which already imports
// internal/gitanchor for exactly this) classify it.
func (p *Publisher) classifyAnchorErr(file string, err error) error {
	if errors.Is(err, gitanchor.ErrFileNotFound) {
		return fmt.Errorf("commenting on %s: %w: %w", file, err, ErrAnchorFileNotFound)
	}
	return fmt.Errorf("validating anchor for %s: %w", file, err)
}
