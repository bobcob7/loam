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

// errEmptyDSN is returned when Migrate or Down is called with an empty DSN,
// so the failure is immediate and legible instead of surfacing deep inside
// pgx's connection-string parser.
var errEmptyDSN = errors.New("dsn is required")

// Migrate applies every pending migration embedded under files/ to the
// database at dsn and reports the resulting version. golang-migrate manages
// its own connection (opened from dsn, independent of any *pgxpool.Pool the
// caller may also build) and its own schema_migrations bookkeeping table,
// so Migrate has no dependency on pool construction and can run before or
// after it. It is idempotent: run again against an already-current
// database, it is a no-op.
func Migrate(ctx context.Context, dsn string, logger *slog.Logger) error {
	m, db, err := newMigrator(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
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

// Down reverts every applied migration embedded under files/ against the
// database at dsn, running each NNNN_name.down.sql in reverse order. It
// shares newMigrator's source/database driver wiring with Migrate rather
// than reconstructing it a second time, so callers that need a real down
// migration -- production rollback tooling, and this bead's own
// migrations-are-idempotent integration proof (loam-li0.6, testing-spec
// Layer 2 Store: "migrations apply cleanly up AND down and are idempotent")
// -- do not each hand-roll the golang-migrate instance a package-external
// test previously had to (internal/db/migrations/integration_test.go's
// migrateDown, now just a thin call to Down). Idempotent the same way
// Migrate is: reverting an already-empty schema is migrate.ErrNoChange, not
// an error. Migrate has no "stop after N" mode and neither does Down -- both
// always run the full chain (loam-maq tracks per-migration target control as
// separate, follow-up work).
func Down(ctx context.Context, dsn string, logger *slog.Logger) error {
	m, db, err := newMigrator(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	defer closeMigrator(ctx, m, logger)
	logger.InfoContext(ctx, "reverting migrations")
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("reverting migrations: %w", err)
	}
	logger.InfoContext(ctx, "migrations reverted")
	return nil
}

// newMigrator opens dsn and wires the embedded iofs source driver plus the
// pgx/v5 database driver Migrate and Down both need, returning a
// migrate.Migrate ready for Up() or Down() alongside the *sql.DB the caller
// must also close. Each call opens its own connection, independent of any
// *pgxpool.Pool the caller may separately build.
func newMigrator(ctx context.Context, dsn string) (*migrate.Migrate, *sql.DB, error) {
	if dsn == "" {
		return nil, nil, fmt.Errorf("migrating: %w", errEmptyDSN)
	}
	sourceDriver, err := iofs.New(migrationFiles, migrationsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("opening embedded migration source: %w", err)
	}
	db, err := sql.Open("pgx/v5", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("opening migration database connection: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("connecting to migration database: %w", err)
	}
	databaseDriver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("creating migration database driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", sourceDriver, "pgx5", databaseDriver)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("constructing migrator: %w", err)
	}
	return m, db, nil
}

// closeMigrator closes the migrator's source and database drivers. Failures
// are logged rather than swallowed: Migrate/Down have already returned by
// the time this runs, so there is no error path left to report through.
func closeMigrator(ctx context.Context, m *migrate.Migrate, logger *slog.Logger) {
	sourceErr, dbErr := m.Close()
	if sourceErr != nil || dbErr != nil {
		logger.ErrorContext(ctx, "closing migrator", "source_error", sourceErr, "database_error", dbErr)
	}
}
