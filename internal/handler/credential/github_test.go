package credential

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/forge"
	"github.com/bobcob7/loam/internal/handler"
)

// redirectToTestServer is an http.RoundTripper that rewrites every
// outgoing request's scheme and host to point at a local httptest
// server, keeping the path and everything else — the mechanism this
// file uses to drive SetUpstreamToken with host="github.com" (so
// forge.NewProvider genuinely resolves KindGitHub and forge.GitHub
// genuinely builds the real https://api.github.com base URL) WITHOUT
// ever letting a request reach the real network: the rewrite happens
// before any DNS lookup or connection attempt, inside RoundTrip itself.
// This is the only way to exercise this bead's actual production
// composition (CredentialService -> forge.Resolver -> KindForHost ->
// forge.GitHub) rather than a lower-level proxy for it, while honoring
// the hard constraint that no test in this tree may call the real
// GitHub API.
type redirectToTestServer struct {
	target *url.URL
}

func (rt *redirectToTestServer) RoundTrip(req *http.Request) (*http.Response, error) {
	redirected := req.Clone(req.Context())
	redirected.URL.Scheme = rt.target.Scheme
	redirected.URL.Host = rt.target.Host
	redirected.Host = rt.target.Host
	return http.DefaultTransport.RoundTrip(redirected)
}

// newGitHubBoundHandler builds a real credential.Handler wired exactly as
// cmd/server's registerCredentialService wires production
// (forge.NewResolver over a real *http.Client — see credential.go's New
// doc comment), except the http.Client's Transport redirects every
// request to ts, a local httptest server standing in for
// https://api.github.com. The store mock accepts any write silently: this
// test is about the VALIDATION verdict reaching the right Connect code,
// not about store persistence, which credential_test.go's own suite
// already covers generically.
func newGitHubBoundHandler(t *testing.T, ts *httptest.Server) *Handler {
	t.Helper()
	target, err := url.Parse(ts.URL)
	require.NoError(t, err)
	httpClient := &http.Client{Transport: &redirectToTestServer{target: target}}
	resolver := forge.NewResolver(httpClient, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	store := &credentialStoreMock{
		UpsertTokenFunc: func(_ context.Context, host, _ string) (credentialstore.CredentialStatus, error) {
			return credentialstore.CredentialStatus{Host: host, HasToken: true}, nil
		},
		SetValidatedFunc: func(_ context.Context, host string, validated bool) (credentialstore.CredentialStatus, error) {
			return credentialstore.CredentialStatus{Host: host, HasToken: true, Validated: validated}, nil
		},
	}
	return New(store, resolver, handler.NewErrorMapper(testLogger(io.Discard)), testLogger(io.Discard))
}

// TestSetUpstreamToken_GitHubHost_EndToEndThroughTheRealResolver is
// loam-tmds.5's AC2, exercised through the ACTUAL production composition
// rather than a mocked tokenValidator: a host that resolves to
// KindGitHub (internal/forge.KindForHost) must have its candidate token
// validated by the real forge.GitHub, distinguishing "does not
// authenticate" from "authenticates but lacks scope" exactly as it does
// for Forgejo — proving the Resolver/KindForHost seam (loam-tmds.1)
// really does reach forge.GitHub's own ValidateToken (loam-tmds.2) for a
// github.com host, not merely that each half works in isolation.
func TestSetUpstreamToken_GitHubHost_EndToEndThroughTheRealResolver(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		statusCode   int
		scopesHeader string
		wantCode     connect.Code
	}{
		{name: "unauthenticated token", statusCode: http.StatusUnauthorized, wantCode: connect.CodeInvalidArgument},
		{name: "authenticated but missing repo scope", statusCode: http.StatusOK, scopesHeader: "gist, notifications", wantCode: connect.CodeFailedPrecondition},
		{name: "authenticated with repo scope", statusCode: http.StatusOK, scopesHeader: "repo, gist", wantCode: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/user", r.URL.Path, "the redirected request must still target GitHub's own /user path")
				if tt.scopesHeader != "" {
					w.Header().Set("X-OAuth-Scopes", tt.scopesHeader)
				}
				w.WriteHeader(tt.statusCode)
			}))
			defer ts.Close()
			h := newGitHubBoundHandler(t, ts)
			_, err := h.SetUpstreamToken(adminCtx(t), setTokenReq("github.com", "a-github-token"))
			if tt.wantCode == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			var connectErr *connect.Error
			require.ErrorAs(t, err, &connectErr)
			assert.Equal(t, tt.wantCode, connectErr.Code())
		})
	}
}
