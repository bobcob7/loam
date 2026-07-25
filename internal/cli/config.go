package cli

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Environment variable names the CLI reads (see docs/cli-spec.md ->
// Environment Variables). This is the CLI's own, much smaller surface —
// distinct from internal/config, which loads the server's 12-var surface.
const (
	envServerURL    = "LOAM_SERVER_URL"
	envAgentName    = "LOAM_AGENT_NAME"
	envAgentID      = "LOAM_AGENT_ID"
	envAgentRole    = "LOAM_AGENT_ROLE"
	envOutputFormat = "LOAM_OUTPUT_FORMAT"
)

// envConfig is the loaded, validated LOAM_* configuration (see
// docs/cli-spec.md -> Environment Variables). Immutable once returned by
// loadConfig; there is no package-level mutable state.
type envConfig struct {
	serverURL    string
	agentName    string
	agentID      string
	agentRole    string
	outputFormat string
	identifier   string
}

// OutputFormat returns the active output format: json, yaml, xml, or human.
func (c *envConfig) OutputFormat() string { return c.outputFormat }

// AgentName returns the calling agent's configured name.
func (c *envConfig) AgentName() string { return c.agentName }

// AgentID returns the calling agent's configured id.
func (c *envConfig) AgentID() string { return c.agentID }

// AgentRole returns the calling agent's configured role.
func (c *envConfig) AgentRole() string { return c.agentRole }

// ServerURL returns the base URL of the Loam server.
func (c *envConfig) ServerURL() string { return c.serverURL }

// Identifier returns the resolved "<name>-<id>-<role>" identifier, reused by
// whoami and by the Connect identity headers (see docs/cli-spec.md ->
// Environment Variables).
func (c *envConfig) Identifier() string { return c.identifier }

// resolveOutputFormat reads LOAM_OUTPUT_FORMAT and falls back to "json" for
// an unset or unrecognized value. This never errors, so it can run even
// when the required identity variables below are missing or malformed:
// main() uses it to pick an encoder before it knows whether the rest of the
// config is valid, so a config error can still be reported in the right
// format.
func resolveOutputFormat() string {
	switch format := os.Getenv(envOutputFormat); format {
	case "json", "yaml", "xml", "human":
		return format
	default:
		return "json"
	}
}

// loadConfig reads and validates every LOAM_* environment variable (see
// docs/cli-spec.md -> Environment Variables). A missing or malformed
// required variable (LOAM_SERVER_URL, LOAM_AGENT_NAME, LOAM_AGENT_ID,
// LOAM_AGENT_ROLE) is a usage error (exit 2, per cli-spec -> whoami);
// LOAM_OUTPUT_FORMAT is the sole optional variable and is lenient.
func loadConfig() (*envConfig, error) {
	serverURL, err := requireServerURL()
	if err != nil {
		return nil, err
	}
	agentName, err := requireAgentName()
	if err != nil {
		return nil, err
	}
	agentID, err := requireNonEmpty(envAgentID)
	if err != nil {
		return nil, err
	}
	agentRole, err := requireNonEmpty(envAgentRole)
	if err != nil {
		return nil, err
	}
	return &envConfig{
		serverURL:    serverURL,
		agentName:    agentName,
		agentID:      agentID,
		agentRole:    agentRole,
		outputFormat: resolveOutputFormat(),
		identifier:   fmt.Sprintf("%s-%s-%s", agentName, agentID, agentRole),
	}, nil
}

// requireNonEmpty returns the value of the named required environment
// variable, or a usage error wrapping errMissingEnv if it is unset or
// empty.
func requireNonEmpty(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", newUsageCLIError(fmt.Sprintf("%s is required but not set", name), errMissingEnv)
	}
	return v, nil
}

// requireAgentName validates LOAM_AGENT_NAME is set and shaped like
// "<first-name>-<last-name>" (see docs/cli-spec.md -> Environment
// Variables).
func requireAgentName() (string, error) {
	v, err := requireNonEmpty(envAgentName)
	if err != nil {
		return "", err
	}
	first, last, ok := strings.Cut(v, "-")
	if !ok || first == "" || last == "" {
		return "", newUsageCLIError(fmt.Sprintf("%s %q: expected <first-name>-<last-name>", envAgentName, v), errMalformedEnv)
	}
	return v, nil
}

// requireServerURL validates LOAM_SERVER_URL is set and parses as an
// absolute URL (scheme and host present) — validated by parsing, never by
// connecting (see docs/cli-spec.md -> Environment Variables).
func requireServerURL() (string, error) {
	v, err := requireNonEmpty(envServerURL)
	if err != nil {
		return "", err
	}
	parsed, parseErr := url.Parse(v)
	if parseErr != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", newUsageCLIError(fmt.Sprintf("%s %q: must be an absolute URL", envServerURL, v), errMalformedEnv)
	}
	return v, nil
}
