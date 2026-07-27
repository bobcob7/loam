// Package orchestrator implements loam-c94.12: the ingest pipeline the
// worker pool (internal/ingest.Pool) invokes once per claimed job, and the
// point where the two halves of loam-c94's dependency graph -- the
// parse->graph track (internal/ingest/graph) and the chunk->embed track
// (internal/ingest/chunker + internal/ingest/vectors) -- rejoin.
//
// # The atomic swap
//
// Every drop, insert, and edge recompute one ingest performs lands in a
// SINGLE Postgres transaction, committed once at the end
// (docs/ingestion-spec.md "Consistency & Failure": "Each ingest is one
// transaction ... readers see the prior index until it commits"). There is
// no table rename and no shadow schema: the atomicity IS that one commit,
// and the isolation readers get is plain MVCC. A reader on any other
// connection therefore observes exactly two states -- the previous index,
// then the new one -- and never an intermediate. A failure anywhere in the
// pipeline rolls the whole thing back, leaving the previous index intact
// and repo_target_branches.ingested_ref unchanged, so the next attempt
// re-plans from the same base.
//
// # Compute first, then a short write phase
//
// A pgx.Tx is not goroutine-safe, and the transaction must not be held
// open across slow work. Run therefore has two strictly ordered phases:
//
//  1. COMPUTE, with no transaction open. Read the plan's file contents out
//     of the bare mirror, then run both tracks CONCURRENTLY (errgroup):
//     Tree-sitter parsing and symbol/reference extraction on one side
//     (graph.Extractor.ExtractFiles), chunking plus embedding over HTTP on
//     the other (chunker.ChunkFiles then vectors.Indexer.Prepare). Neither
//     touches the database.
//  2. WRITE, inside one transaction, entirely sequential on this
//     goroutine. Nothing in this phase parses, embeds, or makes a network
//     call.
//
// Both halves of the split -- graph.ExtractFiles/PersistFiles and
// vectors.Prepare/Persist -- exist for this package. Calling the combined
// graph.IngestFiles / vectors.IngestFileChunks with a transaction-bound
// store would work, but would pin all of the parsing and every Ollama
// round trip inside the open transaction and would make the two tracks
// unable to run concurrently at all.
//
// # Write order inside the transaction
//
// Fixed, and load-bearing:
//
//  1. The plan's drops. For KindFull, one repo-scoped drop of every
//     derived row for (repo, target branch) -- codegraph.DropRepoBranch
//     and chunkstore.DropRepoBranch. For KindIncremental, a per-file drop
//     of every path in Plan.DropFiles (deleted files and a rename's old
//     path) across symbols, symbol_references and chunks; symbol_history
//     and graph_edges both reference symbols (id) ON DELETE CASCADE and go
//     with them.
//  2. The reparse files' symbols and references, per file, drop-then-
//     insert (codegraph.Store.ReplaceFileSymbols / ReplaceFileReferences
//     already are that pair).
//  3. graph_edges, recomputed for the WHOLE repo+branch by joining
//     symbol_references to symbols -- necessarily after step 2, since an
//     unchanged file's reference must resolve against a symbol a changed
//     file has only just redefined.
//  4. The reparse files' chunks, per file, drop-then-insert.
//  5. repo_target_branches.ingested_ref/ingested_at/ingested_versions
//     (this bead's NOTES: the write-back belongs inside the ingest
//     transaction, so the recorded diff base can never disagree with the
//     index it describes).
//
// Step 1 before steps 2/4 is what makes a full rebuild correct: the drop
// is repo-scoped precisely because a full rebuild happens when there is no
// usable diff, so rows may exist for files the new tree does not contain
// and no per-file loop over the new tree could ever name them. Reversing
// the order would drop the rebuild it had just written.
//
// # Symbol history (loam-c94.7) is not implemented here
//
// loam-c94.7 (symbol history via `git log -L`) is still open and is not a
// dependency of this bead. The seam for it is already shaped: it is a
// third compute track in Run's errgroup producing
// []codegraph.HistoryEntryInput, and one more call in writeSwap between
// persistGraph and persistVectors (symbol_history rows carry a symbol_id
// FK, so they can only be written after the symbols they reference land,
// which is what fixes their position). Adding it changes the historyTrack
// seam and those two call sites; it does not restructure Run.
package orchestrator

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bobcob7/loam/internal/diffplan"
	"github.com/bobcob7/loam/internal/ingest/chunker"
	"github.com/bobcob7/loam/internal/ingest/graph"
	"github.com/bobcob7/loam/internal/ingest/vectors"
	"github.com/bobcob7/loam/internal/reposstore"
)

//go:generate go tool moq -out moq_test.go . planner repoReader contentReader graphTrack fileChunker vectorTrack dropper refWriter transactor embedderInfo

// planner is internal/diffplan.Planner's one method: the full-vs-
// incremental decision, including every full-rebuild escalation trigger
// (first ingest, version bump, no valid diff base via git merge-base, an
// unparseable diff record, too many changed files). This package never
// re-derives any of that -- it calls Plan and follows the Plan it gets
// back, including its finalized Kind.
type planner interface {
	Plan(ctx context.Context, mirrorDir string, req diffplan.Request) (diffplan.Plan, error)
}

// repoReader is the read side of internal/reposstore this package needs
// before it opens its transaction: the repo's name (which is what
// mirrorpath.Dir turns into a bare-mirror path) and the target branch's
// recorded diff base and versions, which are the planner's inputs. Both
// reads happen OUTSIDE the ingest transaction, against the pool.
type repoReader interface {
	GetRepoByID(ctx context.Context, id uuid.UUID) (reposstore.Repo, error)
	ListTargetBranches(ctx context.Context, repoID uuid.UUID) ([]reposstore.TargetBranch, error)
}

// contentReader reads the bare mirror. ResolveRef turns a target branch
// name into the commit the mirror currently has for it -- the new_ref the
// whole ingest is relative to -- and ReadFiles reads the blob content of a
// set of paths at that commit.
//
// ReadFiles returns one entry per path it could read, in the order given,
// SKIPPING any path that does not resolve to a blob at ref (a submodule
// gitlink, or a path that vanished between the plan and the read). It is
// not an error: the plan is a snapshot and the mirror is live.
type contentReader interface {
	ResolveRef(ctx context.Context, mirrorDir, branch string) (string, error)
	ReadFiles(ctx context.Context, mirrorDir, ref string, paths []string) ([]File, error)
}

// File is one file's content at a ref, as contentReader.ReadFiles returns
// it and as both compute tracks consume it.
type File struct {
	Path    string
	Content []byte
}

// graphTrack is the parse->graph track, split into its compute and write
// halves so the parsing happens before the transaction opens (see the
// package doc comment). Extract is pure compute; Persist takes the
// transaction and does every symbols/symbol_references write plus the
// whole-repo graph_edges recompute.
//
// Persist takes a pgx.Tx rather than internal/ingest/graph's own store
// seam because that seam is an unexported interface in that package and
// cannot be named from here; the production adapter binds a
// codegraph.Store to the transaction and calls through.
type graphTrack interface {
	Extract(ctx context.Context, files []graph.FileInput) (graph.Extracted, graph.Stats, error)
	Persist(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string, ex graph.Extracted) (graph.Stats, error)
}

// fileChunker is internal/ingest/chunker's batch entry point: pure compute
// over file bytes, no database and no network. Every input file gets an
// output entry, including one that turned out to be binary or unchunkable
// (loam-8uo) -- that entry carries zero units, which is what makes
// vectorTrack drop the file's stale chunks.
type fileChunker interface {
	ChunkFiles(ctx context.Context, files []chunker.FileInput, budgeter chunker.Budgeter) ([]chunker.FileChunks, chunker.Stats, error)
}

// vectorTrack is the chunk->embed track's tail, split the same way
// graphTrack is: Prepare does every Embed call (the slow, network-bound
// part) with no transaction open, Persist writes the already-embedded
// result into the transaction.
type vectorTrack interface {
	Prepare(ctx context.Context, repoID uuid.UUID, targetBranch string, files []chunker.FileChunks) (vectors.Prepared, vectors.Stats, error)
	Persist(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string, p vectors.Prepared) (vectors.Stats, error)
}

// dropper applies the plan's drops inside the transaction. DropRepoBranch
// is the full-rebuild path (every derived row for the repo+branch);
// DropPaths is the incremental path (one named file at a time, across
// symbols, symbol_references and chunks). Both are separate from the
// per-file drop-then-insert the reparse files get, which the stores
// already do as part of their replace calls.
type dropper interface {
	DropRepoBranch(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string) error
	DropPaths(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, targetBranch string, paths []string) error
}

// refWriter records the ingest's own outcome on repo_target_branches,
// inside the same transaction as the index it describes (this bead's
// NOTES). A separate seam from repoReader because this one is transaction-
// bound and that one is not.
type refWriter interface {
	AdvanceIngestedRef(ctx context.Context, tx pgx.Tx, repoID uuid.UUID, branch, ref string, ingestedAt time.Time, versions []byte) error
}

// transactor runs fn inside one transaction, committing if and only if fn
// returns nil and rolling back otherwise -- the same shape (and the same
// unit-testability rationale) as internal/chunkstore's transactor of the
// same name, rather than exposing Begin/Commit/Rollback and requiring
// tests to fake pgx.Tx's full surface.
//
// A panic inside fn is deliberately not recovered here: it unwinds past
// the deferred rollback in the production implementation, which RUNS it,
// so the transaction is closed rather than left dangling. Recovering at
// the per-JOB boundary so one poisoned repo cannot kill the server process
// is a different concern, and a different open bead (loam-337).
type transactor interface {
	withinTx(ctx context.Context, fn func(tx pgx.Tx) error) error
}

// embedderInfo is the two facts this package needs from the embedding
// backend, neither of which involves embedding anything: the token budget
// the chunker must keep every unit under, and the model identity that goes
// into the version triple a model change is detected by. Declared here at
// the consumer rather than importing internal/ingest/embed.Embedder whole,
// so this package cannot accidentally start calling Embed itself -- that
// belongs to internal/ingest/vectors.
//
// Any embedderInfo is also a valid chunker.Budgeter (Go interface
// satisfaction is structural and ContextWindow is a superset member), so
// it is passed straight to ChunkFiles with no adapter.
type embedderInfo interface {
	ContextWindow() int
	ModelID() string
}
