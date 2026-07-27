package gittransport

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"testing"

	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/fakeforge"
	"github.com/stretchr/testify/require"
)

// requireGit skips the test when the git binary is not on PATH, matching
// internal/forge and internal/fakeforge's own convention for tests that
// shell out to a real git subprocess.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not found on PATH; skipping git-transport test")
	}
}

// testLogger is a discard logger for tests that don't inspect log output,
// matching this repo's Go testing standard.
func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// newFakeForgeServer builds a fakeforge.Server wrapped in an
// httptest.Server, the same real git smart-HTTP counterparty
// internal/forge and internal/fakeforge's own tests use, wired for
// cleanup so tests never share state.
func newFakeForgeServer(t *testing.T) (*fakeforge.Server, *httptest.Server) {
	t.Helper()
	srv, err := fakeforge.New(testLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	srv.SetBaseURL(ts.URL)
	return srv, ts
}

// staticCredentialSource is a minimal credentialSource returning the same
// credential for every host, tracking how many times it was called so
// tests can assert the transport resolves a credential fresh on every
// invocation rather than caching one. Used instead of the moq mock where
// a test needs a call counter across many invocations; tests that only
// need a fixed canned response or a single failure use
// credentialSourceMock directly.
type staticCredentialSource struct {
	token string
	err   error
	calls int
}

func (s *staticCredentialSource) GetByHost(_ context.Context, _ string) (credentialstore.Credential, error) {
	s.calls++
	if s.err != nil {
		return credentialstore.Credential{}, s.err
	}
	return credentialstore.Credential{Token: s.token}, nil
}

// newGitCredsConverter returns a gitCredentialConverter backed by
// fakeforge.Client, which *forge.Provider itself also is (both implement
// GitCredentials: any username, the token as password). GitCredentials
// takes its token argument explicitly and ignores the receiver's own
// bound baseURL/token fields, so an unbound Client satisfies this
// package's needs exactly as a real forge.Forgejo{} would (see
// internal/forge/forgejo.go's own doc comment: "host and token may be
// empty when the instance is only ever used for ValidateToken and
// GitCredentials").
func newGitCredsConverter() gitCredentialConverter {
	return fakeforge.NewClient("", "")
}

// basicAuthValue returns the exact base64 payload the transport puts in
// its Authorization header for token. Scrubbing tests need this because
// the header carries base64(user:token), not the token itself: a scrubber
// that only knew the plaintext would happily print the encoded form,
// which is trivially reversible.
func basicAuthValue(t *testing.T, token string) string {
	t.Helper()
	user, pass, err := newGitCredsConverter().GitCredentials(t.Context(), token)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// newBareMirror creates an empty bare git repository at a fresh temp
// directory, standing in for a freshly enrolled repo's mirror.
func newBareMirror(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "git", "init", "--bare", "-q", dir)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git init --bare: %s", out)
	return dir
}

// hostOf returns rawURL's host:port authority, the value production call
// sites pass as Transport's host parameter (the same key
// credentialstore.Store.GetByHost is keyed on).
func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	return u.Host
}

// withCreds returns rawURL with user/pass embedded as HTTP Basic
// credentials, for a test's own OUT-OF-BAND verification steps only --
// never a code path Transport itself takes (Transport injects
// credentials via a header, never the URL).
func withCreds(t *testing.T, rawURL, user, pass string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	u.User = url.UserPassword(user, pass)
	return u.String()
}

// runVerificationGit runs a real git command as an external verifier
// would -- never through Transport -- to check outcomes the test itself
// needs to observe (e.g. what actually landed upstream), isolated from
// the host's own git config the same way internal/fakeforge's test
// helpers are, so the developer machine's credential helper cannot mask
// a verification bug either.
func runVerificationGit(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	home := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+home,
		"XDG_CONFIG_HOME="+home+"/.config",
		"GIT_TERMINAL_PROMPT=0",
		// Identity must be supplied explicitly, precisely BECAUSE the
		// three lines above isolate this git from every config file that
		// would otherwise carry it. Without them `git commit` has no
		// committer: git falls back to guessing username@hostname, which
		// happens to succeed on a developer laptop (with a warning) and
		// fails outright on a CI runner with "Please tell me who you
		// are", so the gap is invisible locally. --author on a commit
		// does NOT cover this -- it sets only the author, never the
		// committer.
		"GIT_AUTHOR_NAME=loam-test", "GIT_AUTHOR_EMAIL=loam-test@example.invalid",
		"GIT_COMMITTER_NAME=loam-test", "GIT_COMMITTER_EMAIL=loam-test@example.invalid",
	)
	return cmd.CombinedOutput()
}
