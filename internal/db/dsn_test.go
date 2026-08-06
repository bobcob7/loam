package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// poolMaxConns is the pool size every test in this package configures, and
// the number is load-bearing rather than arbitrary: pgxpool's default
// MaxConns is max(4, runtime.NumCPU()), so asserting a value a default could
// also produce makes the assertion pass whether or not the parameter
// survived. 17 would be a live example -- a 17-core runner would green the
// test with the parameter silently discarded. 97 is not a plausible core
// count on any machine this suite meets.
//
// Shared from this file, which carries no build tag and is therefore
// compiled into the integration build too, so the reason lives at one site
// instead of being restated (or forgotten) next door.
const poolMaxConns = 97

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

// TestMigrationDSNPostgresqlScheme covers the second URL scheme. pgconn
// dispatches on `postgres://` OR `postgresql://` (config.go:331) and
// internal/config/env.go:274 accepts both, so stripParams mirrors that test
// -- but a mirror of a two-armed condition with only one arm exercised is a
// claim, not a proof: dropping the postgresql:// arm sends the DSN to the
// keyword/value tokenizer, which produces a string that no longer parses to
// the same connection.
func TestMigrationDSNPostgresqlScheme(t *testing.T) {
	t.Parallel()
	dsn := fmt.Sprintf("postgresql://loam:secret@db.example.com:5432/loam?sslmode=disable&application_name=loam&pool_max_conns=%d", poolMaxConns)
	stripped, err := MigrationDSN(dsn)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(stripped, "postgresql://"), "the scheme the operator wrote must survive: %s", stripped)
	cfg, err := pgx.ParseConfig(stripped)
	require.NoError(t, err)
	assert.NotContains(t, cfg.RuntimeParams, "pool_max_conns")
	assert.Equal(t, "loam", cfg.RuntimeParams["application_name"])
	assert.Equal(t, "db.example.com", cfg.Host)
	assert.Equal(t, "secret", cfg.Password)
}

// TestMigrationDSNKeywordValueWhitespaceAroundEquals is the other untested
// parity claim. pgconn trims asciiSpace off both sides of the `=`
// (config.go:699-700), so `pool_max_conns = 8` is the same parameter as
// `pool_max_conns=8` -- a tokenizer that only recognized the tight form
// would leave the parameter in place and the boot would still fail, with the
// operator's DSN looking, to them, exactly like the one in the docs.
func TestMigrationDSNKeywordValueWhitespaceAroundEquals(t *testing.T) {
	t.Parallel()
	dsn := fmt.Sprintf("host = db.example.com  port = 6543  user = loam  pool_max_conns = %d  dbname = loam  sslmode = disable", poolMaxConns)
	stripped, err := MigrationDSN(dsn)
	require.NoError(t, err)
	cfg, err := pgx.ParseConfig(stripped)
	require.NoError(t, err)
	assert.NotContains(t, cfg.RuntimeParams, "pool_max_conns")
	assert.Equal(t, "db.example.com", cfg.Host)
	assert.Equal(t, uint16(6543), cfg.Port)
	assert.Equal(t, "loam", cfg.User)
	assert.Equal(t, "loam", cfg.Database)
	poolCfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	assert.Equal(t, int32(poolMaxConns), poolCfg.MaxConns, "the spaced form must still be the parameter pgxpool consumes")
}

// TestMigrationDSNKeywordValueBackslashEscapes covers the OTHER escaping
// rule in libpq's keyword/value form: an UNQUOTED value may carry a
// backslash-escaped space, and a tokenizer that stopped at the first space
// would cut the password in half and then read its own tail as the next
// keyword. That mutant is not caught by the quoted-value case above, which
// exercises a different branch.
// The pool parameter sits IMMEDIATELY AFTER the escaped value on purpose. A
// tokenizer that stopped at the escaped space would resume mid-value and
// read `cret pool_max_conns` as one keyword -- which no longer matches
// `pool_max_conns`, so the parameter would be kept and the boot would still
// fail. Put the pool parameter anywhere else in the string and that mutant
// survives, because every token is either kept or dropped as raw text and
// the rejoined result comes out identical.
func TestMigrationDSNKeywordValueBackslashEscapes(t *testing.T) {
	t.Parallel()
	dsn := `host=db.example.com user=loam password=se\ cret pool_max_conns=8 dbname=loam sslmode=disable`
	stripped, err := MigrationDSN(dsn)
	require.NoError(t, err)
	cfg, err := pgx.ParseConfig(stripped)
	require.NoError(t, err)
	// `se\ cret`, backslash included, is what pgx itself yields here: its
	// unquoted-value scanner consumes the escaped space so the token does
	// not end there, but only unescapes \\ and \'. The expectation is
	// therefore pgx's answer, not libpq's -- what MigrationDSN owes is that
	// the stripped DSN parses to the SAME thing the original did, and
	// preserving the token's raw bytes is how it gets there.
	assert.Equal(t, `se\ cret`, cfg.Password)
	assert.Equal(t, "loam", cfg.Database)
	assert.NotContains(t, cfg.RuntimeParams, "pool_max_conns")
}

// TestMigrationDSNDoesNotChangeWhatThePoolSees is the other half of the
// split this bead is about: MigrationDSN is not allowed to be implemented by
// mutating the string the pool gets. The operator's pool sizing must survive
// on the pgxpool side, or "the server boots" would have been bought by
// silently ignoring their configuration.
//
// poolMaxConns rather than a round number, for the reason given at its
// declaration: any value pgxpool's max(4, NumCPU) default could also produce
// would make this assertion pass on a machine of that size even if the
// parameter had been thrown away.
func TestMigrationDSNDoesNotChangeWhatThePoolSees(t *testing.T) {
	t.Parallel()
	dsn := fmt.Sprintf("postgres://loam:secret@db.example.com:5432/loam?sslmode=disable&pool_max_conns=%d", poolMaxConns)
	_, err := MigrationDSN(dsn)
	require.NoError(t, err)
	poolCfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	assert.Equal(t, int32(poolMaxConns), poolCfg.MaxConns)
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

// TestVerifyStrippedRejectsARewriteThatMovedTheTarget exercises MigrationDSN's
// last-line guard directly, since no input to MigrationDSN can currently
// trigger it: the guard's whole job is to catch a future bug in the rewriter
// above it, so if it were quietly inert nothing else in this file would
// notice. Each case is a way string surgery could plausibly go wrong -- a
// dropped password, a mangled host, a runtime parameter lost along with the
// pool ones -- and each must be refused rather than returned.
func TestVerifyStrippedRejectsARewriteThatMovedTheTarget(t *testing.T) {
	t.Parallel()
	const original = "postgres://loam:secret@db.example.com:5432/loam?sslmode=disable&application_name=loam&pool_max_conns=8"
	want, err := pgxpool.ParseConfig(original)
	require.NoError(t, err)
	tests := []struct {
		name      string
		stripped  string
		wantError bool
	}{
		{
			name:     "faithful rewrite",
			stripped: "postgres://loam:secret@db.example.com:5432/loam?application_name=loam&sslmode=disable",
		},
		{
			name:      "password dropped",
			stripped:  "postgres://loam@db.example.com:5432/loam?application_name=loam&sslmode=disable",
			wantError: true,
		},
		{
			name:      "host changed",
			stripped:  "postgres://loam:secret@other.example.com:5432/loam?application_name=loam&sslmode=disable",
			wantError: true,
		},
		{
			name:      "database changed",
			stripped:  "postgres://loam:secret@db.example.com:5432/other?application_name=loam&sslmode=disable",
			wantError: true,
		},
		{
			name:      "port changed",
			stripped:  "postgres://loam:secret@db.example.com:6543/loam?application_name=loam&sslmode=disable",
			wantError: true,
		},
		{
			name:      "user changed",
			stripped:  "postgres://other:secret@db.example.com:5432/loam?application_name=loam&sslmode=disable",
			wantError: true,
		},
		{
			name:      "non-pool runtime parameter lost",
			stripped:  "postgres://loam:secret@db.example.com:5432/loam?sslmode=disable",
			wantError: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := verifyStripped(tt.stripped, want.ConnConfig)
			if !tt.wantError {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, errDSNRewriteChangedTarget)
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
