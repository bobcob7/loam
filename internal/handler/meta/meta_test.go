package meta_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/handler/meta"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/rolestore"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func adminCtx(t *testing.T) context.Context {
	t.Helper()
	return httpauth.WithAdmin(t.Context())
}

func agentCtx(t *testing.T, role string) context.Context {
	t.Helper()
	return httpauth.WithIdentity(t.Context(), httpauth.Identity{Name: "ada-lovelace", ID: "7", Role: role})
}

func newHandler(store meta.RoleStore, buf *bytes.Buffer) *meta.Handler {
	mapper := handler.NewErrorMapper(slog.New(slog.NewJSONHandler(buf, nil)))
	return meta.New(store, mapper, testLogger())
}

// commandNames extracts CommandInfo.Name from resp, for order-insensitive
// membership assertions.
func commandNames(commands []*loamv1.CommandInfo) []string {
	names := make([]string, len(commands))
	for i, c := range commands {
		names[i] = c.GetName()
	}
	return names
}

// commandSummary returns the summary of the command named name, failing the
// test if no such command is present.
func commandSummary(t *testing.T, commands []*loamv1.CommandInfo, name string) string {
	t.Helper()
	for _, c := range commands {
		if c.GetName() == name {
			return c.GetSummary()
		}
	}
	t.Fatalf("command %q not found in catalog", name)
	return ""
}

// TestGetInstructions_AuthorRole_SeesOnlyGrantedPlusUngatedCommands is the
// acceptance-critical scenario: roles.feature's "A role's instructions
// reach its agents" -- a reviewer (here, an author, exercising the
// opposite gate) asks for instructions and gets exactly its role's
// commands plus the two always-ungated ones, never a command outside its
// granted operations.
func TestGetInstructions_AuthorRole_SeesOnlyGrantedPlusUngatedCommands(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	store := &meta.RoleStoreMock{
		RoleCapabilitiesFunc: func(_ context.Context, role string) ([]handler.Capability, error) {
			assert.Equal(t, "author", role)
			return []handler.Capability{
				handler.CapabilityWorkStart, handler.CapabilityWorkSet, handler.CapabilityWorkRequestReview,
				handler.CapabilityWorkReply, handler.CapabilityGitClone, handler.CapabilityWorkRead,
				handler.CapabilityGraphQuery, handler.CapabilitySearch,
			}, nil
		},
		RoleInstructionsFunc: func(_ context.Context, role string) (string, error) {
			assert.Equal(t, "author", role)
			return "authors write code and request review.", nil
		},
	}
	h := newHandler(store, &buf)
	resp, err := h.GetInstructions(agentCtx(t, "author"), connect.NewRequest(&loamv1.GetInstructionsRequest{}))
	require.NoError(t, err)
	assert.Equal(t, "authors write code and request review.", resp.Msg.GetRoleInstructions())
	names := commandNames(resp.Msg.GetCommands())
	assert.Contains(t, names, "instructions", "ungated commands are always present")
	assert.Contains(t, names, "whoami", "ungated commands are always present")
	assert.Contains(t, names, "clone")
	assert.Contains(t, names, "work start")
	assert.NotContains(t, names, "work verdict", "an author's role does not grant work.verdict")
	assert.NotEmpty(t, resp.Msg.GetUsage(), "the static usage guide must always be present")
}

// TestGetInstructions_ReviewerRole_CannotSeeAuthorOnlyCommands mirrors the
// above from the reviewer side: work.start and work.set are gated out.
func TestGetInstructions_ReviewerRole_CannotSeeAuthorOnlyCommands(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	store := &meta.RoleStoreMock{
		RoleCapabilitiesFunc: func(_ context.Context, _ string) ([]handler.Capability, error) {
			return []handler.Capability{
				handler.CapabilityWorkRead, handler.CapabilityWorkReply, handler.CapabilityWorkVerdict,
				handler.CapabilityGitClone, handler.CapabilityGraphQuery, handler.CapabilitySearch,
			}, nil
		},
		RoleInstructionsFunc: func(_ context.Context, _ string) (string, error) {
			return "reviewers judge code.", nil
		},
	}
	h := newHandler(store, &buf)
	resp, err := h.GetInstructions(agentCtx(t, "reviewer"), connect.NewRequest(&loamv1.GetInstructionsRequest{}))
	require.NoError(t, err)
	names := commandNames(resp.Msg.GetCommands())
	assert.Contains(t, names, "work verdict")
	assert.Contains(t, names, "clone", "reviewers may clone (docs/web-spec.md -> RoleService)")
	assert.NotContains(t, names, "work start", "a reviewer's role does not grant work.start")
	assert.NotContains(t, names, "work set", "a reviewer's role does not grant work.set")
}

// TestGetInstructions_NeverCapabilityGated proves the defining property of
// this RPC: it succeeds even for a role granted NO capabilities at all --
// unlike RequireCapability, resolveCaller never turns an empty granted set
// into a denial. Only the ungated commands (instructions, whoami) appear.
func TestGetInstructions_NeverCapabilityGated(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	store := &meta.RoleStoreMock{
		RoleCapabilitiesFunc: func(context.Context, string) ([]handler.Capability, error) { return nil, nil },
		RoleInstructionsFunc: func(context.Context, string) (string, error) { return "", nil },
	}
	h := newHandler(store, &buf)
	resp, err := h.GetInstructions(agentCtx(t, "no-operations-role"), connect.NewRequest(&loamv1.GetInstructionsRequest{}))
	require.NoError(t, err, "GetInstructions must never be denied for lacking a capability -- it is the one RPC every identified caller may always reach")
	assert.Equal(t, []string{"instructions", "whoami"}, commandNames(resp.Msg.GetCommands()))
}

// TestGetInstructions_AdminSuperuser_SeesEveryCommand proves an admin
// basic-auth caller (no agent role at all) is treated as a superuser: the
// full command catalog, not an error from trying to resolve a
// nonexistent role.
func TestGetInstructions_AdminSuperuser_SeesEveryCommand(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	store := &meta.RoleStoreMock{
		RoleCapabilitiesFunc: func(context.Context, string) ([]handler.Capability, error) {
			t.Fatal("the role store must not be consulted for an admin superuser")
			return nil, nil
		},
		RoleInstructionsFunc: func(context.Context, string) (string, error) {
			t.Fatal("the role store must not be consulted for an admin superuser")
			return "", nil
		},
	}
	h := newHandler(store, &buf)
	resp, err := h.GetInstructions(adminCtx(t), connect.NewRequest(&loamv1.GetInstructionsRequest{}))
	require.NoError(t, err)
	assert.Contains(t, commandNames(resp.Msg.GetCommands()), "work verdict")
	assert.Contains(t, commandNames(resp.Msg.GetCommands()), "work start")
	assert.Empty(t, resp.Msg.GetRoleInstructions())
}

// TestGetInstructions_StdinOnlyCommands_SummaryNamesStdin pins loam-92b0's
// fix: "work set", "work comment", and "work reply" each take part of their
// input on stdin, which a one-line summary can otherwise hide from an agent
// until it fails an invocation to discover it (docs/cli-spec.md's set,
// comment, and reply sections). Mentioning "stdin" in the summary is the
// contract this guards -- an edit that silently drops it must fail here.
func TestGetInstructions_StdinOnlyCommands_SummaryNamesStdin(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	store := &meta.RoleStoreMock{
		RoleCapabilitiesFunc: func(context.Context, string) ([]handler.Capability, error) {
			t.Fatal("the role store must not be consulted for an admin superuser")
			return nil, nil
		},
		RoleInstructionsFunc: func(context.Context, string) (string, error) {
			t.Fatal("the role store must not be consulted for an admin superuser")
			return "", nil
		},
	}
	h := newHandler(store, &buf)
	resp, err := h.GetInstructions(adminCtx(t), connect.NewRequest(&loamv1.GetInstructionsRequest{}))
	require.NoError(t, err)
	commands := resp.Msg.GetCommands()
	assert.Contains(t, commandSummary(t, commands, "work set"), "stdin")
	assert.Contains(t, commandSummary(t, commands, "work comment"), "stdin")
	assert.Contains(t, commandSummary(t, commands, "work reply"), "stdin")
}

// TestGetInstructions_NoResolvedCaller_ReturnsPermissionDenied is the
// defence-in-depth branch: a context with neither IsAdmin nor an Identity
// (which internal/httpauth.Auth.CLI should never let reach a handler) must
// still fail closed, not resolve an empty role silently.
func TestGetInstructions_NoResolvedCaller_ReturnsPermissionDenied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	store := &meta.RoleStoreMock{
		RoleCapabilitiesFunc: func(context.Context, string) ([]handler.Capability, error) {
			t.Fatal("the role store must not be consulted with no resolved caller")
			return nil, nil
		},
		RoleInstructionsFunc: func(context.Context, string) (string, error) {
			t.Fatal("the role store must not be consulted with no resolved caller")
			return "", nil
		},
	}
	h := newHandler(store, &buf)
	_, err := h.GetInstructions(t.Context(), connect.NewRequest(&loamv1.GetInstructionsRequest{}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodePermissionDenied, connectErr.Code())
}

// TestGetInstructions_UnrecognizedRole_ReturnsPermissionDeniedNotInternal is
// loam-a8z's own reproduction, end to end through this handler: an agent
// presenting a role the store does not recognize (RoleCapabilities
// returning an error wrapping rolestore.ErrNotFound, exactly what
// internal/rolestore.Store.GetRole returns and what the live
// LOAM_AGENT_ROLE=admin incident hit) must answer CodePermissionDenied,
// naming the role -- not fall through to ErrorMapper's unmapped-error
// default (CodeInternal, "internal error", and a logged "unmapped handler
// error" line). Before this bead, an unknown role reached the wire exactly
// that way; this pins the fix and gives loam-0pj.16's `whoami --verify`
// something other than "internal error" to report. Removing the mapping in
// meta.go's resolveCaller makes this fail on the CodePermissionDenied
// assertion (or observe a log line) below, not panic -- RoleCapabilitiesFunc
// unconditionally returns the wrapped ErrNotFound regardless of what
// resolveCaller does with it.
func TestGetInstructions_UnrecognizedRole_ReturnsPermissionDeniedNotInternal(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	store := &meta.RoleStoreMock{
		RoleCapabilitiesFunc: func(_ context.Context, role string) ([]handler.Capability, error) {
			return nil, fmt.Errorf("getting role %s: %w", role, rolestore.ErrNotFound)
		},
		RoleInstructionsFunc: func(context.Context, string) (string, error) {
			t.Fatal("instructions must not be resolved once RoleCapabilities has already failed")
			return "", nil
		},
	}
	h := newHandler(store, &buf)
	_, err := h.GetInstructions(agentCtx(t, "admin"), connect.NewRequest(&loamv1.GetInstructionsRequest{}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodePermissionDenied, connectErr.Code(), "an unrecognized role must not surface as CodeInternal")
	assert.Contains(t, connectErr.Message(), "admin", "the error must name the offending role")
	assert.Empty(t, buf.String(), "a mapped permission denial must not trip ErrorMapper's unmapped-error log")
}

// TestGetInstructions_SpecificCommand_ReturnsOnlyThatEntry proves the
// "command" request field (docs/cli-spec.md -> instructions: "return help
// for just that command") narrows Commands to a single entry.
func TestGetInstructions_SpecificCommand_ReturnsOnlyThatEntry(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	store := &meta.RoleStoreMock{
		RoleCapabilitiesFunc: func(context.Context, string) ([]handler.Capability, error) {
			return []handler.Capability{handler.CapabilityGraphQuery}, nil
		},
		RoleInstructionsFunc: func(context.Context, string) (string, error) { return "", nil },
	}
	h := newHandler(store, &buf)
	command := "graph"
	resp, err := h.GetInstructions(agentCtx(t, "author"), connect.NewRequest(&loamv1.GetInstructionsRequest{Command: &command}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetCommands(), 1)
	assert.Equal(t, "graph", resp.Msg.GetCommands()[0].GetName())
}

// TestGetInstructions_UnknownCommand_ReturnsNotFound proves an unrecognized
// command name is rejected as not found, not silently returning every
// command or an empty list.
func TestGetInstructions_UnknownCommand_ReturnsNotFound(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	store := &meta.RoleStoreMock{
		RoleCapabilitiesFunc: func(context.Context, string) ([]handler.Capability, error) { return nil, nil },
		RoleInstructionsFunc: func(context.Context, string) (string, error) { return "", nil },
	}
	h := newHandler(store, &buf)
	command := "not-a-real-command"
	_, err := h.GetInstructions(agentCtx(t, "author"), connect.NewRequest(&loamv1.GetInstructionsRequest{Command: &command}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodeNotFound, connectErr.Code())
}

// TestGetInstructions_SpecificGatedCommandNotGranted_TreatedAsNotFound
// proves a command the caller's role does NOT grant is hidden identically
// to one that does not exist at all -- requesting it by name must not leak
// its existence via a different error code or a successful response.
func TestGetInstructions_SpecificGatedCommandNotGranted_TreatedAsNotFound(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	store := &meta.RoleStoreMock{
		RoleCapabilitiesFunc: func(context.Context, string) ([]handler.Capability, error) {
			return []handler.Capability{handler.CapabilityWorkRead}, nil
		},
		RoleInstructionsFunc: func(context.Context, string) (string, error) { return "", nil },
	}
	h := newHandler(store, &buf)
	command := "work verdict"
	_, err := h.GetInstructions(agentCtx(t, "reviewer-without-verdict"), connect.NewRequest(&loamv1.GetInstructionsRequest{Command: &command}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodeNotFound, connectErr.Code())
}

// TestGetInstructions_RoleCapabilitiesStoreFailure_MapsToInternalAndLogs
// proves a role-store failure resolving capabilities is surfaced as
// CodeInternal and logged, never silently treated as "no commands" or a
// permission denial.
func TestGetInstructions_RoleCapabilitiesStoreFailure_MapsToInternalAndLogs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	storeErr := errors.New("role store unreachable")
	store := &meta.RoleStoreMock{
		RoleCapabilitiesFunc: func(context.Context, string) ([]handler.Capability, error) { return nil, storeErr },
		RoleInstructionsFunc: func(context.Context, string) (string, error) {
			t.Fatal("instructions must not be resolved once capabilities resolution already failed")
			return "", nil
		},
	}
	h := newHandler(store, &buf)
	_, err := h.GetInstructions(agentCtx(t, "author"), connect.NewRequest(&loamv1.GetInstructionsRequest{}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodeInternal, connectErr.Code())
	assert.Contains(t, buf.String(), "role store unreachable")
}

// TestGetInstructions_RoleInstructionsStoreFailure_MapsToInternalAndLogs
// mirrors the above for the second store call.
func TestGetInstructions_RoleInstructionsStoreFailure_MapsToInternalAndLogs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	storeErr := errors.New("instructions column unreadable")
	store := &meta.RoleStoreMock{
		RoleCapabilitiesFunc: func(context.Context, string) ([]handler.Capability, error) { return nil, nil },
		RoleInstructionsFunc: func(context.Context, string) (string, error) { return "", storeErr },
	}
	h := newHandler(store, &buf)
	_, err := h.GetInstructions(agentCtx(t, "author"), connect.NewRequest(&loamv1.GetInstructionsRequest{}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodeInternal, connectErr.Code())
	assert.Contains(t, buf.String(), "instructions column unreadable")
}
