package chunkstore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/db/gen"
)

func rejectionsLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// TestRecord_PassesTheAttemptCeilingToTheStatement is the test that keeps
// the bound honest. MaxRejectionAttempts is a Go constant, but the decision
// it drives -- pending vs exhausted -- is made by SQL, so the only thing
// standing between the documented ceiling and the one actually enforced is
// this parameter. Asserting the exact value (rather than "non-zero") is
// what makes a drifted constant fail here rather than silently retrying
// forever or exhausting on the first rejection.
func TestRecord_PassesTheAttemptCeilingToTheStatement(t *testing.T) {
	t.Parallel()
	var got gen.RecordChunkRejectionParams
	q := &rejectionQueriesMock{
		RecordChunkRejectionFunc: func(_ context.Context, arg gen.RecordChunkRejectionParams) error {
			got = arg
			return nil
		},
	}
	repoID := uuid.Must(uuid.NewV7())
	jobID := uuid.Must(uuid.NewV7())

	require.NoError(t, newRejections(q, rejectionsLogger()).Record(t.Context(), repoID, "main", RejectionInput{
		File:        "poison.go",
		ChunksState: ChunksStale,
		SQLState:    "22P02",
		Error:       "NaN not allowed in vector",
		JobID:       jobID,
		RejectedRef: "cafebabe",
	}))

	assert.Equal(t, int32(MaxRejectionAttempts), got.Column4,
		"the ceiling the statement applies must be the one MaxRejectionAttempts documents")
	assert.Equal(t, "poison.go", got.File)
	assert.Equal(t, string(ChunksStale), got.ChunksState)
	assert.Equal(t, "22P02", got.Sqlstate.String)
	assert.True(t, got.Sqlstate.Valid, "a present SQLSTATE must be stored, not written as NULL")
	assert.Equal(t, jobID[:], got.JobID.Bytes[:])
	assert.True(t, got.JobID.Valid)
	assert.Equal(t, "cafebabe", got.RejectedRef)
}

// TestRecord_AbsentSQLStateAndJobAreWrittenAsNULL covers the caller that
// has neither: a rejection from a store that is not Postgres carries no
// PgError, and a caller with no ingest_jobs row has no job to name. Both
// are legitimate, so both must round-trip as NULL rather than as the empty
// string and the nil UUID -- an empty string and 00000000-... would both read as real
// values to anyone querying the table.
func TestRecord_AbsentSQLStateAndJobAreWrittenAsNULL(t *testing.T) {
	t.Parallel()
	var got gen.RecordChunkRejectionParams
	q := &rejectionQueriesMock{
		RecordChunkRejectionFunc: func(_ context.Context, arg gen.RecordChunkRejectionParams) error {
			got = arg
			return nil
		},
	}

	require.NoError(t, newRejections(q, rejectionsLogger()).Record(t.Context(), uuid.Must(uuid.NewV7()), "main", RejectionInput{
		File: "x.go", ChunksState: ChunksAbsent, Error: "boom",
	}))

	assert.False(t, got.Sqlstate.Valid, "no SQLSTATE must be NULL, not ''")
	assert.False(t, got.JobID.Valid, "no job must be NULL, not the nil UUID")
}

// TestClear_EmptyPathsIssuesNoStatement pins the healthy path. An empty
// ledger is the normal state of every repo, and this clear runs inside the
// swap transaction, whose whole design goal is to be short.
func TestClear_EmptyPathsIssuesNoStatement(t *testing.T) {
	t.Parallel()
	q := &rejectionQueriesMock{}

	require.NoError(t, newRejections(q, rejectionsLogger()).Clear(t.Context(), uuid.Must(uuid.NewV7()), "main", nil))

	assert.Empty(t, q.DeleteChunkRejectionsCalls(),
		"no paths to clear must mean no round trip -- an unconfigured moq method would panic if one were made")
}

// TestList_DecodesEveryColumnIncludingTheNullableOnes. The ledger's whole
// value is that a row is self-describing, so a column silently dropped in
// decoding is the same defect as never having stored it.
func TestList_DecodesEveryColumnIncludingTheNullableOnes(t *testing.T) {
	t.Parallel()
	jobID := uuid.Must(uuid.NewV7())
	q := &rejectionQueriesMock{
		ListChunkRejectionsFunc: func(_ context.Context, _ gen.ListChunkRejectionsParams) ([]gen.ChunkRejection, error) {
			return []gen.ChunkRejection{
				{
					File: "named.go", Attempts: 2, State: string(RejectionPending),
					ChunksState: string(ChunksStale), Error: "boom", RejectedRef: "deadbeef",
					Sqlstate: pgtype.Text{String: "22P02", Valid: true}, JobID: pgUUID(jobID),
				},
				{
					File: "anonymous.go", Attempts: 3, State: string(RejectionExhausted),
					ChunksState: string(ChunksAbsent), Error: "still boom", RejectedRef: "deadbeef",
				},
			}, nil
		},
	}

	got, err := newRejections(q, rejectionsLogger()).List(t.Context(), uuid.Must(uuid.NewV7()), "main")
	require.NoError(t, err)

	require.Len(t, got, 2)
	assert.Equal(t, Rejection{
		File: "named.go", Attempts: 2, State: RejectionPending, ChunksState: ChunksStale,
		SQLState: "22P02", Error: "boom", JobID: jobID, RejectedRef: "deadbeef",
	}, got[0])
	assert.Equal(t, uuid.Nil, got[1].JobID, "a NULL job_id decodes to the nil UUID, not an error")
	assert.Empty(t, got[1].SQLState, "a NULL sqlstate decodes to the empty string")
	assert.Equal(t, RejectionExhausted, got[1].State)
}

// TestPendingPaths_ExcludesExhaustedRowsOnly is the bound's other half:
// List deliberately returns exhausted rows (an operator needs to see them),
// so the exclusion has to happen here, and getting it backwards would
// either retry a hopeless file forever or stop retrying a fixable one on
// its first failure.
//
// The fixture separates those two mistakes: with one pending and one
// exhausted row, "return everything" and "return nothing" are both wrong
// and both distinguishable from the right answer.
func TestPendingPaths_ExcludesExhaustedRowsOnly(t *testing.T) {
	t.Parallel()
	got := PendingPaths([]Rejection{
		{File: "retry-me.go", State: RejectionPending},
		{File: "hopeless.go", State: RejectionExhausted},
		{File: "retry-me-too.go", State: RejectionPending},
	})

	assert.Equal(t, []string{"retry-me.go", "retry-me-too.go"}, got)
}

func TestPendingPaths_NoRejectionsIsNil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, PendingPaths(nil))
	assert.Nil(t, PendingPaths([]Rejection{{File: "hopeless.go", State: RejectionExhausted}}),
		"a ledger holding only exhausted rows must retry nothing, so WithRetryPaths is handed nothing")
}

// TestRecord_WrapsTheStatementError so a failure here is attributable: a
// ledger write that fails must fail the whole swap (see updateLedger), and
// the error an operator sees has to name the path it was recording.
func TestRecord_WrapsTheStatementError(t *testing.T) {
	t.Parallel()
	boom := errors.New("relation \"chunk_rejections\" does not exist")
	q := &rejectionQueriesMock{
		RecordChunkRejectionFunc: func(_ context.Context, _ gen.RecordChunkRejectionParams) error { return boom },
	}

	err := newRejections(q, rejectionsLogger()).Record(t.Context(), uuid.Must(uuid.NewV7()), "main", RejectionInput{File: "poison.go"})

	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "poison.go")
}
