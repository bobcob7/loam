//go:build acceptance

// The Admin actor driver (testing-spec Layer 1's table): a connect-go
// client authenticating with plain HTTP basic auth, never the SPA (Layer
// 3's own concern).
package main

import (
	"context"
	"fmt"
	"net/http"

	"connectrpc.com/connect"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/gen/loam/admin/v1/adminv1connect"
)

// basicAuthRoundTripper injects a fixed HTTP Basic Authorization header
// into every outgoing request, the Admin actor's one and only auth
// mechanism per testing-spec Layer 1's table (no SPA session, no bearer
// token).
type basicAuthRoundTripper struct {
	user, password string
	base           http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (rt basicAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.SetBasicAuth(rt.user, rt.password)
	return rt.base.RoundTrip(req)
}

// adminHTTPClient builds the connect.HTTPClient every Admin-actor connect-go
// client in this suite authenticates through.
func (h *acceptanceHarness) adminHTTPClient() *http.Client {
	return &http.Client{Transport: basicAuthRoundTripper{user: h.adminUser, password: h.adminPassword, base: http.DefaultTransport}}
}

// newProposalServiceClient builds the Admin actor's connect-go client for
// loam.admin.v1.ProposalService, backing the core vocabulary row "I accept
// it" (docs/testing-spec.md Layer 1's step-vocabulary table).
func (h *acceptanceHarness) newProposalServiceClient() adminv1connect.ProposalServiceClient {
	return adminv1connect.NewProposalServiceClient(h.adminHTTPClient(), h.server.baseURL)
}

// acceptProposal calls ProposalService.AcceptProposal for (repo,
// workBranch) and returns the whole response -- the accepted PR's URL and
// the upstream branch the accept pushed -- so a caller can assert on both
// halves of what the accept engine reported doing.
//
// The error is returned UNWRAPPED. Callers that mean to observe a REFUSED
// accept classify it with connect.CodeOf (requireRPCRejected,
// acceptance_proposal_test.go), and wrapping would not break that -- but
// the wrap adds nothing the caller does not already know, since every call
// site names the repo and work branch itself.
func acceptProposal(ctx context.Context, client adminv1connect.ProposalServiceClient, repo, workBranch string) (*adminv1.AcceptProposalResponse, error) {
	resp, err := client.AcceptProposal(ctx, connect.NewRequest(&adminv1.AcceptProposalRequest{Repo: repo, WorkBranch: workBranch}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// newRepoAdminServiceClient builds the Admin actor's connect-go client for
// loam.admin.v1.RepoAdminService -- the service backing "I am signed in to
// the web interface as the admin" and every "the repo's sync status ..."
// assertion in features/sync.feature.
func (h *acceptanceHarness) newRepoAdminServiceClient() adminv1connect.RepoAdminServiceClient {
	return adminv1connect.NewRepoAdminServiceClient(h.adminHTTPClient(), h.server.baseURL)
}

// listReposAsAdmin calls RepoAdminService.ListRepos as the Admin actor,
// the smallest authenticated round trip that proves the admin credentials
// are accepted.
func (h *acceptanceHarness) listReposAsAdmin(ctx context.Context) ([]*adminv1.EnrolledRepo, error) {
	resp, err := h.newRepoAdminServiceClient().ListRepos(ctx, connect.NewRequest(&adminv1.ListReposRequest{}))
	if err != nil {
		return nil, fmt.Errorf("listing enrolled repos: %w", err)
	}
	return resp.Msg.GetRepos(), nil
}

// getRepoAsAdmin reads one enrolled repo back through the admin API --
// including its SyncStatus, which is how every sync-status assertion in
// this suite observes repos.sync_state: through the surface an admin
// actually sees, not a direct SQL read of the column.
func (h *acceptanceHarness) getRepoAsAdmin(ctx context.Context, repo string) (*adminv1.EnrolledRepo, error) {
	resp, err := h.newRepoAdminServiceClient().GetRepo(ctx, connect.NewRequest(&adminv1.GetRepoRequest{Repo: repo}))
	if err != nil {
		return nil, fmt.Errorf("getting enrolled repo %s: %w", repo, err)
	}
	return resp.Msg.GetRepo(), nil
}
