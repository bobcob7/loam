//go:build integration

// Requires a real Docker (or Docker-API-compatible) daemon; see
// main_integration_test.go's package doc for how to run this file (same
// package, same build tag -- it reuses newPostgres and testEncryptionKey
// from there and seedEncryptedCredential from
// credentialcheck_integration_test.go). On podman also set
// TESTCONTAINERS_RYUK_DISABLED=true.
//
// This file is loam-7dkc's proof, and the case it exercises is deliberately
// NOT startup. Every existing test of these two surfaces boots a process
// against a database that is already broken, which cannot distinguish "the
// connection was never made" from "the connection cannot be REMADE" -- and
// that indistinguishability is precisely why the bug survived. What is
// proven here is the second connection: a pool that connected cleanly,
// against a database that then loses the pgvector extension underneath it.
//
// pgxpool is lazy. internal/db.NewPool installs pgvector-go's type
// registration as AfterConnect, which runs on EVERY connection the pool
// opens, and pgxpool fails the whole acquisition when it errors. Postgres
// stays up, reachable and authenticating throughout what follows; the only
// thing that changes is that this process can no longer finish setting up a
// connection to it.
package main

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/crypto"
	"github.com/bobcob7/loam/internal/db"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/health"
)

// lazyPoolHost and lazyPoolToken are this file's seeded credential. The
// token is a distinctive literal so a "never leaked" assertion cannot pass
// by coincidence.
const (
	lazyPoolHost  = "forgejo.lazypool.example.com"
	lazyPoolToken = "forgejo_lazypool-canary-4c1e77aa9b02"
)

// dropVectorExtension removes the pgvector extension over a connection of
// its own -- never the pool under test, whose whole point here is that it
// must be the thing that notices.
//
// CASCADE is required, not incidental: migration 0002 gives chunks an
// `embedding vector(...)` column, so the extension cannot be dropped
// without it. That is a faithful model of what an operator hits after
// restoring a dump into a database without pgvector, or after a failover
// onto one that never had it.
func dropVectorExtension(t *testing.T, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
	defer cancel()
	conn, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Exec(ctx, "DROP EXTENSION vector CASCADE")
	require.NoError(t, err)
}

// lazyPoolFixture brings up Postgres, migrates it, seeds one credential
// encrypted with testEncryptionKey, and returns a pool built exactly the
// way run() builds the process's own -- through db.NewPool, so AfterConnect
// is wired the same way -- alongside a credentialstore over that pool.
func lazyPoolFixture(t *testing.T) (string, *pgxpool.Pool, *credentialstore.Store) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	dsn := newPostgres(t)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Minute)
	defer cancel()
	require.NoError(t, migrations.Migrate(ctx, dsn, logger))
	seedEncryptedCredential(t, dsn, testEncryptionKey, lazyPoolHost, lazyPoolToken)
	pool, err := db.NewPool(ctx, db.Config{DatabaseURL: dsn}, logger)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	key, err := base64.StdEncoding.DecodeString(testEncryptionKey)
	require.NoError(t, err)
	enc, err := crypto.NewEncryptor(key)
	require.NoError(t, err)
	return dsn, pool, credentialstore.New(pool, enc, logger)
}

// forceFreshConnections makes the pool's NEXT acquisition open a new
// connection, which is the only way AfterConnect runs again.
//
// Without this the test proves nothing: the connections db.NewPool already
// opened registered the vector type while it still existed, and pgx caches
// that registration per connection, so they keep serving the `credentials`
// table (which has no vector column) perfectly well after the extension is
// gone. Pool.Reset closes them all and leaves the pool open, which is what
// a restarted backend, an idle timeout, a max-lifetime expiry or a failover
// does in production -- the pool is fine, its connections are not, and
// every replacement has to survive AfterConnect.
func forceFreshConnections(pool *pgxpool.Pool) {
	pool.Reset()
}

// breakAfterEnumerating delegates ListStatuses to the real store and then,
// exactly once and only after that first call has already returned, runs
// onceEnumerated.
//
// It exists because of a trap the first version of this file fell into. If
// the extension is dropped BEFORE verifyStoredCredentialsDecrypt runs at
// all, the very first ListStatuses fails and GetByHost is never reached --
// so the fixture proves nothing whatsoever about the branch that carried
// the expensive message. The two paths are indistinguishable to it, which
// is the same blindness that let this bug survive in the first place.
//
// What has to be modelled instead is the pool losing its connections
// BETWEEN the enumeration and the credential read: statuses list fine over
// a connection that already exists, then a recycled connection, an idle
// timeout, a max-lifetime expiry or a failover forces a fresh one, and THAT
// is the acquisition AfterConnect fails. Controlling when the database
// breaks is the only deterministic way to sit in that window.
type breakAfterEnumerating struct {
	inner          credentialLister
	once           sync.Once
	onceEnumerated func()
}

func (b *breakAfterEnumerating) ListStatuses(ctx context.Context) ([]credentialstore.CredentialStatus, error) {
	statuses, err := b.inner.ListStatuses(ctx)
	b.once.Do(b.onceEnumerated)
	return statuses, err
}

// TestLazyPool_ExtensionDroppedBetweenEnumerationAndRead_DoesNotBlameTheEncryptionKey
// is this bead's central proof, and the assertion that matters is the
// negative one.
//
// LOAM_ENCRYPTION_KEY cannot be rotated in place and no database backup
// covers it (docs/compose-quickstart.md), so an operator told their key
// "does not match" reasonably concludes it is lost -- and the documented
// recovery from a lost key is deleting every credentials row and
// re-entering every forge token. Before this fix, the failure reproduced
// here produced exactly that sentence, from a database that was up the
// whole time and a key that was correct the whole time.
//
// This is the GetByHost branch specifically -- the one that carried the
// message -- reached with the enumeration having already succeeded. See
// breakAfterEnumerating for why that ordering is the whole point.
func TestLazyPool_ExtensionDroppedBetweenEnumerationAndRead_DoesNotBlameTheEncryptionKey(t *testing.T) {
	dsn, pool, credentials := lazyPoolFixture(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Minute)
	defer cancel()

	// Positive control. Without it a later failure would prove only that
	// something is broken, not that the extension is what broke it.
	require.NoError(t, verifyStoredCredentialsDecrypt(ctx, credentials, credentials, logger),
		"the seeded credential decrypts under the key that encrypted it, so the key is correct for the whole of this test")

	lister := &breakAfterEnumerating{inner: credentials, onceEnumerated: func() {
		forceFreshConnections(pool)
		dropVectorExtension(t, dsn)
	}}
	err := verifyStoredCredentialsDecrypt(ctx, lister, credentials, logger)
	require.Error(t, err, "a pool that can no longer open a connection cannot verify anything, and must not report success")
	message := err.Error()
	assert.NotContains(t, message, "LOAM_ENCRYPTION_KEY does not match",
		"the key is provably correct here -- telling an operator otherwise costs them every stored credential")
	assert.Contains(t, message, "vector type not found in the database",
		"the operator has to be able to see the failure that actually happened, and this is the exact text pgvector-go emits")
	assert.Contains(t, message, "do NOT touch the key",
		"the expensive mistake this failure invites is acting on the key, so the message has to head it off")
	assert.Contains(t, message, lazyPoolHost, "the failing host is still worth naming")
	assert.NotContains(t, message, lazyPoolToken, "no failure path may leak the plaintext token")
	assert.NotContains(t, message, testEncryptionKey, "no failure path may leak the key bytes")
}

// TestLazyPool_ExtensionDroppedBeforeAnyRead_FailsOnTheEnumeration covers
// the other order, and is kept explicitly separate from the test above
// rather than folded into it: here nothing is readable by the time the
// check starts, so it fails on the enumeration and GetByHost is never
// reached at all. That path never carried the key diagnosis, and a fixture
// that only exercised it would say nothing about the one that did -- which
// is exactly the mistake this file's first version made.
func TestLazyPool_ExtensionDroppedBeforeAnyRead_FailsOnTheEnumeration(t *testing.T) {
	dsn, pool, credentials := lazyPoolFixture(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Minute)
	defer cancel()
	require.NoError(t, verifyStoredCredentialsDecrypt(ctx, credentials, credentials, logger))

	forceFreshConnections(pool)
	dropVectorExtension(t, dsn)

	err := verifyStoredCredentialsDecrypt(ctx, credentials, credentials, logger)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "LOAM_ENCRYPTION_KEY does not match")
	assert.Contains(t, err.Error(), "vector type not found in the database")
}

// TestLazyPool_ExtensionDroppedAfterStartup_ReadinessNamesTheExtensionNotTheNetwork
// is the same failure seen through /readyz, where it used to read as
// "database unreachable" -- a network diagnosis for a schema problem, on an
// instance whose Postgres never went anywhere.
//
// It also pins the ordering that makes the reason load-bearing: the schema
// check runs over this same pool, so it fails at the same acquisition and
// never gets to name anything. Whatever the ping's reason claims is the
// whole of what the operator gets.
func TestLazyPool_ExtensionDroppedAfterStartup_ReadinessNamesTheExtensionNotTheNetwork(t *testing.T) {
	dsn, pool, _ := lazyPoolFixture(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	readiness := health.NewReadiness(pool, migrations.NewSchemaCheck(pool), logger)

	require.Equal(t, http.StatusOK, readyzCode(t, readiness), "readiness must be 200 first, or this test proves nothing")

	forceFreshConnections(pool)
	dropVectorExtension(t, dsn)

	rec := readyz(t, readiness)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "a pool that cannot open a connection cannot serve any request correctly")
	body := rec.Body.String()
	assert.Contains(t, body, "pgvector", "the 503 has to name the thing that is actually missing")
	assert.NotContains(t, body, "unreachable",
		"Postgres is up, reachable and authenticating throughout this test; only the pool's per-connection type registration failed")
	assert.NotContains(t, body, dsn, "the /readyz body is unauthenticated and must never carry connection detail")
}

// TestLazyPool_ReadinessStillSaysUnreachableWhenPostgresGenuinelyIs guards
// the direction the pgvector carve-out is allowed to work in. It refines a
// diagnosis; it must not repaint an actual outage as an extension problem.
// docs/deployment-spec.md and helm/loam's postgres-statefulset comment both
// promise operators "database unreachable" for a connection that cannot be
// made, and that promise still has to hold.
func TestLazyPool_ReadinessStillSaysUnreachableWhenPostgresGenuinelyIs(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	container, dsn := newPostgresContainer(t)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Minute)
	defer cancel()
	require.NoError(t, migrations.Migrate(ctx, dsn, logger))
	pool, err := db.NewPool(ctx, db.Config{DatabaseURL: dsn}, logger)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	readiness := health.NewReadiness(pool, migrations.NewSchemaCheck(pool), logger)
	require.Equal(t, http.StatusOK, readyzCode(t, readiness))

	stopTimeout := 30 * time.Second
	require.NoError(t, container.Stop(context.WithoutCancel(t.Context()), &stopTimeout))
	forceFreshConnections(pool)

	rec := readyz(t, readiness)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "database unreachable",
		"a Postgres that is genuinely gone must keep reading as unreachable: the carve-out refines, it never widens")
	assert.False(t, strings.Contains(rec.Body.String(), "pgvector"),
		"an outage is not an extension problem")
}

// readyz drives handler with an unauthenticated GET, the way a probe does.
func readyz(t *testing.T, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.WithoutCancel(t.Context()), http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func readyzCode(t *testing.T, handler http.Handler) int {
	t.Helper()
	return readyz(t, handler).Code
}
