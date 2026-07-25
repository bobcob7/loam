package fakeforge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientValidateToken(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddToken("good-token")
	client := NewClient(ts.URL, "good-token")
	assert.NoError(t, client.ValidateToken(t.Context(), "example.invalid", "good-token"))
	assert.ErrorIs(t, client.ValidateToken(t.Context(), "example.invalid", "bad-token"), errUnauthorized)
}

func TestClientCheckRepo(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	srv.AddToken("rw-token")
	srv.AddReadOnlyToken("ro-token")
	rw := NewClient(ts.URL, "rw-token")
	assert.NoError(t, rw.CheckRepo(ctx, srv.GitURL("acme/widgets")))
	ro := NewClient(ts.URL, "ro-token")
	assert.ErrorIs(t, ro.CheckRepo(ctx, srv.GitURL("acme/widgets")), errNoWriteAccess)
	assert.ErrorIs(t, rw.CheckRepo(ctx, srv.GitURL("acme/nope")), errRepoNotFound)
}

func TestClientCheckRepoRequiresAuth(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	unauthed := NewClient(ts.URL, "never-registered")
	assert.ErrorIs(t, unauthed.CheckRepo(ctx, srv.GitURL("acme/widgets")), errUnauthorized)
}

func TestClientCreatePRGetStateClosePR(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, ts := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/widgets", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	prURL, number, err := client.CreatePR(ctx, "acme/widgets", "wb-feature", "main", "title", "desc")
	require.NoError(t, err)
	assert.NotEmpty(t, prURL)
	assert.Equal(t, 1, number)
	state, err := client.GetPRState(ctx, "acme/widgets", number)
	require.NoError(t, err)
	assert.Equal(t, "open", state)
	require.NoError(t, client.ClosePR(ctx, "acme/widgets", number))
	state, err = client.GetPRState(ctx, "acme/widgets", number)
	require.NoError(t, err)
	assert.Equal(t, "closed", state)
	_, err = client.GetPRState(ctx, "acme/widgets", 999)
	assert.ErrorIs(t, err, errPRNotFound)
}

func TestClientCreatePRRepoNotFound(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddToken("token")
	client := NewClient(ts.URL, "token")
	_, _, err := client.CreatePR(t.Context(), "acme/nope", "wb-x", "main", "t", "d")
	assert.ErrorIs(t, err, errRepoNotFound)
}

func TestClientGitCredentials(t *testing.T) {
	t.Parallel()
	_, ts := newTestServer(t)
	client := NewClient(ts.URL, "some-token")
	user, pass, err := client.GitCredentials(t.Context(), "some-token")
	require.NoError(t, err)
	assert.NotEmpty(t, user)
	assert.Equal(t, "some-token", pass)
}
