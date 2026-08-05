package db

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

// TestQueryName_UsesSqlcHeaderNotStatementText is the property the span
// naming rests on: sqlc's `-- name:` header survives into the SQL constant
// pgx receives, so a bounded operation name is recoverable at a seam that
// otherwise sees only statement text.
//
// The inputs are copied verbatim from internal/db/gen -- header, blank
// spacing and all -- rather than hand-idealised, because the failure mode
// worth catching is sqlc changing its emitted preamble, and a
// hand-written "-- name: X :exec\nSELECT 1" fixture cannot see that.
//
// The cases deliberately differ from each other in name, in verb, and in
// sqlc kind (:exec, :one, :many, :copyfrom). A table where every row shared
// one query would pass just as happily against `return "DeleteChunksByFile"`,
// which is precisely the class of fixture blindness loam-p56y shipped.
func TestQueryName_UsesSqlcHeaderNotStatementText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "exec",
			sql: `-- name: DeleteChunksByFile :exec
DELETE FROM chunks
WHERE repo_id = $1 AND target_branch = $2 AND file = $3
`,
			want: "DeleteChunksByFile",
		},
		{
			name: "many",
			sql: `-- name: ListReposForBranch :many
SELECT id, forge_host FROM repos WHERE target_branch = $1
`,
			want: "ListReposForBranch",
		},
		{
			name: "one",
			sql: `-- name: GetCredentialForHost :one
SELECT token_ciphertext FROM credentials WHERE forge_host = $1
`,
			want: "GetCredentialForHost",
		},
		{
			name: "copyfrom kind still names the query",
			sql: `-- name: InsertGraphEdges :copyfrom
INSERT INTO graph_edges (repo_id, target_branch) VALUES ($1, $2)
`,
			want: "InsertGraphEdges",
		},
		{
			name: "leading whitespace before the header",
			sql:  "\n\t-- name: GetRepoByID :one\nSELECT id FROM repos WHERE id = $1\n",
			want: "GetRepoByID",
		},
		{
			name: "header with no sqlc kind",
			sql:  "-- name: BareName\nSELECT 1\n",
			want: "BareName",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, queryName(tt.sql))
		})
	}
}

// TestQueryName_UnheaderedSQLNeverReturnsTheStatement covers the fallback,
// and asserts the thing that actually matters about it rather than merely
// that it is non-empty: the returned name must not contain the statement
// text. A fallback of `return sql` would satisfy "returns something stable
// for a given input" while reintroducing both the cardinality explosion and
// the leak risk the whole file exists to prevent.
//
// pgxpool.Pool.Ping's bare ";" is in here because it is the one unheadered
// statement this repository is GUARANTEED to execute on every boot.
func TestQueryName_UnheaderedSQLNeverReturnsTheStatement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sql  string
	}{
		{name: "pgxpool ping", sql: ";"},
		{name: "empty", sql: ""},
		{name: "hand written select", sql: "SELECT token_ciphertext FROM credentials WHERE forge_host = 'git.example.com'"},
		{name: "comment that is not a sqlc header", sql: "-- just a comment\nSELECT 1"},
		{name: "header marker mid statement", sql: "SELECT 1 -- name: NotAHeader :one"},
		{name: "header keyword but wrong spacing", sql: "--name: NoSpace :one\nSELECT 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := queryName(tt.sql)
			assert.Equal(t, unnamedQuery, got)
			assert.NotContains(t, got, "SELECT", "the fallback must never be the statement text")
			assert.NotContains(t, got, "credentials", "the fallback must never be the statement text")
		})
	}
}

// TestSQLState_ReportsCodeNeverMessage is the unit-level half of the
// error-path leak guard (the integration half, against a real Postgres
// error that genuinely echoes its input, is
// TestQueryTracer_ErrorPathNeverLeaksArgument). Postgres puts offending
// VALUES in error messages; only the SQLSTATE is safe to record, so
// anything derived from err.Error() must never reach a span.
func TestSQLState_ReportsCodeNeverMessage(t *testing.T) {
	t.Parallel()
	t.Run("pg error yields its sqlstate", func(t *testing.T) {
		t.Parallel()
		err := &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type uuid: "s3cret-token-value"`}
		got := sqlState(err)
		assert.Equal(t, "22P02", got)
		assert.NotContains(t, got, "s3cret-token-value")
	})
	t.Run("wrapped pg error is still unwrapped", func(t *testing.T) {
		t.Parallel()
		wrapped := errors.Join(errors.New("acquiring connection"), &pgconn.PgError{Code: "23505"})
		assert.Equal(t, "23505", sqlState(wrapped))
	})
	t.Run("non-pg error yields a fixed placeholder", func(t *testing.T) {
		t.Parallel()
		got := sqlState(errors.New(`context deadline exceeded while binding "s3cret-token-value"`))
		assert.Equal(t, "unknown", got)
		assert.NotContains(t, got, "s3cret-token-value")
	})
}
