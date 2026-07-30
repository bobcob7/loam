package repoadmin

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
