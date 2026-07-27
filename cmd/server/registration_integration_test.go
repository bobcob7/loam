//go:build integration

// Requires a real Docker (or Docker-API-compatible) daemon; see
// main_integration_test.go's package doc for how to run this file (same
// package, same build tag -- it reuses startServer/newPostgres from
// there, and identityRoundTripper from registration_test.go).
package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/gen/loam/admin/v1/adminv1connect"
	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/gen/loam/v1/loamv1connect"
)

// TestServer_RepoServiceIsRegistered_NotGroupFallback is loam-ofg.11's
// central acceptance proof, against the REAL, booted binary with a REAL
// migrated Postgres -- not just buildRouter called directly with a
// hand-supplied, unreachable pool (registration_test.go's faster,
// container-free variants). It is the trap this bead exists to disarm:
// before RepoService had a handler anywhere, `loam clone`'s preflight
// GetRepo fell through to internal/server's group-level 404 fallback,
// which ALSO answers CodeNotFound -- so Demo M2 would report "repo not
// enrolled" when the real cause was "service not implemented", and its
// exit-3 assertion would pass for entirely the wrong reason.
//
// An unenrolled repo still fails here -- CodeNotFound is the CORRECT
// answer either way, from the real handler or the fallback -- so the code
// alone cannot discriminate the fix from the bug it replaces. The
// MESSAGE is what proves it: the fallback's is a fixed string ("no
// /loam.v1. service registered for ..."), unrelated to the request; the
// real handler's names the specific repo that was not found. Asserting
// on that message, not just the code, is deliberate and load-bearing --
// a regression that re-broke registration would still return
// CodeNotFound and could slip past a code-only assertion.
func TestServer_RepoServiceIsRegistered_NotGroupFallback(t *testing.T) {
	rs := startServer(t, newPostgres(t))
	client := loamv1connect.NewRepoServiceClient(&http.Client{Transport: identityRoundTripper{role: "author", base: newIsolatedTransport(t)}}, "http://"+rs.addr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.GetRepo(ctx, connect.NewRequest(&loamv1.GetRepoRequest{Repo: "bobcob7/never-enrolled"}))
	require.Error(t, err, "bobcob7/never-enrolled is genuinely not enrolled in this freshly migrated, empty database")
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodeNotFound, connectErr.Code(),
		"a genuinely unenrolled repo must be CodeNotFound -- the same code the fallback would also have produced, which is exactly why the message below, not this code, is the discriminating assertion")
	assert.NotContains(t, connectErr.Message(), "service registered",
		"the request must reach the real RepoServiceHandler this bead registers in cmd/server/main.go's buildRouter, never internal/server's group-level fallback for an unregistered /loam.v1.* service")
	assert.Contains(t, connectErr.Message(), "bobcob7/never-enrolled",
		"the real handler's not-found message names the requested repo; the fallback's fixed message never does")
}

// TestServer_MetaServiceIsRegistered_NotGroupFallback mirrors the above
// for MetaService.GetInstructions, the other handler this bead registers.
// Unlike RepoService, GetInstructions is never capability-gated (see
// internal/handler/meta's package doc), so against a real, freshly
// migrated database (author/reviewer seeded by 0001_init.up.sql) this
// call should SUCCEED outright -- the strongest possible proof of
// registration is that it does, and that the response is this bead's
// real, role-filtered command catalog, not a canned 404.
func TestServer_MetaServiceIsRegistered_NotGroupFallback(t *testing.T) {
	rs := startServer(t, newPostgres(t))
	client := loamv1connect.NewMetaServiceClient(&http.Client{Transport: identityRoundTripper{role: "author", base: newIsolatedTransport(t)}}, "http://"+rs.addr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := client.GetInstructions(ctx, connect.NewRequest(&loamv1.GetInstructionsRequest{}))
	require.NoError(t, err, "GetInstructions is never capability-gated and the author role is seeded by 0001_init.up.sql, so this must succeed against a freshly migrated database")
	names := make([]string, len(resp.Msg.GetCommands()))
	for i, command := range resp.Msg.GetCommands() {
		names[i] = command.GetName()
	}
	assert.Contains(t, names, "clone", "the seeded author role grants git.clone")
	assert.Contains(t, names, "instructions", "always-ungated commands are always present")
	assert.NotEmpty(t, resp.Msg.GetUsage())
}

// TestServer_WorkBranchServiceIsRegistered_NotGroupFallback is loam-ofg.8's
// central acceptance proof, mirroring TestServer_RepoServiceIsRegistered_
// NotGroupFallback above against the REAL, booted binary with a REAL
// migrated Postgres. A genuinely unenrolled repo is CodeNotFound either
// way -- from the real handler or the fallback -- so, exactly as that test
// documents, the code alone cannot discriminate the fix from the bug it
// replaces; the MESSAGE is the load-bearing assertion.
func TestServer_WorkBranchServiceIsRegistered_NotGroupFallback(t *testing.T) {
	rs := startServer(t, newPostgres(t))
	client := loamv1connect.NewWorkBranchServiceClient(&http.Client{Transport: identityRoundTripper{role: "author", base: newIsolatedTransport(t)}}, "http://"+rs.addr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.GetWorkBranch(ctx, connect.NewRequest(&loamv1.GetWorkBranchRequest{Repo: "bobcob7/never-enrolled", WorkBranch: "wb-000000"}))
	require.Error(t, err, "bobcob7/never-enrolled is genuinely not enrolled in this freshly migrated, empty database")
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodeNotFound, connectErr.Code(),
		"a genuinely unenrolled repo must be CodeNotFound -- the same code the fallback would also have produced, which is exactly why the message below, not this code, is the discriminating assertion")
	assert.NotContains(t, connectErr.Message(), "service registered",
		"the request must reach the real WorkBranchServiceHandler this bead registers in cmd/server/main.go's buildRouter, never internal/server's group-level fallback for an unregistered /loam.v1.* service")
	assert.Contains(t, connectErr.Message(), "bobcob7/never-enrolled",
		"the real handler's not-found message names the requested repo; the fallback's fixed message never does")
}

// TestServer_RepoAdminServiceIsRegistered_NotGroupFallback is loam-ofg.12's
// central acceptance proof, against the REAL, booted binary with a REAL
// migrated Postgres -- mirroring TestServer_RepoServiceIsRegistered_
// NotGroupFallback's reasoning exactly: before RepoAdminService had a
// handler anywhere, every /loam.admin.v1.RepoAdminService/* request fell
// through to internal/server's group-level 404 fallback, which ALSO
// answers CodeNotFound for a genuinely-unenrolled repo -- so the code
// alone cannot discriminate the fix from the bug it replaces. The MESSAGE
// is what proves it: the fallback's is a fixed string ("no
// /loam.admin.v1. service registered for ..."), unrelated to the request;
// the real handler's names the specific repo that was not found.
func TestServer_RepoAdminServiceIsRegistered_NotGroupFallback(t *testing.T) {
	rs := startServer(t, newPostgres(t))
	client := adminv1connect.NewRepoAdminServiceClient(&http.Client{Transport: adminRoundTripper{user: testAdminUser, password: testAdminPassword, base: newIsolatedTransport(t)}}, "http://"+rs.addr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.GetRepo(ctx, connect.NewRequest(&adminv1.GetRepoRequest{Repo: "bobcob7/never-enrolled"}))
	require.Error(t, err, "bobcob7/never-enrolled is genuinely not enrolled in this freshly migrated, empty database")
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodeNotFound, connectErr.Code(),
		"a genuinely unenrolled repo must be CodeNotFound -- the same code the fallback would also have produced, which is exactly why the message below, not this code, is the discriminating assertion")
	assert.NotContains(t, connectErr.Message(), "service registered",
		"the request must reach the real RepoAdminServiceHandler this bead registers in cmd/server/main.go's buildRouter, never internal/server's group-level fallback for an unregistered /loam.admin.v1.* service")
	assert.Contains(t, connectErr.Message(), "bobcob7/never-enrolled",
		"the real handler's not-found message names the requested repo; the fallback's fixed message never does")
}

// TestServer_GraphServiceIsRegistered_NotGroupFallback is loam-ofg.10's
// central acceptance proof for GraphService, against the REAL, booted
// binary with a REAL migrated Postgres -- the exact trap this bead's own
// brief calls out by name: loam-ofg.11 initially registered its services
// only when a pool was supplied while run() supplied nil, so the handler
// was unreachable in production while every test passed. A freshly
// migrated, empty database has no enrolled repos, so an unscoped Query
// resolves QueryScope's empty `repos` to "every enrolled repo" (zero of
// them) and the requested symbol is, correctly, not found anywhere in that
// empty scope -- CodeNotFound, the SAME code internal/server's group-level
// 404 fallback would also produce for an unregistered service, so the code
// alone cannot discriminate the real handler from the fallback it replaces.
// The MESSAGE is what proves it: the fallback's is a fixed string ("no
// /loam.v1. service registered for ..."), unrelated to the request; the
// real handler's names the specific symbol that was not found.
func TestServer_GraphServiceIsRegistered_NotGroupFallback(t *testing.T) {
	rs := startServer(t, newPostgres(t))
	client := loamv1connect.NewGraphServiceClient(&http.Client{Transport: identityRoundTripper{role: "author", base: newIsolatedTransport(t)}}, "http://"+rs.addr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.Query(ctx, connect.NewRequest(&loamv1.QueryRequest{Query: &loamv1.QueryRequest_Definition{Definition: &loamv1.DefinitionQuery{Symbol: "NeverDefinedAnywhere"}}}))
	require.Error(t, err, "a freshly migrated, empty database has no enrolled repos, so no symbol can be found anywhere in scope")
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodeNotFound, connectErr.Code(),
		"a symbol not found in an empty scope must be CodeNotFound -- the same code the fallback would also have produced, which is exactly why the message below, not this code, is the discriminating assertion")
	assert.NotContains(t, connectErr.Message(), "service registered",
		"the request must reach the real GraphServiceHandler this bead registers in cmd/server/main.go's buildRouter, never internal/server's group-level fallback for an unregistered /loam.v1.* service")
	assert.Contains(t, connectErr.Message(), "NeverDefinedAnywhere",
		"the real handler's not-found message names the requested symbol; the fallback's fixed message never does")
}

// TestServer_SearchServiceIsRegistered_NotGroupFallback mirrors
// TestServer_GraphServiceIsRegistered_NotGroupFallback for SearchService,
// the second handler this bead registers. Unlike GraphService, Search
// always calls its Embedder before consulting scope, so this proof does
// not depend on the freshly migrated database having zero enrolled repos:
// whatever the real embedder does (succeed against a live Ollama, or fail
// reaching an absent one in this test environment), the response comes from
// the real SearchServiceHandler either way -- the discriminating assertion
// is the same "never the group fallback's fixed message" check.
func TestServer_SearchServiceIsRegistered_NotGroupFallback(t *testing.T) {
	rs := startServer(t, newPostgres(t))
	client := loamv1connect.NewSearchServiceClient(&http.Client{Transport: identityRoundTripper{role: "author", base: newIsolatedTransport(t)}}, "http://"+rs.addr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.Search(ctx, connect.NewRequest(&loamv1.SearchRequest{Query: "how is authentication handled"}))
	require.Error(t, err, "no embedder is reachable in this test environment (LOAM_EMBEDDER_URL defaults to an unreachable localhost Ollama), so the real handler's embed step fails")
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.NotContains(t, connectErr.Message(), "service registered",
		"the request must reach the real SearchServiceHandler this bead registers in cmd/server/main.go's buildRouter, never internal/server's group-level fallback for an unregistered /loam.v1.* service")
}
