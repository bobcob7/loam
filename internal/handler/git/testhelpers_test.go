package git

import (
	"crypto/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/httpauth"
)

// runGit runs a real git subcommand in dir (empty means no cwd change),
// failing the test immediately on a nonzero exit so a broken fixture never
// silently produces a confusing failure three lines later. It returns
// combined stdout+stderr, trimmed, for callers that assert on git's own
// output (e.g. "branch -> branch" on a push).
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// seedBareMirror creates a real bare mirror at mirrorDir (parents included)
// seeded with a single commit on branch "main" adding f.txt="hello\n", by
// committing into a throwaway working tree and bare-cloning it -- exactly
// what a real enrolled repo's mirror looks like on disk, never a hand-
// rolled fixture. It returns the seeded commit's SHA.
func seedBareMirror(t *testing.T, mirrorDir string) string {
	t.Helper()
	src := t.TempDir()
	runGit(t, src, "init", "--quiet", "--initial-branch=main")
	runGit(t, src, "config", "user.email", "seed@example.com")
	runGit(t, src, "config", "user.name", "seed")
	require.NoError(t, os.WriteFile(filepath.Join(src, "f.txt"), []byte("hello\n"), 0o644))
	runGit(t, src, "add", "f.txt")
	runGit(t, src, "commit", "--quiet", "-m", "init")
	require.NoError(t, os.MkdirAll(filepath.Dir(mirrorDir), 0o755))
	runGit(t, "", "clone", "--quiet", "--bare", src, mirrorDir)
	return runGit(t, "", "--git-dir="+mirrorDir, "rev-parse", "refs/heads/main")
}

// seedBareMirrorWithLargeBlob is seedBareMirror's variant for tests that
// need a subprocess response large enough to guarantee it cannot be
// written into an unread connection without blocking (a write-blocked
// subprocess is a genuinely different state than one blocked reading
// stdin -- see TestServeRPC_ContextCancellationKillsAWriteBlockedSubprocess).
// The blob is crypto/rand bytes, not a repeated pattern, specifically so
// git's own delta/zlib compression cannot shrink the resulting pack back
// down below whatever this test needs it to exceed.
func seedBareMirrorWithLargeBlob(t *testing.T, mirrorDir string, blobSize int) string {
	t.Helper()
	src := t.TempDir()
	runGit(t, src, "init", "--quiet", "--initial-branch=main")
	runGit(t, src, "config", "user.email", "seed@example.com")
	runGit(t, src, "config", "user.name", "seed")
	blob := make([]byte, blobSize)
	_, err := rand.Read(blob)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(src, "big.bin"), blob, 0o644))
	runGit(t, src, "add", "big.bin")
	runGit(t, src, "commit", "--quiet", "-m", "large blob")
	require.NoError(t, os.MkdirAll(filepath.Dir(mirrorDir), 0o755))
	runGit(t, "", "clone", "--quiet", "--bare", src, mirrorDir)
	return runGit(t, "", "--git-dir="+mirrorDir, "rev-parse", "refs/heads/main")
}

// withTestIdentity stands in for internal/httpauth.GitIdentity (loam-
// ofg.3/.17), which resolves the Loam-Agent-* headers into the request
// context before this handler's mux entry is ever reached in production.
// Testing this package in isolation from that whole chain still needs a
// context carrying an identity for the receive-pack env-propagation seam
// to read, so this fixture places one directly rather than reimplementing
// header parsing.
func withTestIdentity(identity httpauth.Identity, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(httpauth.WithIdentity(r.Context(), identity)))
	})
}
