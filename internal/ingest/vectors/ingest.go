package vectors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"

	"github.com/bobcob7/loam/internal/chunkstore"
	"github.com/bobcob7/loam/internal/ingest/chunk"
	"github.com/bobcob7/loam/internal/ingest/chunker"
	"github.com/bobcob7/loam/internal/ingest/embed/ollama"
)

// maxEmbedBatch caps how many chunk texts go into a single Embed call.
//
// The Embedder interface is a batch API and the production Ollama client
// sends the whole slice as ONE HTTP request (/api/embed's "input" array,
// internal/ingest/embed/ollama), so embedding a full rebuild's chunks in a
// single call would mean one request holding every chunk in the repo --
// tens of megabytes of JSON, and a server-side batch large enough to
// exhaust the model's memory, failing the whole ingest rather than one
// request. Batching in bounded groups keeps the per-request cost flat and
// independent of repo size while still amortizing the round trip the way
// one-call-per-chunk never would.
//
// 64 is a round working figure, not a measured optimum: it is small enough
// that a batch of even p99-sized chunks (~2KB, per internal/ingest/chunk's
// bytesPerTokenBudget doc comment) is a ~128KB request, and large enough
// that a 10,000-chunk repo costs ~157 requests instead of 10,000.
const maxEmbedBatch = 64

var (
	// errVectorCount means the Embedder returned a different number of
	// vectors than the texts it was given, so no chunk can be matched to
	// its vector with any confidence. Persisting anything after that would
	// mean pairing chunk content with some OTHER chunk's embedding -- a
	// corrupt index that looks healthy and misranks silently forever --
	// so this aborts the ingest instead. It is checked at every actual
	// Embed call embedAll or its split-retry path makes (a batch call in
	// embedAll's main loop, or a single-text call in embedWithSplit), each
	// time comparing that ONE call's own request/response pair -- never a
	// separate total computed from two lists assembled independently.
	// groupByFile's defensive recount (see its doc comment) is the last
	// line of defense against a mismatch this per-call checking somehow
	// missed, not the primary guard: a chunk is paired with its vector at
	// the moment that vector is returned (see flatUnit/embedAll), so there
	// is no later zip step that could misalign the two lists in the first
	// place.
	errVectorCount = errors.New("vectors: embedder returned the wrong number of vectors")
	// errDimensionMismatch means a returned vector's width is not the
	// Embedder's own reported Dimension(). It is checked here, at the
	// write boundary, rather than trusted: a vector must be stored whole
	// or not at all, never truncated to fit or zero-padded out to the
	// column width, since either would produce a vector that no longer
	// represents its chunk while still inserting cleanly (the same
	// silent-corruption failure mode docs/ingestion-spec.md's
	// truncate:false rule exists to prevent on the token side).
	//
	// This catches the embedder disagreeing with ITSELF. The other half of
	// the invariant -- the Embedder's Dimension() agreeing with the
	// vector(768) chunks.embedding is pinned to -- is enforced by Postgres
	// at INSERT ("expected 768 dimensions, not N"), which aborts the
	// orchestrator's transaction just as loudly; there is deliberately no
	// second copy of the 768 literal in Go production code to drift from
	// the migration (0002_code_intel.up.sql's own comment explains where
	// that invariant is pinned instead).
	errDimensionMismatch = errors.New("vectors: embedder returned a vector of the wrong width")
	// errBadDimension means the Embedder reports a non-positive
	// Dimension(), which no vector can ever match. Caught up front, before
	// a single embed call is spent, since every subsequent check would
	// fail anyway.
	errBadDimension = errors.New("vectors: embedder reports a non-positive dimension")
)

// Stats reports what one IngestFileChunks call did, mirroring
// internal/ingest/graph.Stats's role on the other track so the orchestrator
// (loam-c94.12) and the ingest_jobs.stats writer (loam-c94.13) can log and
// persist per-ingest counts the same way for both.
type Stats struct {
	// FilesReplaced is how many files had their chunks replaced -- every
	// file in the input batch, since each one's prior chunks are dropped
	// whether or not it produced any new ones.
	FilesReplaced int
	// FilesWithoutChunks is how many of FilesReplaced chunked to zero
	// units and therefore only dropped their prior chunks without
	// inserting replacements (a non-exclusive subset of FilesReplaced,
	// counted separately because "the file's chunks are gone on purpose"
	// is worth seeing in an operator's log rather than inferring from a
	// row count that went to zero).
	FilesWithoutChunks int
	// ChunksWritten is the total number of chunks rows inserted across
	// every file in the batch.
	ChunksWritten int
	// FilesRejected is how many files in the batch the store itself
	// refused to write (loam-c94.21) -- a malformed row, a constraint, a
	// size limit, anything Postgres raised for that ONE file's statement
	// rather than for the connection or transaction as a whole (see
	// Persist's doc comment for exactly where that line is drawn). Each
	// one is also logged at ERROR with the file's path, so this count is
	// never the only record of which files are missing. It is deliberately
	// NOT folded into FilesReplaced (a rejected file was NOT replaced --
	// its prior chunks are what a reader still sees, per Persist's own doc
	// comment) or treated as reason alone to fail the batch: see Persist's
	// doc comment for why returning an error here would defeat the point
	// of counting rejections in the first place.
	FilesRejected int
	// EmbedCalls is how many Embed requests the batch cost -- the
	// expensive, network-bound step, so it is worth reporting directly
	// rather than leaving a reader to divide ChunksWritten by
	// maxEmbedBatch. A batch whose files all chunked to zero units costs
	// zero embed calls.
	EmbedCalls int
}

// Indexer embeds chunk units and persists them. e is the embedding backend
// (constructor-injected, so tests use a mock or internal/testembed's
// deterministic double rather than a live Ollama); logger carries the
// per-file operator visibility. Construct with New.
type Indexer struct {
	embedder embedder
	logger   *slog.Logger
}

// New builds an Indexer over e. logger must be non-nil.
func New(e embedder, logger *slog.Logger) *Indexer {
	return &Indexer{embedder: e, logger: logger}
}

// IngestFileChunks embeds every chunk unit in files (typically the
// []chunker.FileChunks a chunker.ChunkFiles call just returned for the
// plan's reparse set) and persists them through st's per-file
// delete-and-replace, one ReplaceFileChunks call per file.
//
// st MUST be bound to a transaction the CALLER owns and will commit --
// chunkstore.NewInTx over the swap orchestrator's transaction
// (loam-c94.12). This method begins no transaction, commits nothing, and
// rolls nothing back: on any error it returns with whatever it had already
// staged still staged, for the caller to discard by rolling back. That is
// the whole point of the arrangement (docs/ingestion-spec.md "Consistency
// & Failure": readers keep serving the prior index until one commit swaps
// it). Handing it a chunkstore.Store built with New instead would silently
// commit each file on its own and defeat the atomic swap.
//
// Every file in files gets a ReplaceFileChunks call, INCLUDING one whose
// Units is empty: a reparsed file that now chunks to nothing (its content
// was emptied, or reduced to only whitespace/imports) must still have its
// prior chunks dropped, and an empty inputs slice is exactly
// ReplaceFileChunks's documented "delete without inserting" case. Skipping
// it would leave that file's stale chunks searchable against content that
// no longer exists.
//
// Embedding runs FIRST, for the whole batch, before any store call: chunk
// texts are flattened across all files in input order and sent in Embed
// requests of up to maxEmbedBatch texts each (a single request may
// therefore span a file boundary -- batching is by text count, not by
// file). Doing all the network work up front means an embedder failure
// costs zero writes rather than aborting halfway through the batch.
//
// It does NOT, on its own, keep the caller's transaction short. This
// method is Prepare followed by Persist, so a caller that invokes it with
// a transaction-bound store holds that transaction open for the embedding
// run too -- the exact thing loam-c94.12's DESIGN says not to do ("prefer
// computing outside the tx and keeping the write phase short"). A caller
// that owns a transaction should call Prepare BEFORE opening it and
// Persist inside it; this method is the convenience composition for a
// caller with no such constraint, and it is what this package's own
// single-call tests exercise.
//
// ctx cancellation, an embed failure, or a vector count/width that
// disagrees with the Embedder's own Dimension() still returns immediately,
// wrapped -- Prepare has nothing partial to skip past (see its own doc
// comment). A per-file store write failing during Persist does NOT abort
// the batch on its own as of loam-c94.21: see Persist's doc comment for
// which errors are survivable, which still abort, and why.
func (ix *Indexer) IngestFileChunks(ctx context.Context, st store, repoID uuid.UUID, targetBranch string, files []chunker.FileChunks) (Stats, error) {
	prepared, stats, err := ix.Prepare(ctx, repoID, targetBranch, files)
	if err != nil {
		return stats, err
	}
	writeStats, err := ix.Persist(ctx, st, repoID, targetBranch, prepared)
	stats.merge(writeStats)
	return stats, err
}

// Prepared is one batch's embedding work, already done: every file's chunk
// units paired with the vector that represents it, ready to be written
// with no further network calls. It is deliberately opaque -- a caller
// obtains one only from Prepare and can only spend it on Persist, so there
// is no way to construct a Prepared whose vectors were never validated
// against the Embedder's own Dimension().
type Prepared struct {
	files []preparedFile
}

// preparedFile is one file's share of a Prepared: its path, and the
// ChunkInputs (possibly none) that must replace whatever chunks that file
// currently has.
type preparedFile struct {
	path   string
	inputs []chunkstore.ChunkInput
}

// Files reports how many files this Prepared will replace the chunks of --
// including any that prepared to zero units. Exposed so the swap
// orchestrator (loam-c94.12) can log the size of the write phase it is
// about to enter without Prepared having to leak its contents.
func (p Prepared) Files() int { return len(p.files) }

// Prepare does the whole NETWORK half of IngestFileChunks and none of the
// database half: it embeds every chunk unit in files and pairs each unit
// with its vector, returning a Prepared that Persist can write with no
// further Embed calls.
//
// It exists as its own entry point because the swap orchestrator
// (loam-c94.12) must not hold its transaction open across slow Ollama
// calls, and it cannot avoid that by calling IngestFileChunks -- that
// method embeds INSIDE the call, so a caller that invokes it with a
// transaction-bound store necessarily has the transaction open for the
// whole embedding run. Splitting the phases is what actually delivers the
// property IngestFileChunks's own doc comment claims: the orchestrator
// calls Prepare before it begins its transaction (concurrently with the
// parse->graph track's own compute), then Persist inside it. It is also
// what makes the two tracks' compute genuinely concurrent, since a pgx.Tx
// is not goroutine-safe and nothing that touches one can run in parallel
// with anything else that does.
//
// repoID and targetBranch are used only to identify the batch in error
// messages and logs; Prepare touches no database.
func (ix *Indexer) Prepare(ctx context.Context, repoID uuid.UUID, targetBranch string, files []chunker.FileChunks) (Prepared, Stats, error) {
	var stats Stats
	if err := ctx.Err(); err != nil {
		return Prepared{}, stats, fmt.Errorf("embedding chunks for %s@%s: %w", repoID, targetBranch, err)
	}
	dimension := ix.embedder.Dimension()
	if dimension <= 0 {
		return Prepared{}, stats, fmt.Errorf("embedding chunks for %s@%s: %w: %d", repoID, targetBranch, errBadDimension, dimension)
	}
	items := flattenUnits(files)
	finalItems, embeddings, calls, err := ix.embedAll(ctx, items, dimension)
	stats.EmbedCalls = calls
	if err != nil {
		return Prepared{}, stats, fmt.Errorf("embedding chunks for %s@%s: %w", repoID, targetBranch, err)
	}
	perFile, err := groupByFile(len(files), finalItems, embeddings)
	if err != nil {
		return Prepared{}, stats, fmt.Errorf("pairing chunks with vectors for %s@%s: %w", repoID, targetBranch, err)
	}
	prepared := Prepared{files: make([]preparedFile, len(files))}
	for i, f := range files {
		prepared.files[i] = preparedFile{path: f.Path, inputs: perFile[i]}
	}
	return prepared, stats, nil
}

// Persist does the whole DATABASE half of IngestFileChunks and none of the
// network half: one st.ReplaceFileChunks call per file in p, in the order
// Prepare received them, making no Embed call at all. This is the only
// part of this package that belongs inside the swap orchestrator's
// transaction (see Prepare's doc comment).
//
// st MUST be bound to a transaction the CALLER owns and will commit --
// chunkstore.NewInTx over the orchestrator's transaction. Persist begins
// no transaction, commits nothing, and rolls nothing back: on any error it
// returns with whatever it had already staged still staged, for the caller
// to discard by rolling back.
//
// A file whose Prepared entry has zero inputs still gets its
// ReplaceFileChunks call, with an empty slice -- ReplaceFileChunks's
// documented delete-without-inserting case. That is what drops the stale
// chunks of a file that now chunks to nothing, or that chunker.ChunkFiles
// could only emit a zero-unit entry for because it turned binary or
// stopped parsing (loam-8uo).
//
// # Which errors are survivable (loam-c94.21)
//
// A single file's ReplaceFileChunks call failing is, by default, treated
// as a REJECTION: counted in Stats.FilesRejected, logged at ERROR naming
// the file, and skipped -- Persist moves on to the next file rather than
// returning. A malformed row, a constraint hit, or a size limit is that
// file's own problem and says nothing about whether the file after it can
// be written, so one such file must not cost the rest of the batch, the
// exact asymmetry with chunker.ChunkFiles's own per-file skip-and-continue
// (see the package doc comment) this bead exists to close.
//
// isFatalStoreError narrows that default: ctx cancellation (checked here
// AND at the top of every iteration, so a cancellation observed only via
// the store call's own error still stops the loop immediately, matching
// embedAll's identical re-check after an Embed error) and a fixed set of
// Postgres error classes that mean the CONNECTION or the TRANSACTION
// itself is unusable -- 08 (connection exception), 25 (invalid
// transaction state, which is what a PRIOR statement's error turns every
// later one on the same transaction into -- see below), 40 (transaction
// rollback, e.g. a deadlock), 42 (syntax/access-rule violation, a code or
// permissions bug that will recur identically for every remaining file),
// 53 (insufficient resources), 57 (operator intervention, e.g. an admin
// shutdown), and 58 (system error) -- plus pgx's own ErrTxClosed,
// ErrTxCommitRollback, and puddle.ErrClosedPool (what pgxpool.Pool.Begin
// actually surfaces once the pool is closed) sentinels for the same
// "the pipe itself is gone" case a bare PgError might not carry. None of
// that is guessable from a mock's plain error in a unit test, which is
// deliberate: an unclassified error defaults to "rejection," so this
// package's own tests (a moq store returning errors.New(...)) exercise the
// common case, and the fatal classes are exercised with the real pgx/pgconn
// values production actually returns.
//
// A fatal classification still returns immediately, exactly like before
// loam-c94.21: continuing to hammer a dead connection or an already-aborted
// transaction cannot recover anything and would just turn one real failure
// into a wall of identical ones. If an earlier file in this same call was
// already counted as a rejection, that file and its error are folded into
// the returned error too (firstRejection below) -- see the next section for
// why that matters.
//
// # Why a rejection does not make Persist itself return an error
//
// It would be tempting to have Persist return a distinguishable error once
// Stats.FilesRejected > 0, so the caller can decide the job is a "failure
// wearing a green badge." That is wrong for THIS caller specifically: the
// swap orchestrator (loam-c94.12, see internal/ingest/orchestrator's
// writeSwap) rolls back its ENTIRE transaction -- the graph track's writes
// included -- the moment Persist returns any error. Doing that over a
// handful of rejected files would re-commit the exact bug this bead exists
// to fix, just one layer up: the files this call DID successfully replace
// would be discarded along with the rejected ones. So Persist reports
// rejections only through Stats and the ERROR-level log line for each one;
// a caller that wants "success with warnings" vs. "failure" as a job-level
// verdict must read Stats.FilesRejected AFTER a successful commit and
// decide there (loam-c94.13, the ingest_jobs.stats writer, is the natural
// place -- not yet built, and out of this package's reach). There is
// deliberately no numeric threshold here for the same reason: any
// threshold Persist enforced by returning an error would trigger the same
// whole-transaction rollback as a single rejection would.
//
// # This is necessary, not sufficient, against a REAL store
//
// Everything above holds unconditionally against the store interface this
// package depends on, and is exactly what this package's own tests (a moq
// mock that rejects one named file among several) prove. It does NOT, on
// its own, make "one bad file costs one file" true when st is
// chunkstore.NewInTx over the orchestrator's shared transaction, which is
// how production actually constructs it (internal/ingest/orchestrator/
// production.go's vectorAdapter.Persist). Postgres aborts that ENTIRE
// transaction -- not just the failing statement -- the instant one
// statement in it errors, with no way back short of a SAVEPOINT neither
// this package nor chunkstore.ReplaceFileChunks currently takes. Confirmed
// against a real server, not assumed: see
// TestIngestFileChunks_RejectionInASharedTransactionStillDoomsTheWholeCommit
// in integration_test.go, which shows the caller's tx.Commit failing with
// "commit unexpectedly resulted in rollback" even when the rejected file is
// the LAST one Persist ever touches -- so continuing the loop past a
// rejection cannot be what saves the transaction; only a SAVEPOINT around
// each ReplaceFileChunks call (in chunkstore, outside this package) can.
// Skipping and counting here is still the correct, necessary half of the
// fix -- it is what makes a single-file batch, a rejection that lands last,
// or a future savepoint-protected store actually recover -- it is simply
// not the WHOLE fix for today's shared-transaction wiring, and this doc
// comment says so rather than leaving that gap to be discovered later.
func (ix *Indexer) Persist(ctx context.Context, st store, repoID uuid.UUID, targetBranch string, p Prepared) (Stats, error) {
	var stats Stats
	var firstRejection error
	for _, f := range p.files {
		if err := ctx.Err(); err != nil {
			return stats, fmt.Errorf("replacing chunks for %s: %w", f.path, err)
		}
		if _, err := st.ReplaceFileChunks(ctx, repoID, targetBranch, f.path, f.inputs); err != nil {
			if isFatalStoreError(ctx, err) {
				if firstRejection != nil {
					return stats, fmt.Errorf("replacing chunks for %s: %w (transaction already unusable after an earlier rejected file: %v)", f.path, err, firstRejection)
				}
				return stats, fmt.Errorf("replacing chunks for %s: %w", f.path, err)
			}
			stats.FilesRejected++
			if firstRejection == nil {
				firstRejection = fmt.Errorf("file %s: %w", f.path, err)
			}
			ix.logger.ErrorContext(ctx, "store rejected file's chunks; skipping and counting it so the rest of the batch still lands",
				"file", f.path, "repo_id", repoID, "target_branch", targetBranch, "error", err)
			continue
		}
		stats.FilesReplaced++
		stats.ChunksWritten += len(f.inputs)
		if len(f.inputs) == 0 {
			stats.FilesWithoutChunks++
			ix.logger.InfoContext(ctx, "file chunked to zero units; dropping its prior chunks", "file", f.path, "repo_id", repoID, "target_branch", targetBranch)
		}
	}
	return stats, nil
}

// isFatalStoreError reports whether err from a ReplaceFileChunks call means
// the FAILURE IS NOT SPECIFIC TO THAT FILE -- ctx was cancelled, or the
// connection/transaction itself is unusable -- as opposed to a rejection
// (a malformed row, a constraint, a size limit) that only that one file's
// statement raised. See Persist's doc comment ("Which errors are
// survivable") for the reasoning; this function is only the mechanics of
// it.
//
// ctx.Err() is checked first and takes priority over anything err itself
// says, matching the check the top of Persist's loop already makes and
// embedAll's identical re-check after an Embed error: a cancellation can
// surface as almost any wrapped error shape depending on where in the
// call it was observed, so asking ctx directly is the one check that
// cannot be fooled by how the driver happened to phrase it.
//
// Everything else here is Postgres-specific because the store this
// package actually writes through in production is chunkstore.Store, a
// pgx-backed implementation; a caller's own mock in a unit test will not
// produce any of these shapes, so an unclassified error is NOT fatal by
// default (see Persist's doc comment for why that default is the correct
// one).
func isFatalStoreError(ctx context.Context, err error) bool {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return true
	}
	if errors.Is(err, pgx.ErrTxClosed) || errors.Is(err, pgx.ErrTxCommitRollback) || errors.Is(err, puddle.ErrClosedPool) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgerrcode.IsConnectionException(pgErr.Code) ||
			pgerrcode.IsInvalidTransactionState(pgErr.Code) ||
			pgerrcode.IsTransactionRollback(pgErr.Code) ||
			pgerrcode.IsSyntaxErrororAccessRuleViolation(pgErr.Code) ||
			pgerrcode.IsInsufficientResources(pgErr.Code) ||
			pgerrcode.IsOperatorIntervention(pgErr.Code) ||
			pgerrcode.IsSystemError(pgErr.Code)
	}
	return false
}

// merge folds other's write-phase counters into s, which already carries
// the embed-phase ones. Prepare and Persist each populate a disjoint set
// of Stats fields, so this is an addition, never an overwrite.
func (s *Stats) merge(other Stats) {
	s.FilesReplaced += other.FilesReplaced
	s.FilesWithoutChunks += other.FilesWithoutChunks
	s.ChunksWritten += other.ChunksWritten
	s.FilesRejected += other.FilesRejected
	s.EmbedCalls += other.EmbedCalls
}

// flatUnit is one chunk unit flattened out of its file, tagged with the
// index of that file in the Prepare call's own files slice so a unit (or,
// after a split retry, one of its pieces) can find its way back to the
// right preparedFile without carrying a full path string around.
type flatUnit struct {
	fileIdx int
	unit    chunk.Unit
}

// flattenUnits flattens every file's chunk units into one slice, in input
// order (every unit of files[0], then every unit of files[1], ...), so
// embedAll can batch across file boundaries the same way flattenTexts used
// to. Unlike that predecessor, each entry keeps its originating file index
// and its full chunk.Unit (not just Content), because a unit that the
// embedder rejects as oversized must be split by embedWithSplit using its
// line numbers, and the pieces that result must still know which file they
// belong to.
func flattenUnits(files []chunker.FileChunks) []flatUnit {
	total := 0
	for _, f := range files {
		total += len(f.Units)
	}
	out := make([]flatUnit, 0, total)
	for fi, f := range files {
		for _, u := range f.Units {
			out = append(out, flatUnit{fileIdx: fi, unit: u})
		}
	}
	return out
}

// groupByFile buckets items (and their positionally-paired embeddings) back
// into one ChunkInput slice per file, by each item's own fileIdx -- an
// empty, non-nil slice for a file with no units, exactly what
// ReplaceFileChunks's delete-without-inserting case takes.
//
// Every item in items was paired with its vector at the exact moment that
// vector was returned from an Embed call (see embedAll and embedWithSplit),
// so there is no separate "vector list" this function zips against a
// differently-built "unit list" -- the classic way that kind of pairing
// silently drifts. The len(items) != len(embeddings) check below can only
// ever fire if a future change to embedAll breaks that invariant; it exists
// so such a bug degrades into errVectorCount here (still inside the swap
// orchestrator's transaction, so the caller rolls back cleanly) rather than
// an index-out-of-range panic that unwinds past the deferred rollback
// (internal/chunkstore's transactor doc comment says so explicitly).
func groupByFile(numFiles int, items []flatUnit, embeddings [][]float32) ([][]chunkstore.ChunkInput, error) {
	if len(items) != len(embeddings) {
		return nil, fmt.Errorf("%w: %d chunks to pair with %d vectors", errVectorCount, len(items), len(embeddings))
	}
	perFile := make([][]chunkstore.ChunkInput, numFiles)
	for i := range perFile {
		perFile[i] = []chunkstore.ChunkInput{}
	}
	for i, it := range items {
		perFile[it.fileIdx] = append(perFile[it.fileIdx], chunkstore.ChunkInput{
			StartLine: it.unit.StartLine,
			EndLine:   it.unit.EndLine,
			Content:   it.unit.Content,
			Embedding: embeddings[i],
		})
	}
	return perFile, nil
}

// embedAll embeds items in groups of at most maxEmbedBatch, returning every
// item actually embedded (finalItems) paired positionally with its vector
// (embeddings), plus the number of Embed calls made. finalItems is
// items itself on the fast path -- one vector per input item, same order --
// EXCEPT for any item the embedder rejected as too long for the model's
// context window (ollama.IsContextLengthExceeded): that item is replaced,
// in place, by the pieces embedWithSplit split it into and successfully
// embedded, so finalItems can be longer than items. This is loam-c94.16's
// fix: internal/ingest/chunk's byte-budget estimate (bytesPerTokenBudget)
// cannot bound a real token count for every content type -- dense JSON and
// base64 can still exceed the model's context window despite fitting the
// estimate's byte budget -- so rather than let that one embed call fail the
// whole ingest job (as ollama.IsContextLengthExceeded's own doc comment
// says was always the intended reaction point), this function reacts to the
// embedder's own rejection and splits, reusing chunk.SplitUnit -- the exact
// routine EnforceBudget already uses -- rather than re-guessing a ratio.
//
// Each batch's response is validated against dimension before it is
// accepted (see errVectorCount and errDimensionMismatch), both on the fast
// batch path and inside embedWithSplit's single-item retries; an empty
// items makes no call at all.
func (ix *Indexer) embedAll(ctx context.Context, items []flatUnit, dimension int) ([]flatUnit, [][]float32, int, error) {
	finalItems := make([]flatUnit, 0, len(items))
	embeddings := make([][]float32, 0, len(items))
	calls := 0
	for start := 0; start < len(items); start += maxEmbedBatch {
		if err := ctx.Err(); err != nil {
			return nil, nil, calls, fmt.Errorf("embedding batch at offset %d: %w", start, err)
		}
		batch := items[start:min(start+maxEmbedBatch, len(items))]
		texts := make([]string, len(batch))
		for i, it := range batch {
			texts[i] = it.unit.Content
		}
		vecs, err := ix.embedder.Embed(ctx, texts)
		calls++
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, nil, calls, ctxErr
			}
			if !ollama.IsContextLengthExceeded(err) {
				return nil, nil, calls, fmt.Errorf("embedding %d chunks at offset %d: %w", len(batch), start, err)
			}
			recItems, recVecs, recCalls, recErr := ix.recoverBatch(ctx, batch, dimension)
			calls += recCalls
			if recErr != nil {
				return nil, nil, calls, fmt.Errorf("embedding %d chunks at offset %d: %w", len(batch), start, recErr)
			}
			finalItems = append(finalItems, recItems...)
			embeddings = append(embeddings, recVecs...)
			continue
		}
		if len(vecs) != len(batch) {
			return nil, nil, calls, fmt.Errorf("%w: sent %d texts at offset %d, got %d vectors", errVectorCount, len(batch), start, len(vecs))
		}
		for i, vec := range vecs {
			if len(vec) != dimension {
				return nil, nil, calls, fmt.Errorf("%w: vector %d has width %d, want %d", errDimensionMismatch, start+i, len(vec), dimension)
			}
		}
		finalItems = append(finalItems, batch...)
		embeddings = append(embeddings, vecs...)
	}
	return finalItems, embeddings, calls, nil
}

// recoverBatch re-embeds batch one item at a time after the whole batch was
// rejected with ollama.IsContextLengthExceeded: Ollama's /api/embed rejects
// the whole request when ANY one input in it is too long (per this bead's
// own live measurements, batching itself is not the problem), so the
// batched response gives no way to tell which input in the batch was the
// culprit. Embedding one at a time re-establishes that: an item that
// succeeds alone is kept as-is, and only the one(s) that still fail alone
// go through embedWithSplit. This only runs on the rare failing batch, not
// on every batch, so the fast path's throughput is unaffected.
func (ix *Indexer) recoverBatch(ctx context.Context, batch []flatUnit, dimension int) ([]flatUnit, [][]float32, int, error) {
	items := make([]flatUnit, 0, len(batch))
	embeddings := make([][]float32, 0, len(batch))
	calls := 0
	for _, it := range batch {
		if err := ctx.Err(); err != nil {
			return nil, nil, calls, err
		}
		gotItems, gotVecs, n, err := ix.embedWithSplit(ctx, it, dimension)
		calls += n
		if err != nil {
			return nil, nil, calls, err
		}
		items = append(items, gotItems...)
		embeddings = append(embeddings, gotVecs...)
	}
	return items, embeddings, calls, nil
}

// embedWithSplit embeds one item alone. If the embedder accepts it, that is
// the whole answer -- one item, one vector, paired at the point the vector
// is returned. If the embedder rejects it specifically as too long
// (ollama.IsContextLengthExceeded), it splits it via chunk.SplitUnit and
// recurses on each piece, so a chunk that is still too big after one split
// (rare, but possible for a large enough dense blob) keeps halving until
// every piece embeds or splitForRetry reports the content cannot be reduced
// any further -- the same terminal case chunk.splitUnit's own doc comment
// describes: a single line (here, a single already-unsplittable piece) that
// alone exceeds the budget is hard-split on rune boundaries, and a piece
// that hard-splitting cannot shrink any more is genuinely un-embeddable, so
// that case's original rejection is returned rather than looping forever.
//
// Every returned item is paired with its vector in the same return
// statement that produced the vector -- there is no later pass that
// re-associates pieces with vectors by position, which is what keeps a
// split chunk's persisted content and its persisted embedding from ever
// disagreeing (see groupByFile's doc comment and errVectorCount).
func (ix *Indexer) embedWithSplit(ctx context.Context, it flatUnit, dimension int) ([]flatUnit, [][]float32, int, error) {
	vecs, err := ix.embedder.Embed(ctx, []string{it.unit.Content})
	if err == nil {
		if len(vecs) != 1 {
			return nil, nil, 1, fmt.Errorf("%w: sent 1 text, got %d vectors", errVectorCount, len(vecs))
		}
		if len(vecs[0]) != dimension {
			return nil, nil, 1, fmt.Errorf("%w: vector has width %d, want %d", errDimensionMismatch, len(vecs[0]), dimension)
		}
		return []flatUnit{it}, vecs, 1, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, 1, ctxErr
	}
	if !ollama.IsContextLengthExceeded(err) {
		return nil, nil, 1, err
	}
	pieces := splitForRetry(it.unit)
	if pieces == nil {
		return nil, nil, 1, fmt.Errorf("splitting a %d-byte chunk the embedder still rejects as too long for the model's context window: %w", len(it.unit.Content), err)
	}
	ix.logger.WarnContext(ctx, "embed call rejected a chunk as exceeding the model's context window; splitting and retrying",
		"file_index", it.fileIdx, "start_line", it.unit.StartLine, "end_line", it.unit.EndLine, "content_bytes", len(it.unit.Content), "pieces", len(pieces))
	items := make([]flatUnit, 0, len(pieces))
	embeddings := make([][]float32, 0, len(pieces))
	calls := 1
	for _, piece := range pieces {
		gotItems, gotVecs, n, perr := ix.embedWithSplit(ctx, flatUnit{fileIdx: it.fileIdx, unit: piece}, dimension)
		calls += n
		if perr != nil {
			return nil, nil, calls, perr
		}
		items = append(items, gotItems...)
		embeddings = append(embeddings, gotVecs...)
	}
	return items, embeddings, calls, nil
}

// splitForRetry halves u's byte budget and reuses chunk.SplitUnit -- the
// same line-accumulating, rune-boundary-safe splitting EnforceBudget uses
// at chunk time -- to produce smaller pieces. It returns nil, the terminal
// signal embedWithSplit treats as "cannot be reduced any further," when
// u.Content is a single byte already or when SplitUnit at that halved
// budget could not actually split it (chunk.splitUnit returns the content
// unchanged, as one piece, when it is a single rune that a rune-boundary
// split cannot cut any smaller -- see its own doc comment). Halving rather
// than computing a new byte/token estimate is deliberate: this package has
// no access to the embedding model's ContextWindow (see the embedder
// interface's doc comment for why) and does not need one -- it is reacting
// to the embedder's own truth about what fits, not predicting it, which is
// this bead's whole point.
func splitForRetry(u chunk.Unit) []chunk.Unit {
	if len(u.Content) <= 1 {
		return nil
	}
	pieces := chunk.SplitUnit(u, len(u.Content)/2)
	if len(pieces) <= 1 {
		return nil
	}
	return pieces
}
