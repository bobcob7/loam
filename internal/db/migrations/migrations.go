// Package migrations wires golang-migrate to apply the schema migrations
// embedded under files/ against Postgres via the iofs source driver and the
// pgx/v5 database driver. It owns no schema of its own: 0001_init carries
// the metadata tables (loam-54o.3, docs/persistence-spec.md "Metadata"), and
// 0002_code_intel carries the derived code-intelligence tables + pgvector
// extension (loam-54o.4, docs/persistence-spec.md "Code intelligence").
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// migrationsDir is the path within migrationFiles the iofs source reads
// from, and the directory NNNN_name.up.sql / NNNN_name.down.sql files live
// under.
const migrationsDir = "files"

//go:embed files/*.sql
var migrationFiles embed.FS

// errEmptyDSN is returned when Migrate is called with an empty DSN, so the
// failure is immediate and legible instead of surfacing deep inside pgx's
// connection-string parser.
var errEmptyDSN = errors.New("dsn is required")

// Migrate applies every pending migration embedded under files/ to the
// database at dsn and reports the resulting version. golang-migrate manages
// its own connection (opened from dsn, independent of any *pgxpool.Pool the
// caller may also build) and its own schema_migrations bookkeeping table,
// so Migrate has no dependency on pool construction and can run before or
// after it. It is idempotent: run again against an already-current
// database, it is a no-op.
func Migrate(ctx context.Context, dsn string, logger *slog.Logger) error {
	if dsn == "" {
		return fmt.Errorf("migrating: %w", errEmptyDSN)
	}
	sourceDriver, err := iofs.New(migrationFiles, migrationsDir)
	if err != nil {
		return fmt.Errorf("opening embedded migration source: %w", err)
	}
	db, err := sql.Open("pgx/v5", dsn)
	if err != nil {
		return fmt.Errorf("opening migration database connection: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connecting to migration database: %w", err)
	}
	databaseDriver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		return fmt.Errorf("creating migration database driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", sourceDriver, "pgx5", databaseDriver)
	if err != nil {
		return fmt.Errorf("constructing migrator: %w", err)
	}
	defer closeMigrator(ctx, m, logger)
	logger.InfoContext(ctx, "applying migrations")
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("applying migrations: %w", err)
	}
	version, dirty, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return fmt.Errorf("reading migration version: %w", err)
	}
	logger.InfoContext(ctx, "migrations up to date", "version", version, "dirty", dirty)
	return nil
}

// closeMigrator closes the migrator's source and database drivers. Failures
// are logged rather than swallowed: Migrate has already returned by the
// time this runs, so there is no error path left to report through.
func closeMigrator(ctx context.Context, m *migrate.Migrate, logger *slog.Logger) {
	sourceErr, dbErr := m.Close()
	if sourceErr != nil || dbErr != nil {
		logger.ErrorContext(ctx, "closing migrator", "source_error", sourceErr, "database_error", dbErr)
	}
}
