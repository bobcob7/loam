package forgehost

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCanonicalize_AcceptedForms is the bead's own table (loam-0hjq): a
// bare host, the same host with an https scheme, and a plaintext-HTTP host
// with its scheme, all resolving to the canonical form
// internal/handler/repoadmin's forgeHostOf would derive from the
// equivalent upstream URL.
func TestCanonicalize_AcceptedForms(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "bare host", raw: "git.bobcob7.com", want: "git.bobcob7.com"},
		{name: "https scheme is stripped to the bare host", raw: "https://git.bobcob7.com", want: "git.bobcob7.com"},
		{name: "http scheme with port is kept scheme-qualified", raw: "http://git.bobcob7.com:3000", want: "http://git.bobcob7.com:3000"},
		{name: "bare host with a port", raw: "git.bobcob7.com:3000", want: "git.bobcob7.com:3000"},
		{name: "leading and trailing whitespace is trimmed", raw: "  git.bobcob7.com  ", want: "git.bobcob7.com"},
		{name: "a bare trailing slash is tolerated", raw: "https://git.bobcob7.com/", want: "git.bobcob7.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Canonicalize(tt.raw)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestCanonicalize_AgreesWithFromURL proves the two exported functions
// this package exists to reconcile actually produce the same string for
// the same forge -- the property loam-0hjq's fix depends on, not just each
// function's own table in isolation.
func TestCanonicalize_AgreesWithFromURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		credentialRaw string
		upstreamURL   string
	}{
		{name: "https forge", credentialRaw: "https://git.bobcob7.com", upstreamURL: "https://git.bobcob7.com/acme/widgets"},
		{name: "plain http forge", credentialRaw: "http://forge.internal:3000", upstreamURL: "http://forge.internal:3000/acme/widgets"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fromCredential, err := Canonicalize(tt.credentialRaw)
			require.NoError(t, err)
			u, err := url.Parse(tt.upstreamURL)
			require.NoError(t, err)
			fromUpstream := FromURL(u)
			assert.Equal(t, fromUpstream, fromCredential,
				"a credential entered as %q must resolve to the same host EnrollRepo/ProbeRepo derive from %q", tt.credentialRaw, tt.upstreamURL)
		})
	}
}

// TestCanonicalize_Rejected covers every rejection case loam-0hjq's fix
// requires: a path, embedded userinfo, an unparseable host, an empty host,
// and a non-http(s) scheme. Each case is deliberately something WRONG,
// not merely differently spelled -- the accepted-forms table above already
// covers every legitimate spelling.
func TestCanonicalize_Rejected(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "whitespace only", raw: "   "},
		{name: "a path component", raw: "https://host/owner/repo"},
		{name: "a path component on a plain-http host", raw: "http://host/owner/repo"},
		{name: "a query component", raw: "https://host?foo=bar"},
		{name: "a fragment component", raw: "https://host#section"},
		{name: "embedded userinfo", raw: "https://token@host"},
		{name: "embedded userinfo with a password", raw: "https://user:pass@host"},
		{name: "a non-http(s) scheme", raw: "ftp://host"},
		{name: "unparseable", raw: "https://[::1"},
		{name: "empty host after a scheme", raw: "https://"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Canonicalize(tt.raw)
			require.Error(t, err)
			assert.ErrorIs(t, err, errInvalid)
		})
	}
}

// TestCanonicalize_NeverEchoesUserinfo is the loam-ra1k-consistent leak
// check: a host carrying a credential in its userinfo component must not
// have that credential appear anywhere in the rejection error.
func TestCanonicalize_NeverEchoesUserinfo(t *testing.T) {
	t.Parallel()
	const secret = "super-secret-pat-value-8f3c1a"
	_, err := Canonicalize("https://" + secret + "@git.bobcob7.com")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
}

// TestFromURL_MatchesTheBareVsQualifiedRule pins FromURL's own table
// directly, independent of Canonicalize, since forgeHostOf
// (internal/handler/repoadmin) calls FromURL, never Canonicalize.
func TestFromURL_MatchesTheBareVsQualifiedRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		u    *url.URL
		want string
	}{
		{name: "https is bare", u: &url.URL{Scheme: "https", Host: "github.com"}, want: "github.com"},
		{name: "http is scheme-qualified", u: &url.URL{Scheme: "http", Host: "forge.internal:3000"}, want: "http://forge.internal:3000"},
		{name: "the path is ignored", u: &url.URL{Scheme: "https", Host: "github.com", Path: "/acme/widgets"}, want: "github.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, FromURL(tt.u))
		})
	}
}
