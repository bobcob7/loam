package db

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// poolOnlyParams is every parameter pgxpool.ParseConfig consumes for itself
// in the pgx version currently pinned in go.mod. It exists ONLY to give the
// test below something to enumerate; MigrationDSN itself never sees this
// list, and deliberately so -- see its doc comment. A future pgx that adds a
// pool parameter is handled by the implementation without this list being
// touched; the list going stale costs coverage of the new key, not
// correctness.
var poolOnlyParams = []string{
	"pool_max_conns",
	"pool_min_conns",
	"pool_min_idle_conns",
	"pool_max_conn_lifetime",
	"pool_max_conn_idle_time",
	"pool_health_check_period",
	"pool_max_conn_lifetime_jitter",
}

// TestMigrationDSNStripsEveryPoolParameter is the regression proof for
// loam-lhc9: each parameter pgxpool owns must be gone from the DSN handed to
// database/sql, because pgx's plain parser would file it into RuntimeParams
// and the server would reject the startup packet with SQLSTATE 42704.
// Asserting on the PARSED RuntimeParams rather than on the string means the
// test is checking the property that actually matters (what gets sent to the
// server), not the spelling of the rewrite.
func TestMigrationDSNStripsEveryPoolParameter(t *testing.T) {
	t.Parallel()
	for _, param := range poolOnlyParams {
		t.Run(param, func(t *testing.T) {
			t.Parallel()
			value := "8"
			if param != "pool_max_conns" && param != "pool_min_conns" && param != "pool_min_idle_conns" {
				value = "30s"
			}
			dsn := "postgres://loam:secret@db.example.com:5432/loam?sslmode=disable&" + param + "=" + value
			stripped, err := MigrationDSN(dsn)
			require.NoError(t, err)
			cfg, err := pgx.ParseConfig(stripped)
			require.NoError(t, err)
			assert.NotContains(t, cfg.RuntimeParams, param, "%s must not reach the server as a startup option", param)
		})
	}
}

// TestMigrationDSNKeepsNonPoolRuntimeParams is the discriminating half:
// stripping "every parameter pgx does not recognize" would also be a fix for
// the reported symptom, and would be wrong -- application_name and friends
// are genuine server settings the operator meant to set. Only the keys
// pgxpool claims may go.
func TestMigrationDSNKeepsNonPoolRuntimeParams(t *testing.T) {
	t.Parallel()
	dsn := "postgres://loam:secret@db.example.com:5432/loam?sslmode=disable&application_name=loam&search_path=public&pool_max_conns=8"
	stripped, err := MigrationDSN(dsn)
	require.NoError(t, err)
	cfg, err := pgx.ParseConfig(stripped)
	require.NoError(t, err)
	assert.Equal(t, "loam", cfg.RuntimeParams["application_name"])
	assert.Equal(t, "public", cfg.RuntimeParams["search_path"])
	assert.NotContains(t, cfg.RuntimeParams, "pool_max_conns")
}

// TestMigrationDSNPreservesConnectionTarget proves the rewrite changed
// nothing an operator cares about: same host, port, database, user, and --
// the one most likely to be corrupted by naive string surgery -- password.
func TestMigrationDSNPreservesConnectionTarget(t *testing.T) {
	t.Parallel()
	dsn := "postgres://loam:p%40ss%2Fword@db.example.com:6543/loamdb?sslmode=disable&pool_max_conns=8&pool_max_conn_lifetime=1h"
	stripped, err := MigrationDSN(dsn)
	require.NoError(t, err)
	cfg, err := pgx.ParseConfig(stripped)
	require.NoError(t, err)
	assert.Equal(t, "db.example.com", cfg.Host)
	assert.Equal(t, uint16(6543), cfg.Port)
	assert.Equal(t, "loamdb", cfg.Database)
	assert.Equal(t, "loam", cfg.User)
	assert.Equal(t, "p@ss/word", cfg.Password)
}

// TestMigrationDSNLeavesPoolFreeDSNUntouched pins the cheap path: a DSN with
// no pgxpool parameters is returned byte-for-byte, so the overwhelmingly
// common configuration never depends on the rewriter being faithful and
// never has its query re-ordered under the operator.
func TestMigrationDSNLeavesPoolFreeDSNUntouched(t *testing.T) {
	t.Parallel()
	tests := []string{
		"postgres://loam:secret@db.example.com:5432/loam?sslmode=disable&application_name=loam",
		"postgres://loam@db/loam",
		"host=db.example.com user=loam password='s e c r e t' dbname=loam sslmode=disable",
	}
	for _, dsn := range tests {
		t.Run(dsn, func(t *testing.T) {
			t.Parallel()
			stripped, err := MigrationDSN(dsn)
			require.NoError(t, err)
			assert.Equal(t, dsn, stripped)
		})
	}
}

// TestMigrationDSNKeywordValueForm covers the other DSN shape pgx accepts
// and internal/config explicitly supports (see its
// TestLoad_DatabaseURLKeywordValueForm). The quoted password carries a space
// and an escaped quote, which is exactly what a rewriter that re-serialized
// values by its own rules would get wrong.
func TestMigrationDSNKeywordValueForm(t *testing.T) {
	t.Parallel()
	dsn := `host=db.example.com port=6543 user=loam password='s e\'cret' dbname=loam sslmode=disable pool_max_conns=8 application_name=loam`
	stripped, err := MigrationDSN(dsn)
	require.NoError(t, err)
	cfg, err := pgx.ParseConfig(stripped)
	require.NoError(t, err)
	assert.NotContains(t, stripped, "pool_max_conns")
	assert.Equal(t, "db.example.com", cfg.Host)
	assert.Equal(t, uint16(6543), cfg.Port)
	assert.Equal(t, "loam", cfg.User)
	assert.Equal(t, `s e'cret`, cfg.Password)
	assert.Equal(t, "loam", cfg.Database)
	assert.Equal(t, "loam", cfg.RuntimeParams["application_name"])
	assert.NotContains(t, cfg.RuntimeParams, "pool_max_conns")
}

// TestMigrationDSNDoesNotChangeWhatThePoolSees is the other half of the
// split this bead is about: MigrationDSN is not allowed to be implemented by
// mutating the string the pool gets. The operator's pool sizing must survive
// on the pgxpool side, or "the server boots" would have been bought by
// silently ignoring their configuration.
func TestMigrationDSNDoesNotChangeWhatThePoolSees(t *testing.T) {
	t.Parallel()
	dsn := "postgres://loam:secret@db.example.com:5432/loam?sslmode=disable&pool_max_conns=17"
	_, err := MigrationDSN(dsn)
	require.NoError(t, err)
	poolCfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	assert.Equal(t, int32(17), poolCfg.MaxConns)
}

// TestMigrationDSNRejectsUnusableDSN checks the failure surfaces here, at
// config time, rather than as a connection error later: an unparseable DSN
// and a pool parameter pgxpool itself refuses both stop the boot with an
// error naming the URL.
func TestMigrationDSNRejectsUnusableDSN(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		dsn  string
	}{
		{name: "not a dsn", dsn: "not a dsn at all"},
		{name: "unparseable pool value", dsn: "postgres://db/loam?pool_max_conns=lots"},
		{name: "pool_max_conns too small", dsn: "postgres://db/loam?pool_max_conns=0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stripped, err := MigrationDSN(tt.dsn)
			require.Error(t, err)
			assert.Empty(t, stripped)
			assert.Contains(t, err.Error(), "parsing database url")
		})
	}
}

// TestMigrationDSNDoesNotLeakPassword guards the operator-facing surface of
// the error path: MigrationDSN's errors are logged at boot, and pgx redacts
// the password in its own ParseConfigError, so this package must not undo
// that by wrapping the raw DSN back in.
func TestMigrationDSNDoesNotLeakPassword(t *testing.T) {
	t.Parallel()
	_, err := MigrationDSN("postgres://loam:hunter2@db.example.com:5432/loam?pool_max_conns=lots")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2")
}
