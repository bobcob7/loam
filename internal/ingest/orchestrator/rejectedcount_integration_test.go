//go:build integration

// Run explicitly with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/ingest/orchestrator/... -v
//
// See integration_test.go's header for the shared container and the
// podman/ryuk note; this file reuses both.
//
// These are loam-2d44's tests: not "does a rejection survive the commit"
// (loam-c94.24 proved that, in integration_test.go), but "does anything
// downstream ever LEARN that it happened". They need a real Postgres for
// the same reason c94.24's did -- the rejection is pgvector refusing a NaN
// coordinate at INSERT, a server-side per-statement error no mock produces
// and no Go-level fake can stand in for without assuming the answer.
package orchestrator

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/testembed"
)

// --- what a rejection tells an operator (loam-2d44) ---

// capturedRecord is one log record flattened to the two things these tests
// assert on: the level (operators alert on level, so a partial ingest that
// only ever speaks at INFO is not reachable by an alert) and the attributes
// by key.
type capturedRecord struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

// logCapture collects records from every goroutine the pipeline logs on.
// The compute phase genuinely runs its two tracks concurrently and both
// log, so the mutex is load-bearing under -race, not decorative.
type logCapture struct {
	mu      sync.Mutex
	records []capturedRecord
}

// withMessage returns every captured record whose message matches exactly.
// Matching the message rather than a substring is deliberate: the whole
// point of the WARN line is that it is a DISTINCT message from "ingest
// committed", so a test that matched loosely could not tell the two apart
// and would pass if they were merged back together.
func (c *logCapture) withMessage(msg string) []capturedRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capturedRecord
	for _, r := range c.records {
		if r.msg == msg {
			out = append(out, r)
		}
	}
	return out
}

func (c *logCapture) atLevel(level slog.Level) []capturedRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capturedRecord
	for _, r := range c.records {
		if r.level == level {
			out = append(out, r)
		}
	}
	return out
}

type capturingHandler struct {
	capture *logCapture
	base    []slog.Attr
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{level: r.Level, msg: r.Message, attrs: make(map[string]any, r.NumAttrs()+len(h.base))}
	for _, a := range h.base {
		rec.attrs[a.Key] = a.Value.Resolve().Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.Resolve().Any()
		return true
	})
	h.capture.mu.Lock()
	defer h.capture.mu.Unlock()
	h.capture.records = append(h.capture.records, rec)
	return nil
}

func (h *capturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &capturingHandler{capture: h.capture, base: append(append([]slog.Attr(nil), h.base...), attrs...)}
}

func (h *capturingHandler) WithGroup(string) slog.Handler { return h }

func newCapturingLogger() (*slog.Logger, *logCapture) {
	capture := &logCapture{}
	return slog.New(&capturingHandler{capture: capture}), capture
}

// jobStatsColumn round-trips stats through the exact encoding
// internal/ingest.Pool.succeed uses to write ingest_jobs.stats
// (json.Marshal of the ingest.Stats value the orchestrator returned), and
// decodes it as an untyped object -- so the assertion is on the COLUMN's
// key names and values, not on Go field names a rename would silently take
// with it. That column is the durable, queryable surface this bead chose;
// asserting only the Go struct would leave its actual contract untested.
//
// Reaching for the seam rather than the Pool itself is deliberate:
// internal/ingest.Pool is a different agent's territory this cycle, and
// the marshalling is the whole of what stands between these two values.
func jobStatsColumn(t *testing.T, stats ingest.Stats) map[string]any {
	t.Helper()
	raw, err := json.Marshal(stats)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// TestIngest_RejectedFiles_AreCountedAndReachEveryChosenSurface is the
// verification loam-2d44 asks for: a real ingest that rejects some but not
// all of its files, asserting the count reaches the surfaces this bead
// picked -- ingest.Stats (hence ingest_jobs.stats) and the orchestrator's
// log lines.
//
// # What the fixture is built to make DISTINGUISHABLE
//
// Two rejections out of four files, not one out of three, and the four
// files are given different content and different symbol names. That is
// what separates the outcomes a weaker fixture fuses together:
//
//   - Two rejections, so a count of 2 cannot be produced by a boolean
//     ("any rejection") widened to an int, nor by a first-rejection-only
//     latch. One rejection could not tell those apart from a correct sum.
//   - Two SURVIVORS, so a count of 2 cannot be produced by miscounting the
//     batch size (4) or the number of files that succeeded -- which happen
//     to be equal here, so the survivor-count reading is separately ruled
//     out by asserting FilesParsed is 4 in the same breath.
//   - Distinct files with distinct symbol names, and a per-file chunk-count
//     assertion on all four, so the test shows the count identified the
//     RIGHT two. Identical files could show that two were rejected and
//     never that the two rejected were the two poisoned.
//   - An exact equality, never assert.Positive: "non-zero" cannot tell one
//     rejection from five, which is precisely the discrimination an
//     operator needs from this number.
func TestIngest_RejectedFiles_AreCountedAndReachEveryChosenSurface(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commit(t, "two poisoned of four", map[string]string{
		"clean_alpha.go": "package alpha\n\nfunc AlphaKeeps() {}\n",
		"bad_one.go":     "package badone\n\nfunc RejectedFirst() {}\n",
		"bad_two.go":     "package badtwo\n\nfunc RejectedSecond() {}\n",
		"clean_omega.go": "package omega\n\nfunc OmegaKeeps() {}\n",
	})
	embedder := nanVectorEmbedder{Embedder: testembed.New(), marker: "Rejected"}
	logger, capture := newCapturingLogger()
	job := f.job(ingest.KindIncremental)

	stats, err := newOrchestratorWithLogger(t, f, realTransactor(), embedder, logger).Run(t.Context(), job)
	require.NoError(t, err, "rejected files must not fail the job -- loam-c94.24's whole trade, and the reason this counter is the only signal left")

	assert.Equal(t, 2, stats.FilesRejected,
		"exactly the two poisoned files were rejected: 0 is the pre-loam-2d44 behaviour, 1 would be a first-rejection latch, 4 would be the batch size")
	assert.Equal(t, 4, stats.FilesParsed,
		"the graph track is unaffected by a chunk rejection, so a rejection count that came from the survivor count would show up here as a shrunken FilesParsed")

	assert.Zero(t, chunkCountFor(t, f, "bad_one.go"), "the first poisoned file is one of the two the count refers to")
	assert.Zero(t, chunkCountFor(t, f, "bad_two.go"), "the second poisoned file is the other")
	assert.Positive(t, chunkCountFor(t, f, "clean_alpha.go"), "a clean file must still be indexed, or the count would be describing a different two files")
	assert.Positive(t, chunkCountFor(t, f, "clean_omega.go"))
	assert.Equal(t, chunkCountFor(t, f, "clean_alpha.go")+chunkCountFor(t, f, "clean_omega.go"), stats.ChunksEmbedded,
		"ChunksEmbedded counts only chunks that actually landed, so a rejected file's chunks are absent from it as well as from the table")

	column := jobStatsColumn(t, stats)
	assert.Equal(t, float64(2), column["files_rejected"],
		"ingest_jobs.stats is this bead's durable, queryable surface, and its key is the contract -- json.Number decodes as float64")

	committed := capture.withMessage("ingest committed")
	require.Len(t, committed, 1)
	assert.Equal(t, int64(2), committed[0].attrs["files_rejected"],
		"the commit line carries the count, so a log-only operator gets it without a query")
	assert.Equal(t, slog.LevelInfo, committed[0].level,
		"the commit line stays INFO and stays greppable for ALL completed ingests; moving the partial ones off it would hide them")

	partial := capture.withMessage("ingest committed with rejected files; this repo is partially indexed until those files change again or a full rebuild runs")
	require.Len(t, partial, 1, "the incompleteness needs a line of its own, because operators alert on level and a field on an INFO line is not reachable by an alert")
	assert.Equal(t, slog.LevelWarn, partial[0].level)
	assert.Equal(t, int64(2), partial[0].attrs["files_rejected"])
	assert.Equal(t, job.TargetBranch, partial[0].attrs["target_branch"])
	assert.Equal(t, job.ID, partial[0].attrs["job_id"],
		"the WARN must name THIS job, or it cannot be joined to the ingest_jobs row carrying the same count")
}

// TestIngest_NoRejections_ReportsZeroAndStaysSilent is the control the
// test above is worthless without. A WARN that fires on every ingest
// carries no information, and a files_rejected key that is only ever
// asserted when it is non-zero could be a constant. Both halves are
// checked here on the same pipeline, same fixture shape, same assertions
// -- only the embedder differs.
func TestIngest_NoRejections_ReportsZeroAndStaysSilent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.commit(t, "nothing poisoned", map[string]string{
		"clean_alpha.go": "package alpha\n\nfunc AlphaKeeps() {}\n",
		"clean_omega.go": "package omega\n\nfunc OmegaKeeps() {}\n",
	})
	logger, capture := newCapturingLogger()

	stats, err := newOrchestratorWithLogger(t, f, realTransactor(), testembed.New(), logger).Run(t.Context(), f.job(ingest.KindIncremental))
	require.NoError(t, err)

	assert.Zero(t, stats.FilesRejected)
	assert.Positive(t, stats.ChunksEmbedded, "a clean ingest of two real files must have written chunks, or 'no rejections' would be vacuous")
	column := jobStatsColumn(t, stats)
	require.Contains(t, column, "files_rejected", "the key must be present and 0 rather than omitted, so a query can distinguish 'no rejections' from 'a job that predates the field'")
	assert.Equal(t, float64(0), column["files_rejected"])

	committed := capture.withMessage("ingest committed")
	require.Len(t, committed, 1)
	assert.Equal(t, int64(0), committed[0].attrs["files_rejected"])
	assert.Empty(t, capture.atLevel(slog.LevelWarn),
		"a clean ingest must emit no WARN at all: a warning that is always present is one an operator learns to ignore")
}
