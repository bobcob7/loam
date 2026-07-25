package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnresolvedWorkspace_AlwaysFails(t *testing.T) {
	t.Parallel()
	ws := &unresolvedWorkspace{}
	_, err := ws.ResolveRepo()
	assert.ErrorIs(t, err, errWorkspaceUnresolved)
	_, err = ws.ResolveWorkBranch()
	assert.ErrorIs(t, err, errWorkspaceUnresolved)
}

// TestNewPlaceholderDeps_ConstructsAllCollaborators proves NewPlaceholderDeps
// wires the real config/encoder/error-mapper collaborators (this bead's
// group) alongside the still-placeholder workspace resolver (loam-0pj.5).
func TestNewPlaceholderDeps_ConstructsAllCollaborators(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	baseEnv(t)
	var buf bytes.Buffer
	deps, err := NewPlaceholderDeps(testLogger(), &buf)
	require.NoError(t, err)
	require.NotNil(t, deps)
	require.NoError(t, deps.encoder.Encode(map[string]string{"ok": "true"}))
	assert.JSONEq(t, `{"ok":"true"}`, buf.String())
	assert.Equal(t, 1, deps.errorMapper.ExitCode(errNotImplemented))
	assert.Equal(t, "ada-lovelace-7-reviewer", deps.config.Identifier())
	_, err = deps.workspace.ResolveRepo()
	assert.ErrorIs(t, err, errWorkspaceUnresolved)
}

// TestNewPlaceholderDeps_MissingRequiredVar_ReturnsErrorAndEncodesUsagePayload
// proves a missing required LOAM_* variable both fails construction and
// still reports through the resolved output format (LOAM_OUTPUT_FORMAT
// alone never errors, so the encoder is available even when the rest of
// config is invalid).
func TestNewPlaceholderDeps_MissingRequiredVar_ReturnsErrorAndEncodesUsagePayload(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	baseEnv(t)
	t.Setenv(envAgentRole, "")
	var buf bytes.Buffer
	deps, err := NewPlaceholderDeps(testLogger(), &buf)
	require.Error(t, err)
	assert.ErrorIs(t, err, errMissingEnv)
	assert.Nil(t, deps)
	assert.Contains(t, buf.String(), `"code":"usage"`)
}
