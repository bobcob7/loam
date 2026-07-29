package migrations

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Querier issues a single-row read over a database connection the CALLER
// already owns. SchemaCheck (schema.go) consumes it so a readiness probe
// can ask "is this schema current?" over the live *pgxpool.Pool the
// process is already holding, instead of opening a second connection per
// request the way Migrate/Down's own golang-migrate instance necessarily
// does (newMigrator: sql.Open + PingContext + WithInstance, three round
// trips before it can answer anything).
//
// *pgxpool.Pool satisfies it structurally, which is the only
// implementation production ever passes.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
