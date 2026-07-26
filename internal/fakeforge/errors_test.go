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

// TestClientValidateTokenMissingPRScopeIsForgeErrInsufficientScope covers a
// token that authenticates but lacks PR-opening scope. forge.Provider's
// ValidateToken contract has a dedicated sentinel for this case,
// ErrInsufficientScope, distinct from ErrInvalidToken — verified
// empirically against a real Forgejo 9.0.3 instance that a 403 (wrong
// scope) and a 401 (no auth) are genuinely different wire responses
// (loam-1ao) — so the fake must match on that same class, not fold it
// into ErrInvalidToken.
func TestClientValidateTokenMissingPRScopeIsForgeErrInsufficientScope(t *testing.T) {
	t.Parallel()
	srv, ts := newTestServer(t)
	srv.AddTokenWithoutPRScope("push-only-token")
	client := NewClient(ts.URL, "push-only-token")
	err := client.ValidateToken(t.Context(), "example.invalid", "push-only-token")
	require.Error(t, err)
	assert.ErrorIs(t, err, forge.ErrInsufficientScope)
	assert.NotErrorIs(t, err, forge.ErrInvalidToken, "a scope failure must not also read as an auth failure")
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

// TestClientAuthedActionBadTokenIsForgeErrInvalidToken covers
// requireProviderAuth rejecting an unregistered client token on each of the
// provider REST surface's three authed actions (CreatePR/GetPRState/
// ClosePR). Nothing else in this file exercises call's authed=true branch:
// ValidateToken and CheckRepo above never send the Client's own
// Authorization header down this path.
func TestClientAuthedActionBadTokenIsForgeErrInvalidToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		call func(*Client) error
	}{
		{"CreatePR", func(c *Client) error {
			_, _, err := c.CreatePR(t.Context(), "acme/widgets", "wb-x", "main", "t", "d")
			return err
		}},
		{"GetPRState", func(c *Client) error {
			_, err := c.GetPRState(t.Context(), "acme/widgets", 1)
			return err
		}},
		{"ClosePR", func(c *Client) error {
			return c.ClosePR(t.Context(), "acme/widgets", 1)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, ts := newTestServer(t)
			client := NewClient(ts.URL, "never-registered")
			err := tt.call(client)
			require.Error(t, err)
			assert.ErrorIs(t, err, forge.ErrInvalidToken)
		})
	}
}

// TestFakeforgeSentinelsMatchOnlyTheirOwnForgeClass guards the errors.go var
// block against a future edit silently blurring which forge.* sentinel each
// fakeforge sentinel maps to. A wrong-class match here would not fail this
// package's own tests above (they only assert the positive case) — it would
// surface only as a silently-wrong-class PASS inside loam-li0.9's shared
// suite, which is exactly the failure mode loam-4k7 exists to prevent.
//
// forgeSentinels is sourced from forge.AllSentinels() rather than a
// hand-copied literal (loam-ddv): a hand-copied list is blind by
// construction to a sentinel added to internal/forge after the list was
// last touched, which is exactly how errMissingScope's drift to
// ErrInvalidToken (it should have followed forge.ErrInsufficientScope)
// passed this guard green — the three-element literal here never gave the
// loop a chance to check the new class at all. The require.Len canary
// below forces a human to notice and reconcile the table above the moment
// forge grows a sentinel, rather than relying on the loop's NotErrorIs
// checks alone: those pass trivially for a class nobody has been told to
// think about yet, which is no protection by itself.
func TestFakeforgeSentinelsMatchOnlyTheirOwnForgeClass(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want error // the one forge sentinel this fakeforge sentinel should match, or nil for none
	}{
		{"errUnauthorized", errUnauthorized, forge.ErrInvalidToken},
		{"errMissingScope", errMissingScope, forge.ErrInsufficientScope},
		{"errRepoNotFound", errRepoNotFound, forge.ErrRepoNotFound},
		{"errNoWriteAccess", errNoWriteAccess, forge.ErrNoWriteAccess},
		{"errTargetBranchNotFound", errTargetBranchNotFound, forge.ErrRepoNotFound},
		{"errPRNotFound", errPRNotFound, forge.ErrRepoNotFound},
		{"errPRExists", errPRExists, forge.ErrDuplicatePR},
		{"errRepoExists", errRepoExists, nil},
		{"errBranchNotFound", errBranchNotFound, nil},
		{"errPRMerged", errPRMerged, nil},
		{"errInvalidBranch", errInvalidBranch, nil},
		{"errMergeConflict", errMergeConflict, nil},
		{"errGitUnavailable", errGitUnavailable, nil},
		{"errInvalidUpstream", errInvalidUpstream, nil},
	}
	forgeSentinels := forge.AllSentinels()
	require.Len(t, forgeSentinels, 5, "forge.AllSentinels() grew (or shrank) a sentinel: reconcile the `want` column above against the new class, then update this count")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, sentinel := range forgeSentinels {
				if tt.want == sentinel {
					assert.ErrorIs(t, tt.err, sentinel)
					continue
				}
				assert.NotErrorIs(t, tt.err, sentinel)
			}
		})
	}
}
