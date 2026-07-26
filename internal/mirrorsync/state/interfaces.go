// Package state implements mirrorsync.SyncStateReporter against Postgres
// repos.sync_state (docs/sync-spec.md -> Mirror Sync, :85; docs/persistence-spec.md
// -> "repos"). Reporter is the only exported type: construct one with New and
// wire it into mirrorsync.New as the SyncStateReporter collaborator.
package state

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"
)

//go:generate go tool moq -out moq_test.go . execer

// execer is the minimal Postgres surface Reporter needs: a single
// parameterized statement executor. Defined here at the consumer, per repo
// convention, so Reporter can be unit-tested against a moq mock instead of a
// live pool; *pgxpool.Pool satisfies it in production without modification.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}
