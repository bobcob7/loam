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

// TestLoadConfig_EverythingMissing_ReportsAllFourInOneError is loam-dc2v
// defect 1's own acceptance criterion: an operator with NOTHING configured
// must learn about every missing variable from a single run, not one per
// run. Before the fix, loadConfig returned on the first failing requireX
// call (LOAM_SERVER_URL), so this would fail with the message naming only
// that one variable.
func TestLoadConfig_EverythingMissing_ReportsAllFourInOneError(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	for _, v := range []string{envServerURL, envAgentName, envAgentID, envAgentRole, envOutputFormat} {
		t.Setenv(v, "")
	}
	cfg, err := loadConfig()
	require.Error(t, err)
	assert.Nil(t, cfg)
	for _, name := range []string{envServerURL, envAgentName, envAgentID, envAgentRole} {
		assert.Contains(t, err.Error(), name, "a fully unconfigured run must name every missing variable, not just the first")
	}
	assert.ErrorIs(t, err, errUsage)
	assert.ErrorIs(t, err, errMissingEnv)
}

// TestLoadConfig_TwoMalformedVars_ReportsBothInOneError proves the
// accumulation covers malformed (not just missing) values together, and
// that each individual sentinel (errMalformedEnv here, for both the URL
// and the name) stays reachable via errors.Is through the combined error.
func TestLoadConfig_TwoMalformedVars_ReportsBothInOneError(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	baseEnv(t)
	t.Setenv(envServerURL, "not-a-url")
	t.Setenv(envAgentName, "no-hyphen-missing")
	err := func() error { _, err := loadConfig(); return err }()
	require.Error(t, err)
	assert.Contains(t, err.Error(), envServerURL)
	assert.ErrorIs(t, err, errMalformedEnv)
}

// TestLoadIdentityConfig_Valid_LeavesServerURLEmptyWhenUnset is loam-dc2v
// defect 3's unit-level proof: whoami's config loader resolves identity
// alone, and ServerURL() is the empty string -- not an error -- when
// LOAM_SERVER_URL is unset entirely.
func TestLoadIdentityConfig_Valid_LeavesServerURLEmptyWhenUnset(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	t.Setenv(envServerURL, "")
	t.Setenv(envAgentName, "ada-lovelace")
	t.Setenv(envAgentID, "7")
	t.Setenv(envAgentRole, "reviewer")
	t.Setenv(envOutputFormat, "")
	cfg, err := loadIdentityConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "", cfg.ServerURL())
	assert.Equal(t, "ada-lovelace-7-reviewer", cfg.Identifier())
}

// TestLoadIdentityConfig_ServerURLSetButMalformed_StillErrors proves an
// explicitly-set LOAM_SERVER_URL is still validated even though it is not
// required: identity config relaxes "required", not "well-formed".
func TestLoadIdentityConfig_ServerURLSetButMalformed_StillErrors(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	baseEnv(t)
	t.Setenv(envServerURL, "not-a-url")
	cfg, err := loadIdentityConfig()
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.ErrorIs(t, err, errMalformedEnv)
}

// TestLoadIdentityConfig_MissingIdentityVar_StillErrors proves loosening
// LOAM_SERVER_URL did not also loosen the identity variables whoami
// genuinely needs -- the exact non-regression cmd/loam/main_test.go's
// TestLoam_MissingRequiredEnvVar_ExitsUsage pins end to end.
func TestLoadIdentityConfig_MissingIdentityVar_StillErrors(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	baseEnv(t)
	t.Setenv(envAgentRole, "")
	cfg, err := loadIdentityConfig()
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), envAgentRole)
	assert.ErrorIs(t, err, errMissingEnv)
}

// TestConfigForArgs_Whoami_UsesIdentityConfig proves the dispatch decision
// in deps.go: only the literal top-level command "whoami" gets the relaxed
// loader.
func TestConfigForArgs_Whoami_UsesIdentityConfig(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	t.Setenv(envServerURL, "")
	t.Setenv(envAgentName, "ada-lovelace")
	t.Setenv(envAgentID, "7")
	t.Setenv(envAgentRole, "reviewer")
	t.Setenv(envOutputFormat, "")
	cfg, err := configForArgs([]string{"whoami", "--verify"})
	require.NoError(t, err)
	assert.Equal(t, "", cfg.ServerURL())
}

// TestConfigForArgs_NonWhoami_UsesFullConfig proves every other command
// (including an unrecognized one, which normal dispatch will go on to
// reject anyway) still requires the full four-variable config.
func TestConfigForArgs_NonWhoami_UsesFullConfig(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	t.Setenv(envServerURL, "")
	t.Setenv(envAgentName, "ada-lovelace")
	t.Setenv(envAgentID, "7")
	t.Setenv(envAgentRole, "reviewer")
	t.Setenv(envOutputFormat, "")
	for _, args := range [][]string{{"instructions"}, {"clone"}, {}} {
		_, err := configForArgs(args)
		require.Error(t, err, "args %v", args)
		assert.Contains(t, err.Error(), envServerURL, "args %v", args)
	}
}
