package vectors

import (
	"context"
	"hash/fnv"
	"io"
	"log/slog"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/bobcob7/loam/internal/chunkstore"
	"github.com/bobcob7/loam/internal/ingest/chunk"
	"github.com/bobcob7/loam/internal/ingest/chunker"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// capturingHandler is a minimal slog.Handler that keeps every record it
// receives, so a test can assert not just that a rejection returned the
// right stats but that it was ALSO logged, and logged naming the right
// file -- Persist's contract is "counted AND logged," and a test that only
// checks Stats cannot tell those apart from a bug that drops the log line.
type capturingHandler struct {
	records *[]slog.Record
}

func (h capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h capturingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r)
	return nil
}

func (h capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h capturingHandler) WithGroup(string) slog.Handler { return h }

// newCapturingLogger returns a logger backed by capturingHandler and a
// pointer to its recorded slog.Records, for tests that need to assert on
// log content rather than just Stats/error return values.
func newCapturingLogger() (*slog.Logger, *[]slog.Record) {
	records := &[]slog.Record{}
	return slog.New(capturingHandler{records: records}), records
}

// recordAttr returns the string value of key on r, or "" if r carries no
// such attribute -- a small helper so log-content assertions read as plain
// equality checks.
func recordAttr(r slog.Record, key string) string {
	var out string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out = a.Value.String()
			return false
		}
		return true
	})
	return out
}

// testDimension is the narrow vector width the unit tests in this package
// run against. It is deliberately NOT 768: these tests exercise this
// package's own batching, offset arithmetic, and width checking, none of
// which depend on the production width, and a 4-wide vector makes a failure
// message readable. The real 768 is exercised end to end against the real
// vector(768) column in integration_test.go, via internal/testembed.
const testDimension = 4

// vectorFor is the deterministic, per-text vector the fake embedder below
// returns: a testDimension-wide vector whose first element is an FNV-1a
// hash of the text. Hashing the TEXT (rather than, say, the text's position
// in the batch) is the point -- it makes every assertion of the form
// "chunk X was stored with the vector belonging to chunk X" meaningful, so
// a bug that pairs a chunk's content with a neighbour's embedding (an
// off-by-one in the flatten/reassemble arithmetic, or a batch boundary
// mishandled) shows up as a wrong number rather than passing unnoticed.
func vectorFor(text string) []float32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(text))
	v := make([]float32, testDimension)
	v[0] = float32(h.Sum32() % 1_000_003)
	return v
}

// newFakeEmbedder returns an embedderMock that reports testDimension and
// embeds each text via vectorFor. BOTH methods are configured, per this
// codebase's "an unconfigured mock method panics, so real assertions never
// run" trap: a test that reaches a call this fake does not expect fails
// loudly instead of silently.
func newFakeEmbedder() *embedderMock {
	return &embedderMock{
		DimensionFunc: func() int { return testDimension },
		EmbedFunc: func(_ context.Context, texts []string) ([][]float32, error) {
			out := make([][]float32, len(texts))
			for i, text := range texts {
				out[i] = vectorFor(text)
			}
			return out, nil
		},
	}
}

// replaceCall is one recorded ReplaceFileChunks invocation, capturing every
// argument this package is responsible for choosing.
type replaceCall struct {
	repoID       uuid.UUID
	targetBranch string
	file         string
	inputs       []chunkstore.ChunkInput
}

// newFakeStore returns a storeMock recording every ReplaceFileChunks call
// in order, plus a pointer to the recorded slice.
func newFakeStore() (*storeMock, *[]replaceCall) {
	calls := &[]replaceCall{}
	mock := &storeMock{
		ReplaceFileChunksFunc: func(_ context.Context, repoID uuid.UUID, targetBranch, file string, inputs []chunkstore.ChunkInput) ([]chunkstore.Chunk, error) {
			*calls = append(*calls, replaceCall{repoID: repoID, targetBranch: targetBranch, file: file, inputs: inputs})
			return nil, nil
		},
	}
	return mock, calls
}

// unitsFor builds one chunker.FileChunks whose units carry the given
// contents on consecutive single-line ranges, so line numbers are
// predictable and distinct per unit.
func unitsFor(path string, contents ...string) chunker.FileChunks {
	units := make([]chunk.Unit, len(contents))
	for i, c := range contents {
		units[i] = chunk.Unit{StartLine: i + 1, EndLine: i + 1, Content: c}
	}
	return chunker.FileChunks{Path: path, Units: units}
}

// syntheticUnits builds one file with n units whose contents are all
// distinct ("<path>#0", "<path>#1", ...), for the batching tests.
func syntheticUnits(t *testing.T, path string, n int) chunker.FileChunks {
	t.Helper()
	contents := make([]string, n)
	for i := range contents {
		contents[i] = path + "#" + strconv.Itoa(i)
	}
	return unitsFor(path, contents...)
}
