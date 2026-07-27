package reviewstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bobcob7/loam/internal/db/gen"
)

// ErrThreadNotFound is returned when a thread id does not name an existing
// threads row -- or names one belonging to a DIFFERENT work branch than the
// request is scoped to, which is indistinguishable from "no such thread"
// from the caller's point of view and deliberately reported the same way,
// so a thread id cannot be probed for existence across work branches.
var ErrThreadNotFound = errors.New("review thread not found")

// ErrNotThreadAuthor is returned by ThreadStore.Resolve when the thread
// exists on the named work branch but was opened by someone else: only the
// thread's original author may resolve it (docs/cli-spec.md -> "comment").
// Distinguishable from ErrThreadNotFound so a caller can answer "you did
// not open that thread" instead of the misleading "no such thread".
var ErrNotThreadAuthor = errors.New("only the thread's author may resolve it")

// Thread is one threads row, decorated with its round's number. RoundID/
// RoundNumber is the round the thread was RAISED in and never changes as
// the work branch moves through later rounds -- see Comment.RoundNumber for
// the independent value a reply carries.
type Thread struct {
	ID           uuid.UUID
	WorkBranchID uuid.UUID
	RoundID      uuid.UUID
	RoundNumber  int32
	Author       string
	File         *string
	Line         *int32
	Resolved     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Comment is one comments row, decorated with its round's number.
// RoundID/RoundNumber is the branch's round at the moment THIS comment was
// posted, which for a reply can be LATER than its thread's own round
// (docs/persistence-spec.md -> "comments"). It is never inherited from the
// thread.
type Comment struct {
	ID          uuid.UUID
	ThreadID    uuid.UUID
	RoundID     uuid.UUID
	RoundNumber int32
	Author      string
	Body        string
	CreatedAt   time.Time
}

// ThreadWithComments is a published thread and its comments, oldest first.
// The opening comment is always present: OpenThread writes the thread and
// that comment in one statement, so a comment-less thread is not a state
// this store can produce.
type ThreadWithComments struct {
	Thread
	Comments []Comment
}

// ThreadStore implements the review-discussion tables: opening a thread
// with its opening comment, appending replies, listing a work branch's
// published threads with their comments, and author-only resolution.
//
// Staged comments are NOT stored here at all -- they live locally in the
// CLI's .loam until a verdict publishes them (docs/persistence-spec.md ->
// "comments"). Every row this store can read or write is already published,
// so "invisible until the verdict lands" is not a flag this store checks:
// it is the property that the whole publish happens inside ONE transaction
// (see internal/reviewpublish), so a concurrent reader sees none of it
// until the verdict commits.
type ThreadStore struct {
	q      *gen.Queries
	logger *slog.Logger
}

// NewThreadStore builds a ThreadStore backed by db, typically a
// *pgxpool.Pool (standalone reads) or a pgx.Tx (atomic with other stores'
// writes in the same transaction) -- both satisfy querier (gen.DBTX)
// directly. See NewThreadStoreInTx for the latter as a named, pgx.Tx-typed
// entry point matching this package's siblings.
func NewThreadStore(db querier, logger *slog.Logger) *ThreadStore {
	return &ThreadStore{q: gen.New(db), logger: logger}
}

// NewThreadStoreInTx builds a ThreadStore bound to tx, an already-open
// transaction the caller owns and will commit or roll back itself: it is
// exactly NewThreadStore(tx, logger), given a name so callers composing
// several stores' writes into one commit -- internal/reviewpublish's atomic
// verdict publish -- have one consistent constructor to reach for across
// every store package. ThreadStore never calls tx.Begin/Commit/Rollback
// itself, so there is no nested-transaction path to guard against here.
func NewThreadStoreInTx(tx pgx.Tx, logger *slog.Logger) *ThreadStore {
	return NewThreadStore(tx, logger)
}

// OpenThread opens a new thread on workBranchID, raised in roundID by
// author, optionally anchored to file/line, together with its opening
// comment -- one statement, so a thread with no comments is never
// observable (internal/db/queries/threads.sql "OpenThreadWithComment").
// roundNumber decorates the returned value; it is the caller's already-
// resolved current round, not a second query.
func (s *ThreadStore) OpenThread(ctx context.Context, workBranchID, roundID uuid.UUID, roundNumber int32, author string, file *string, line *int32, body string) (ThreadWithComments, error) {
	threadID, err := uuid.NewV7()
	if err != nil {
		return ThreadWithComments{}, fmt.Errorf("generating thread id: %w", err)
	}
	commentID, err := uuid.NewV7()
	if err != nil {
		return ThreadWithComments{}, fmt.Errorf("generating comment id: %w", err)
	}
	row, err := s.q.OpenThreadWithComment(ctx, gen.OpenThreadWithCommentParams{
		ID:           pgUUID(threadID),
		WorkBranchID: pgUUID(workBranchID),
		RoundID:      pgUUID(roundID),
		Author:       author,
		File:         pgTextPtr(file),
		Line:         pgInt4Ptr(line),
		ID_2:         pgUUID(commentID),
		Body:         body,
	})
	if err != nil {
		return ThreadWithComments{}, fmt.Errorf("opening thread on work branch %s in round %s: %w", workBranchID, roundID, err)
	}
	s.logger.InfoContext(ctx, "opened review thread", "work_branch_id", workBranchID, "round_id", roundID, "thread_id", threadID, "author", author)
	thread := Thread{
		ID:           uuidFromPg(row.ID),
		WorkBranchID: uuidFromPg(row.WorkBranchID),
		RoundID:      uuidFromPg(row.RoundID),
		RoundNumber:  roundNumber,
		Author:       row.Author,
		File:         textFromPg(row.File),
		Line:         int4FromPg(row.Line),
		Resolved:     row.Resolved,
		CreatedAt:    row.CreatedAt.Time,
		UpdatedAt:    row.UpdatedAt.Time,
	}
	return ThreadWithComments{Thread: thread, Comments: []Comment{{
		ID:          uuidFromPg(row.CommentID),
		ThreadID:    thread.ID,
		RoundID:     thread.RoundID,
		RoundNumber: roundNumber,
		Author:      row.Author,
		Body:        row.CommentBody,
		CreatedAt:   row.CommentCreatedAt.Time,
	}}}, nil
}

// Reply appends a comment to an existing thread. roundID is the branch's
// CURRENT round at the moment of the reply, supplied by the caller and
// deliberately independent of the thread's own round -- it may be a later
// one, and this method never reads or changes the thread's round.
func (s *ThreadStore) Reply(ctx context.Context, threadID, roundID uuid.UUID, roundNumber int32, author, body string) (Comment, error) {
	commentID, err := uuid.NewV7()
	if err != nil {
		return Comment{}, fmt.Errorf("generating comment id: %w", err)
	}
	row, err := s.q.AddComment(ctx, gen.AddCommentParams{
		ID:       pgUUID(commentID),
		ThreadID: pgUUID(threadID),
		RoundID:  pgUUID(roundID),
		Author:   author,
		Body:     body,
	})
	if err != nil {
		return Comment{}, fmt.Errorf("replying to thread %s in round %s: %w", threadID, roundID, err)
	}
	s.logger.InfoContext(ctx, "posted reply", "thread_id", threadID, "round_id", roundID, "comment_id", commentID, "author", author)
	return Comment{
		ID:          uuidFromPg(row.ID),
		ThreadID:    uuidFromPg(row.ThreadID),
		RoundID:     uuidFromPg(row.RoundID),
		RoundNumber: roundNumber,
		Author:      row.Author,
		Body:        row.Body,
		CreatedAt:   row.CreatedAt.Time,
	}, nil
}

// Get returns the thread identified by id, scoped to workBranchID: a
// thread belonging to another work branch reports ErrThreadNotFound, the
// same as one that does not exist, so a thread id cannot be probed across
// work branches. RoundNumber is not populated here -- callers that need it
// use List, which joins review_rounds.
func (s *ThreadStore) Get(ctx context.Context, workBranchID, id uuid.UUID) (Thread, error) {
	row, err := s.q.GetThread(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Thread{}, fmt.Errorf("getting thread %s: %w", id, ErrThreadNotFound)
		}
		return Thread{}, fmt.Errorf("getting thread %s: %w", id, err)
	}
	if uuidFromPg(row.WorkBranchID) != workBranchID {
		return Thread{}, fmt.Errorf("thread %s does not belong to work branch %s: %w", id, workBranchID, ErrThreadNotFound)
	}
	return Thread{
		ID:           uuidFromPg(row.ID),
		WorkBranchID: uuidFromPg(row.WorkBranchID),
		RoundID:      uuidFromPg(row.RoundID),
		Author:       row.Author,
		File:         textFromPg(row.File),
		Line:         int4FromPg(row.Line),
		Resolved:     row.Resolved,
		CreatedAt:    row.CreatedAt.Time,
		UpdatedAt:    row.UpdatedAt.Time,
	}, nil
}

// Resolve marks thread id on workBranchID resolved on behalf of author.
// The ownership check is part of the guarded UPDATE itself, not a
// read-then-write (internal/db/queries/threads.sql "ResolveThread"), so a
// concurrent racer cannot slip past it. Zero rows matched means one of
// three things, so the row is re-read to answer precisely:
// ErrNotThreadAuthor when it exists on this branch under another author,
// ErrThreadNotFound when it does not exist here at all.
func (s *ThreadStore) Resolve(ctx context.Context, workBranchID, id uuid.UUID, author string) (Thread, error) {
	row, err := s.q.ResolveThread(ctx, gen.ResolveThreadParams{
		ID:           pgUUID(id),
		WorkBranchID: pgUUID(workBranchID),
		Author:       author,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Thread{}, s.classifyResolveMiss(ctx, workBranchID, id, author)
		}
		return Thread{}, fmt.Errorf("resolving thread %s: %w", id, err)
	}
	s.logger.InfoContext(ctx, "resolved review thread", "thread_id", id, "work_branch_id", workBranchID, "author", author)
	return Thread{
		ID:           uuidFromPg(row.ID),
		WorkBranchID: uuidFromPg(row.WorkBranchID),
		RoundID:      uuidFromPg(row.RoundID),
		Author:       row.Author,
		File:         textFromPg(row.File),
		Line:         int4FromPg(row.Line),
		Resolved:     row.Resolved,
		CreatedAt:    row.CreatedAt.Time,
		UpdatedAt:    row.UpdatedAt.Time,
	}, nil
}

// classifyResolveMiss turns Resolve's zero-row result into the specific
// error the caller can act on. It always returns a non-nil error: the row
// existing under this author is exactly the case the guarded UPDATE would
// have matched, so reaching here with that shape means the row changed
// underneath us, which is reported as ErrNotThreadAuthor rather than
// silently retried.
func (s *ThreadStore) classifyResolveMiss(ctx context.Context, workBranchID, id uuid.UUID, author string) error {
	existing, err := s.Get(ctx, workBranchID, id)
	if err != nil {
		return fmt.Errorf("resolving thread %s: %w", id, err)
	}
	return fmt.Errorf("thread %s was opened by %s, not %s: %w", id, existing.Author, author, ErrNotThreadAuthor)
}

// List returns workBranchID's published threads, oldest first, each with
// its comments, offset-paginated by thread (limit/offset), plus the total
// thread count for PageInfo.total. Comments for the whole page are fetched
// in one query, not one per thread.
func (s *ThreadStore) List(ctx context.Context, workBranchID uuid.UUID, limit, offset int32) ([]ThreadWithComments, int64, error) {
	rows, err := s.q.ListThreadsForWorkBranch(ctx, gen.ListThreadsForWorkBranchParams{
		WorkBranchID: pgUUID(workBranchID),
		Limit:        limit,
		Offset:       offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("listing threads for work branch %s: %w", workBranchID, err)
	}
	total, err := s.q.CountThreadsForWorkBranch(ctx, pgUUID(workBranchID))
	if err != nil {
		return nil, 0, fmt.Errorf("counting threads for work branch %s: %w", workBranchID, err)
	}
	threads := make([]ThreadWithComments, 0, len(rows))
	ids := make([]pgtype.UUID, 0, len(rows))
	for _, row := range rows {
		threads = append(threads, ThreadWithComments{Thread: Thread{
			ID:           uuidFromPg(row.ID),
			WorkBranchID: uuidFromPg(row.WorkBranchID),
			RoundID:      uuidFromPg(row.RoundID),
			RoundNumber:  row.RoundNumber,
			Author:       row.Author,
			File:         textFromPg(row.File),
			Line:         int4FromPg(row.Line),
			Resolved:     row.Resolved,
			CreatedAt:    row.CreatedAt.Time,
			UpdatedAt:    row.UpdatedAt.Time,
		}})
		ids = append(ids, row.ID)
	}
	if len(ids) == 0 {
		return threads, total, nil
	}
	comments, err := s.q.ListCommentsForThreads(ctx, ids)
	if err != nil {
		return nil, 0, fmt.Errorf("listing comments for work branch %s: %w", workBranchID, err)
	}
	byThread := make(map[uuid.UUID]int, len(threads))
	for i, thread := range threads {
		byThread[thread.ID] = i
	}
	for _, row := range comments {
		i, ok := byThread[uuidFromPg(row.ThreadID)]
		if !ok {
			continue
		}
		threads[i].Comments = append(threads[i].Comments, Comment{
			ID:          uuidFromPg(row.ID),
			ThreadID:    uuidFromPg(row.ThreadID),
			RoundID:     uuidFromPg(row.RoundID),
			RoundNumber: row.RoundNumber,
			Author:      row.Author,
			Body:        row.Body,
			CreatedAt:   row.CreatedAt.Time,
		})
	}
	return threads, total, nil
}
