//go:build integration

// Package storesuite is the cross-store integration suite loam-ejr's
// `task demo:m1` wraps (loam-li0.6, docs/testing-spec.md Layer 2 Store).
// Each store already has its own thorough integration suite --
// internal/reviewstore, internal/codegraph, internal/chunkstore,
// internal/db/migrations -- and this package deliberately does not
// duplicate that coverage. Its job is narrower and different: assemble the
// three things a demo watcher must see live in ONE package, against ONE
// shared container, narrated with t.Logf so a green `-v` run reads as a
// proof instead of a wall of "--- PASS" lines (testify only prints
// assertion messages on FAILURE, so the narration has to be explicit
// t.Logf beats, not doc comments or assert messages -- see this bead's
// brief, "THE OUTPUT PROBLEM"):
//
//  1. UNIQUE(round_id, reviewer) with DERIVED round staleness
//     (internal/reviewstore's real store API, same narrative
//     TestReviewRounds_DerivedStaleness_Narrative proves with more
//     edge-case rigor than this package repeats).
//  2. HNSW nearest-neighbour ordering on seeded vectors, honestly --
//     see hnsw_integration_test.go's doc comment for why an unforced
//     query on this small a table does NOT exercise chunks_embedding
//     (loam-962), and why this suite forces the plan rather than silently
//     demoing a btree-then-sort as if it were the index.
//  3. The dependents recursive CTE terminating on a mutual-recursion cycle
//     (internal/codegraph's real Store.Dependents/Deps, same CYCLE-clause
//     guarded query production runs -- not a hand-rolled stand-in).
//
// Plus the two assertions the bead's DESIGN names that no other package
// covers: migrations apply and revert idempotently (migrations_integration_test.go),
// and work_branches.conflict's transition values are enforced by the real
// schema (conflict_integration_test.go -- see that file's doc comment for
// why this is schema-level only: internal/work_branches, loam-54o.10, has
// not landed, so there is no store method to test a transition THROUGH; see
// this file's DEFERRED-WIP).
//
// Container discipline (per this wave's shared brief): ONE pgvector
// container for this entire test binary, started here in TestMain, never
// one per test. Every parallel test below scopes its own rows to a freshly
// generated repoID and relies on cascading FKs for isolation, the same
// pattern internal/codegraph and internal/chunkstore's TestMain already
// establish.
//
// DEFERRED-WIP: none of this suite's tests un-@wip any features/*.feature
// scenario -- testing-spec Layer 2 Store is infrastructure, matching
// loam-li0.6's own ACCEPTANCE CRITERIA ("no scenario mapping").
// work_branches.conflict STATE-MACHINE enforcement (which transitions are
// legal from which state, and the accompanying work_branches.state
// demotion/promotion git-spec's "Target Advances & Catch-Up" describes) is
// explicitly NOT covered here -- it belongs to whichever store method
// loam-54o.10 (work_branches store, still OPEN as of this bead) adds; see
// conflict_integration_test.go.
package storesuite

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/db"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/testdb"
)

// sharedDSN is the one migrated Postgres container's connection string every
// test in this package runs against.
var sharedDSN string

// sharedPool is built AFTER TestStoreSuite_MigrationsUpDownUp_Idempotent
// completes its down/up cycle, not in TestMain -- rebuilding the pool only
// once the final schema is in place avoids a real hazard: Postgres assigns
// the pgvector "vector" type a fresh OID on every CREATE EXTENSION, so a
// pool whose connections registered the type map against the PRE-down OID
// would silently misdecode embedding columns after the extension is
// dropped and recreated. See migrations_integration_test.go for the
// ordering guarantee that makes this safe (that test is deliberately not
// t.Parallel(), and runs first in file order, so it always finishes before
// any test below reads sharedPool).
var sharedPool *pgxpool.Pool

// TestMain starts one pgvector-enabled Postgres container for the whole
// package, applies the production migration set once so the container is
// usable even if Go's test scheduler ever changed which test runs first,
// runs every test, then tears the container down.
//
// LOAM_DEMO_KEEP_CONTAINER=1 (loam-ejr, `task demo:m1`) skips that teardown
// and instead prints the container's id and resolved DSN to stdout on
// well-known marker lines, so a wrapping shell step can capture them and
// `docker exec` a psql peek at the seeded rows AFTER this process exits --
// testcontainers-go owns the container's lifecycle inside this binary, so
// there is no way to peek from outside while it is still running this
// suite. The container is NOT left to ryuk in this mode: ryuk is disabled
// on this repo's podman setups (TESTCONTAINERS_RYUK_DISABLED=true, see
// Taskfile.yml's test:integration desc), so on a developer machine ryuk
// is not running at all and would never reap it. `task demo:m1` reads the
// printed LOAM_DEMO_CONTAINER_ID line and issues its own `docker rm -f`
// once the psql peek finishes, so the keep flag never causes a real leak
// when driven through the task -- only a manual, non-Taskfile invocation
// of this test binary with the env var set would leak, and that risk is
// documented at the call site (Taskfile.yml).
func TestMain(m *testing.M) {
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	fmt.Println("=== loam-li0.6 store integration suite: starting pgvector container ===")
	container, err := postgres.Run(ctx, testdb.PostgresImage,
		postgres.WithDatabase("loam"),
		postgres.WithUsername("loam"),
		postgres.WithPassword("loam"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting shared pgvector container:", err)
		os.Exit(1)
	}
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolving shared container DSN:", err)
		os.Exit(1)
	}
	if err := migrations.Migrate(ctx, dsn, logger); err != nil {
		fmt.Fprintln(os.Stderr, "migrating shared container:", err)
		os.Exit(1)
	}
	sharedDSN = dsn
	fmt.Println("=== pgvector container ready, schema migrated up ===")
	code := m.Run()
	if sharedPool != nil {
		sharedPool.Close()
	}
	if os.Getenv("LOAM_DEMO_KEEP_CONTAINER") == "1" {
		fmt.Println("=== LOAM_DEMO_KEEP_CONTAINER=1: skipping teardown for the psql peek ===")
		fmt.Println("LOAM_DEMO_CONTAINER_ID=" + container.GetContainerID())
		fmt.Println("LOAM_DEMO_DSN=" + dsn)
		os.Exit(code)
	}
	if err := container.Terminate(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "terminating shared pgvector container:", err)
	}
	os.Exit(code)
}

// testLogger returns an io.Discard slog logger, per repo test convention.
func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// insertRepo inserts a minimal repos row and returns its id, the FK every
// table this suite touches (work_branches, chunks, symbols) needs.
func insertRepo(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO repos (id, name, upstream_url, forge_host, indexed_branch)
		 VALUES (gen_random_uuid(), $1, 'https://example.com/repo.git', 'example.com', 'main')
		 RETURNING id`,
		name,
	).Scan(&id))
	return id
}

// pgUUID adapts a uuid.UUID to the pgtype.UUID the raw SQL in this package
// binds against, mirroring every store package's own pgUUID helper.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// mustPool returns sharedPool, failing the test loudly (rather than a nil
// pointer panic) if it is somehow read before
// TestStoreSuite_MigrationsUpDownUp_Idempotent has finished building it --
// which would indicate the file-order/parallel-scheduling assumption this
// package depends on has broken.
func mustPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if sharedPool == nil {
		t.Fatal("sharedPool is nil: TestStoreSuite_MigrationsUpDownUp_Idempotent must run (and finish) before any parallel test in this package reads sharedPool")
	}
	return sharedPool
}

// buildRegisteredPool constructs a pool via internal/db.NewPool, the same
// production path every store's own integration tests use, so pgvector's
// codec is genuinely registered via AfterConnect (needed for chunkstore's
// Search and the raw ARRAY-scan-shaped queries this suite runs).
func buildRegisteredPool(ctx context.Context, dsn string, logger *slog.Logger) (*pgxpool.Pool, error) {
	return db.NewPool(ctx, db.Config{DatabaseURL: dsn}, logger)
}

// connectPlain opens a short-lived, unregistered pool for schema-shape
// checks (information_schema / pg_extension) that never touch a vector
// column, so callers that only need to confirm a table exists or is gone
// do not depend on pgvector registration succeeding -- which matters
// specifically around a Down/Migrate cycle, since the vector type may not
// exist at all for part of that cycle.
func connectPlain(ctx context.Context, t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	return pool
}
