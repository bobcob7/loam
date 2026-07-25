package fakeforge

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initSourceRepo creates a real, non-bare git repo under t.TempDir with one
// commit on branch, for use as SeedRepo's sourcePath.
func initSourceRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	runClientGit(t, dir, "init", "--initial-branch="+branch)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi\n"), 0o644))
	runClientGit(t, dir, "add", "-A")
	runClientGit(t, dir, "commit", "-m", "seed source commit")
	return dir
}

func TestSeedRepoClonesRefsAndEnablesPush(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, _ := newTestServer(t)
	ctx := t.Context()
	source := initSourceRepo(t, "trunk")
	require.NoError(t, srv.SeedRepo(ctx, "acme/seeded", source))
	sha := branchSHA(t, srv, "acme/seeded", "trunk")
	assert.NotEmpty(t, sha)
	subject, err := srv.runGit(ctx, "", "--git-dir="+srv.repoDir("acme/seeded"), "log", "-1", "--format=%s", sha)
	require.NoError(t, err)
	assert.Equal(t, "seed source commit\n", string(subject))
	srv.AddToken("token")
	cloneDir := t.TempDir()
	cloneURL := withCreds(t, srv.GitURL("acme/seeded"), "anyuser", "token")
	runClientGit(t, "", "clone", cloneURL, cloneDir)
	require.NoError(t, os.WriteFile(filepath.Join(cloneDir, "b.txt"), []byte("more\n"), 0o644))
	runClientGit(t, cloneDir, "add", "-A")
	runClientGit(t, cloneDir, "commit", "-m", "push after seed")
	runClientGit(t, cloneDir, "push", "origin", "HEAD:refs/heads/trunk")
	after := branchSHA(t, srv, "acme/seeded", "trunk")
	assert.NotEqual(t, sha, after, "http.receivepack must be enabled on a repo seeded via SeedRepo")
}

func TestSeedRepoRejectsExistingName(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, _ := newTestServer(t)
	ctx := t.Context()
	source := initSourceRepo(t, "trunk")
	require.NoError(t, srv.SeedRepo(ctx, "acme/dup", source))
	err := srv.SeedRepo(ctx, "acme/dup", source)
	assert.ErrorIs(t, err, errRepoExists)
}

func TestSeedRepoFilesRejectsExistingName(t *testing.T) {
	t.Parallel()
	requireGit(t)
	srv, _ := newTestServer(t)
	ctx := t.Context()
	require.NoError(t, srv.SeedRepoFiles(ctx, "acme/dup2", map[string][]byte{"a": []byte("b")}, SeedOptions{}))
	err := srv.SeedRepoFiles(ctx, "acme/dup2", map[string][]byte{"a": []byte("b")}, SeedOptions{})
	assert.ErrorIs(t, err, errRepoExists)
}
