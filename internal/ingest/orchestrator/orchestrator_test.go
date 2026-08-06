package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/chunkstore"
	"github.com/bobcob7/loam/internal/diffplan"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/ingest/chunker"
	"github.com/bobcob7/loam/internal/ingest/graph"
	"github.com/bobcob7/loam/internal/ingest/vectors"
	"github.com/bobcob7/loam/internal/reposstore"
)

const (
	testBranch   = "main"
	testRepoName = "acme/widgets"
	testNewRef   = "cafebabecafebabecafebabecafebabecafebabe"
	testOldRef   = "0123456789012345678901234567890123456789"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// callLog records, in order, every collaborator call the harness below
// observes. It is the only way this package's tests can assert on ORDER
// -- which is the whole subject of this bead: heavy compute before the
// transaction opens, drops before inserts, edges after symbols, commit
// last. A plain "was it called" mock cannot discriminate any of those.
//
// It is mutex-guarded because the compute phase genuinely runs its two
// tracks on separate goroutines; without the lock, `go test -race` on this
// package's own harness would report a data race that has nothing to do
// with the code under test.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) record(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, name)
}

func (l *callLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

// indexOf returns the position of name in the log, or -1. Callers assert
// on relative positions, never on absolute ones, so adding a step to the
// pipeline does not invalidate an ordering assertion that is still true.
func (l *callLog) indexOf(name string) int {
	for i, c := range l.snapshot() {
		if c == name {
			return i
		}
	}
	return -1
}

// harness is a fully-wired Orchestrator over moq mocks, with every mock
// method configured (this codebase's rule: an unconfigured moq method
// panics, and a test that dies on a panic has not run its assertions) and
// every call recorded into log.
type harness struct {
	orch     *Orchestrator
	log      *callLog
	planner  *plannerMock
	repos    *repoReaderMock
	content  *contentReaderMock
	graph    *graphTrackMock
	chunker  *fileChunkerMock
	vectors  *vectorTrackMock
	dropper  *dropperMock
	refs     *refWriterMock
	ledger   *rejectionLedgerMock
	tx       *transactorMock
	repoID   uuid.UUID
	commits  int
	rollback int
}

// newHarness builds the happy path: an incremental plan dropping one file
// and reparsing one, a transactor that runs fn and "commits" when fn
// returns nil, and stub compute results with distinguishable counts.
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{log: &callLog{}, repoID: uuid.Must(uuid.NewV7())}
	h.planner = &plannerMock{
		PlanFunc: func(ctx context.Context, mirrorDir string, req diffplan.Request) (diffplan.Plan, error) {
			h.log.record("plan")
			return diffplan.Plan{
				Kind:         ingest.KindIncremental,
				DropFiles:    []string{"gone.go"},
				ReparseFiles: []string{"kept.go"},
			}, nil
		},
	}
	h.repos = &repoReaderMock{
		GetRepoByIDFunc: func(ctx context.Context, id uuid.UUID) (reposstore.Repo, error) {
			h.log.record("getRepo")
			return reposstore.Repo{ID: id, Name: testRepoName, IndexedBranch: testBranch}, nil
		},
		ListTargetBranchesFunc: func(ctx context.Context, repoID uuid.UUID) ([]reposstore.TargetBranch, error) {
			h.log.record("listTargetBranches")
			return []reposstore.TargetBranch{{
				RepoID:      repoID,
				Branch:      testBranch,
				IngestedRef: reposstore.IngestedRef{Ref: testOldRef, Ok: true},
			}}, nil
		},
	}
	h.content = &contentReaderMock{
		ResolveRefFunc: func(ctx context.Context, mirrorDir, branch string) (string, error) {
			h.log.record("resolveRef")
			return testNewRef, nil
		},
		ReadFilesFunc: func(ctx context.Context, mirrorDir, ref string, paths []string) ([]File, error) {
			h.log.record("readFiles")
			out := make([]File, len(paths))
			for i, p := range paths {
				out[i] = File{Path: p, Content: []byte("package a\n\nfunc A() {}\n")}
			}
			return out, nil
		},
	}
	h.graph = &graphTrackMock{
		ExtractFunc: func(ctx context.Context, files []graph.FileInput) (graph.Extracted, graph.Stats, error) {
			h.log.record("graph.Extract")
			return graph.Extracted{}, graph.Stats{FilesExtracted: len(files), SymbolsWritten: 7}, nil
		},
		PersistFunc: func(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string, ex graph.Extracted) (graph.Stats, error) {
			h.log.record("graph.Persist")
			return graph.Stats{EdgesRecomputed: 11}, nil
		},
	}
	h.chunker = &fileChunkerMock{
		ChunkFilesFunc: func(ctx context.Context, files []chunker.FileInput, budgeter chunker.Budgeter) ([]chunker.FileChunks, chunker.Stats, error) {
			h.log.record("chunkFiles")
			out := make([]chunker.FileChunks, len(files))
			for i, f := range files {
				out[i] = chunker.FileChunks{Path: f.Path}
			}
			return out, chunker.Stats{FilesChunked: len(files)}, nil
		},
	}
	h.vectors = &vectorTrackMock{
		PrepareFunc: func(ctx context.Context, repoID uuid.UUID, targetBranch string, files []chunker.FileChunks) (vectors.Prepared, vectors.Stats, error) {
			h.log.record("vectors.Prepare")
			return vectors.Prepared{}, vectors.Stats{EmbedCalls: 2}, nil
		},
		PersistFunc: func(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string, p vectors.Prepared) (vectors.Stats, error) {
			h.log.record("vectors.Persist")
			return vectors.Stats{FilesReplaced: 1, ChunksWritten: 5}, nil
		},
	}
	h.dropper = &dropperMock{
		DropPathsFunc: func(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string, paths []string) error {
			h.log.record("dropPaths")
			return nil
		},
		DropRepoBranchFunc: func(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string) error {
			h.log.record("dropRepoBranch")
			return nil
		},
	}
	h.refs = &refWriterMock{
		AdvanceIngestedRefFunc: func(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, branch, ref string, ingestedAt time.Time, versions []byte) error {
			h.log.record("advanceIngestedRef")
			return nil
		},
	}
	// The ledger's default is the healthy shape: no outstanding
	// rejections, so List returns nothing and no write is expected. Tests
	// that care override the funcs they need.
	h.ledger = &rejectionLedgerMock{
		ListFunc: func(ctx context.Context, repoID uuid.UUID, targetBranch string) ([]chunkstore.Rejection, error) {
			h.log.record("ledger.List")
			return nil, nil
		},
		ClearFunc: func(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string, paths []string) error {
			h.log.record("ledger.Clear")
			return nil
		},
		ClearAllFunc: func(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string) error {
			h.log.record("ledger.ClearAll")
			return nil
		},
		RecordFunc: func(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string, in chunkstore.RejectionInput) error {
			h.log.record("ledger.Record")
			return nil
		},
	}
	h.tx = &transactorMock{
		withinTxFunc: func(ctx context.Context, fn func(tx pgx.Tx) error) error {
			h.log.record("beginTx")
			if err := fn(nil); err != nil {
				h.rollback++
				h.log.record("rollback")
				return err
			}
			h.commits++
			h.log.record("commit")
			return nil
		},
	}
	h.orch = newOrchestrator(
		testLogger(), t.TempDir(), h.planner, h.repos, h.content, h.graph, h.chunker,
		h.vectors, h.dropper, h.refs, h.ledger, h.tx, fixedBudgeter(2048),
		diffplan.Versions{Grammar: GrammarVersion, Pipeline: PipelineVersion, EmbeddingModel: "test-model"},
	)
	return h
}

func (h *harness) job(kind ingest.Kind) ingest.Job {
	return ingest.Job{ID: uuid.Must(uuid.NewV7()), RepoID: h.repoID, TargetBranch: testBranch, Kind: kind}
}

// fixedBudgeter is a chunker.Budgeter reporting a constant token budget.
type fixedBudgeter int

func (b fixedBudgeter) ContextWindow() int { return int(b) }

// TestRun_AllHeavyComputeHappensBeforeTheTransactionOpens is THE
// transaction-boundary test. The bead's DESIGN forbids holding the ingest
// transaction open across Tree-sitter parsing and Ollama round trips, and
// the only observable form of that rule is ordering: every compute call
// must appear in the log strictly before "beginTx".
//
// Asserting positions rather than mere presence is what makes this
// falsifiable -- moving any compute step inside the withinTx callback
// leaves every call still made, and only its POSITION changes.
func TestRun_AllHeavyComputeHappensBeforeTheTransactionOpens(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)
	begin := h.log.indexOf("beginTx")
	require.NotEqual(t, -1, begin, "the orchestrator must open a transaction")
	for _, compute := range []string{"getRepo", "listTargetBranches", "resolveRef", "plan", "readFiles", "graph.Extract", "chunkFiles", "vectors.Prepare"} {
		at := h.log.indexOf(compute)
		require.NotEqual(t, -1, at, "%s must run", compute)
		assert.Less(t, at, begin, "%s is compute or a pre-read: it must finish BEFORE the ingest transaction opens, never inside it", compute)
	}
}

// TestRun_EveryWriteHappensInsideTheOneTransactionAndCommitsOnce proves
// the other half of the boundary: no write escapes the transaction, and
// there is exactly ONE commit for the whole ingest -- the atomic swap.
func TestRun_EveryWriteHappensInsideTheOneTransactionAndCommitsOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)
	begin := h.log.indexOf("beginTx")
	commit := h.log.indexOf("commit")
	require.NotEqual(t, -1, commit, "a successful ingest must commit")
	for _, write := range []string{"dropPaths", "graph.Persist", "vectors.Persist", "advanceIngestedRef"} {
		at := h.log.indexOf(write)
		require.NotEqual(t, -1, at, "%s must run", write)
		assert.Greater(t, at, begin, "%s is a write: it must happen inside the transaction", write)
		assert.Less(t, at, commit, "%s must be staged before the single commit, not after it", write)
	}
	assert.Equal(t, 1, h.commits, "an ingest is ONE transaction: exactly one commit, never one per file or per track")
	assert.Zero(t, h.rollback)
}

// TestRun_IncrementalDropsHappenBeforeAnyInsert pins the write ORDER the
// bead names: the plan's drops first, then the reparse files' inserts.
func TestRun_IncrementalDropsHappenBeforeAnyInsert(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)
	drop := h.log.indexOf("dropPaths")
	require.NotEqual(t, -1, drop, "an incremental plan with DropFiles must drop them")
	assert.Less(t, drop, h.log.indexOf("graph.Persist"), "the plan's drops must precede the symbol/reference inserts")
	assert.Less(t, drop, h.log.indexOf("vectors.Persist"), "the plan's drops must precede the chunk inserts")
	assert.Equal(t, -1, h.log.indexOf("dropRepoBranch"), "an incremental ingest must never repo-scope-drop the whole branch")
}

// TestRun_FullRebuildDropsTheWholeRepoBranchBeforeReinserting is the
// full-rebuild half of the same ordering rule, and it is the one with
// teeth: a repo-scoped drop issued AFTER the rebuild's inserts would
// delete the index it had just written and commit an empty one.
func TestRun_FullRebuildDropsTheWholeRepoBranchBeforeReinserting(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.planner.PlanFunc = func(ctx context.Context, mirrorDir string, req diffplan.Request) (diffplan.Plan, error) {
		h.log.record("plan")
		return diffplan.Plan{Kind: ingest.KindFull, Reason: "first ingest", ReparseFiles: []string{"a.go", "b.go"}}, nil
	}
	_, err := h.orch.Run(t.Context(), h.job(ingest.KindFull))
	require.NoError(t, err)
	drop := h.log.indexOf("dropRepoBranch")
	require.NotEqual(t, -1, drop, "a full rebuild must drop every derived row for the repo+branch")
	assert.Less(t, drop, h.log.indexOf("graph.Persist"), "the repo-scoped drop must precede the rebuild's symbol inserts, or it deletes them")
	assert.Less(t, drop, h.log.indexOf("vectors.Persist"), "the repo-scoped drop must precede the rebuild's chunk inserts, or it deletes them")
	assert.Equal(t, -1, h.log.indexOf("dropPaths"), "a full plan carries no DropFiles: the drop is repo-scoped, not per-file")
}

// TestRun_GraphPersistPrecedesChunkPersist pins the remaining intra-
// transaction ordering: symbols and the graph_edges recompute that depends
// on them come before the chunk writes. graph.Persist is what runs the
// whole-repo edge recompute internally, so its position IS the "edges
// after symbols" guarantee at this level.
func TestRun_GraphPersistPrecedesChunkPersist(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)
	assert.Less(t, h.log.indexOf("graph.Persist"), h.log.indexOf("vectors.Persist"),
		"symbols and the edge recompute land before chunks, which is where loam-c94.7's symbol_history write slots in between them")
}

// TestRun_IngestedRefIsRecordedInsideTheSameTransaction proves the bead's
// NOTES correction: the write-back onto repo_target_branches is part of
// the swap, not a follow-up statement after it. A recorded diff base that
// committed separately from the index it describes could disagree with it
// after a crash, and the next incremental would diff from a base the index
// does not actually reflect.
func TestRun_IngestedRefIsRecordedInsideTheSameTransaction(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var gotRef, gotBranch string
	var gotVersions []byte
	h.refs.AdvanceIngestedRefFunc = func(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, branch, ref string, ingestedAt time.Time, versions []byte) error {
		h.log.record("advanceIngestedRef")
		gotRef, gotBranch, gotVersions = ref, branch, versions
		return nil
	}
	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)
	at := h.log.indexOf("advanceIngestedRef")
	require.NotEqual(t, -1, at)
	assert.Greater(t, at, h.log.indexOf("beginTx"))
	assert.Less(t, at, h.log.indexOf("commit"))
	assert.Equal(t, testNewRef, gotRef, "the recorded diff base must be the ref this ingest actually built from")
	assert.Equal(t, testBranch, gotBranch)
	var recorded diffplan.Versions
	require.NoError(t, json.Unmarshal(gotVersions, &recorded))
	assert.Equal(t, diffplan.Versions{Grammar: GrammarVersion, Pipeline: PipelineVersion, EmbeddingModel: "test-model"}, recorded,
		"ingested_versions must record the versions this binary built with, so the next ingest can detect a bump")
}

// TestRun_PlanRequestCarriesTheStoredDiffBaseAndVersions proves the
// orchestrator FEEDS the planner rather than second-guessing it: the
// stored ingested_ref becomes OldRef, the resolved mirror tip becomes
// NewRef, the job's kind is passed through unmodified, and the stored
// version triple is handed over for the planner's own mismatch check.
func TestRun_PlanRequestCarriesTheStoredDiffBaseAndVersions(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	stored := diffplan.Versions{Grammar: "old-grammar", Pipeline: "old-pipeline", EmbeddingModel: "old-model"}
	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	h.repos.ListTargetBranchesFunc = func(ctx context.Context, repoID uuid.UUID) ([]reposstore.TargetBranch, error) {
		h.log.record("listTargetBranches")
		return []reposstore.TargetBranch{{
			RepoID:           repoID,
			Branch:           testBranch,
			IngestedRef:      reposstore.IngestedRef{Ref: testOldRef, Ok: true},
			IngestedVersions: raw,
		}}, nil
	}
	var got diffplan.Request
	h.planner.PlanFunc = func(ctx context.Context, mirrorDir string, req diffplan.Request) (diffplan.Plan, error) {
		h.log.record("plan")
		got = req
		return diffplan.Plan{Kind: ingest.KindIncremental, ReparseFiles: []string{"kept.go"}}, nil
	}
	_, err = h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)
	assert.Equal(t, testOldRef, got.OldRef, "OldRef must be the recorded ingested_ref, which is the only valid diff base")
	assert.Equal(t, testNewRef, got.NewRef, "NewRef must be the mirror's live tip for the target branch")
	assert.Equal(t, ingest.KindIncremental, got.RequestedKind, "the job's kind is the planner's INPUT; only the planner may escalate it")
	require.NotNil(t, got.StoredVersions)
	assert.Equal(t, stored, *got.StoredVersions, "the stored version triple must reach the planner so it can detect a bump")
}

// TestRun_NeverIngestedBranchSendsAnEmptyOldRefRatherThanInventingOne
// proves the orchestrator does not fabricate a diff base for a branch that
// has never been ingested. diffplan reads an empty OldRef as "first
// ingest" and escalates; passing anything else would ask it to diff
// against a commit no ingest ever built from.
func TestRun_NeverIngestedBranchSendsAnEmptyOldRefRatherThanInventingOne(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.repos.ListTargetBranchesFunc = func(ctx context.Context, repoID uuid.UUID) ([]reposstore.TargetBranch, error) {
		h.log.record("listTargetBranches")
		return []reposstore.TargetBranch{{RepoID: repoID, Branch: testBranch}}, nil
	}
	var got diffplan.Request
	h.planner.PlanFunc = func(ctx context.Context, mirrorDir string, req diffplan.Request) (diffplan.Plan, error) {
		h.log.record("plan")
		got = req
		return diffplan.Plan{Kind: ingest.KindFull, Reason: "first ingest"}, nil
	}
	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)
	assert.Empty(t, got.OldRef, "a never-ingested branch has no diff base: OldRef must be empty, which is diffplan's first-ingest signal")
	assert.Nil(t, got.StoredVersions, "no recorded versions must stay nil, which diffplan treats as a mismatch")
}

// TestRun_FollowsThePlannersEscalatedKindNotTheJobsRequestedKind proves the
// orchestrator obeys diffplan's finalized Kind. The job asks for
// incremental; the planner escalates to full; the write phase must take the
// repo-scoped drop, not the per-file one. Re-deriving the decision here --
// or keying the drop off job.Kind -- would leave a force-pushed repo's
// stale rows in place forever.
func TestRun_FollowsThePlannersEscalatedKindNotTheJobsRequestedKind(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.planner.PlanFunc = func(ctx context.Context, mirrorDir string, req diffplan.Request) (diffplan.Plan, error) {
		h.log.record("plan")
		return diffplan.Plan{Kind: ingest.KindFull, Reason: "no valid diff base", ReparseFiles: []string{"a.go"}}, nil
	}
	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)
	assert.NotEqual(t, -1, h.log.indexOf("dropRepoBranch"), "an escalated plan must take the full-rebuild drop even though the JOB said incremental")
	assert.Equal(t, -1, h.log.indexOf("dropPaths"))
}

// TestRun_ComputeFailureNeverOpensATransaction proves a failure in the
// expensive phase costs zero database work: the transaction is never even
// begun, so there is nothing to roll back and the previous index is
// untouched by construction.
func TestRun_ComputeFailureNeverOpensATransaction(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	boom := errors.New("ollama unreachable")
	h.vectors.PrepareFunc = func(ctx context.Context, repoID uuid.UUID, targetBranch string, files []chunker.FileChunks) (vectors.Prepared, vectors.Stats, error) {
		h.log.record("vectors.Prepare")
		return vectors.Prepared{}, vectors.Stats{}, boom
	}
	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
	assert.Equal(t, -1, h.log.indexOf("beginTx"), "an embed failure must not open a transaction at all")
	assert.Zero(t, h.commits)
}

// TestRun_WriteFailurePartwayThroughRollsBackAndCommitsNothing is the
// failure-atomicity proof at the unit level: a store error after the drops
// and the symbol writes have already been staged must roll back, never
// commit, and never report success to the worker. The staged drops are
// exactly the rows a partial commit would have destroyed.
func TestRun_WriteFailurePartwayThroughRollsBackAndCommitsNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	boom := errors.New("chunk insert exploded")
	h.vectors.PersistFunc = func(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string, p vectors.Prepared) (vectors.Stats, error) {
		h.log.record("vectors.Persist")
		return vectors.Stats{}, boom
	}
	stats, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
	assert.Equal(t, ingest.Stats{}, stats, "a failed ingest must report no stats: the worker records failure, not a partial success")
	assert.Zero(t, h.commits, "nothing may commit once any write in the swap fails")
	assert.Equal(t, 1, h.rollback)
	require.NotEqual(t, -1, h.log.indexOf("dropPaths"), "the drops were already staged, which is precisely why the rollback matters")
	assert.Equal(t, -1, h.log.indexOf("advanceIngestedRef"), "the ingested_ref write-back must not be reached once an earlier write failed")
}

// TestRun_RefWriteBackFailureRollsBackTheWholeIndex covers the last write
// in the sequence: even a failure at the very end -- with every index row
// already staged -- must discard all of it rather than commit an index
// whose recorded diff base never landed.
func TestRun_RefWriteBackFailureRollsBackTheWholeIndex(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	boom := errors.New("advance failed")
	h.refs.AdvanceIngestedRefFunc = func(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, branch, ref string, ingestedAt time.Time, versions []byte) error {
		h.log.record("advanceIngestedRef")
		return boom
	}
	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
	assert.Zero(t, h.commits)
	assert.Equal(t, 1, h.rollback)
}

// TestRun_ReportsFilesParsedAndChunksEmbedded proves the stats the worker
// persists onto ingest_jobs.stats come from the real per-track counters,
// not from a placeholder. The two mocked counts are deliberately different
// numbers so a swapped assignment is visible.
func TestRun_ReportsFilesParsedAndChunksEmbedded(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	stats, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)
	assert.Equal(t, 1, stats.FilesParsed, "FilesParsed is the parse track's extracted-file count")
	assert.Equal(t, 5, stats.ChunksEmbedded, "ChunksEmbedded is the chunk track's written-row count, not its embed-call count")
}

// TestRun_UnenrolledTargetBranchFailsLoudlyBeforeAnyWork proves a job for
// a branch with no repo_target_branches row fails immediately with a
// labeled error, rather than planning against a fabricated base or writing
// an index nothing can record a diff base for.
func TestRun_UnenrolledTargetBranchFailsLoudlyBeforeAnyWork(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.repos.ListTargetBranchesFunc = func(ctx context.Context, repoID uuid.UUID) ([]reposstore.TargetBranch, error) {
		h.log.record("listTargetBranches")
		return []reposstore.TargetBranch{{RepoID: repoID, Branch: "some-other-branch"}}, nil
	}
	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.Error(t, err)
	assert.ErrorIs(t, err, errNoSuchTargetBranch)
	assert.Equal(t, -1, h.log.indexOf("plan"), "a job for an unenrolled branch must not reach the planner")
	assert.Equal(t, -1, h.log.indexOf("beginTx"))
}

// TestRun_UnparseableStoredVersionsForcesAMismatchRatherThanGuessing
// proves a corrupt ingested_versions column degrades to "no recorded
// versions" -- which diffplan escalates to a full rebuild -- instead of
// being partially believed. Guessing here would certify an incremental
// reuse that may not be safe.
func TestRun_UnparseableStoredVersionsForcesAMismatchRatherThanGuessing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.repos.ListTargetBranchesFunc = func(ctx context.Context, repoID uuid.UUID) ([]reposstore.TargetBranch, error) {
		h.log.record("listTargetBranches")
		return []reposstore.TargetBranch{{
			RepoID:           repoID,
			Branch:           testBranch,
			IngestedRef:      reposstore.IngestedRef{Ref: testOldRef, Ok: true},
			IngestedVersions: json.RawMessage(`{"Grammar": `),
		}}, nil
	}
	var got diffplan.Request
	h.planner.PlanFunc = func(ctx context.Context, mirrorDir string, req diffplan.Request) (diffplan.Plan, error) {
		h.log.record("plan")
		got = req
		return diffplan.Plan{Kind: ingest.KindFull}, nil
	}
	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)
	assert.Nil(t, got.StoredVersions, "an unparseable version column must reach the planner as nil, which it treats as a mismatch")
}

// TestRun_BothTracksSeeTheSameFileContent proves the compute phase hands
// identical inputs to both tracks: the graph track and the chunk track
// must index the SAME bytes at the SAME paths, or a symbol and the chunk
// that is supposed to contain it can describe different revisions of a
// file.
func TestRun_BothTracksSeeTheSameFileContent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	content := []byte("package a\n\nfunc Unique() {}\n")
	h.content.ReadFilesFunc = func(ctx context.Context, mirrorDir, ref string, paths []string) ([]File, error) {
		h.log.record("readFiles")
		return []File{{Path: "kept.go", Content: content}}, nil
	}
	var graphInputs []graph.FileInput
	var chunkInputs []chunker.FileInput
	h.graph.ExtractFunc = func(ctx context.Context, files []graph.FileInput) (graph.Extracted, graph.Stats, error) {
		h.log.record("graph.Extract")
		graphInputs = files
		return graph.Extracted{}, graph.Stats{FilesExtracted: len(files)}, nil
	}
	h.chunker.ChunkFilesFunc = func(ctx context.Context, files []chunker.FileInput, budgeter chunker.Budgeter) ([]chunker.FileChunks, chunker.Stats, error) {
		h.log.record("chunkFiles")
		chunkInputs = files
		return nil, chunker.Stats{}, nil
	}
	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)
	require.Len(t, graphInputs, 1)
	require.Len(t, chunkInputs, 1)
	assert.Equal(t, "kept.go", graphInputs[0].Path)
	assert.Equal(t, "kept.go", chunkInputs[0].Path)
	assert.Equal(t, content, graphInputs[0].Content)
	assert.Equal(t, content, chunkInputs[0].Content)
}

// TestRun_ReadsTheReparseSetAtTheResolvedNewRef proves the content read is
// keyed on the ref the ingest actually built from and on the plan's own
// reparse set -- not on the old ref (which would index the previous
// revision and record it as the new one) and not on the whole tree.
func TestRun_ReadsTheReparseSetAtTheResolvedNewRef(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var gotRef string
	var gotPaths []string
	h.content.ReadFilesFunc = func(ctx context.Context, mirrorDir, ref string, paths []string) ([]File, error) {
		h.log.record("readFiles")
		gotRef, gotPaths = ref, paths
		return nil, nil
	}
	_, err := h.orch.Run(t.Context(), h.job(ingest.KindIncremental))
	require.NoError(t, err)
	assert.Equal(t, testNewRef, gotRef, "file content must be read at the new ref, never the diff base")
	assert.Equal(t, []string{"kept.go"}, gotPaths, "only the plan's reparse set is read; dropped files have no content to read")
}
