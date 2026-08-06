//go:build integration

// Run explicitly with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/ingest/orchestrator/... -v
//
// See integration_test.go's header for the shared container and the
// podman/ryuk note; this file reuses both, along with its fixture,
// nanVectorEmbedder and chunkCountFor.
//
// These are loam-qj21's tests, and they need a real Postgres for two
// independent reasons. The rejection is pgvector refusing a NaN coordinate
// at INSERT (SQLSTATE 22000), a server-side per-statement error no mock
// produces. And the RETRY is a property of what a real `git diff
// ingested_ref..tip` reports -- specifically, that it reports NOTHING for
// a file nobody touched, which is the whole defect. A fake planner would
// have to be told to omit the path, i.e. to assume the answer.
package orchestrator

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/chunkstore"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/testembed"
)

// ledgerRows reads the rejection ledger on the READER pool, never the
// writer, for the same reason every other observation in this package
// does: it must be a genuinely separate session, so a row it sees is a row
// that actually committed.
func ledgerRows(t *testing.T, f *fixture) []chunkstore.Rejection {
	t.Helper()
	rows, err := chunkstore.NewRejections(readerPool, testLogger()).List(t.Context(), f.repoID, "main")
	require.NoError(t, err)
	return rows
}

// ledgerFor returns the one ledger row for file, failing if there is none.
func ledgerFor(t *testing.T, f *fixture, file string) chunkstore.Rejection {
	t.Helper()
	for _, r := range ledgerRows(t, f) {
		if r.File == file {
			return r
		}
	}
	t.Fatalf("no ledger row for %s; ledger holds %v", file, ledgerPaths(t, f))
	return chunkstore.Rejection{}
}

func ledgerPaths(t *testing.T, f *fixture) []string {
	t.Helper()
	rows := ledgerRows(t, f)
	paths := make([]string, 0, len(rows))
	for _, r := range rows {
		paths = append(paths, r.File)
	}
	return paths
}

// TestIngest_RejectedFilesAreLedgeredAndTheNextIngestRetriesThem is this
// bead's headline test: the whole defect and the whole fix in one run.
//
// # Why the second ingest makes no commit at all
//
// That is the defect, stated as a fixture. After the first ingest,
// repo_target_branches.ingested_ref names the commit whose files were
// rejected. The next incremental ingest plans from `git diff
// ingested_ref..tip`, and here those two refs are THE SAME COMMIT, so the
// diff is empty and git can name nothing. Before this change the second
// run reparsed zero files and the rejected ones stayed out of search
// forever. The only thing that can put them back is the ledger, so this
// test is the difference between the two behaviours with nothing else
// varying -- not even a commit.
//
// # What the fixture makes distinguishable
//
// THREE rejections of FIVE files, with the two survivors carrying
// different symbol counts, is loam-2d44's arithmetic and it is kept for
// the same reason: it separates FilesRejected (3) from the survivor count
// (2), the chunk count (4) and the batch size (5), so no neighbouring
// quantity can stand in for it.
//
// This bead needs one more separation that a count cannot give: that the
// ledger named the RIGHT three. So every file carries distinct content and
// a distinct symbol, the ledger's paths are asserted as an exact sorted
// set, and each file's chunk count is asserted individually both before
// and after the retry. Identical files could show three rows existed and
// nothing about which three.
//
// The retry is asserted the same way. It is not enough that the ledger
// empties -- an implementation that cleared the ledger unconditionally
// would pass that. The three files must have CHUNKS afterwards, which only
// a genuine reparse-and-re-embed can produce, and the two survivors' chunk
// counts must be unchanged, which rules out a full rebuild having quietly
// happened instead.
func TestIngest_RejectedFilesAreLedgeredAndTheNextIngestRetriesThem(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commit(t, "three poisoned of five", map[string]string{
		"clean_alpha.go": "package alpha\n\nfunc AlphaKeeps() {}\n",
		"bad_one.go":     "package badone\n\nfunc RejectedFirst() {}\n",
		"bad_two.go":     "package badtwo\n\nfunc RejectedSecond() {}\n",
		"bad_three.go":   "package badthree\n\nfunc RejectedThird() {}\n",
		"clean_omega.go": "package omega\n\nfunc OmegaKeepsOne() {}\n\nfunc OmegaKeepsTwo() {}\n\nfunc OmegaKeepsThree() {}\n",
	})
	poisoning := nanVectorEmbedder{Embedder: testembed.New(), marker: "Rejected"}
	logger, capture := newCapturingLogger()
	job := f.job(ingest.KindIncremental)

	stats, err := newOrchestratorWithLogger(t, f, realTransactor(), poisoning, logger).Run(t.Context(), job)
	require.NoError(t, err, "rejected files must not fail the job -- loam-c94.24's trade, and the reason the ledger has to carry the signal")
	require.Equal(t, 3, stats.FilesRejected)
	refAfterFirst := ingestedRef(t, f)
	require.NotEmpty(t, refAfterFirst, "the ref must have advanced past the rejection: that is the condition the retry has to work around")

	assert.Equal(t, []string{"bad_one.go", "bad_three.go", "bad_two.go"}, ledgerPaths(t, f),
		"the ledger must name exactly the three rejected files, not any three: each file has distinct content and a distinct symbol so a wrong set is visible")
	for _, path := range []string{"bad_one.go", "bad_two.go", "bad_three.go"} {
		row := ledgerFor(t, f, path)
		assert.Equal(t, 1, row.Attempts, "a first rejection is one attempt, not zero and not the batch size")
		assert.Equal(t, chunkstore.RejectionPending, row.State, "one attempt is well inside the budget, so the path must still be retried")
		assert.Equal(t, chunkstore.ChunksAbsent, row.ChunksState,
			"these files were never indexed, so there were no prior chunks to survive the rollback: they are ABSENT from search, not stale")
		assert.Equal(t, "22000", row.SQLState,
			"MEASURED: pgvector raises data_exception (22000) for a NaN coordinate, NOT the 22P02 (invalid_text_representation) this bead's briefing and loam-c94.24's notes both name -- 22P02 is the TEXT input function's code and pgx sends vectors in binary. The first version of this assertion said 22P02 and failed here")
		assert.Equal(t, job.ID, row.JobID, "the row must name the job whose stats counted it -- this is the join the per-file ERROR lines never had")
		assert.Equal(t, refAfterFirst, row.RejectedRef, "the row must name the ref the index now claims but this path does not reflect")
		assert.Zero(t, chunkCountFor(t, f, path))
	}
	assert.Equal(t, 1, chunkCountFor(t, f, "clean_alpha.go"))
	assert.Equal(t, 3, chunkCountFor(t, f, "clean_omega.go"))

	partial := capture.withMessage("ingest committed with rejected files; they are recorded in the rejection ledger and will be retried by the next ingest")
	require.Len(t, partial, 1)
	assert.Equal(t, slog.LevelWarn, partial[0].level)
	assert.Equal(t, []string{"bad_one.go", "bad_three.go", "bad_two.go"}, partial[0].attrs["files"],
		"the WARN must name the FILES, not only the count: before this bead the count was joinable to a job and the filenames were not. The order is the order they were ATTEMPTED, which is the plan's order, which is git diff --name-status's own path sort -- so bad_three precedes bad_two")
	assert.Equal(t, []string{"bad_one.go", "bad_three.go", "bad_two.go"}, partial[0].attrs["absent_chunks"])
	assert.Empty(t, partial[0].attrs["stale_chunks"])

	// The retry. NOTHING is committed between the two runs -- the mirror
	// is untouched, so `git diff ingested_ref..tip` is empty and the
	// planner can name no file at all. Only the ledger can.
	retryLogger, retryCapture := newCapturingLogger()
	retryStats, err := newOrchestratorWithLogger(t, f, realTransactor(), testembed.New(), retryLogger).Run(t.Context(), f.job(ingest.KindIncremental))
	require.NoError(t, err)

	planned := retryCapture.withMessage("planned ingest")
	require.Len(t, planned, 1)
	assert.Equal(t, ingest.KindIncremental, planned[0].attrs["kind"],
		"the retry must NOT escalate to a full rebuild: adding ledgered files to a plan is not an escalation, and a rebuild would prove nothing about the union")
	assert.Equal(t, int64(3), planned[0].attrs["reparse_files"],
		"exactly the three ledgered files are reparsed; the diff itself named zero, which is the defect this number disproves")
	assert.Equal(t, int64(3), planned[0].attrs["retried_files"])

	assert.Zero(t, retryStats.FilesRejected)
	assert.Empty(t, ledgerRows(t, f), "a path whose chunks landed must leave the ledger, or the signal never clears and stops being trusted")
	assert.Equal(t, 1, chunkCountFor(t, f, "bad_one.go"), "the retried file must now have real chunks -- clearing the ledger without writing them would be the same defect with a cleaner table")
	assert.Equal(t, 1, chunkCountFor(t, f, "bad_two.go"))
	assert.Equal(t, 1, chunkCountFor(t, f, "bad_three.go"))
	assert.Positive(t, chunkTextMentioning(t, f, "RejectedFirst"), "and must be SEARCHABLE, which is the outcome the bead is about")
	assert.Equal(t, 1, chunkCountFor(t, f, "clean_alpha.go"), "the untouched survivors must not have been rewritten: a full rebuild would also make the assertions above pass")
	assert.Equal(t, 3, chunkCountFor(t, f, "clean_omega.go"))
	assert.Empty(t, retryCapture.atLevel(slog.LevelWarn), "an ingest that resolved everything must warn about nothing")
}

// TestIngest_RejectedReingestIsStale_ThenAFullRebuildMakesItAbsent proves,
// at the ORCHESTRATOR level, the distinction this bead's design rests on.
// loam-2d44's own note is explicit that both of its integration tests ran
// KindFull, so the stale-survives-rollback behaviour was proved only one
// layer down in internal/ingest/vectors; this bead relies on it, so it is
// proved here instead of inherited.
//
// The two halves are the same file in the same repo, one ingest apart:
//
//  1. RE-INGEST of an already-indexed file. The rejection unwinds to the
//     per-file savepoint, taking the DELETE back with the INSERTs, so the
//     file keeps the chunks it had. It is searchable, at an older commit
//     than the ingested ref claims: STALE.
//  2. FULL REBUILD of the same file. applyDrops issues a repo-scoped
//     DropRepoBranch BEFORE the write phase and OUTSIDE every savepoint,
//     so nothing unwinds that delete. The same rejection now leaves
//     nothing behind at all: ABSENT.
//
// That second half is why a "degraded flag cleared only by a full rebuild"
// was rejected in favour of this ledger. KindFull is both the operation
// that would clear such a flag and the operation that makes the damage
// worse, and a clearing mechanism whose failure mode is data loss is a
// poor clearing mechanism. Here the escalation is not exotic: it is what a
// grammar or pipeline version bump does to every enrolled repo.
func TestIngest_RejectedReingestIsStale_ThenAFullRebuildMakesItAbsent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commit(t, "first, all clean", map[string]string{
		"fragile.go": "package fragile\n\nfunc FragileOne() {}\n",
		"stable.go":  "package stable\n\nfunc StableOne() {}\n",
	})
	_, err := newOrchestratorFor(t, f, realTransactor()).Run(t.Context(), f.job(ingest.KindIncremental))
	require.NoError(t, err)
	require.Equal(t, 1, chunkCountFor(t, f, "fragile.go"), "the file must be indexed first, or 'stale' has nothing to be stale about")
	require.Positive(t, chunkTextMentioning(t, f, "FragileOne"))

	// Edit it, so this really is a re-ingest of an indexed file rather
	// than a first ingest with extra steps -- the second commit puts
	// fragile.go in the diff on its own merits.
	f.commit(t, "edit the fragile file", map[string]string{
		"fragile.go": "package fragile\n\nfunc FragileTwo() {}\n",
	})
	poisoning := nanVectorEmbedder{Embedder: testembed.New(), marker: "Fragile"}
	stats, err := newOrchestratorWithEmbedder(t, f, realTransactor(), poisoning).Run(t.Context(), f.job(ingest.KindIncremental))
	require.NoError(t, err)
	require.Equal(t, 1, stats.FilesRejected)

	assert.Equal(t, chunkstore.ChunksStale, ledgerFor(t, f, "fragile.go").ChunksState,
		"a re-ingest's rejection unwinds to the savepoint, so the file's PRIOR chunks survive: it is searchable but stale")
	assert.Equal(t, 1, chunkCountFor(t, f, "fragile.go"),
		"and the row count proves it: the delete went back with the inserts")
	assert.Positive(t, chunkTextMentioning(t, f, "FragileOne"),
		"the OLD content is still searchable -- that is exactly what makes this stale rather than absent, and what makes it dangerous")
	assert.Zero(t, chunkTextMentioning(t, f, "FragileTwo"), "the new content never landed")

	// Now the same rejection under a full rebuild. Nothing about the file
	// changes; only the plan's kind does.
	fullStats, err := newOrchestratorWithEmbedder(t, f, realTransactor(), poisoning).Run(t.Context(), f.job(ingest.KindFull))
	require.NoError(t, err)
	require.Equal(t, 1, fullStats.FilesRejected)

	row := ledgerFor(t, f, "fragile.go")
	assert.Equal(t, chunkstore.ChunksAbsent, row.ChunksState,
		"a full rebuild's repo-scoped drop ran before the write phase and outside the savepoints, so there was nothing left to survive the rejection")
	assert.Zero(t, chunkCountFor(t, f, "fragile.go"),
		"the prior chunks are gone and no new ones landed: the full rebuild converted a stale file into an absent one")
	assert.Zero(t, chunkTextMentioning(t, f, "FragileOne"), "the stale content an operator was at least still getting is now gone too")
	assert.Equal(t, 1, row.Attempts,
		"a full rebuild empties the ledger and re-records what rejects, so the attempt budget restarts -- deliberate: the ceiling exists to spare INCREMENTAL ingests work a rebuild does anyway")
	assert.Positive(t, chunkCountFor(t, f, "stable.go"), "the healthy file must have been rebuilt normally")
}

// TestIngest_RepeatedRejectionExhaustsItsBudgetAndStopsBeingRetried is the
// bound. Without it the retry is a permanent tax: a file that is malformed
// for a reason no retry can fix would be re-read, re-chunked, re-embedded
// (a network round trip) and re-rejected on every ingest of that repo
// forever.
//
// Every run below commits NOTHING, so each ingest reaches the file only
// through the ledger. That makes "retried" and "not retried" directly
// observable as the planner's own reparse count -- 1 while the budget
// lasts, 0 once it is spent -- rather than something inferred from a row
// that did not change.
//
// The attempt numbers are asserted exactly (1, 2, 3), never "increasing":
// an off-by-one that exhausted a file on its first rejection and one that
// never exhausted it at all would both satisfy a monotonicity check.
func TestIngest_RepeatedRejectionExhaustsItsBudgetAndStopsBeingRetried(t *testing.T) {
	t.Parallel()
	require.Equal(t, 3, chunkstore.MaxRejectionAttempts,
		"this test walks the budget to its exact ceiling; if the constant moves, the walk below has to move with it")
	f := newFixture(t)
	f.commit(t, "one permanently bad file", map[string]string{
		"hopeless.go": "package hopeless\n\nfunc HopelessSymbol() {}\n",
		"fine.go":     "package fine\n\nfunc FineSymbol() {}\n",
	})
	poisoning := nanVectorEmbedder{Embedder: testembed.New(), marker: "Hopeless"}

	for attempt := 1; attempt <= chunkstore.MaxRejectionAttempts; attempt++ {
		logger, capture := newCapturingLogger()
		stats, err := newOrchestratorWithLogger(t, f, realTransactor(), poisoning, logger).Run(t.Context(), f.job(ingest.KindIncremental))
		require.NoError(t, err, "attempt %d", attempt)
		require.Equal(t, 1, stats.FilesRejected, "attempt %d must reach the file and be rejected by it", attempt)

		row := ledgerFor(t, f, "hopeless.go")
		assert.Equal(t, attempt, row.Attempts, "the attempt count must be exactly the number of rejections so far")
		if attempt < chunkstore.MaxRejectionAttempts {
			assert.Equal(t, chunkstore.RejectionPending, row.State, "still inside the budget after %d attempt(s)", attempt)
		} else {
			assert.Equal(t, chunkstore.RejectionExhausted, row.State, "the ceiling is reached AT MaxRejectionAttempts, not one past it")
		}
		planned := capture.withMessage("planned ingest")
		require.Len(t, planned, 1)
		if attempt > 1 {
			assert.Equal(t, int64(1), planned[0].attrs["retried_files"],
				"attempt %d reaches the file only through the ledger: nothing was committed, so the diff names nothing", attempt)
		}
	}

	// One more ingest, still with nothing committed. The path is
	// exhausted, so it must not be planned at all.
	logger, capture := newCapturingLogger()
	before := ledgerFor(t, f, "hopeless.go")
	stats, err := newOrchestratorWithLogger(t, f, realTransactor(), poisoning, logger).Run(t.Context(), f.job(ingest.KindIncremental))
	require.NoError(t, err)

	assert.Zero(t, stats.FilesRejected, "an exhausted path is not attempted, so it cannot be rejected again")
	planned := capture.withMessage("planned ingest")
	require.Len(t, planned, 1)
	assert.Equal(t, int64(0), planned[0].attrs["retried_files"], "the exhausted path must not be unioned into the plan")
	assert.Equal(t, int64(0), planned[0].attrs["reparse_files"], "and nothing else is in the diff, so the plan is genuinely empty")
	assert.Equal(t, int64(1), planned[0].attrs["ledgered_files"], "the row stays: it is the only durable record that this file's chunks are missing")

	after := ledgerFor(t, f, "hopeless.go")
	assert.Equal(t, chunkstore.MaxRejectionAttempts, after.Attempts, "the count must stop climbing once the budget is spent")
	assert.Equal(t, before.LastRejectedAt, after.LastRejectedAt, "and the row must not be rewritten by an ingest that never touched the file")
	assert.Equal(t, chunkstore.RejectionExhausted, after.State)

	exhausted := capture.withMessage("files have exhausted their chunk-write attempts and are no longer retried automatically; their chunks stay stale or absent until the file changes or a full rebuild runs")
	require.Len(t, exhausted, 1, "an exhausted path is by definition not in any diff, so this recurring WARN is the only surface it has left")
	assert.Equal(t, slog.LevelWarn, exhausted[0].level)
	assert.Equal(t, []string{"hopeless.go"}, exhausted[0].attrs["files"])

	assert.Positive(t, chunkCountFor(t, f, "fine.go"), "the healthy file must have been indexed by the first ingest and left alone since")
}

// TestIngest_ExhaustedPathIsStillRetriedWhenTheFileItselfChanges is the
// other half of the bound, and the operator remedy that costs nothing.
// Exhausted stops the AUTOMATIC retry only; a real edit puts the path in
// `git diff ingested_ref..tip` on its own merits, where the ledger's
// opinion is irrelevant. If it were not so, one exhausted file could never
// be fixed by fixing it.
func TestIngest_ExhaustedPathIsStillRetriedWhenTheFileItselfChanges(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commit(t, "a bad file", map[string]string{"hopeless.go": "package hopeless\n\nfunc HopelessSymbol() {}\n"})
	poisoning := nanVectorEmbedder{Embedder: testembed.New(), marker: "Hopeless"}
	for range chunkstore.MaxRejectionAttempts {
		_, err := newOrchestratorWithEmbedder(t, f, realTransactor(), poisoning).Run(t.Context(), f.job(ingest.KindIncremental))
		require.NoError(t, err)
	}
	require.Equal(t, chunkstore.RejectionExhausted, ledgerFor(t, f, "hopeless.go").State)

	f.commit(t, "fix it", map[string]string{"hopeless.go": "package hopeless\n\nfunc RepairedSymbol() {}\n"})
	stats, err := newOrchestratorWithEmbedder(t, f, realTransactor(), poisoning).Run(t.Context(), f.job(ingest.KindIncremental))
	require.NoError(t, err)

	assert.Zero(t, stats.FilesRejected, "the edited content no longer trips the embedder, so the write lands")
	assert.Empty(t, ledgerRows(t, f), "a successful write clears the row regardless of how exhausted it was")
	assert.Positive(t, chunkCountFor(t, f, "hopeless.go"))
	assert.Positive(t, chunkTextMentioning(t, f, "RepairedSymbol"))
}

// TestIngest_DeletingALedgeredFileClearsIt. Nothing is owed for a file
// that no longer exists, and a row that outlived its file would be
// permanently true and permanently useless -- and would be unioned into
// every future plan, asking the mirror for a blob that is not there.
func TestIngest_DeletingALedgeredFileClearsIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commit(t, "poisoned", map[string]string{
		"doomed.go": "package doomed\n\nfunc DoomedSymbol() {}\n",
		"kept.go":   "package kept\n\nfunc KeptSymbol() {}\n",
	})
	poisoning := nanVectorEmbedder{Embedder: testembed.New(), marker: "Doomed"}
	_, err := newOrchestratorWithEmbedder(t, f, realTransactor(), poisoning).Run(t.Context(), f.job(ingest.KindIncremental))
	require.NoError(t, err)
	require.Equal(t, []string{"doomed.go"}, ledgerPaths(t, f))

	f.commit(t, "delete the doomed file", map[string]string{
		"kept.go": "package kept\n\nfunc KeptSymbol() {}\n",
	}, "doomed.go")
	_, err = newOrchestratorWithEmbedder(t, f, realTransactor(), poisoning).Run(t.Context(), f.job(ingest.KindIncremental))
	require.NoError(t, err)

	assert.Empty(t, ledgerRows(t, f), "the file is gone, so nothing about it is missing any more")
	assert.Zero(t, chunkCountFor(t, f, "doomed.go"))
	assert.Positive(t, chunkCountFor(t, f, "kept.go"))
}

// TestIngest_CleanIngestWritesNoLedgerRows is the control every test above
// is worthless without. A ledger that gained a row on a healthy ingest
// would report every repo permanently broken, and every assertion about
// "the ledger names the rejected files" would still pass.
func TestIngest_CleanIngestWritesNoLedgerRows(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commit(t, "nothing poisoned", map[string]string{
		"alpha.go": "package alpha\n\nfunc AlphaOnly() {}\n",
		"omega.go": "package omega\n\nfunc OmegaOnly() { AlphaOnly() }\n",
	})
	logger, capture := newCapturingLogger()

	stats, err := newOrchestratorWithLogger(t, f, realTransactor(), testembed.New(), logger).Run(t.Context(), f.job(ingest.KindIncremental))
	require.NoError(t, err)

	assert.Zero(t, stats.FilesRejected)
	assert.Positive(t, stats.ChunksEmbedded, "a clean ingest of two real files must have written chunks, or 'no rejections' is vacuous")
	assert.Empty(t, ledgerRows(t, f))
	assert.Empty(t, capture.atLevel(slog.LevelWarn), "no rejection, no exhaustion, and therefore no warning of any kind")
}
