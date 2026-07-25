package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvConfig_OutputFormat_DefaultsToJSON(t *testing.T) {
	t.Setenv("LOAM_OUTPUT_FORMAT", "")
	cfg := NewEnvConfig()
	assert.Equal(t, "json", cfg.OutputFormat())
}

func TestEnvConfig_OutputFormat_ReadsEnv(t *testing.T) {
	t.Setenv("LOAM_OUTPUT_FORMAT", "yaml")
	cfg := NewEnvConfig()
	assert.Equal(t, "yaml", cfg.OutputFormat())
}

func TestEnvConfig_ReadsAgentIdentityFromEnv(t *testing.T) {
	t.Setenv("LOAM_AGENT_NAME", "ada-lovelace")
	t.Setenv("LOAM_AGENT_ID", "7")
	t.Setenv("LOAM_AGENT_ROLE", "reviewer")
	t.Setenv("LOAM_SERVER_URL", "https://loam.example")
	t.Setenv("LOAM_GIT_URL", "ssh://git@loam.example")
	cfg := NewEnvConfig()
	assert.Equal(t, "ada-lovelace", cfg.AgentName())
	assert.Equal(t, "7", cfg.AgentID())
	assert.Equal(t, "reviewer", cfg.AgentRole())
	assert.Equal(t, "https://loam.example", cfg.ServerURL())
	assert.Equal(t, "ssh://git@loam.example", cfg.GitURL())
}

func TestJSONEncoder_Encode_WritesJSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	encoder := NewJSONEncoder(&buf)
	require.NoError(t, encoder.Encode(map[string]string{"a": "b"}))
	assert.JSONEq(t, `{"a":"b"}`, buf.String())
}

func TestCoarseErrorMapper_ExitCode(t *testing.T) {
	t.Parallel()
	mapper := NewCoarseErrorMapper()
	assert.Equal(t, 0, mapper.ExitCode(nil))
	assert.Equal(t, 1, mapper.ExitCode(errNotImplemented))
}

func TestUnresolvedWorkspace_AlwaysFails(t *testing.T) {
	t.Parallel()
	ws := NewUnresolvedWorkspace()
	_, err := ws.ResolveRepo()
	assert.ErrorIs(t, err, errWorkspaceUnresolved)
	_, err = ws.ResolveWorkBranch()
	assert.ErrorIs(t, err, errWorkspaceUnresolved)
}
