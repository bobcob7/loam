package repoadmin

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
