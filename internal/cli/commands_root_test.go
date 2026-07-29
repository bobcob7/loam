package cli

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

// instructionsTestDeps wires a Deps for an `instructions` test: client
// governs the MetaService response and encoded captures whatever the
// handler encodes on success. The real error mapper is injected (not a
// mock) so exit codes asserted here are the ones the binary would produce.
func instructionsTestDeps(client MetaClient, encoded *any) *Deps {
	connectClient := &ConnectClientMock{MetaFunc: func() MetaClient { return client }}
	encoder := &OutputEncoderMock{EncodeFunc: func(v any) error { *encoded = v; return nil }}
	return NewDeps(testLogger(), &ConfigMock{}, encoder, newErrorMapper(), &WorkspaceResolverMock{}, connectClient, nil, nil)
}

// whoamiTestDeps wires a Deps for a `whoami` test from the four identity
// strings a Config resolves. connect is fakeConnect{}, whose accessors all
// return nil: any RPC attempt from whoami would nil-panic, which is the
// point -- the "no server call" promise is checked structurally here and
// end to end against an unreachable URL in cmd/loam/main_test.go.
func whoamiTestDeps(name, id, role, identifier string, encoded *any) *Deps {
	cfg := &ConfigMock{
		AgentNameFunc:  func() string { return name },
		AgentIDFunc:    func() string { return id },
		AgentRoleFunc:  func() string { return role },
		IdentifierFunc: func() string { return identifier },
	}
	encoder := &OutputEncoderMock{EncodeFunc: func(v any) error { *encoded = v; return nil }}
	return NewDeps(testLogger(), cfg, encoder, newErrorMapper(), &WorkspaceResolverMock{}, fakeConnect{}, nil, nil)
}

// --- whoami ---

// TestRunWhoami_ReportsEveryIdentityFieldIncludingTheFullIdentifier is the
// core whoami contract (docs/cli-spec.md -> whoami -> Output). The
// identifier is asserted as the full "<name>-<id>-<role>" string against a
// name that is a strict prefix of it, so an implementation that reported
// AgentName() in the identifier field -- the loam-ppb P0 -- fails here
// rather than passing a substring check.
func TestRunWhoami_ReportsEveryIdentityFieldIncludingTheFullIdentifier(t *testing.T) {
	t.Parallel()
	var encoded any
	deps := whoamiTestDeps("ada-lovelace", "7", "reviewer", "ada-lovelace-7-reviewer", &encoded)

	err := runWhoami(t.Context(), deps, nil)
	require.NoError(t, err)

	out, ok := encoded.(whoamiOutput)
	require.True(t, ok, "whoami must encode a whoamiOutput")
	assert.Equal(t, "ada-lovelace", out.Name)
	assert.Equal(t, "7", out.ID)
	assert.Equal(t, "reviewer", out.Role)
	assert.Equal(t, "ada-lovelace-7-reviewer", out.Identifier)
}

// TestRunWhoami_IdentifierComesFromConfigNotRecomposedLocally proves whoami
// reports Config.Identifier() verbatim rather than rebuilding
// "<name>-<id>-<role>" itself. Config is deliberately given an identifier
// that does NOT match its three parts: a handler that recomposed the string
// would emit "a-b-c" and fail. Only one place may own that format
// (config.go -> loadConfig, which the Connect identity headers also derive
// from); a second, silently-agreeing copy here is exactly how the two would
// eventually disagree.
func TestRunWhoami_IdentifierComesFromConfigNotRecomposedLocally(t *testing.T) {
	t.Parallel()
	var encoded any
	deps := whoamiTestDeps("a", "b", "c", "sentinel-identifier-from-config", &encoded)

	require.NoError(t, runWhoami(t.Context(), deps, nil))

	out, ok := encoded.(whoamiOutput)
	require.True(t, ok)
	assert.Equal(t, "sentinel-identifier-from-config", out.Identifier)
}

// TestRunWhoami_PositionalArgument_IsUsageError proves `whoami` takes no
// arguments (docs/cli-spec.md -> whoami -> Arguments: "none"), exit 2.
func TestRunWhoami_PositionalArgument_IsUsageError(t *testing.T) {
	t.Parallel()
	var encoded any
	deps := whoamiTestDeps("ada-lovelace", "7", "reviewer", "ada-lovelace-7-reviewer", &encoded)

	err := runWhoami(t.Context(), deps, []string{"extra"})

	require.Error(t, err)
	var ue *usageError
	assert.ErrorAs(t, err, &ue)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded, "a usage error must not encode a result")
}

// TestRunWhoami_UnknownFlag_IsUsageError proves whoami defines no flags of
// its own, so anything flag-shaped is rejected as a usage error (exit 2)
// rather than silently ignored as a positional.
func TestRunWhoami_UnknownFlag_IsUsageError(t *testing.T) {
	t.Parallel()
	var encoded any
	deps := whoamiTestDeps("ada-lovelace", "7", "reviewer", "ada-lovelace-7-reviewer", &encoded)

	err := runWhoami(t.Context(), deps, []string{"--verbose"})

	require.Error(t, err)
	var ue *usageError
	assert.ErrorAs(t, err, &ue)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
}

// TestRunWhoami_MakesNoServerCall is the acceptance criterion "whoami works
// without contacting the server" at the unit level: every ConnectClient
// accessor is wired to fail the test if it is even reached, so a whoami
// that acquired any client -- never mind called one -- is caught. The
// end-to-end counterpart, against an unreachable LOAM_SERVER_URL through
// the real binary, is in cmd/loam/main_test.go.
func TestRunWhoami_MakesNoServerCall(t *testing.T) {
	t.Parallel()
	connectClient := &ConnectClientMock{
		MetaFunc:       func() MetaClient { t.Error("whoami must not reach the Meta client"); return nil },
		WorkBranchFunc: func() WorkBranchClient { t.Error("whoami must not reach the WorkBranch client"); return nil },
		RepoFunc:       func() RepoClient { t.Error("whoami must not reach the Repo client"); return nil },
		GraphFunc:      func() GraphClient { t.Error("whoami must not reach the Graph client"); return nil },
		SearchFunc:     func() SearchClient { t.Error("whoami must not reach the Search client"); return nil },
	}
	cfg := &ConfigMock{
		AgentNameFunc:  func() string { return "ada-lovelace" },
		AgentIDFunc:    func() string { return "7" },
		AgentRoleFunc:  func() string { return "reviewer" },
		IdentifierFunc: func() string { return "ada-lovelace-7-reviewer" },
	}
	encoder := &OutputEncoderMock{EncodeFunc: func(any) error { return nil }}
	deps := NewDeps(testLogger(), cfg, encoder, newErrorMapper(), &WorkspaceResolverMock{}, connectClient, nil, nil)

	require.NoError(t, runWhoami(t.Context(), deps, nil))
	assert.Empty(t, connectClient.MetaCalls())
	assert.Empty(t, connectClient.WorkBranchCalls())
}

// --- instructions ---

// TestRunInstructions_Success_EmitsUsageCommandsAndRoleInstructions pins
// `instructions`' output shape (docs/cli-spec.md -> instructions ->
// Output): all three fields, sourced from the server's response.
func TestRunInstructions_Success_EmitsUsageCommandsAndRoleInstructions(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.GetInstructionsRequest
	client := &MetaClientMock{
		GetInstructionsFunc: func(_ context.Context, req *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.GetInstructionsResponse{
				Usage: "Loam CLI: agents orient with instructions.",
				Commands: []*loamv1.CommandInfo{
					{Name: "work list", Summary: "List work branches across all enrolled repos."},
					{Name: "whoami", Summary: "Report the calling agent's identity and role."},
				},
				RoleInstructions: "Review carefully; disapprove blocks the merge.",
			}), nil
		},
	}
	var encoded any
	deps := instructionsTestDeps(client, &encoded)

	err := runInstructions(t.Context(), deps, nil)
	require.NoError(t, err)

	require.NotNil(t, capturedReq)
	assert.Nil(t, capturedReq.Command, "with no positional argument the optional command field must be left unset, not sent as an empty string")

	out, ok := encoded.(instructionsOutput)
	require.True(t, ok, "instructions must encode an instructionsOutput")
	assert.Equal(t, "Loam CLI: agents orient with instructions.", out.Usage)
	assert.Equal(t, "Review carefully; disapprove blocks the merge.", out.RoleInstructions)
	require.Len(t, out.Commands, 2)
	assert.Equal(t, "work list", out.Commands[0].Name)
	assert.Equal(t, "List work branches across all enrolled repos.", out.Commands[0].Summary)
	assert.Equal(t, "whoami", out.Commands[1].Name)
}

// TestRunInstructions_UsageComesFromServerNotABuiltInCopy guards the
// judgement call recorded in runInstructions' doc comment: docs/cli-spec.md
// says the usage guide is "built into the binary", but loam-ofg.11 put it
// in the SERVER binary and returns it over the wire. This asserts the CLI
// renders the server's text verbatim -- a client-side static guide, or any
// concatenation of one with the server's, would fail.
func TestRunInstructions_UsageComesFromServerNotABuiltInCopy(t *testing.T) {
	t.Parallel()
	client := &MetaClientMock{
		GetInstructionsFunc: func(context.Context, *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error) {
			return connect.NewResponse(&loamv1.GetInstructionsResponse{Usage: "SERVER-USAGE-SENTINEL"}), nil
		},
	}
	var encoded any
	deps := instructionsTestDeps(client, &encoded)

	require.NoError(t, runInstructions(t.Context(), deps, nil))

	out, ok := encoded.(instructionsOutput)
	require.True(t, ok)
	assert.Equal(t, "SERVER-USAGE-SENTINEL", out.Usage)
}

// TestRunInstructions_SingleCommandArgument_IsSentAsTheCommandFilter proves
// the optional positional reaches the RPC as GetInstructionsRequest.command
// (docs/cli-spec.md -> instructions -> Arguments), so the server can narrow
// the response to that one entry. The CLI does no narrowing of its own: it
// encodes whatever list came back.
func TestRunInstructions_SingleCommandArgument_IsSentAsTheCommandFilter(t *testing.T) {
	t.Parallel()
	var capturedReq *loamv1.GetInstructionsRequest
	client := &MetaClientMock{
		GetInstructionsFunc: func(_ context.Context, req *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error) {
			capturedReq = req.Msg
			return connect.NewResponse(&loamv1.GetInstructionsResponse{
				Usage:    "usage",
				Commands: []*loamv1.CommandInfo{{Name: "work list", Summary: "List work branches."}},
			}), nil
		},
	}
	var encoded any
	deps := instructionsTestDeps(client, &encoded)

	require.NoError(t, runInstructions(t.Context(), deps, []string{"work list"}))

	require.NotNil(t, capturedReq)
	require.NotNil(t, capturedReq.Command)
	assert.Equal(t, "work list", capturedReq.GetCommand())

	out, ok := encoded.(instructionsOutput)
	require.True(t, ok)
	require.Len(t, out.Commands, 1)
	assert.Equal(t, "work list", out.Commands[0].Name)
}

// TestRunInstructions_CommandListIsNotFilteredLocally proves the role
// filtering is entirely the server's (internal/handler/meta/catalog.go ->
// filterCommands, driven by the identity headers connect.go attaches). The
// CLI is handed a deliberately odd two-entry list and must reproduce it
// exactly -- neither dropping an entry it does not recognize nor adding the
// ungated commands back in. Anything else would let the CLI tell an agent
// it may run something its role cannot.
func TestRunInstructions_CommandListIsNotFilteredLocally(t *testing.T) {
	t.Parallel()
	client := &MetaClientMock{
		GetInstructionsFunc: func(context.Context, *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error) {
			return connect.NewResponse(&loamv1.GetInstructionsResponse{
				Commands: []*loamv1.CommandInfo{
					{Name: "not-a-real-loam-command", Summary: "whatever the server says."},
				},
			}), nil
		},
	}
	var encoded any
	deps := instructionsTestDeps(client, &encoded)

	require.NoError(t, runInstructions(t.Context(), deps, nil))

	out, ok := encoded.(instructionsOutput)
	require.True(t, ok)
	require.Len(t, out.Commands, 1)
	assert.Equal(t, "not-a-real-loam-command", out.Commands[0].Name)
}

// TestRunInstructions_NoCommands_EncodesEmptyListNotNull holds the
// convention loam-0pj.10 established: a list-shaped field must encode as
// `[]`, never `null`, so an agent parsing the response can iterate it
// unconditionally.
func TestRunInstructions_NoCommands_EncodesEmptyListNotNull(t *testing.T) {
	t.Parallel()
	client := &MetaClientMock{
		GetInstructionsFunc: func(context.Context, *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error) {
			return connect.NewResponse(&loamv1.GetInstructionsResponse{Usage: "usage"}), nil
		},
	}
	var encoded any
	deps := instructionsTestDeps(client, &encoded)

	require.NoError(t, runInstructions(t.Context(), deps, nil))

	out, ok := encoded.(instructionsOutput)
	require.True(t, ok)
	assert.Empty(t, out.Commands)
	assert.NotNil(t, out.Commands, "commands must encode as [] not null")
}

// TestRunInstructions_ServerUnreachable_ExitsInternal is the error contract
// docs/cli-spec.md -> instructions -> Errors pins: "exit 1 if the server is
// unreachable while fetching role instructions". A transport failure
// reaches the CLI as connect.CodeUnavailable, which classifyConnectError
// deliberately does not recognize, so it falls through to the unexpected-
// internal-error class.
func TestRunInstructions_ServerUnreachable_ExitsInternal(t *testing.T) {
	t.Parallel()
	client := &MetaClientMock{
		GetInstructionsFunc: func(context.Context, *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error) {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("dial tcp: connection refused"))
		},
	}
	var encoded any
	deps := instructionsTestDeps(client, &encoded)

	err := runInstructions(t.Context(), deps, nil)

	require.Error(t, err)
	assert.Equal(t, 1, newErrorMapper().ExitCode(err), "an unreachable server must exit 1, not be reclassified into the 2/3 classes")
	assert.Nil(t, encoded, "a failed fetch must not encode a partial orientation")
}

// TestRunInstructions_UnknownCommandArgument_ExitsNotFound records the
// behaviour for an argument naming a command the caller cannot see -- one
// that does not exist, or one hidden from them by the role filter, which
// the server deliberately makes indistinguishable
// (internal/handler/meta/catalog.go -> findCommand). The server answers
// NotFound and the standard %w wrap carries it to exit 3.
//
// docs/cli-spec.md -> instructions -> Errors does not mention this case at
// all; it lists only the exit-1 unreachable-server failure. Exit 3 is the
// server's existing contract (loam-ofg.11) surfaced honestly, not a
// decision taken here.
func TestRunInstructions_UnknownCommandArgument_ExitsNotFound(t *testing.T) {
	t.Parallel()
	client := &MetaClientMock{
		GetInstructionsFunc: func(context.Context, *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("command bogus: not found"))
		},
	}
	var encoded any
	deps := instructionsTestDeps(client, &encoded)

	err := runInstructions(t.Context(), deps, []string{"bogus"})

	require.Error(t, err)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
	// The handler wraps the raw *connect.Error rather than pre-classifying
	// it, exactly as every other command does; mapCommandError is the
	// boundary that turns it into a not_found cliError, so the code is
	// asserted through it rather than with errors.Is on the raw chain.
	classified := mapCommandError(err)
	require.NotNil(t, classified)
	assert.Equal(t, codeNotFound, classified.code)
	assert.Equal(t, "command bogus: not found", classified.Error(), "the message must be the server's own, not the connect code prefix")
}

// TestRunInstructions_TwoPositionalArguments_IsUsageError holds
// docs/cli-spec.md -> instructions to its synopsis, `loam instructions
// [command]`: ONE optional argument. A command name containing a space
// ("work list") must therefore be quoted into a single argv entry -- see
// TestRunInstructions_SingleCommandArgument_IsSentAsTheCommandFilter, which
// passes exactly that. Joining stray positionals with a space instead would
// turn `loam instructions bogus extra` into a lookup of "bogus extra",
// answered NotFound (exit 3) by the server, when it is plainly a
// malformed invocation the CLI can reject locally as exit 2.
func TestRunInstructions_TwoPositionalArguments_IsUsageError(t *testing.T) {
	t.Parallel()
	// The mock returns a perfectly valid response rather than failing on
	// sight, so that a regression which lets the extra positional through
	// reports the missing usage error as a plain assertion failure --
	// require.Error below -- instead of nil-panicking somewhere downstream
	// and hiding what actually broke.
	client := &MetaClientMock{
		GetInstructionsFunc: func(context.Context, *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error) {
			return connect.NewResponse(&loamv1.GetInstructionsResponse{Usage: "usage"}), nil
		},
	}
	var encoded any
	deps := instructionsTestDeps(client, &encoded)

	err := runInstructions(t.Context(), deps, []string{"work", "list"})

	require.Error(t, err, "a second positional must be rejected locally, never joined into a command name and sent")
	var ue *usageError
	assert.ErrorAs(t, err, &ue)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	assert.Empty(t, client.GetInstructionsCalls(), "a malformed invocation must never reach the network")
	assert.Nil(t, encoded)
}

// TestRunInstructions_UnknownFlag_IsUsageError proves instructions defines
// no flags of its own (docs/cli-spec.md -> Conventions: "the CLI has no
// global flags", and this command declares none).
func TestRunInstructions_UnknownFlag_IsUsageError(t *testing.T) {
	t.Parallel()
	client := &MetaClientMock{
		GetInstructionsFunc: func(context.Context, *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error) {
			return connect.NewResponse(&loamv1.GetInstructionsResponse{Usage: "usage"}), nil
		},
	}
	var encoded any
	deps := instructionsTestDeps(client, &encoded)

	err := runInstructions(t.Context(), deps, []string{"--role", "author"})

	require.Error(t, err)
	var ue *usageError
	assert.ErrorAs(t, err, &ue)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	assert.Empty(t, client.GetInstructionsCalls(), "a malformed invocation must never reach the network")
}

// --- router dispatch reachability ---

// TestRouterDispatch_Instructions_ReachesRealHandler proves the router
// dispatches "instructions" through to the real handler, not a routing
// usageError. Named in commandImplementationProofs (router_test.go) as the
// test that proves instructions' coverage.
func TestRouterDispatch_Instructions_ReachesRealHandler(t *testing.T) {
	t.Parallel()
	called := false
	client := &MetaClientMock{
		GetInstructionsFunc: func(context.Context, *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error) {
			called = true
			return connect.NewResponse(&loamv1.GetInstructionsResponse{Usage: "usage"}), nil
		},
	}
	var encoded any
	router := NewRouter(instructionsTestDeps(client, &encoded))

	require.NoError(t, router.Dispatch(t.Context(), []string{"instructions"}))
	assert.True(t, called, "dispatching instructions must reach the MetaService RPC")
	assert.IsType(t, instructionsOutput{}, encoded)
}

// TestRouterDispatch_Whoami_ReachesRealHandler proves the router dispatches
// "whoami" through to the real handler. Named in
// commandImplementationProofs (router_test.go) as the test that proves
// whoami's coverage.
func TestRouterDispatch_Whoami_ReachesRealHandler(t *testing.T) {
	t.Parallel()
	var encoded any
	router := NewRouter(whoamiTestDeps("ada-lovelace", "7", "reviewer", "ada-lovelace-7-reviewer", &encoded))

	require.NoError(t, router.Dispatch(t.Context(), []string{"whoami"}))

	out, ok := encoded.(whoamiOutput)
	require.True(t, ok, "dispatching whoami must encode a whoamiOutput")
	assert.Equal(t, "ada-lovelace-7-reviewer", out.Identifier)
}
