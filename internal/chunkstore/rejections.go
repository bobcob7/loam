package chunkstore

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bobcob7/loam/internal/db/gen"
)

// MaxRejectionAttempts bounds how many times a path the store keeps
// refusing is automatically retried before Rejections.Record marks it
// RejectionExhausted and the planner stops unioning it into incremental
// plans (loam-qj21).
//
// A bound is required rather than nice to have. The retry works by putting
// a ledgered path back into every subsequent ingest's reparse set, and a
// file that is malformed for a reason no retry can fix -- a chunk whose
// embedding genuinely cannot be represented, a row that will always
// violate a constraint -- would otherwise be re-read, re-parsed,
// re-chunked, re-embedded (a network round trip to the embedder) and
// re-rejected on EVERY ingest of that repo, forever. That is a permanent
// tax levied by one bad file on every future commit.
//
// 3 is chosen for what the retries are actually FOR, which is transient
// server-side conditions -- a size or resource limit the server was under
// at that moment, a constraint that another concurrent write had
// momentarily made unsatisfiable. Those clear within an attempt or two or
// they are not transient. It is deliberately not larger: each attempt
// costs a full re-embed of the file, so a generous ceiling buys very
// little and spends real money on the exact files least likely to repay
// it.
//
// WHAT EXHAUSTED MEANS, AND WHAT AN OPERATOR DOES ABOUT IT. Exhausted is
// not "give up and forget" -- the row STAYS in the ledger, which is what
// keeps the path visible and keeps the repo reporting an incomplete index.
// It only stops the AUTOMATIC retry. Two things still reach an exhausted
// path with no operator action at all: a real change to the file (it then
// appears in `git diff ingested_ref..tip` on its own merits, is reparsed,
// and clears the ledger if it lands), and a full rebuild (which re-chunks
// every file in the tree and resets the ledger wholesale). So the operator
// remedy is, in order of cost: read the row -- it names the path, the
// SQLSTATE, the error text, the job, and the ref -- and fix the file
// upstream; or, if the file is correct and the store was wrong, request a
// manual reindex of that repo, which runs KindFull and gives every
// exhausted path a fresh budget. Doing nothing is also a defensible
// choice, and is why the row is not deleted: an exhausted row is an
// accurate, permanent statement that this path's chunks are not the ones
// the ingested ref claims.
const MaxRejectionAttempts = 3

// RejectionState is chunk_rejections.state: whether the next incremental
// ingest should retry this path.
type RejectionState string

const (
	// RejectionPending means the path is unioned into the next
	// incremental plan's reparse set.
	RejectionPending RejectionState = "pending"
	// RejectionExhausted means it is not -- see MaxRejectionAttempts for
	// what still reaches it and what an operator does about it.
	RejectionExhausted RejectionState = "exhausted"
)

// ChunksState is chunk_rejections.chunks_state: what a SEARCHER sees for a
// rejected path right now. The two values are different failure modes with
// different urgency, and conflating them is the imprecision this ledger
// exists partly to remove.
type ChunksState string

const (
	// ChunksStale means the file was already indexed and its PRIOR chunks
	// survived the rejection, because ReplaceFileChunks unwinds to a
	// per-file SAVEPOINT and the delete goes back with the inserts
	// (loam-c94.24). The file is still searchable -- it just serves
	// content from an older commit while repo_target_branches.ingested_ref
	// claims a newer one.
	ChunksStale ChunksState = "stale"
	// ChunksAbsent means there were no prior chunks to survive: either the
	// file's first ingest, or a KindFull ingest whose repo-scoped
	// DropRepoBranch deleted every chunks row BEFORE the write phase and
	// OUTSIDE the savepoints, so nothing unwound that delete. The file is
	// not in RAG search at all. A full rebuild therefore converts stale
	// into absent for any file that rejects again during it.
	ChunksAbsent ChunksState = "absent"
)

// Rejection is one ledger row: a path whose chunks the store refused to
// write, and everything needed to diagnose it without scraping logs.
type Rejection struct {
	File        string
	Attempts    int
	State       RejectionState
	ChunksState ChunksState
	// SQLState is Postgres's five-character code when the error carried
	// one, or "" when it did not (which is what a non-Postgres store
	// produces).
	SQLState string
	Error    string
	// JobID names the ingest_jobs row whose stats.files_rejected counted
	// this path, or uuid.Nil when the writer had no job to name. It is the
	// join the per-file ERROR log lines never provided: before this ledger
	// the COUNT was joinable to a job and the FILENAMES were not.
	JobID uuid.UUID
	// RejectedRef is the commit the ingest was writing when this path was
	// rejected -- the ref ingested_ref advanced to despite this path not
	// landing.
	RejectedRef     string
	FirstRejectedAt time.Time
	LastRejectedAt  time.Time
}

// RejectionInput is one rejection to record. Attempts and State are
// deliberately absent: both are derived by the upsert from whatever the
// row already held, so no caller can compute them and get them wrong.
type RejectionInput struct {
	File        string
	ChunksState ChunksState
	SQLState    string
	Error       string
	JobID       uuid.UUID
	RejectedRef string
}

// Rejections is the per-path rejection ledger over the chunk_rejections
// table (loam-qj21). Construct with NewRejections for the pool-bound read
// the planner needs before the swap transaction opens, or
// NewRejectionsInTx for the writes, which MUST happen inside the swap's
// own transaction so the ledger commits with the index it describes.
//
// It opens no transaction of its own in either form. The read is a single
// statement and needs none; the writes are the caller's to commit, and a
// ledger that committed separately from the swap would be able to say the
// opposite of what landed -- recording a rejection for an ingest that then
// rolled back, or missing one for an ingest that committed.
type Rejections struct {
	q      rejectionQueries
	logger *slog.Logger
}

// NewRejections builds a pool-bound ledger, for reads made outside any
// transaction.
func NewRejections(pool *pgxpool.Pool, logger *slog.Logger) *Rejections {
	return &Rejections{q: gen.New(pool), logger: logger}
}

// NewRejectionsInTx builds a ledger bound to tx, for the writes the swap
// stages inside its own transaction.
func NewRejectionsInTx(tx pgx.Tx, logger *slog.Logger) *Rejections {
	return &Rejections{q: gen.New(tx), logger: logger}
}

// newRejections is the seam-taking core both constructors share, so this
// package's unit tests drive the ledger against a moq mock.
func newRejections(q rejectionQueries, logger *slog.Logger) *Rejections {
	return &Rejections{q: q, logger: logger}
}

// List returns every ledgered path for repoID+targetBranch in path order,
// exhausted rows included. Callers that are building a retry set want
// PendingPaths over this result; callers reporting to an operator want all
// of it.
func (r *Rejections) List(ctx context.Context, repoID uuid.UUID, targetBranch string) ([]Rejection, error) {
	rows, err := r.q.ListChunkRejections(ctx, gen.ListChunkRejectionsParams{
		RepoID:       pgUUID(repoID),
		TargetBranch: targetBranch,
	})
	if err != nil {
		return nil, fmt.Errorf("listing chunk rejections for repo %s branch %s: %w", repoID, targetBranch, err)
	}
	out := make([]Rejection, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromGenRejection(row))
	}
	return out, nil
}

// Record upserts one rejection, incrementing that path's attempt count and
// letting the statement itself decide whether the path has exhausted its
// budget (MaxRejectionAttempts).
func (r *Rejections) Record(ctx context.Context, repoID uuid.UUID, targetBranch string, in RejectionInput) error {
	if err := r.q.RecordChunkRejection(ctx, gen.RecordChunkRejectionParams{
		RepoID:       pgUUID(repoID),
		TargetBranch: targetBranch,
		File:         in.File,
		Column4:      int32(MaxRejectionAttempts),
		ChunksState:  string(in.ChunksState),
		Sqlstate:     pgtype.Text{String: in.SQLState, Valid: in.SQLState != ""},
		Error:        in.Error,
		JobID:        pgtype.UUID{Bytes: in.JobID, Valid: in.JobID != uuid.Nil},
		RejectedRef:  in.RejectedRef,
	}); err != nil {
		return fmt.Errorf("recording chunk rejection for repo %s file %s: %w", repoID, in.File, err)
	}
	r.logger.WarnContext(ctx, "ledgered a rejected file so the next ingest retries it",
		"repo_id", repoID, "target_branch", targetBranch, "file", in.File,
		"chunks_state", in.ChunksState, "sqlstate", in.SQLState, "job_id", in.JobID,
		"rejected_ref", in.RejectedRef)
	return nil
}

// Clear removes the named paths from the ledger: the paths whose chunks
// this ingest wrote successfully, and the paths the plan dropped. An empty
// paths issues no statement at all -- the healthy case is an empty ledger
// and nothing to clear, and a no-op DELETE inside a transaction whose
// whole point is to be short is pure overhead.
func (r *Rejections) Clear(ctx context.Context, repoID uuid.UUID, targetBranch string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if err := r.q.DeleteChunkRejections(ctx, gen.DeleteChunkRejectionsParams{
		RepoID:       pgUUID(repoID),
		TargetBranch: targetBranch,
		Column3:      paths,
	}); err != nil {
		return fmt.Errorf("clearing %d chunk rejection(s) for repo %s: %w", len(paths), repoID, err)
	}
	r.logger.InfoContext(ctx, "cleared ledgered rejections whose chunks landed",
		"repo_id", repoID, "target_branch", targetBranch, "files", paths)
	return nil
}

// ClearAll empties the ledger for a repo+branch, which the full-rebuild
// path does alongside its repo-scoped drop. See the query's own comment
// (DeleteChunkRejectionsForRepoBranch) for why a full rebuild cannot use a
// per-path clear and why resetting the attempt budget is correct there.
func (r *Rejections) ClearAll(ctx context.Context, repoID uuid.UUID, targetBranch string) error {
	if err := r.q.DeleteChunkRejectionsForRepoBranch(ctx, gen.DeleteChunkRejectionsForRepoBranchParams{
		RepoID:       pgUUID(repoID),
		TargetBranch: targetBranch,
	}); err != nil {
		return fmt.Errorf("clearing the chunk rejection ledger for repo %s branch %s: %w", repoID, targetBranch, err)
	}
	return nil
}

// PendingPaths is the retry set: the files of every rejection still under
// its attempt budget, in the order List returned them. Exhausted rows are
// excluded HERE rather than in the query, so the same read serves both the
// planner and any operator surface -- see ListChunkRejections' own comment.
func PendingPaths(rejections []Rejection) []string {
	var paths []string
	for _, r := range rejections {
		if r.State == RejectionPending {
			paths = append(paths, r.File)
		}
	}
	return paths
}

// fromGenRejection converts a sqlc row into this package's domain type. It
// returns no error: unlike fromGenChunk it decodes no uuid the caller must
// be able to trust as a key -- repo_id is echoed back from the caller's own
// argument and job_id is correlation only, so a NULL there is a legitimate
// value (uuid.Nil) rather than a schema-contract violation.
func fromGenRejection(row gen.ChunkRejection) Rejection {
	r := Rejection{
		File:            row.File,
		Attempts:        int(row.Attempts),
		State:           RejectionState(row.State),
		ChunksState:     ChunksState(row.ChunksState),
		Error:           row.Error,
		RejectedRef:     row.RejectedRef,
		FirstRejectedAt: row.FirstRejectedAt.Time,
		LastRejectedAt:  row.LastRejectedAt.Time,
	}
	if row.Sqlstate.Valid {
		r.SQLState = row.Sqlstate.String
	}
	if row.JobID.Valid {
		r.JobID = uuid.UUID(row.JobID.Bytes)
	}
	return r
}
