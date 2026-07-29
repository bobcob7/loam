package health

import "context"

//go:generate go tool moq -out moq_test.go . Pinger SchemaChecker

// Pinger reports whether the process's Postgres connection pool can still
// reach its backend right now. *pgxpool.Pool satisfies it structurally,
// and is the only implementation production passes.
//
// This is a POOL-level seam, not a "can I open a connection to some
// database" seam: the question readiness asks is whether the very pool
// every handler in this process is about to use is usable, which is what
// Pool.Ping answers (it acquires from the pool, so it also observes an
// exhausted or wedged pool, not merely a reachable server).
type Pinger interface {
	Ping(ctx context.Context) error
}

// SchemaChecker reports whether the database's applied migration version
// still matches the migration set this binary embeds.
// migrations.SchemaCheck satisfies it structurally.
type SchemaChecker interface {
	CheckSchema(ctx context.Context) error
}
