package fakeforge

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewCreatesIsolatedStorage(t *testing.T) {
	t.Parallel()
	srv1, _ := newTestServer(t)
	srv2, _ := newTestServer(t)
	assert.NotEqual(t, srv1.root, srv2.root)
	_, err := os.Stat(srv1.root)
	require.NoError(t, err)
}

func TestCloseRemovesStorage(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	root := srv.root
	require.NoError(t, srv.Close())
	_, err := os.Stat(root)
	assert.True(t, os.IsNotExist(err))
}

func TestAddTokenThenHasToken(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	assert.False(t, srv.hasToken("secret"))
	srv.AddToken("secret")
	assert.True(t, srv.hasToken("secret"))
}

// TestTokenScopeIsSingleAxis pins loam-2uy's corrected model: push
// (git-receive-pack) and PR-opening (the provider REST surface) are NOT
// independently grantable, because real Forgejo 9.0.3 gates both on the
// identical write:repository scope (verified live: a read:repository
// token 403s on both the receive-pack ref advertisement and
// POST .../pulls, not just one). Before this bead, AddReadOnlyToken
// modeled a token that could open PRs but not push -- a state Forgejo
// cannot issue.
func TestTokenScopeIsSingleAxis(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		register     func(srv *Server, token string)
		wantReadOnly bool
		wantPRScope  bool
	}{
		"full access token can push and open PRs":              {register: (*Server).AddToken, wantReadOnly: false, wantPRScope: true},
		"read-only token can do neither: no push, no PR scope": {register: (*Server).AddReadOnlyToken, wantReadOnly: true, wantPRScope: false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, _ := newTestServer(t)
			tc.register(srv, "tok")
			require.True(t, srv.hasToken("tok"))
			assert.Equal(t, tc.wantReadOnly, srv.tokenReadOnly("tok"))
			assert.Equal(t, tc.wantPRScope, srv.tokenHasPRScope("tok"))
		})
	}
}
