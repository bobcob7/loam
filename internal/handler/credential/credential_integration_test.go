//go:build integration

// Requires a real Docker (or Docker-API-compatible) daemon; run with
// (see internal/db/migrations/integration_test.go for why
// TESTCONTAINERS_RYUK_DISABLED is a podman-only workaround):
//
//	TESTCONTAINERS_RYUK_DISABLED=true go test -tags=integration ./internal/handler/credential/... -v
//
// This file closes the coverage gap loam-0hjq's own bug DESCRIPTION names
// directly: "features/credentials.feature passes today because the
// acceptance harness seeds http://<host> consistently at BOTH ends, so
// nothing exercises an https forge whose credential was entered with a
// scheme." Reproducing THAT specific gap end to end through the acceptance
// harness would need a TLS-capable fake forge -- the harness's identity
// proxy (cmd/server's newCredentialsIdentityProxy) is a plain
// httptest.Server, and forge.Forgejo's apiBaseURL never retries an
// explicitly-https host the way it does a bare one, so an https-scheme
// credential genuinely has to reach a TLS listener to validate. Standing
// one up is disproportionate infrastructure for this one regression, so
// this is an integration test instead: real Postgres, the real
// credentialstore.Store (so credentials_host_key's UNIQUE(host) and the
// real AES-GCM round trip are both in play), and the real
// internal/forgehost.FromURL -- the SAME function forgeHostOf
// (internal/handler/repoadmin/handler.go) delegates to -- standing in for
// what EnrollRepo/ProbeRepo would derive from an upstream URL, without
// needing a live forge or git subprocess on that side at all.
package credential

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/crypto"
	"github.com/bobcob7/loam/internal/db/migrations"
	"github.com/bobcob7/loam/internal/forgehost"
	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/testdb"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newTestEncryptor builds a real AES-256-GCM *crypto.Encryptor over a
// fixed, distinctive 32-byte key -- this file's tests only need the
// encryption to genuinely round-trip, never a specific key value.
func newTestEncryptor(t *testing.T) *crypto.Encryptor {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i) + 7
	}
	enc, err := crypto.NewEncryptor(key)
	require.NoError(t, err)
	return enc
}

// integrationSharedDSN is the one migrated Postgres every test in this file
// runs against, started once here rather than one container per test --
// following credentialstore/integration_test.go's own TestMain pattern
// exactly (deliberately duplicated, not imported: the two packages'
// integration suites must be able to run independently of one another).
var integrationSharedDSN string

// TestMain starts one Postgres container, applies the production migration
// set, and hands every test in this file the same DSN, tearing the
// container down once after the whole package's tests finish.
func TestMain(m *testing.M) {
	ctx := context.Background()
	logger := testLogger(io.Discard)
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
	integrationSharedDSN = dsn
	code := m.Run()
	if err := container.Terminate(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "terminating shared postgres container:", err)
	}
	os.Exit(code)
}

// alwaysValidTokenValidator is the tokenValidator stub every test in this
// file uses: this file is about the STORE agreeing with itself across two
// derivations of "the forge host", not about a real forge round trip
// (internal/handler/credential's own unit tests already cover
// ValidateToken's error mapping against a mock; internal/forge's own tests
// cover apiBaseURL/ValidateToken itself).
type alwaysValidTokenValidator struct{}

func (alwaysValidTokenValidator) ValidateToken(context.Context, string, string) error { return nil }

func newIntegrationStore(t *testing.T) *credentialstore.Store {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), integrationSharedDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	enc := newTestEncryptor(t)
	return credentialstore.New(pool, enc, testLogger(io.Discard))
}

func newIntegrationHandler(t *testing.T) *Handler {
	t.Helper()
	logger := testLogger(io.Discard)
	return New(newIntegrationStore(t), alwaysValidTokenValidator{}, handler.NewErrorMapper(logger), logger)
}

// TestSetUpstreamToken_HTTPSSchemeCredential_IsFoundByForgeHostOfsBareDerivation
// is this bead's central regression proof. Before loam-0hjq, an admin
// entering "https://forge-integration.example.com" (the natural thing to
// do when pasting a forge URL) got a credential that reported
// validated=true yet was permanently unreachable by
// internal/handler/repoadmin's forgeHostOf -- which always derives the
// BARE host for an https upstream -- because credentialstore.GetByHost is
// an exact string match with no normalization.
//
// This drives the real SetUpstreamToken RPC with the scheme-qualified
// spelling, then calls the real credentialstore.Store.GetByHost directly
// (bypassing this package's own narrow credentialStore seam, which
// deliberately omits GetByHost -- see this package's doc comment) with
// EXACTLY what forgeHostOf would compute for an upstream repo URL on the
// same forge (internal/forgehost.FromURL, the identical function
// forgeHostOf delegates to). Finding the credential there is the whole
// property this bead's fix exists to establish.
func TestSetUpstreamToken_HTTPSSchemeCredential_IsFoundByForgeHostOfsBareDerivation(t *testing.T) {
	t.Parallel()
	const rawHost = "https://forge-integration-https.example.com"
	const plaintextToken = "ghp_integration-https-scheme-token-value"
	h := newIntegrationHandler(t)
	ctx := httpauth.WithAdmin(t.Context())

	resp, err := h.SetUpstreamToken(ctx, connect.NewRequest(&adminv1.SetUpstreamTokenRequest{Host: rawHost, Token: plaintextToken}))
	require.NoError(t, err)
	require.True(t, resp.Msg.GetStatus().GetValidated(), "positive control: the credential must genuinely validate, exactly as the live incident's did")

	upstreamURL, err := url.Parse("https://forge-integration-https.example.com/acme/widgets")
	require.NoError(t, err)
	derivedHost := forgehost.FromURL(upstreamURL)
	require.NotEqual(t, rawHost, derivedHost, "positive control: forgeHostOf's derivation must differ from the raw scheme-qualified string -- otherwise this test cannot distinguish the fix from a no-op")

	store := newIntegrationStore(t)
	cred, err := store.GetByHost(ctx, derivedHost)
	require.NoError(t, err, "the host EnrollRepo/ProbeRepo would derive from this forge's upstream URL must resolve the credential SetUpstreamToken just wrote")
	assert.Equal(t, plaintextToken, cred.Token)
	assert.True(t, cred.Validated)

	_, err = store.GetByHost(ctx, rawHost)
	assert.ErrorIs(t, err, credentialstore.ErrNotFound, "the raw scheme-qualified string must NOT be a row key any more -- only the canonical form is")
}

// TestSetUpstreamToken_PlainHTTPCredential_IsFoundByForgeHostOfsQualifiedDerivation
// is the symmetric case for a plaintext-HTTP forge, where forgeHostOf
// keeps the scheme-qualified form rather than stripping it -- proving the
// fix did not accidentally strip a scheme SetUpstreamToken must keep
// (loam-4kz's own regression, which this bead must not reopen).
func TestSetUpstreamToken_PlainHTTPCredential_IsFoundByForgeHostOfsQualifiedDerivation(t *testing.T) {
	t.Parallel()
	const rawHost = "http://forge-integration-http.example.com:3000"
	const plaintextToken = "ghp_integration-http-scheme-token-value"
	h := newIntegrationHandler(t)
	ctx := httpauth.WithAdmin(t.Context())

	resp, err := h.SetUpstreamToken(ctx, connect.NewRequest(&adminv1.SetUpstreamTokenRequest{Host: rawHost, Token: plaintextToken}))
	require.NoError(t, err)
	require.True(t, resp.Msg.GetStatus().GetValidated())

	upstreamURL, err := url.Parse("http://forge-integration-http.example.com:3000/acme/widgets")
	require.NoError(t, err)
	derivedHost := forgehost.FromURL(upstreamURL)
	require.Equal(t, rawHost, derivedHost, "positive control: for a plain-HTTP forge, forgeHostOf's derivation is the SAME scheme-qualified string -- this pins that the fix keeps it that way")

	store := newIntegrationStore(t)
	cred, err := store.GetByHost(ctx, derivedHost)
	require.NoError(t, err)
	assert.Equal(t, plaintextToken, cred.Token)
}
