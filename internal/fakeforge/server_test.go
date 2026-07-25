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

func TestParseUpstreamRepo(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		url     string
		want    string
		wantErr bool
	}{
		"group and repo with dot git": {url: "http://host/git/acme/widgets.git", want: "acme/widgets"},
		"no dot git suffix":           {url: "http://host/git/acme/widgets", want: "acme/widgets"},
		"single segment repo":         {url: "http://host/git/widgets.git", want: "widgets"},
		"empty path is invalid":       {url: "http://host/git/", wantErr: true},
		"malformed url is invalid":    {url: "http://host/git/\x7f", wantErr: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := parseUpstreamRepo(tc.url)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
