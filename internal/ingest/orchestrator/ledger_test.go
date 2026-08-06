package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/chunkstore"
	"github.com/bobcob7/loam/internal/diffplan"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/ingest/vectors"
)

// These are loam-qj21's ORDER tests. Like the rest of this file's
// neighbours they run against mocks, because order is what a mock can pin
// and a live database cannot: every assertion below would still pass
// against Postgres with the statements in the wrong sequence, right up
// until the one interleaving that loses data.

// rejectionOf builds the vectors.Rejection a rejecting Persist would
// report, so the tests below can drive updateLedger without a real store.
func rejectionOf(path string, state chunkstore.ChunksState) vectors.Rejection {
	return vectors.Rejection{Path: path, ChunksState: state, SQLState: "22P02", Err: errors.New("NaN not allowed in vector")}
}

// rejectPersist makes the harness's vector track reject the named paths,
// reporting them exactly as the real Persist does.
func rejectPersist(h *harness, rejections ...vectors.Rejection) {
	h.vectors.PersistFunc = func(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string, p vectors.Prepared) (vectors.Stats, error) {
		h.log.record("vectors.Persist")
		return vectors.Stats{FilesRejected: len(rejections), Rejected: rejections}, nil
	}
}

// TestRun_LedgerIsWrittenBeforeTheIngestedRefAdvances is the ordering this
// whole bead rests on, and it is the one that is invisible without an
// order test.
//
// The defect being fixed is that AdvanceIngestedRef moves the diff base
// past a rejected file, after which no `git diff ingested_ref..tip` can
// ever name that path again. The ledger is what re-plans it. So "the
// rejection is recorded" and "the ref advanced past it" must be one atomic
// fact -- same transaction, ledger first. Record it AFTER the ref and a
// crash between the two statements is impossible (they are in one
// transaction), but the ordering still documents the dependency; put it
// OUTSIDE the transaction and the two can genuinely disagree.
func TestRun_LedgerIsWrittenBeforeTheIngestedRefAdvances(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rejectPersist(h, rejectionOf("poison.go", chunkstore.ChunksAbsent))

	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)

	record := h.log.indexOf("ledger.Record")
	advance := h.log.indexOf("advanceIngestedRef")
	begin := h.log.indexOf("beginTx")
	commit := h.log.indexOf("commit")
	require.Positive(t, record, "the rejection must be recorded at all")
	assert.Less(t, record, advance, "the ledger entry must be staged before the diff base moves past the file it names")
	assert.Less(t, begin, record, "and inside the transaction, never before it opens")
	assert.Less(t, record, commit, "and before the commit, so it lands with the index it describes or not at all")
}

// TestRun_LedgerIsReadBeforeTheTransactionOpens. The read is an INPUT to
// the plan, not a write, and holding the swap transaction open across it
// would extend the transaction for no benefit -- the same rule the repo and
// target-branch reads already follow.
func TestRun_LedgerIsReadBeforeTheTransactionOpens(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)

	list := h.log.indexOf("ledger.List")
	require.Positive(t, list)
	assert.Less(t, list, h.log.indexOf("readFiles"),
		"the ledger's paths join the reparse set, so they must be known before the files are read")
	assert.Less(t, list, h.log.indexOf("beginTx"))
}

// TestRun_LedgeredPathsReachTheFileReader is the retry, observed at the
// only place in this package where it is observable: the set of paths
// handed to contentReader.ReadFiles.
//
// The harness's plan reparses "kept.go" and nothing else -- exactly the
// shape of an incremental ingest in which the rejected file did not change,
// which is every ingest after the one that rejected it. Without the union,
// "poison.go" is never read, never chunked, never re-embedded and never
// retried, and the assertion below is the difference.
func TestRun_LedgeredPathsReachTheFileReader(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ledger.ListFunc = func(context.Context, uuid.UUID, string) ([]chunkstore.Rejection, error) {
		h.log.record("ledger.List")
		return []chunkstore.Rejection{
			{File: "poison.go", State: chunkstore.RejectionPending},
			{File: "hopeless.go", State: chunkstore.RejectionExhausted},
		}, nil
	}

	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)

	calls := h.content.ReadFilesCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"kept.go", "poison.go"}, calls[0].Paths,
		"the pending ledgered path must be read even though the diff never named it; the exhausted one must not, or the bound is not a bound")
}

// TestRun_ClearsALedgeredPathThatWasWrittenSuccessfully is the other half
// of the retry: the mechanism has to be able to STOP. A clear that never
// fires leaves every fixed file reported broken forever, which would make
// the ledger exactly the untrustworthy signal loam-2d44 rejected.
func TestRun_ClearsALedgeredPathThatWasWrittenSuccessfully(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ledger.ListFunc = func(context.Context, uuid.UUID, string) ([]chunkstore.Rejection, error) {
		h.log.record("ledger.List")
		return []chunkstore.Rejection{{File: "poison.go", State: chunkstore.RejectionPending}}, nil
	}

	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)

	calls := h.ledger.ClearCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"poison.go"}, calls[0].Paths)
	assert.Empty(t, h.ledger.RecordCalls(), "a clean ingest records nothing")
}

// TestRun_DoesNotClearALedgeredPathThatRejectedAgain. The clear set is
// "attempted and did not reject", not "attempted": clearing a path that
// failed again would delete the row the very next statement is about to
// rewrite, and if the two ever got out of order it would delete the record
// entirely.
func TestRun_DoesNotClearALedgeredPathThatRejectedAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ledger.ListFunc = func(context.Context, uuid.UUID, string) ([]chunkstore.Rejection, error) {
		h.log.record("ledger.List")
		return []chunkstore.Rejection{
			{File: "poison.go", State: chunkstore.RejectionPending},
			{File: "fixed.go", State: chunkstore.RejectionPending},
		}, nil
	}
	rejectPersist(h, rejectionOf("poison.go", chunkstore.ChunksStale))

	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)

	clears := h.ledger.ClearCalls()
	require.Len(t, clears, 1)
	assert.Equal(t, []string{"fixed.go"}, clears[0].Paths,
		"only the path that actually landed is cleared; the one that rejected again keeps its row and its attempt history")
	records := h.ledger.RecordCalls()
	require.Len(t, records, 1)
	assert.Equal(t, "poison.go", records[0].In.File)
	assert.Equal(t, chunkstore.ChunksStale, records[0].In.ChunksState)
}

// TestRun_DoesNotClearALedgeredPathTheMirrorNeverHandedBack is the subtle
// one, and the reason the clear set is computed from what was ATTEMPTED
// rather than from the plan.
//
// contentReader.ReadFiles deliberately SKIPS a path that no longer
// resolves to a blob at the new ref (a submodule gitlink, or a file that
// vanished between the plan and the read) and reports that as success, not
// an error. Such a path is in the plan's reparse set and in no chunk write
// at all, so treating "it was planned" as "it was retried" would clear the
// only record that its chunks are still missing -- silently, on an ingest
// that did nothing about it.
func TestRun_DoesNotClearALedgeredPathTheMirrorNeverHandedBack(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ledger.ListFunc = func(context.Context, uuid.UUID, string) ([]chunkstore.Rejection, error) {
		h.log.record("ledger.List")
		return []chunkstore.Rejection{{File: "vanished.go", State: chunkstore.RejectionPending}}, nil
	}
	h.content.ReadFilesFunc = func(ctx context.Context, mirrorDir, ref string, paths []string) ([]File, error) {
		h.log.record("readFiles")
		var out []File
		for _, p := range paths {
			if p == "vanished.go" {
				continue
			}
			out = append(out, File{Path: p, Content: []byte("package a\n\nfunc A() {}\n")})
		}
		return out, nil
	}

	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)

	assert.Empty(t, h.ledger.ClearCalls(),
		"a path the reader skipped was never retried, so nothing about it was resolved and its row must survive")
}

// TestRun_ClearsALedgeredPathThePlanIsDropping. A rejected file that is
// then DELETED owes nothing: its chunks are gone on purpose, so a ledger
// row claiming they are missing would be permanently, misleadingly true.
// The harness's plan drops "gone.go".
func TestRun_ClearsALedgeredPathThePlanIsDropping(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ledger.ListFunc = func(context.Context, uuid.UUID, string) ([]chunkstore.Rejection, error) {
		h.log.record("ledger.List")
		return []chunkstore.Rejection{{File: "gone.go", State: chunkstore.RejectionPending}}, nil
	}

	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)

	calls := h.ledger.ClearCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"gone.go"}, calls[0].Paths)
}

// TestRun_FullRebuildEmptiesTheWholeLedgerThenReRecords is the KindFull
// rule, and the assertion order matters as much as the calls.
//
// A full rebuild exists for the cases with no usable diff, so the ledger
// may hold paths the new tree does not contain at all and no per-path clear
// keyed on what was attempted could ever name them -- the same argument
// that makes chunks' own full-rebuild drop repo-scoped. Emptying and
// re-recording in one transaction is what keeps that from being a window
// in which a still-broken path has no row.
func TestRun_FullRebuildEmptiesTheWholeLedgerThenReRecords(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.planner.PlanFunc = func(ctx context.Context, mirrorDir string, req diffplan.Request) (diffplan.Plan, error) {
		h.log.record("plan")
		return diffplan.Plan{Kind: ingest.KindFull, ReparseFiles: []string{"kept.go", "poison.go"}}, nil
	}
	h.ledger.ListFunc = func(context.Context, uuid.UUID, string) ([]chunkstore.Rejection, error) {
		h.log.record("ledger.List")
		return []chunkstore.Rejection{{File: "poison.go", State: chunkstore.RejectionPending}}, nil
	}
	rejectPersist(h, rejectionOf("poison.go", chunkstore.ChunksAbsent))

	_, err := h.orch.Run(t.Context(), h.job(ingest.KindFull))
	require.NoError(t, err)

	require.Len(t, h.ledger.ClearAllCalls(), 1, "a full rebuild empties the ledger wholesale")
	assert.Empty(t, h.ledger.ClearCalls(), "and never per-path: a per-path clear cannot name a file the new tree no longer has")
	records := h.ledger.RecordCalls()
	require.Len(t, records, 1, "and re-records whatever rejected during it, in the same transaction")
	assert.Equal(t, chunkstore.ChunksAbsent, records[0].In.ChunksState,
		"a full rebuild's repo-scoped drop ran before the write phase and outside the savepoints, so a file that rejects during it loses its prior chunks: stale becomes absent")
	assert.Less(t, h.log.indexOf("ledger.ClearAll"), h.log.indexOf("ledger.Record"),
		"emptying after re-recording would delete the row it had just written")
}

// TestRun_LedgerReadFailureAbortsBeforeAnythingIsWritten. A ledger this
// ingest could not read is not the same as an empty one: proceeding would
// silently skip every retry the ledger was holding, which is exactly the
// behaviour before this bead.
func TestRun_LedgerReadFailureAbortsBeforeAnythingIsWritten(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	boom := errors.New("ledger unreadable")
	h.ledger.ListFunc = func(context.Context, uuid.UUID, string) ([]chunkstore.Rejection, error) {
		h.log.record("ledger.List")
		return nil, boom
	}

	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))

	require.ErrorIs(t, err, boom)
	assert.Zero(t, h.commits)
	assert.Equal(t, -1, h.log.indexOf("beginTx"), "the transaction must never open")
}

// TestRun_LedgerWriteFailureRollsBackTheWholeIngest. This is the property
// that makes "the ledger cannot disagree with what committed" true rather
// than merely intended: if the rejection cannot be recorded, the ingest
// that would have advanced past it must not commit either.
func TestRun_LedgerWriteFailureRollsBackTheWholeIngest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	boom := errors.New("ledger unwritable")
	rejectPersist(h, rejectionOf("poison.go", chunkstore.ChunksAbsent))
	h.ledger.RecordFunc = func(context.Context, pgx.Tx, uuid.UUID, string, chunkstore.RejectionInput) error {
		h.log.record("ledger.Record")
		return boom
	}

	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))

	require.ErrorIs(t, err, boom)
	assert.Zero(t, h.commits)
	assert.Equal(t, 1, h.rollback)
	assert.Equal(t, -1, h.log.indexOf("advanceIngestedRef"),
		"the ref must not advance past a rejection the ledger could not record")
}

// TestRun_RecordCarriesTheJobAndTheRefItWasWriting. The ledger row is
// meant to be self-describing: job_id is the join the per-file ERROR log
// lines never had (before this, the COUNT was joinable to a job and the
// FILENAMES were not), and rejected_ref is what makes the row readable
// months later as "this path's chunks are not the ones <ref> claims".
func TestRun_RecordCarriesTheJobAndTheRefItWasWriting(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rejectPersist(h, rejectionOf("poison.go", chunkstore.ChunksAbsent))
	job := h.job(ingest.KindIncremental)

	_, err := h.orch.Run(t.Context(), job)
	require.NoError(t, err)

	records := h.ledger.RecordCalls()
	require.Len(t, records, 1)
	assert.Equal(t, job.ID, records[0].In.JobID)
	assert.Equal(t, testNewRef, records[0].In.RejectedRef, "the ref recorded must be the one this ingest advanced to")
	assert.Equal(t, "22P02", records[0].In.SQLState)
	assert.Contains(t, records[0].In.Error, "NaN not allowed in vector")
}

// TestRun_HealthyIngestTouchesTheLedgerOnlyToReadIt is the cost control
// and the noise control at once. Every repo in a deployment runs this path
// on every ingest; a mechanism that wrote to the ledger when there was
// nothing to say would put a statement in every swap transaction forever.
func TestRun_HealthyIngestTouchesTheLedgerOnlyToReadIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)

	require.Len(t, h.ledger.ListCalls(), 1)
	assert.Empty(t, h.ledger.RecordCalls())
	assert.Empty(t, h.ledger.ClearCalls(), "nothing ledgered means nothing to clear, so no statement at all")
	assert.Empty(t, h.ledger.ClearAllCalls())
}
