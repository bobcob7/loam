package migrations

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
)

// migrationsTable is the bookkeeping table golang-migrate's pgx/v5
// database driver writes the applied version and dirty flag into. It is
// the driver's own DefaultMigrationsTable, which newMigrator gets by
// passing an empty &pgxmigrate.Config{} -- so this constant and that
// zero-value config must stay in agreement. Naming it here, in the same
// package that builds that config, is what keeps the agreement local:
// nothing outside this package needs to know the table exists at all.
const migrationsTable = "schema_migrations"

var (
	// errSchemaNotCurrent means the database's applied migration version
	// and this binary's embedded migration set disagree in either
	// direction -- the database is BEHIND (a migration this binary ships
	// has not been applied) or AHEAD (something migrated it past what
	// this binary understands). Both are "migrations not current" for
	// docs/server-spec.md -> Health's purposes: this process cannot
	// honestly claim to be serving the schema it was built against.
	errSchemaNotCurrent = errors.New("migration version does not match this binary's embedded migration set")
	// errSchemaDirty means golang-migrate recorded a migration that
	// started and never finished. The schema is in an unknown
	// intermediate state and needs an operator (`migrate force`), not a
	// retry.
	errSchemaDirty = errors.New("migration state is dirty: a migration failed part-way and was never repaired")
)

// SchemaCheck answers "does the database behind q have exactly the
// migration set this binary embeds, cleanly applied?" -- the migration
// half of docs/server-spec.md -> Health's readiness definition
// ("Postgres reachable and migrations current").
//
// It is deliberately a per-call read of the live database rather than a
// value captured at startup: startup's own Migrate call proves the schema
// was current THEN, which is exactly the claim a readiness endpoint is not
// allowed to make. A schema can go dirty or move underneath a running
// process (a second migrator, an operator, a restored backup), and a
// readiness probe that cached startup's answer would keep reporting ready
// through all of it.
type SchemaCheck struct {
	q Querier
}

// NewSchemaCheck binds a SchemaCheck to q, typically the process's live
// *pgxpool.Pool.
func NewSchemaCheck(q Querier) SchemaCheck {
	return SchemaCheck{q: q}
}

// CheckSchema returns nil when the database is at exactly the embedded
// migration set's highest version and not dirty, and a describing error
// otherwise. Callers are expected to log that error rather than serve it
// verbatim: it names versions and can wrap a driver error.
func (c SchemaCheck) CheckSchema(ctx context.Context) error {
	expected, err := embeddedVersion()
	if err != nil {
		return err
	}
	var applied int64
	var dirty bool
	//nolint:gosec // migrationsTable is a package constant, not caller input.
	row := c.q.QueryRow(ctx, "SELECT version, dirty FROM "+migrationsTable)
	if err := row.Scan(&applied, &dirty); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: nothing has been applied, this binary embeds up to version %d", errSchemaNotCurrent, expected)
		}
		return fmt.Errorf("reading %s: %w", migrationsTable, err)
	}
	if dirty {
		return fmt.Errorf("%w: version %d", errSchemaDirty, applied)
	}
	if applied != int64(expected) {
		return fmt.Errorf("%w: database is at version %d, this binary embeds up to version %d", errSchemaNotCurrent, applied, expected)
	}
	return nil
}

// embeddedVersion reports the highest version in the embedded migration
// set, read through the SAME iofs source driver newMigrator hands
// golang-migrate rather than by re-parsing filenames: the number this
// compares against must be the number migrate itself would have applied,
// and reusing the driver is what guarantees that (loam-54o.2's harness).
//
// It is recomputed per call rather than memoized in package state. The
// walk is over an in-memory embed.FS with a handful of entries -- orders
// of magnitude cheaper than the database round trip it accompanies -- and
// package-level mutable state is against this repo's Go standards.
func embeddedVersion() (uint, error) {
	source, err := iofs.New(migrationFiles, migrationsDir)
	if err != nil {
		return 0, fmt.Errorf("opening embedded migration source: %w", err)
	}
	defer source.Close()
	version, err := source.First()
	if err != nil {
		return 0, fmt.Errorf("reading the first embedded migration: %w", err)
	}
	for {
		next, err := source.Next(version)
		if errors.Is(err, fs.ErrNotExist) {
			return version, nil
		}
		if err != nil {
			return 0, fmt.Errorf("walking embedded migrations from version %d: %w", version, err)
		}
		version = next
	}
}
