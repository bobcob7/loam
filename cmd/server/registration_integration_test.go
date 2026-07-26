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
