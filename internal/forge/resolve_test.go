package forge

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resolveTestLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// TestKindForHost_ExistingForgejoHostsAreUnchanged pins loam-tmds.1's
// central promise: every host shape this project already enrolls
// resolves to KindForgejo exactly as it did before this epic, with no
// migration and no new column to backfill (AC6) — the resolution is a
// pure function of the host string, so a pre-existing repo's forge_host
// keeps resolving without needing to be touched at all.
func TestKindForHost_ExistingForgejoHostsAreUnchanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		host string
	}{
		{name: "a typical self-hosted bare host", host: "git.bobcob7.com"},
		{name: "a self-hosted host with a port", host: "forge.internal:3000"},
		{name: "a scheme-qualified host, as tests and CheckRepo's bound-host guard use", host: "https://git.bobcob7.com"},
		{name: "a plain-http self-hosted host", host: "http://forge.internal:3000"},
		{name: "an httptest-style loopback host", host: "127.0.0.1:54321"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			kind, err := KindForHost(tt.host)
			require.NoError(t, err)
			assert.Equal(t, KindForgejo, kind)
		})
	}
}

// TestKindForHost_GitHubAliasesResolveToGitHub covers the two host
// spellings loam-tmds.5 says must agree: the web/git host an operator
// enters, and the REST-API host in case they enter that one instead.
func TestKindForHost_GitHubAliasesResolveToGitHub(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		host string
	}{
		{name: "github.com", host: "github.com"},
		{name: "api.github.com", host: "api.github.com"},
		{name: "uppercase is tolerated", host: "GitHub.com"},
		{name: "a scheme-qualified github.com", host: "https://github.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			kind, err := KindForHost(tt.host)
			require.NoError(t, err)
			assert.Equal(t, KindGitHub, kind)
		})
	}
}

// TestKindForHost_UnsupportedHostsFailLoudly is loam-tmds.1's AC4: an
// unknown or unsupported forge kind must fail at resolution, naming the
// host, and must NOT silently fall back to a default (KindForgejo). Two
// distinct cases exercise this: a host that looks like GitHub Enterprise
// Server (out of scope, and the exact case loam-tmds.1's notes call the
// worst failure mode if silently misrouted), and an empty host (the
// "no repo row to read yet, and no host either" case a resolution call
// site should never actually reach).
func TestKindForHost_UnsupportedHostsFailLoudly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		host string
	}{
		{name: "a GitHub Enterprise Server-shaped host", host: "github.example.com"},
		{name: "another GitHub Enterprise Server-shaped host", host: "my-github-enterprise.internal"},
		{name: "empty host", host: ""},
		{name: "whitespace-only host", host: "   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			kind, err := KindForHost(tt.host)
			require.Error(t, err)
			assert.ErrorIs(t, err, errUnsupportedForgeKind)
			assert.Empty(t, kind, "an unresolvable host must not silently resolve to a default Kind")
		})
	}
}

// TestKindForHost_GitHubEnterpriseErrorNamesTheHost proves the failure is
// actionable, not just present: the error must name the actual host that
// could not be resolved, so an admin (or a maintainer reading a log)
// knows what to fix.
func TestKindForHost_GitHubEnterpriseErrorNamesTheHost(t *testing.T) {
	t.Parallel()
	_, err := KindForHost("github.example.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "github.example.com")
}

// TestKindForHost_NonGitHubNamedEnterpriseHostsResolveToForgejoUnmitigated
// is the boundary a review found this package's own comments and
// docs/sync-spec.md previously overstated: the "contains github" check
// is a substring heuristic, not GitHub Enterprise Server detection.
// GHE installs at a hostname NOT containing "github" (the ordinary
// case -- an operator names their GHE instance after their own company,
// not GitHub's) are indistinguishable from a genuine self-hosted
// Forgejo and silently resolve to KindForgejo here, exactly the
// silent-misrouting failure this package's other tests prove is caught
// for the narrower, vendor-named case. This test exists so that gap is
// demonstrated, not merely described in prose that could drift from the
// code — see docs/sync-spec.md's Limits section for the operator-facing
// statement this test backs.
func TestKindForHost_NonGitHubNamedEnterpriseHostsResolveToForgejoUnmitigated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		host string
	}{
		{name: "a GHE host named after the customer's own company", host: "git.acme.com"},
		{name: "a GHE host with no forge-branded token at all", host: "source.corp.io"},
		{name: "a GHE host named scm", host: "scm.example.net"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			kind, err := KindForHost(tt.host)
			require.NoError(t, err, "this is the documented gap: a non-github-named GHE host resolves with no error at all")
			assert.Equal(t, KindForgejo, kind, "and resolves to Forgejo, exactly as an actual self-hosted Forgejo at this host would -- the two are indistinguishable by host alone")
		})
	}
}

// TestNewProvider_Forgejo proves NewProvider constructs a working
// *Forgejo for a Forgejo-shaped host — the seam plus Forgejo behind it,
// with everything still passing, loam-tmds.1's own summary of its
// deliverable.
func TestNewProvider_Forgejo(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
	}))
	t.Cleanup(server.Close)
	provider, err := NewProvider(server.URL, "tkn", server.Client(), resolveTestLogger())
	require.NoError(t, err)
	require.IsType(t, &Forgejo{}, provider)
	require.NoError(t, provider.ValidateToken(t.Context(), server.URL, "tkn"))
}

// TestNewProvider_UnsupportedForgeKind_DoesNotFallBackToForgejo is the
// AC4 test at NewProvider's own level (KindForHost's own test above
// covers the resolution function directly): a host NewProvider cannot
// resolve must return an error, and must NOT return a *Forgejo bound to
// that host as a silent default.
func TestNewProvider_UnsupportedForgeKind_DoesNotFallBackToForgejo(t *testing.T) {
	t.Parallel()
	provider, err := NewProvider("github.example.com", "tkn", &http.Client{}, resolveTestLogger())
	require.Error(t, err)
	assert.ErrorIs(t, err, errUnsupportedForgeKind)
	assert.Nil(t, provider, "an unresolvable host must not silently construct any provider, Forgejo included")
}

// TestNewProvider_GitHub proves NewProvider constructs a working
// *GitHub for a GitHub-shaped host (loam-tmds.2 wires this in; before
// that bead landed, this same host resolved to an explicit
// not-implemented error — see loam-tmds.1's commit).
func TestNewProvider_GitHub(t *testing.T) {
	t.Parallel()
	kind, err := KindForHost("github.com")
	require.NoError(t, err)
	assert.Equal(t, KindGitHub, kind)
	provider, err := NewProvider("github.com", "tkn", &http.Client{}, resolveTestLogger())
	require.NoError(t, err)
	assert.IsType(t, &GitHub{}, provider)
}

// TestResolver_ValidateToken_DispatchesPerCallHost proves the whole
// reason Resolver exists: ONE instance, reused across calls naming
// DIFFERENT hosts, must resolve each call's host independently rather
// than being bound to whichever host constructed it (there is no such
// host — NewResolver takes none).
func TestResolver_ValidateToken_DispatchesPerCallHost(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
	}))
	t.Cleanup(server.Close)
	resolver := NewResolver(server.Client(), resolveTestLogger())
	require.NoError(t, resolver.ValidateToken(t.Context(), server.URL, "tkn"))
	_, err := NewProvider("github.example.com", "tkn", server.Client(), resolveTestLogger())
	require.Error(t, err)
	err = resolver.ValidateToken(t.Context(), "github.example.com", "tkn")
	require.Error(t, err, "the SAME Resolver instance must refuse an unresolvable host on its very next call, proving it re-resolves Kind every time rather than caching the first host it saw")
	assert.ErrorIs(t, err, errUnsupportedForgeKind)
}

// TestResolver_GitCredentials_NeedsNoHost proves GitCredentials answers
// without ever resolving a Kind: an empty token is still rejected (the
// one thing gitCredentialsConvention itself validates), but a
// nonexistent/unresolvable host is never in play because the method
// signature has no host parameter at all.
func TestResolver_GitCredentials_NeedsNoHost(t *testing.T) {
	t.Parallel()
	resolver := NewResolver(&http.Client{}, resolveTestLogger())
	username, password, err := resolver.GitCredentials(t.Context(), "a-token")
	require.NoError(t, err)
	assert.NotEmpty(t, username)
	assert.Equal(t, "a-token", password)
	_, _, err = resolver.GitCredentials(t.Context(), "")
	assert.ErrorIs(t, err, ErrInvalidToken)
}
