package repoadmin

import (
	"errors"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/gittransport"
)

// TestDeriveRepoIdentity_HostByScheme is the loam-4kz regression at the
// point the bug was actually rooted: an https upstream (the overwhelming
// majority) must derive the exact same BARE host this function has
// always derived, so every existing credentials/repos row and every
// documented admin workflow keeps working unmodified; an http upstream
// (a plaintext-HTTP, typically self-hosted forge) must derive a
// scheme-QUALIFIED host, the one form internal/forge's apiBaseURL can
// address correctly without silently dialling https at a listener that
// never speaks TLS.
func TestDeriveRepoIdentity_HostByScheme(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		upstreamURL string
		wantHost    string
		wantName    string
	}{
		{name: "https upstream derives a bare host, unchanged from before loam-4kz", upstreamURL: "https://forgejo.example.com/acme/widgets.git", wantHost: "forgejo.example.com", wantName: "acme/widgets"},
		{name: "http upstream derives a scheme-qualified host", upstreamURL: "http://127.0.0.1:13030/e2eadmin/e2e-repo.git", wantHost: "http://127.0.0.1:13030", wantName: "e2eadmin/e2e-repo"},
		{name: "http upstream with no port still qualifies the scheme", upstreamURL: "http://forge.internal/group/repo.git", wantHost: "http://forge.internal", wantName: "group/repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			host, name, err := deriveRepoIdentity(tt.upstreamURL)
			require.NoError(t, err)
			assert.Equal(t, tt.wantHost, host)
			assert.Equal(t, tt.wantName, name)
		})
	}
}

// TestDeriveRepoIdentity_HTTPHostMatchesForgeHostOf_AsProbeRepoMustAgree
// pins the invariant EnrollRepo and ProbeRepo both depend on: they MUST
// derive byte-identical host strings from the same upstream URL, because
// a credential set against one has to be found by the other
// (internal/credentialstore.GetByHost is exact-string keyed, with no
// normalization chokepoint reconciling a mismatched pair -- see
// forgeHostOf's own doc comment). This test would catch either function
// drifting to a different derivation independently.
func TestDeriveRepoIdentity_HTTPHostMatchesForgeHostOf_AsProbeRepoMustAgree(t *testing.T) {
	t.Parallel()
	host, _, err := deriveRepoIdentity("http://127.0.0.1:13030/e2eadmin/e2e-repo.git")
	require.NoError(t, err)
	u, err := url.Parse("http://127.0.0.1:13030/e2eadmin/e2e-repo.git")
	require.NoError(t, err)
	assert.Equal(t, host, forgeHostOf(u), "EnrollRepo (deriveRepoIdentity) and ProbeRepo (forgeHostOf) must derive the identical host string")
}

// TestDeriveRepoIdentity_UserinfoRejected is loam-ra1k's fail-fast half at
// EnrollRepo's own choke point: deriveRepoIdentity must reject an upstream
// URL carrying embedded credentials via the SAME shared sentinel
// gittransport.Transport rejects it with (loam-ys1), rather than a second,
// drifting "does this URL carry credentials" check -- and the rejection
// must never echo the credential itself, in either the standard
// user:password form or the password-less PAT form
// ("https://<token>@host/path"), the likelier real-world shape and the one
// a naive ":"-based string replace would miss entirely.
func TestDeriveRepoIdentity_UserinfoRejected(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		upstreamURL string
	}{
		{"username and password", "https://user:leaked-token@forge.example.com/acme/widgets.git"},
		{"username only, no password (PAT form)", "https://leaked-token@forge.example.com/acme/widgets.git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			host, name, err := deriveRepoIdentity(tt.upstreamURL)
			require.Error(t, err)
			assert.ErrorIs(t, err, gittransport.ErrUpstreamURLHasUserinfo)
			assert.NotContains(t, err.Error(), "leaked-token", "the rejected URL's embedded credential must never appear in the error")
			assert.Empty(t, host)
			assert.Empty(t, name)
		})
	}
}

// TestDeriveRepoIdentity_UnparseableURL_NeverWrapsTheUnderlyingURLError
// covers the other route *url.Error itself can leak a credential through:
// its own Error() renders as `parse "<raw url>": <reason>`, so %w-wrapping
// it would still echo an embedded credential even when validation never
// gets as far as inspecting u.User. deriveRepoIdentity must not do that.
func TestDeriveRepoIdentity_UnparseableURL_NeverWrapsTheUnderlyingURLError(t *testing.T) {
	t.Parallel()
	// A raw space inside the userinfo component makes net/url refuse to
	// parse the URL at all, rather than surfacing it via u.User.
	const poisoned = "https://user:leaked-token with a space@forge.example.com/acme/widgets.git"
	_, _, err := deriveRepoIdentity(poisoned)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "leaked-token", "an unparseable URL's embedded credential must never appear in the error")
	var urlErr *url.Error
	assert.False(t, errors.As(err, &urlErr), "the underlying *url.Error must never be %w-wrapped into the returned error, since its own Error() renders the raw URL verbatim")
}
