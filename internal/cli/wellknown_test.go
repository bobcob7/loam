package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/gen/loam/v1/loamv1connect"
	"github.com/bobcob7/loam/internal/httpauth"
)

// loam-hi5o.31's CLI half: `loam instructions` with no LOAM_AGENT_*
// configured resolves to the well-known orchestrator identity and calls
// GetInstructions through the ORDINARY authenticated path, while
// LOAM_SERVER_URL stays genuinely required.
//
// NOTE: every test in this file uses t.Setenv, which the testing package
// forbids combining with t.Parallel() since environment variables are
// process-global. None of them call t.Parallel(); each one blanks all four
// LOAM_* variables itself rather than inheriting whatever the developer's
// or CI's ambient environment happens to export, since "unset" is the
// precondition under test and an ambient LOAM_AGENT_ROLE would silently
// turn several of these green for the wrong reason.

// noIdentityEnv blanks every LOAM_AGENT_* variable and LOAM_OUTPUT_FORMAT,
// leaving LOAM_SERVER_URL at serverURL -- the exact state an orchestrator
// that configured nothing but the server address is in.
func noIdentityEnv(t *testing.T, serverURL string) {
	t.Helper()
	t.Setenv(envServerURL, serverURL)
	t.Setenv(envAgentName, "")
	t.Setenv(envAgentID, "")
	t.Setenv(envAgentRole, "")
	t.Setenv(envOutputFormat, "")
}

// TestLoadOrchestratorConfig_NoIdentity_ResolvesWellKnownIdentity is the
// first half of acceptance criterion 13: with LOAM_SERVER_URL set and every
// LOAM_AGENT_* unset, config loading succeeds and yields the well-known
// orchestrator identity rather than an error.
func TestLoadOrchestratorConfig_NoIdentity_ResolvesWellKnownIdentity(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	noIdentityEnv(t, "https://loam.example")
	cfg, err := loadOrchestratorConfig()
	require.NoError(t, err)
	assert.Equal(t, "https://loam.example", cfg.ServerURL())
	assert.Equal(t, "loam-orchestrator", cfg.AgentName())
	assert.Equal(t, "0", cfg.AgentID())
	assert.Equal(t, "orchestrator", cfg.AgentRole())
	assert.Equal(t, "loam-orchestrator-0-orchestrator", cfg.Identifier())
}

// TestLoadOrchestratorConfig_MissingServerURL_NamesOnlyThatVariable is the
// second half of acceptance criterion 13, and the specific regression it
// guards is the OLD behaviour: an unconfigured workspace used to be told
// all four of LOAM_SERVER_URL, LOAM_AGENT_NAME, LOAM_AGENT_ID and
// LOAM_AGENT_ROLE were missing. Three of those are no longer required for
// this command, so naming them would send an operator to configure things
// that would change nothing. Asserting only that the message CONTAINS
// LOAM_SERVER_URL would pass against that old four-variable message
// verbatim, so this asserts the other three are ABSENT too.
func TestLoadOrchestratorConfig_MissingServerURL_NamesOnlyThatVariable(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	noIdentityEnv(t, "")
	_, err := loadOrchestratorConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), envServerURL)
	for _, name := range []string{envAgentName, envAgentID, envAgentRole} {
		assert.NotContainsf(t, err.Error(), name, "%s is not required by `instructions` and must not be named", name)
	}
	assert.ErrorIs(t, err, errUsage, "a missing required variable is still a usage error (exit 2)")
	assert.ErrorIs(t, err, errMissingEnv)
}

// TestLoadOrchestratorConfig_MalformedServerURL_StillRejected proves the
// relaxed identity requirement did not relax LOAM_SERVER_URL's own
// validation: a set-but-unparseable value is still a usage error, exactly
// as loadConfig treats it.
func TestLoadOrchestratorConfig_MalformedServerURL_StillRejected(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	noIdentityEnv(t, "not-a-url")
	_, err := loadOrchestratorConfig()
	require.Error(t, err)
	assert.ErrorIs(t, err, errMalformedEnv)
}

// TestConfigForArgs_InstructionsWithoutIdentity_UsesOrchestratorConfig
// proves the fallback is reached through the real dispatch-time seam, not
// only by calling loadOrchestratorConfig directly.
func TestConfigForArgs_InstructionsWithoutIdentity_UsesOrchestratorConfig(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	noIdentityEnv(t, "https://loam.example")
	for _, args := range [][]string{{"instructions"}, {"instructions", "work start"}} {
		cfg, err := configForArgs(args)
		require.NoErrorf(t, err, "args %v", args)
		assert.Equal(t, "orchestrator", cfg.AgentRole(), "args %v", args)
	}
}

// TestConfigForArgs_InstructionsWithAnyIdentitySet_UsesFullConfig is the
// fallback's boundary, and the surprising case it exists to rule out. A
// PARTIALLY configured identity is a mistake, not an orchestrator: silently
// answering it as the orchestrator would tell an agent that misspelled
// LOAM_AGENT_NAME it may do less than its real role allows, and it would
// look like a success. Each subtest sets exactly one of the three and
// asserts the error still names the two that are genuinely missing.
func TestConfigForArgs_InstructionsWithAnyIdentitySet_UsesFullConfig(t *testing.T) {
	for _, tt := range []struct {
		name       string
		set        string
		value      string
		wantNamed  []string
		wantUnamed []string
	}{
		{name: "only name", set: envAgentName, value: "ada-lovelace", wantNamed: []string{envAgentID, envAgentRole}, wantUnamed: []string{envAgentName}},
		{name: "only id", set: envAgentID, value: "7", wantNamed: []string{envAgentName, envAgentRole}, wantUnamed: []string{envAgentID}},
		{name: "only role", set: envAgentRole, value: "author", wantNamed: []string{envAgentName, envAgentID}, wantUnamed: []string{envAgentRole}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Not parallel: t.Setenv is incompatible with t.Parallel.
			noIdentityEnv(t, "https://loam.example")
			t.Setenv(tt.set, tt.value)
			_, err := configForArgs([]string{"instructions"})
			require.Error(t, err, "a partial identity must not silently resolve to the orchestrator")
			for _, name := range tt.wantNamed {
				assert.Contains(t, err.Error(), name)
			}
			for _, name := range tt.wantUnamed {
				assert.NotContains(t, err.Error(), name+" is required")
			}
		})
	}
}

// TestConfigForArgs_InstructionsWithFullIdentity_UnchangedFromBefore proves
// the ordinary case did not move: an agent that configured all four
// variables gets its own role, never the orchestrator's.
func TestConfigForArgs_InstructionsWithFullIdentity_UnchangedFromBefore(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	baseEnv(t)
	cfg, err := configForArgs([]string{"instructions"})
	require.NoError(t, err)
	assert.Equal(t, "reviewer", cfg.AgentRole())
	assert.Equal(t, "ada-lovelace-7-reviewer", cfg.Identifier())
}

// TestConfigForArgs_WhoamiWithoutIdentity_StillErrors pins the deliberate
// asymmetry between the two ungated orientation commands. `whoami` reports
// the identity an operator CONFIGURED; handing it a synthetic one nobody
// set would make "I misconfigured this agent" indistinguishable from "I ran
// this deliberately anonymous" in the one command whose whole job is
// diagnosing the former.
func TestConfigForArgs_WhoamiWithoutIdentity_StillErrors(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	noIdentityEnv(t, "https://loam.example")
	_, err := configForArgs([]string{"whoami"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), envAgentName)
	assert.Contains(t, err.Error(), envAgentID)
	assert.Contains(t, err.Error(), envAgentRole)
}

// TestInstructionsWithoutIdentity_SendsWellKnownHeadersOverTheOrdinaryAuthPath
// is this bead's end-to-end CLI proof, and the reason it routes through
// internal/httpauth's REAL CLI() wrapper rather than reading the raw
// headers: the design's central claim is that the well-known identity uses
// the ORDINARY AUTHENTICATED PATH and needs no unauthenticated route. A
// request that failed httpauth's own fail-closed check would 401 here
// exactly as it would in production, so this fails if the fallback ever
// stopped producing a complete, resolvable identity.
//
// It drives the whole binary's entry sequence -- NewProductionDeps (which
// is where configForArgs runs), NewRouter, Run -- rather than calling
// runInstructions with a hand-built Deps, because the fallback lives in
// Deps CONSTRUCTION and a test that built Deps itself would skip it
// entirely.
func TestInstructionsWithoutIdentity_SendsWellKnownHeadersOverTheOrdinaryAuthPath(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	var gotIdentifier string
	path, handler := loamv1connect.NewMetaServiceHandler(metaHandlerFunc(
		func(ctx context.Context, req *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error) {
			identity, ok := httpauth.IdentityFromContext(ctx)
			require.True(t, ok, "the request must carry a resolved agent identity")
			gotIdentifier = identity.Identifier()
			return connect.NewResponse(&loamv1.GetInstructionsResponse{
				Usage:            "usage",
				Commands:         []*loamv1.CommandInfo{{Name: "search", Summary: "Run a natural-language semantic search over ingested docs/code."}},
				RoleInstructions: "An orchestrator supervises work it does not perform.",
			}), nil
		}))
	auth := httpauth.New("admin", "admin-password")
	mux := http.NewServeMux()
	mux.Handle(path, auth.CLI(handler))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	noIdentityEnv(t, srv.URL)

	var out bytes.Buffer
	args := []string{"instructions"}
	deps, err := NewProductionDeps(slog.New(slog.NewJSONHandler(io.Discard, nil)), srv.Client(), &out, strings.NewReader(""), args)
	require.NoError(t, err)
	require.Equal(t, 0, Run(context.Background(), NewRouter(deps), args), "output: %s", out.String())

	assert.Equal(t, "loam-orchestrator-0-orchestrator", gotIdentifier)
	var payload instructionsOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &payload))
	assert.Equal(t, "An orchestrator supervises work it does not perform.", payload.RoleInstructions)
	assert.Len(t, payload.Commands, 1, "the command list is whatever the server's role filter returned, never widened here")
}

// TestInstructionsWithoutIdentityOrServerURL_FailsNamingOnlyServerURL is
// acceptance criterion 13's failure half at the CLI's real entry point, and
// it checks the EXIT CODE as well as the message: NewProductionDeps reports
// a config failure through the encoder before any Deps exists, so a
// regression that turned this into an internal error (exit 1) rather than a
// usage error (exit 2) would still print a plausible message.
func TestInstructionsWithoutIdentityOrServerURL_FailsNamingOnlyServerURL(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	noIdentityEnv(t, "")
	var out bytes.Buffer
	_, err := NewProductionDeps(slog.New(slog.NewJSONHandler(io.Discard, nil)), http.DefaultClient, &out, strings.NewReader(""), []string{"instructions"})
	require.Error(t, err)
	assert.Equal(t, 2, NewErrorMapper().ExitCode(err))
	var payload errorPayload
	require.NoError(t, json.Unmarshal(out.Bytes(), &payload))
	assert.Equal(t, codeUsage, payload.Error.Code)
	assert.Contains(t, payload.Error.Message, envServerURL)
	for _, name := range []string{envAgentName, envAgentID, envAgentRole} {
		assert.NotContainsf(t, payload.Error.Message, name, "%s is not required by `instructions` and must not be named", name)
	}
}
