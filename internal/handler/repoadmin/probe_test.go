package repoadmin

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/credentialstore"
	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
)

func probeReq(url string) *connect.Request[adminv1.ProbeRepoRequest] {
	return connect.NewRequest(&adminv1.ProbeRepoRequest{UpstreamUrl: url})
}

// TestProbeRepo_ListsBranchesAndHead proves ls-remote output is parsed
// into a branch list plus the symref-derived HEAD.
func TestProbeRepo_ListsBranchesAndHead(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.cloner.LsRemoteFunc = func(_ context.Context, host, upstreamURL string) ([]byte, error) {
		assert.Equal(t, "example.com", host)
		assert.Equal(t, "https://example.com/acme/widgets.git", upstreamURL)
		return []byte("ref: refs/heads/main\tHEAD\nabc123\tHEAD\nabc123\trefs/heads/main\ndef456\trefs/heads/release\n"), nil
	}
	h := d.handler(t, "/data")
	resp, err := h.ProbeRepo(t.Context(), probeReq("https://example.com/acme/widgets.git"))
	require.NoError(t, err)
	assert.Equal(t, "main", resp.Msg.GetHead())
	assert.Equal(t, []string{"main", "release"}, resp.Msg.GetBranches())
}

// TestProbeRepo_PlaintextHTTPForge_DerivesSameSchemeQualifiedHostAsEnroll
// is the loam-4kz regression for ProbeRepo's side of the coupling: a
// credential set for a plaintext-HTTP forge is keyed by the SAME
// scheme-qualified host EnrollRepo's deriveRepoIdentity derives
// (forgeHostOf, handler.go). Before this fix, ProbeRepo resolved and
// passed upstreamURL's BARE u.Host, so it would have looked up (and
// LsRemote'd against) a different, non-existent credential key than the
// one an operator actually set.
func TestProbeRepo_PlaintextHTTPForge_DerivesSameSchemeQualifiedHostAsEnroll(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	const wantHost = "http://127.0.0.1:13030"
	var sawCredentialHost, sawLsRemoteHost string
	d.credentials.GetByHostFunc = func(_ context.Context, host string) (credentialstore.Credential, error) {
		sawCredentialHost = host
		return credentialstore.Credential{Token: "tok"}, nil
	}
	d.cloner.LsRemoteFunc = func(_ context.Context, host, _ string) ([]byte, error) {
		sawLsRemoteHost = host
		return []byte("ref: refs/heads/main\tHEAD\nabc\trefs/heads/main\n"), nil
	}
	h := d.handler(t, "/data")
	_, err := h.ProbeRepo(t.Context(), probeReq("http://127.0.0.1:13030/e2eadmin/e2e-repo.git"))
	require.NoError(t, err)
	assert.Equal(t, wantHost, sawCredentialHost)
	assert.Equal(t, wantHost, sawLsRemoteHost)
}

// TestProbeRepo_EmptyURL_InvalidArgument proves rejection precedes any
// credential lookup or git subprocess.
func TestProbeRepo_EmptyURL_InvalidArgument(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	h := d.handler(t, "/data")
	_, err := h.ProbeRepo(t.Context(), probeReq(""))
	require.Error(t, err)
	var connErr *connect.Error
	require.ErrorAs(t, err, &connErr)
	assert.Equal(t, connect.CodeInvalidArgument, connErr.Code())
	assert.Empty(t, d.cloner.LsRemoteCalls())
}

// TestProbeRepo_LsRemoteFails_FailedPrecondition proves an ls-remote
// failure (unreachable/nonexistent repo) maps to CodeFailedPrecondition,
// not a generic internal error, and never fabricates a branch list.
func TestProbeRepo_LsRemoteFails_FailedPrecondition(t *testing.T) {
	t.Parallel()
	d := newTestDeps()
	d.cloner.LsRemoteFunc = func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("repository not found")
	}
	h := d.handler(t, "/data")
	_, err := h.ProbeRepo(t.Context(), probeReq("https://example.com/acme/widgets.git"))
	require.Error(t, err)
	var connErr *connect.Error
	require.ErrorAs(t, err, &connErr)
	assert.Equal(t, connect.CodeFailedPrecondition, connErr.Code())
}

// TestProbeRepo_UpstreamURLHasUserinfo_RejectedBeforeCredentialOrGitCall is
// loam-ra1k's fail-fast half for ProbeRepo: an upstream URL carrying
// embedded credentials (user:token@host, or the password-less PAT form
// "https://<token>@host/path") must be rejected as InvalidArgument before
// ProbeRepo ever resolves a credential or runs ls-remote -- transport-level
// rejection (loam-ys1) is necessary but not sufficient, since ProbeRepo's
// own %w-wrapped RPC error would otherwise hand the raw credential-bearing
// URL straight back to whoever submitted it, regardless of what
// gittransport does downstream. The embedded credential must never appear
// in the returned error's message, in either form.
func TestProbeRepo_UpstreamURLHasUserinfo_RejectedBeforeCredentialOrGitCall(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		upstreamURL string
	}{
		{"username and password", "https://user:leaked-token@example.com/acme/widgets.git"},
		{"username only, no password (PAT form)", "https://leaked-token@example.com/acme/widgets.git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := newTestDeps()
			h := d.handler(t, "/data")
			_, err := h.ProbeRepo(t.Context(), probeReq(tt.upstreamURL))
			require.Error(t, err)
			assert.Equal(t, connect.CodeInvalidArgument, connectCode(t, err))
			assert.NotContains(t, err.Error(), "leaked-token", "the rejected URL's embedded credential must never appear in the returned error")
			assert.Empty(t, d.credentials.GetByHostCalls(), "no credential should be resolved for a URL rejected before host derivation")
			assert.Empty(t, d.cloner.LsRemoteCalls(), "ls-remote must never run for a URL rejected before it")
		})
	}
}
