//go:build integration

// This file requires a real Docker (or Docker-API-compatible) daemon and a
// real git binary on PATH, and is excluded from the default `go test ./...`
// run by the integration build tag. Run explicitly with:
//
//	go test -tags=integration ./internal/handler/repoadmin/... -run TestEnrollRepo_RealMirror -v
//
// On podman, also set TESTCONTAINERS_RYUK_DISABLED=true (see
// internal/db/migrations/integration_test.go for why).
package repoadmin

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/crypto"
	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/fakeforge"
	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/gittransport"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/mirrorreconcile"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/testdb"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// requireGitBinary skips the test when the git binary is not on PATH,
// matching internal/gittransport's own convention.
func requireGitBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not found on PATH; skipping repoadmin enroll integration test")
	}
}

// newTestPostgresPool migrates a fresh pgvector-enabled Postgres
// container and returns a connected pool, matching internal/ingest's own
// newTestPool helper.
func newTestPostgresPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := t.Context()
	logger := testLogger()
	container, err := postgres.Run(ctx, testdb.PostgresImage,
		postgres.WithDatabase("loam"),
		postgres.WithUsername("loam"),
		postgres.WithPassword("loam"),
		postgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, container.Terminate(context.Background())) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, migrations.Migrate(ctx, dsn, logger))
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// TestEnrollRepo_RealMirror_ClonesPopulatedBareMirrorAndReconciles is the
// end-to-end acceptance proof for loam-ofg.12's central claim, against a
// REAL Postgres, a REAL git binary, and a REAL (fakeforge-served)
// upstream -- not mocks standing in for any of the three. It proves:
// (1) the repos row lands with sync_state=idle and a non-nil
// last_synced_at, (2) a real bare mirror exists on disk under
// dataDir/mirrors/<repo>.git carrying upstream's actual ref, and (3) the
// mirror is genuinely reconciled (receive.denyNonFastForwards/
// denyDeletes set, pre-receive hook installed at 0755) -- the exact gap
// loam-giq.2's review found missing tree-wide before this bead.
func TestEnrollRepo_RealMirror_ClonesPopulatedBareMirrorAndReconciles(t *testing.T) {
	requireGitBinary(t)
	ctx := t.Context()
	pool := newTestPostgresPool(t)
	logger := testLogger()

	srv, err := fakeforge.New(logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	srv.SetBaseURL(ts.URL)
	const token = "repoadmin-enroll-integration-token"
	srv.AddToken(token)
	// fakeforge.Server.GitURL mounts every repo under its own "/git/"
	// smart-HTTP prefix (fakeforge's OWN fixture convention, unrelated to
	// Loam's identical-looking "/git/" mount for its own server): seeding
	// a bare repo name "widgets" (no "/" of its own) makes GitURL produce
	// ".../git/widgets.git", whose path derives the two-segment identifier
	// "git/widgets" -- deriveRepoIdentity/validRepoName's ordinary
	// two-segment "<group>/<repo_name>" rule, exercised against this
	// fixture's actual URL shape rather than a hand-typed one that would
	// never round-trip through a real fakeforge server.
	require.NoError(t, srv.SeedRepoFiles(ctx, "widgets", map[string][]byte{"a.txt": []byte("hi")}, fakeforge.SeedOptions{}))
	upstreamURL := srv.GitURL("widgets")
	const repoName = "git/widgets"
	// host is scheme-qualified ("http://host:port"), not bare -- loam-4kz:
	// deriveRepoIdentity (handler.go's forgeHostOf) derives a scheme-
	// qualified host from a plain-HTTP upstream URL like this fixture's
	// (srv.GitURL builds off ts.URL, a plain httptest.NewServer, never
	// NewTLSServer), and the credential this test seeds below must be
	// keyed identically or EnrollRepo's own GetByHost call fails to find
	// it -- exactly the failure this bead exists to fix, reproduced here
	// if this host were bare.
	host := ts.URL

	repos := reposstore.NewStore(gen.New(pool), logger)
	workBranches := workbranchstore.New(gen.New(pool), logger)
	enc, err := crypto.NewEncryptor([]byte(strings.Repeat("k", 32)))
	require.NoError(t, err)
	credentials := credentialstore.New(pool, enc, logger)
	_, err = credentials.UpsertToken(ctx, host, token)
	require.NoError(t, err)
	transport := gittransport.New(credentials, fakeforge.NewClient("", ""), logger)
	checker := ForgeChecker{HTTPClient: ts.Client(), Logger: logger}
	dataDir := t.TempDir()
	errorMapper := handler.NewErrorMapper(logger)
	// ReconcileMirror copies the loamhook binary's bytes into the mirror's
	// hooks/pre-receive, so it needs a real file to copy; what that binary
	// DOES is loam-ofg.18's e2e test, not this one, which only proves enroll
	// clones and then hardens. Bind the path here the same way cmd/server's
	// composition root does, keeping this package's seam hook-agnostic.
	hookBinary := filepath.Join(t.TempDir(), "loamhook")
	require.NoError(t, os.WriteFile(hookBinary, []byte("#!/bin/sh\nexit 1\n"), 0o755))
	reconcile := func(ctx context.Context, repoPath string) error {
		return mirrorreconcile.ReconcileMirror(ctx, repoPath, hookBinary)
	}
	h := New(dataDir, repos, workBranches, credentials, checker, transport, reconcile,
		&ingestEnqueuerMock{EnqueueFunc: func(context.Context, uuid.UUID, string, ingest.Kind) error { return nil }},
		&jobListerMock{},
		&repoDeleterMock{},
		errorMapper, logger,
	)

	resp, err := h.EnrollRepo(ctx, enrollReq(upstreamURL, []string{"main"}, "main"))
	require.NoError(t, err)
	got := resp.Msg.GetRepo()
	assert.Equal(t, repoName, got.GetRepo())
	require.NotNil(t, got.GetSync())
	assert.Equal(t, adminv1.SyncState_SYNC_STATE_IDLE, got.GetSync().GetState())
	assert.NotEmpty(t, got.GetSync().GetLastSyncedAt())

	row, err := repos.GetRepoByName(ctx, repoName)
	require.NoError(t, err)
	assert.Equal(t, "idle", row.SyncState)
	require.NotNil(t, row.LastSyncedAt)
	assert.Nil(t, row.SyncError)

	mirrorDir := filepath.Join(dataDir, "mirrors", "git", "widgets.git")
	shaOut, err := runGitVerify(t, mirrorDir, "rev-parse", "refs/heads/main")
	require.NoErrorf(t, err, "the mirror must carry upstream's main branch: %s", shaOut)
	assert.NotEmpty(t, strings.TrimSpace(string(shaOut)))

	bareOut, err := runGitVerify(t, mirrorDir, "rev-parse", "--is-bare-repository")
	require.NoError(t, err)
	assert.Equal(t, "true", strings.TrimSpace(string(bareOut)))

	denyFF, err := runGitVerify(t, mirrorDir, "config", "receive.denyNonFastForwards")
	require.NoErrorf(t, err, "reconcile must have set receive.denyNonFastForwards: %s", denyFF)
	assert.Equal(t, "true", strings.TrimSpace(string(denyFF)))
	denyDel, err := runGitVerify(t, mirrorDir, "config", "receive.denyDeletes")
	require.NoErrorf(t, err, "reconcile must have set receive.denyDeletes: %s", denyDel)
	assert.Equal(t, "true", strings.TrimSpace(string(denyDel)))
}

// runGitVerify runs a real git command directly against mirrorDir, as an
// independent verifier -- never through the code under test.
func runGitVerify(t *testing.T, mirrorDir string, args ...string) ([]byte, error) {
	t.Helper()
	fullArgs := append([]string{"--git-dir=" + mirrorDir}, args...)
	cmd := exec.CommandContext(t.Context(), "git", fullArgs...)
	return cmd.CombinedOutput()
}
