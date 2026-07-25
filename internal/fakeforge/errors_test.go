package fakeforge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/forge"
)

// These tests assert Client's errors against internal/forge's exported
// sentinels, not fakeforge's own unexported ones (provider_test.go already
// covers those). loam-li0.9's contract suite runs one shared table against
// both this fake and the real Forgejo provider using exactly these
// assertions, so a regression here must fail in this package, not there
// (loam-4k7).

// TestClientValidateTokenBadTokenIsForgeErrInvalidToken covers an
// unregistered token: real Forgejo's ValidateToken returns ErrInvalidToken
// on a 401/403 from GET /user, and the fake must match on the same
// sentinel for an unrecognized token.
func TestClientValidateTokenBadTokenIsForgeErrInvalidToken(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddToken("good-token")
	client := NewClient(ts.URL, "bad-token")
	err := client.ValidateToken(t.Context(), "example.invalid", "bad-token")
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrInvalidToken)
}

// TestClientValidateTokenMissingPRScopeIsForgeErrInvalidToken covers a
// token that authenticates but lacks PR-opening scope. forge.Provider's
// ValidateToken contract has no separate sentinel for this case — it is
// folded into ErrInvalidToken (see errors.go and Forgejo.ValidateToken,
// which treats 401 and 403 identically) — so the fake must match the same
// way rather than being indistinguishable only via a fake-internal
// sentinel.
func TestClientValidateTokenMissingPRScopeIsForgeErrInvalidToken(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddTokenWithoutPRScope("push-only-token")
	client := NewClient(ts.URL, "push-only-token")
	err := client.ValidateToken(t.Context(), "example.invalid", "push-only-token")
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrInvalidToken)
}

// TestClientCheckRepoMissingIsForgeErrRepoNotFound covers CheckRepo against
// a repo that was never seeded.
func TestClientCheckRepoMissingIsForgeErrRepoNotFound(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	err := client.CheckRepo(t.Context(), srv.GitURL("acme/nope"))
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrRepoNotFound)
}

// TestClientCheckRepoNoWriteAccessIsForgeErrNoWriteAccess covers CheckRepo
// with a read-only token against a repo that exists: the read probe
// succeeds but the write probe is denied.
func TestClientCheckRepoNoWriteAccessIsForgeErrNoWriteAccess(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	srv.AddReadOnlyToken("ro-token")
	client := NewClient(ts.URL, "ro-token")
	err := client.CheckRepo(ctx, srv.GitURL("acme/widgets"))
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrNoWriteAccess)
}

// TestClientCreatePRRepoNotFoundIsForgeErrRepoNotFound covers the other
// path that can return ErrRepoNotFound: a provider REST call against a repo
// that does not exist, distinct from CheckRepo's git-probe path above.
func TestClientCreatePRRepoNotFoundIsForgeErrRepoNotFound(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	_, _, err := client.CreatePR(t.Context(), "acme/nope", "wb-x", "main", "t", "d")
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrRepoNotFound)
}
