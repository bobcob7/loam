package db

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

// NewPool builds a pgx connection pool from cfg and pings it, failing fast
// with a wrapped error if the DSN is malformed or the database is
// unreachable. Each pooled connection registers the pgvector types so
// `vector` columns can be scanned into pgvector-go's Vector type.
//
// CALLERS MUST RUN migrations.Migrate AGAINST cfg.DatabaseURL BEFORE calling
// NewPool. pgxvec.RegisterTypes resolves the `vector` OID via
// `to_regtype('vector')` and returns an error when the pgvector extension
// has not been created yet; pgxpool fails EVERY connection acquisition
// (Ping included) when AfterConnect errors. If NewPool ran before
// migrations on a virgin database, the pool would fail before migrations
// ever got a chance to run `CREATE EXTENSION vector`, permanently
// deadlocking first boot — this happened once already (loam-54o.1) and the
// hook was ripped out rather than left to fail that way. The fix is not to
// make RegisterTypes tolerant of a missing type (that trades a loud
// deployment failure for silent corruption discovered later); it is to
// guarantee the extension exists first. So the required boot order is:
// load config -> migrations.Migrate(ctx, dsn, logger) -> db.NewPool(...).
// Under that order CREATE EXTENSION vector has always already run by the
// time any pooled connection opens, and AfterConnect registration is safe.
// Do not "simplify" this by building the pool first and migrating after.
//
// The caller owns the returned pool and must Close it.
func NewPool(ctx context.Context, cfg Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("creating pool: %w", ErrMissingDatabaseURL)
	}
	poolCfg, err := newPoolConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("parsing database url: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("creating connection pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}
	logger.InfoContext(ctx, "connected to database")
	return pool, nil
}

// newPoolConfig parses cfg.DatabaseURL into a pgxpool.Config with pgvector
// type registration wired into AfterConnect and, when cfg.TracerProvider is
// set, this package's pgx tracing hook wired into ConnConfig.Tracer. Split
// out from NewPool so both are unit-testable without a live database
// connection (pgxpool.ParseConfig and NewWithConfig do not dial network).
//
// ConnConfig.Tracer is the ONE seam for query tracing: pgx consults it on
// every Query/QueryRow/Exec/CopyFrom on every connection the pool opens, so
// no call site anywhere in the tree has to know that tracing exists. Do not
// reintroduce per-query instrumentation at the store layer.
func newPoolConfig(cfg Config) (*pgxpool.Config, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	poolCfg.AfterConnect = pgxvec.RegisterTypes
	if cfg.TracerProvider != nil {
		poolCfg.ConnConfig.Tracer = newQueryTracer(cfg.TracerProvider)
	}
	return poolCfg, nil
}
