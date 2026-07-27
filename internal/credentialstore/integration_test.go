//go:build integration

// Requires a real Docker (or Docker-API-compatible) daemon. Run with:
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/credentialstore/... -v
//
// (see internal/db/migrations/integration_test.go for why TESTCONTAINERS_RYUK_DISABLED
// is a podman-only workaround, not a CI setting).
//
// These tests apply the REAL migration set (migrations.Migrate against
// 0001_init.up.sql), so credentials_host_key is the actual constraint
// Postgres enforces, and every encryption round trip goes through the real
// internal/crypto.Encryptor (AES-256-GCM) -- not a fake stand-in the way
// store_test.go's xorEncryptor is for fast, database-free unit tests.
package credentialstore

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/crypto"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/testdb"
)

// sharedDSN is the one migrated Postgres every test in this package runs
// against, started once in TestMain rather than one container per test --
// the same fix this bead's sibling stores (chunkstore, codegraph) already
// use under this same shared build machine's container contention: fewer
// concurrent testcontainers, not a shortcut on coverage. Isolation between
// tests comes from each seeding its own uniquely-named host rather than
// from separate databases.
var sharedDSN string

// TestMain starts one Postgres container, applies the production migration
// set, and hands every test in this package the same DSN, tearing the
// container down once after the whole package's tests finish. Exactly one
// container is ever running at a time for this package.
func TestMain(m *testing.M) {
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	container, err := postgres.Run(ctx, testdb.PostgresImage,
		postgres.WithDatabase("loam"),
		postgres.WithUsername("loam"),
		postgres.WithPassword("loam"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting shared postgres container:", err)
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
	code := m.Run()
	if err := container.Terminate(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "terminating shared postgres container:", err)
	}
	os.Exit(code)
}

// newTestPool connects a plain pgxpool.Pool to the shared migrated
// container -- credentials needs no pgvector type registration, unlike
// chunkstore's sibling helper.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, sharedDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// testEncryptorKey returns a distinctive (non-zero, non-repeating) 32-byte
// AES-256 key so tests can prove decryption with the WRONG key fails,
// rather than two all-zero keys accidentally comparing equal.
func testEncryptorKey(seed byte) []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = seed + byte(i)
	}
	return key
}

func newTestEncryptor(t *testing.T, seed byte) *crypto.Encryptor {
	t.Helper()
	enc, err := crypto.NewEncryptor(testEncryptorKey(seed))
	require.NoError(t, err)
	return enc
}

// rawTokenCiphertext reads credentials.token_ciphertext directly off the
// real table for host, bypassing the store entirely -- so a test can prove
// what is actually persisted at rest, not just what the store's own
// decrypt path reports back.
func rawTokenCiphertext(ctx context.Context, t *testing.T, pool *pgxpool.Pool, host string) []byte {
	t.Helper()
	var ciphertext []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT token_ciphertext FROM credentials WHERE host = $1`, host).Scan(&ciphertext))
	return ciphertext
}

// rawRowCount counts credentials rows for host directly, bypassing the
// store -- proves an upsert replaced in place rather than inserting a
// second row.
func rawRowCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, host string) int {
	t.Helper()
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM credentials WHERE host = $1`, host).Scan(&count))
	return count
}

// TestUpsertToken_GetByHost_RoundTrip_RealDB is this bead's central
// acceptance: a plaintext token written through UpsertToken round-trips
// back out through GetByHost as the identical plaintext, via the real
// AES-GCM encryptor and a real Postgres row -- and the raw
// token_ciphertext column, read directly (not through the store), never
// equals the plaintext that was written.
func TestUpsertToken_GetByHost_RoundTrip_RealDB(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	enc := newTestEncryptor(t, 0x01)
	s := New(pool, enc, testLogger())
	const host = "github.com-roundtrip"
	const plaintext = "ghp_thisIsARealLookingTokenValue123"

	status, err := s.UpsertToken(ctx, host, plaintext)
	require.NoError(t, err)
	assert.True(t, status.HasToken)
	assert.False(t, status.Validated, "a freshly written token is not validated until SetValidated is called")

	cred, err := s.GetByHost(ctx, host)
	require.NoError(t, err)
	assert.Equal(t, plaintext, cred.Token, "the store's decrypted token must match what was written")
	assert.Equal(t, host, cred.Host)

	raw := rawTokenCiphertext(ctx, t, pool, host)
	assert.NotEqual(t, []byte(plaintext), raw, "the raw column must never hold the plaintext token")
	assert.NotContains(t, string(raw), plaintext, "the plaintext token must not even appear as a substring of the raw ciphertext bytes")
}

// TestUpsertToken_Reupsert_ReplacesNotDuplicates proves credentials_host_key
// (UNIQUE(host)) is honored: calling UpsertToken twice for the same host
// replaces the token in place -- exactly one row exists in the real table
// afterward, and GetByHost decrypts the LATEST token, not the first.
func TestUpsertToken_Reupsert_ReplacesNotDuplicates(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	enc := newTestEncryptor(t, 0x02)
	s := New(pool, enc, testLogger())
	const host = "github.com-reupsert"

	_, err := s.UpsertToken(ctx, host, "first-token-value")
	require.NoError(t, err)
	_, err = s.UpsertToken(ctx, host, "second-token-value")
	require.NoError(t, err)

	assert.Equal(t, 1, rawRowCount(ctx, t, pool, host), "re-upserting the same host must not create a second row")
	cred, err := s.GetByHost(ctx, host)
	require.NoError(t, err)
	assert.Equal(t, "second-token-value", cred.Token, "the latest upsert's token must be what decrypts back out")
}

// TestDecrypt_WrongKey_FailsCleanly proves that reading a real ciphertext
// written under one key, then decrypting it with a DIFFERENT key, fails
// with a clean error rather than returning garbage plaintext or panicking
// -- AES-GCM's authentication tag check is what makes this possible.
func TestDecrypt_WrongKey_FailsCleanly(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	writeEnc := newTestEncryptor(t, 0x03)
	const host = "github.com-wrongkey"
	writer := New(pool, writeEnc, testLogger())
	_, err := writer.UpsertToken(ctx, host, "a-real-secret-token")
	require.NoError(t, err)

	wrongKeyEnc := newTestEncryptor(t, 0x99)
	reader := New(pool, wrongKeyEnc, testLogger())
	cred, err := reader.GetByHost(ctx, host)
	require.Error(t, err, "decrypting with the wrong key must fail, not silently return garbage")
	assert.Empty(t, cred.Token)
}

// TestGetStatus_ListStatuses_ReflectPresenceAndValidated_WithoutDecrypting
// proves has_token and validated are correct against the real schema for
// both a host with a token and one seeded with a NULL token_ciphertext
// directly (bypassing the store, since UpsertToken always writes a
// non-null ciphertext) -- and that these reads never touch decryption: an
// encryptor whose Decrypt panics is passed in, and no panic occurs.
func TestGetStatus_ListStatuses_ReflectPresenceAndValidated_WithoutDecrypting(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	enc := &panicOnDecryptEncryptor{Encryptor: newTestEncryptor(t, 0x04)}
	s := New(pool, enc, testLogger())
	const hostWithToken = "github.com-status-with-token"
	const hostWithoutToken = "github.com-status-without-token"

	_, err := s.UpsertToken(ctx, hostWithToken, "some-token")
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO credentials (id, host, token_ciphertext, validated) VALUES (gen_random_uuid(), $1, NULL, false)`,
		hostWithoutToken)
	require.NoError(t, err)

	statusWith, err := s.GetStatus(ctx, hostWithToken)
	require.NoError(t, err)
	assert.True(t, statusWith.HasToken)

	statusWithout, err := s.GetStatus(ctx, hostWithoutToken)
	require.NoError(t, err)
	assert.False(t, statusWithout.HasToken, "a row with a NULL token_ciphertext must report has_token = false")

	all, err := s.ListStatuses(ctx)
	require.NoError(t, err)
	byHost := make(map[string]CredentialStatus, len(all))
	for _, st := range all {
		byHost[st.Host] = st
	}
	require.Contains(t, byHost, hostWithToken)
	require.Contains(t, byHost, hostWithoutToken)
	assert.True(t, byHost[hostWithToken].HasToken)
	assert.False(t, byHost[hostWithoutToken].HasToken)
}

// panicOnDecryptEncryptor wraps a real *crypto.Encryptor but panics if
// Decrypt is ever called, so a test can prove a code path (GetStatus,
// ListStatuses) never decrypts -- a mutation that made either method
// decrypt "just in case" would panic this test instead of passing it
// silently.
type panicOnDecryptEncryptor struct {
	*crypto.Encryptor
}

func (e *panicOnDecryptEncryptor) Decrypt(_ []byte) ([]byte, error) {
	panic("Decrypt must never be called by GetStatus or ListStatuses")
}

// TestSetValidated_UpdatesFlag_RealDB proves SetValidated flips the flag
// against the real table, and leaves token_ciphertext untouched.
func TestSetValidated_UpdatesFlag_RealDB(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	enc := newTestEncryptor(t, 0x05)
	s := New(pool, enc, testLogger())
	const host = "github.com-setvalidated"
	_, err := s.UpsertToken(ctx, host, "token-for-validation-test")
	require.NoError(t, err)
	before := rawTokenCiphertext(ctx, t, pool, host)

	status, err := s.SetValidated(ctx, host, true)
	require.NoError(t, err)
	assert.True(t, status.Validated)

	after := rawTokenCiphertext(ctx, t, pool, host)
	assert.Equal(t, before, after, "SetValidated must never touch token_ciphertext")

	status, err = s.GetStatus(ctx, host)
	require.NoError(t, err)
	assert.True(t, status.Validated)
}

// TestReupsert_ResetsValidated_RealDB pins that replacing a host's token
// clears validated. The flag describes ONE token, not the host, so a
// freshly-written token must never inherit the previous token's verdict:
// otherwise a caller whose re-validation errors out before it can call
// SetValidated(false) leaves GetStatus reporting validated:true for a
// token nothing ever checked. Without the ON CONFLICT clause's
// `validated = false`, this test is the only thing that fails.
func TestReupsert_ResetsValidated_RealDB(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	enc := newTestEncryptor(t, 0x09)
	s := New(pool, enc, testLogger())
	const host = "github.com-reupsert-resets-validated"
	_, err := s.UpsertToken(ctx, host, "first-token")
	require.NoError(t, err)

	status, err := s.SetValidated(ctx, host, true)
	require.NoError(t, err)
	require.True(t, status.Validated, "precondition: the first token is marked validated")

	_, err = s.UpsertToken(ctx, host, "second-token")
	require.NoError(t, err)

	status, err = s.GetStatus(ctx, host)
	require.NoError(t, err)
	assert.False(t, status.Validated, "replacing the token must reset validated -- the flag describes the token, not the host")
	assert.True(t, status.HasToken, "the replacement token is still present")

	got, err := s.GetByHost(ctx, host)
	require.NoError(t, err)
	assert.Equal(t, "second-token", got.Token, "the replacement token must round-trip")
}

// TestGetByHost_UnknownHost_ReturnsErrNotFound proves the distinguishable
// errNotFound surfaces against the real schema, not a bare pgx.ErrNoRows.
func TestGetByHost_UnknownHost_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	enc := newTestEncryptor(t, 0x06)
	s := New(pool, enc, testLogger())
	_, err := s.GetByHost(ctx, "never-enrolled.example.com")
	require.Error(t, err)
	assert.ErrorIs(t, err, errNotFound)
}

// TestCredentialsHostUniqueConstraint_EnforcedByRealSchema bypasses the
// store entirely and inserts two credentials rows for the same host
// directly, proving credentials_host_key is a real constraint the applied
// migration creates -- not an assumption this package's tests could pass
// against a schema that silently dropped it.
func TestCredentialsHostUniqueConstraint_EnforcedByRealSchema(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool := newTestPool(t)
	const host = "github.com-raw-constraint"
	_, err := pool.Exec(ctx, `INSERT INTO credentials (id, host, token_ciphertext) VALUES (gen_random_uuid(), $1, NULL)`, host)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `INSERT INTO credentials (id, host, token_ciphertext) VALUES (gen_random_uuid(), $1, NULL)`, host)
	require.Error(t, err, "a second raw insert for the same host must be rejected by the real schema")
}
