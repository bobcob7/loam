package handler_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"connectrpc.com/connect"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCapabilityChecker_RequireCapability(t *testing.T) {
	t.Parallel()
	agentCtx := httpauth.WithIdentity(t.Context(), httpauth.Identity{Name: "ada-lovelace", ID: "7", Role: "author"})
	tests := []struct {
		name           string
		ctx            context.Context
		capability     handler.Capability
		roleCaps       []handler.Capability
		roleErr        error
		wantErr        bool
		wantPermission bool
		wantUnknown    bool
		wantStoreCall  bool
	}{
		{
			name:          "admin superuser bypasses without consulting the role store",
			ctx:           httpauth.WithAdmin(t.Context()),
			capability:    handler.CapabilityWorkVerdict,
			wantErr:       false,
			wantStoreCall: false,
		},
		{
			name:          "agent role granting the capability is allowed",
			ctx:           agentCtx,
			capability:    handler.CapabilityWorkStart,
			roleCaps:      []handler.Capability{handler.CapabilityWorkStart, handler.CapabilityGitPush},
			wantErr:       false,
			wantStoreCall: true,
		},
		{
			name:           "agent role lacking the capability is denied",
			ctx:            agentCtx,
			capability:     handler.CapabilityWorkVerdict,
			roleCaps:       []handler.Capability{handler.CapabilityWorkStart, handler.CapabilityGitPush},
			wantErr:        true,
			wantPermission: true,
			wantStoreCall:  true,
		},
		{
			name:           "no resolved identity at all is denied without consulting the role store",
			ctx:            t.Context(),
			capability:     handler.CapabilityGraphQuery,
			wantErr:        true,
			wantPermission: true,
			wantStoreCall:  false,
		},
		{
			name:          "a role store failure is surfaced, not silently treated as permission denied",
			ctx:           agentCtx,
			capability:    handler.CapabilitySearch,
			roleErr:       errors.New("role store unreachable"),
			wantErr:       true,
			wantStoreCall: true,
		},
		{
			name:          "unknown capability is rejected without consulting the role store",
			ctx:           agentCtx,
			capability:    handler.Capability("work.reqest_review"),
			wantErr:       true,
			wantUnknown:   true,
			wantStoreCall: false,
		},
		{
			name:          "unknown capability is rejected even for an admin superuser",
			ctx:           httpauth.WithAdmin(t.Context()),
			capability:    handler.Capability("bogus.capability"),
			wantErr:       true,
			wantUnknown:   true,
			wantStoreCall: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			called := false
			store := &handler.RoleStoreMock{
				RoleCapabilitiesFunc: func(ctx context.Context, role string) ([]handler.Capability, error) {
					called = true
					return tt.roleCaps, tt.roleErr
				},
			}
			checker := handler.NewCapabilityChecker(store)
			err := checker.RequireCapability(tt.ctx, tt.capability)
			assert.Equal(t, tt.wantStoreCall, called, "role store call mismatch")
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			switch {
			case tt.wantPermission:
				assert.ErrorIs(t, err, handler.ErrPermissionDenied)
			case tt.wantUnknown:
				assert.NotErrorIs(t, err, handler.ErrPermissionDenied, "an unknown capability must not be misreported as permission denied")
			default:
				assert.NotErrorIs(t, err, handler.ErrPermissionDenied, "a store-level failure must not be misreported as permission denied")
			}
		})
	}
}

// TestCapabilityChecker_RequireCapability_UnknownCapabilityIsLoggedAsInternal
// proves the distinct unknown-capability error is not just "not
// ErrPermissionDenied" in isolation, but actually falls through
// ErrorMapper.ToConnectErr's unmapped-error branch end to end: CodeInternal
// on the wire, and the raw error observed by the logger. A regression that
// made RequireCapability wrap ErrPermissionDenied for an unknown capability
// instead would make this test observe CodePermissionDenied and no log
// line, and fail.
func TestCapabilityChecker_RequireCapability_UnknownCapabilityIsLoggedAsInternal(t *testing.T) {
	t.Parallel()
	store := &handler.RoleStoreMock{
		RoleCapabilitiesFunc: func(ctx context.Context, role string) ([]handler.Capability, error) {
			t.Fatal("role store must not be consulted for an unknown capability")
			return nil, nil
		},
	}
	checker := handler.NewCapabilityChecker(store)
	err := checker.RequireCapability(httpauth.WithAdmin(t.Context()), handler.Capability("work.reqest_review"))
	require.Error(t, err)
	assert.NotErrorIs(t, err, handler.ErrPermissionDenied)
	var buf bytes.Buffer
	mapper := handler.NewErrorMapper(slog.New(slog.NewJSONHandler(&buf, nil)))
	got := mapper.ToConnectErr(err)
	require.NotNil(t, got)
	assert.Equal(t, connect.CodeInternal, got.Code())
	assert.Contains(t, buf.String(), "work.reqest_review", "the unknown capability must be logged, not silently dropped")
}

// TestCapability_Valid is exhaustive over the ten-operation vocabulary
// (docs/web-spec.md -> RoleService) plus a handful of strings that must
// stay invalid: near-miss typos, an empty capability, and a plausible but
// wrong value (a role name, not a capability).
func TestCapability_Valid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		capability handler.Capability
		want       bool
	}{
		{name: "work.start", capability: handler.CapabilityWorkStart, want: true},
		{name: "work.set", capability: handler.CapabilityWorkSet, want: true},
		{name: "work.request_review", capability: handler.CapabilityWorkRequestReview, want: true},
		{name: "work.reply", capability: handler.CapabilityWorkReply, want: true},
		{name: "work.verdict", capability: handler.CapabilityWorkVerdict, want: true},
		{name: "work.read", capability: handler.CapabilityWorkRead, want: true},
		{name: "git.clone", capability: handler.CapabilityGitClone, want: true},
		{name: "git.push", capability: handler.CapabilityGitPush, want: true},
		{name: "graph.query", capability: handler.CapabilityGraphQuery, want: true},
		{name: "search", capability: handler.CapabilitySearch, want: true},
		{name: "typo of work.request_review", capability: handler.Capability("work.reqest_review"), want: false},
		{name: "empty string", capability: handler.Capability(""), want: false},
		{name: "role name, not a capability", capability: handler.Capability("author"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.capability.Valid())
		})
	}
}
