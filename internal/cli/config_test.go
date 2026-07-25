package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// baseEnv sets every required LOAM_* variable to a value that passes
// validation and blanks LOAM_OUTPUT_FORMAT, so tests are hermetic: they
// must not depend on (or be broken by) whatever LOAM_* variables happen to
// be exported in the developer's or CI's ambient environment. Callers
// override individual variables afterward to exercise specific values or
// failure paths.
//
// NOTE: this helper (and every test in this file) uses t.Setenv, which the
// testing package forbids combining with t.Parallel() since environment
// variables are process-global. None of the tests below call t.Parallel().
func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envServerURL, "https://loam.example")
	t.Setenv(envAgentName, "ada-lovelace")
	t.Setenv(envAgentID, "7")
	t.Setenv(envAgentRole, "reviewer")
	t.Setenv(envOutputFormat, "")
}

func TestLoadConfig_Valid_ResolvesEveryField(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	baseEnv(t)
	cfg, err := loadConfig()
	require.NoError(t, err)
	assert.Equal(t, "https://loam.example", cfg.ServerURL())
	assert.Equal(t, "ada-lovelace", cfg.AgentName())
	assert.Equal(t, "7", cfg.AgentID())
	assert.Equal(t, "reviewer", cfg.AgentRole())
	assert.Equal(t, "json", cfg.OutputFormat())
	assert.Equal(t, "ada-lovelace-7-reviewer", cfg.Identifier())
}

func TestLoadConfig_MissingRequiredVar(t *testing.T) {
	tests := []struct {
		name   string
		envVar string
	}{
		{"missing server URL", envServerURL},
		{"missing agent name", envAgentName},
		{"missing agent id", envAgentID},
		{"missing agent role", envAgentRole},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Not parallel: t.Setenv is incompatible with t.Parallel.
			baseEnv(t)
			t.Setenv(tt.envVar, "")
			cfg, err := loadConfig()
			require.Error(t, err)
			assert.ErrorIs(t, err, errMissingEnv)
			assert.ErrorIs(t, err, errUsage, "a missing required var must be a usage-class error")
			assert.Nil(t, cfg)
		})
	}
}

func TestLoadConfig_MalformedServerURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"no scheme", "loam.example/path"},
		{"scheme with no host", "https://"},
		{"not a URL at all", "\x7f"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Not parallel: t.Setenv is incompatible with t.Parallel.
			baseEnv(t)
			t.Setenv(envServerURL, tt.url)
			cfg, err := loadConfig()
			require.Error(t, err)
			assert.ErrorIs(t, err, errMalformedEnv)
			assert.ErrorIs(t, err, errUsage)
			assert.Nil(t, cfg)
		})
	}
}

func TestLoadConfig_MalformedAgentName(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"no hyphen", "adalovelace"},
		{"leading hyphen", "-lovelace"},
		{"trailing hyphen", "ada-"},
		{"just a hyphen", "-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Not parallel: t.Setenv is incompatible with t.Parallel.
			baseEnv(t)
			t.Setenv(envAgentName, tt.value)
			cfg, err := loadConfig()
			require.Error(t, err)
			assert.ErrorIs(t, err, errMalformedEnv)
			assert.Nil(t, cfg)
		})
	}
}

func TestLoadConfig_OutputFormat_UnknownFallsBackToJSON(t *testing.T) {
	tests := []struct {
		name   string
		format string
	}{
		{"unset", ""},
		{"garbage", "not-a-real-format"},
		{"case-sensitive miss", "JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Not parallel: t.Setenv is incompatible with t.Parallel.
			baseEnv(t)
			t.Setenv(envOutputFormat, tt.format)
			cfg, err := loadConfig()
			require.NoError(t, err)
			assert.Equal(t, "json", cfg.OutputFormat())
		})
	}
}

func TestLoadConfig_OutputFormat_RecognizesEveryFormat(t *testing.T) {
	for _, format := range []string{"json", "yaml", "xml", "human"} {
		t.Run(format, func(t *testing.T) {
			// Not parallel: t.Setenv is incompatible with t.Parallel.
			baseEnv(t)
			t.Setenv(envOutputFormat, format)
			cfg, err := loadConfig()
			require.NoError(t, err)
			assert.Equal(t, format, cfg.OutputFormat())
		})
	}
}

func TestResolveOutputFormat_NeverErrors(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	t.Setenv(envOutputFormat, "garbage")
	assert.Equal(t, "json", resolveOutputFormat())
}
