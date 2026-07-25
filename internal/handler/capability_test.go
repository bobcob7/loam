package handler_test

import (
	"context"
	"errors"
	"testing"

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
		capability     string
		roleCaps       []string
		roleErr        error
		wantErr        bool
		wantPermission bool
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
			roleCaps:      []string{handler.CapabilityWorkStart, handler.CapabilityGitPush},
			wantErr:       false,
			wantStoreCall: true,
		},
		{
			name:           "agent role lacking the capability is denied",
			ctx:            agentCtx,
			capability:     handler.CapabilityWorkVerdict,
			roleCaps:       []string{handler.CapabilityWorkStart, handler.CapabilityGitPush},
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			called := false
			store := &handler.RoleStoreMock{
				RoleCapabilitiesFunc: func(ctx context.Context, role string) ([]string, error) {
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
			if tt.wantPermission {
				assert.ErrorIs(t, err, handler.ErrPermissionDenied)
			} else {
				assert.NotErrorIs(t, err, handler.ErrPermissionDenied, "a store-level failure must not be misreported as permission denied")
			}
		})
	}
}
