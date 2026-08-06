package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bobcob7/loam/internal/config"
	"github.com/bobcob7/loam/internal/db"
)

// migrateFunc matches migrations.Migrate's signature. connectDatabase
// takes it as a parameter, rather than calling migrations.Migrate
// directly, so a test can substitute a recording spy and prove the
// ordering below without a real Postgres.
type migrateFunc func(ctx context.Context, dsn string, logger *slog.Logger) error

// newPoolFunc matches db.NewPool's signature. See migrateFunc's doc
// comment.
type newPoolFunc func(ctx context.Context, cfg db.Config, logger *slog.Logger) (*pgxpool.Pool, error)

// connectDatabase runs docs/server-spec.md Startup step 2 in the only
// order that does not deadlock a virgin database: migrate MUST complete
// before newPool is ever called. internal/db/pool.go's NewPool doc
// comment has the full mechanism (pgxpool's AfterConnect registers the
// pgvector type, which fails until migration 0002's CREATE EXTENSION
// vector has run, and a failing AfterConnect wedges every later
// acquisition including NewPool's own readiness ping) -- this is the
// ordering loam-ut9 filed against docs/server-spec.md's prose, fixed
// alongside this wiring. migrate and newPool are accepted as parameters,
// rather than this function calling migrations.Migrate and db.NewPool
// directly, purely so TestConnectDatabase_* can prove the order and the
// abort-on-migrate-failure property with recording spies instead of a
// real Postgres; run always passes the real functions.
//
// THE TWO COLLABORATORS GET DIFFERENT DSNs, ON PURPOSE (loam-lhc9). One
// operator-supplied LOAM_DATABASE_URL, two parsers: migrate reaches
// Postgres through database/sql, which forwards any key it does not
// recognize to the server as a startup option, so a DSN carrying pgxpool's
// own pool_max_conns is rejected with `FATAL: unrecognized configuration
// parameter` and the server never boots. newPool gets the URL intact --
// those parameters are addressed to it and dropping them would silently
// discard the operator's pool sizing. db.MigrationDSN produces migrate's
// copy by removing exactly the keys pgxpool claims, asking pgx which those
// are rather than keeping a list here that would go stale.
func connectDatabase(ctx context.Context, cfg config.Config, migrate migrateFunc, newPool newPoolFunc) (*pgxpool.Pool, error) {
	migrationDSN, err := db.MigrationDSN(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("preparing migration database url: %w", err)
	}
	if err := migrate(ctx, migrationDSN, cfg.Logger); err != nil {
		return nil, fmt.Errorf("running migrations: %w", err)
	}
	pool, err := newPool(ctx, db.Config{DatabaseURL: cfg.DatabaseURL, EncryptionKey: string(cfg.EncryptionKey), TracerProvider: cfg.TracerProvider, AcquireSpanThreshold: cfg.OTelDBAcquireThreshold}, cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	return pool, nil
}
