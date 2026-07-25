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

func TestTokenScopeAxesAreIndependent(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		register     func(srv *Server, token string)
		wantReadOnly bool
		wantPRScope  bool
	}{
		"full access token can push and open PRs":      {register: (*Server).AddToken, wantReadOnly: false, wantPRScope: true},
		"read-only token cannot push but can open PRs": {register: (*Server).AddReadOnlyToken, wantReadOnly: true, wantPRScope: true},
		"push-only token can push but not open PRs":    {register: (*Server).AddTokenWithoutPRScope, wantReadOnly: false, wantPRScope: false},
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
