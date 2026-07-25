package cli

import (
	"encoding/json"
	"errors"
	"io"
	"os"
)

// The types below are placeholder implementations of the interfaces in
// interfaces.go. They exist only so main() has something concrete to
// construct and inject while loam-0pj.2 through loam-0pj.5 build the real
// packages (internal/config, internal/output, an error-mapping package, and
// a workspace package respectively). None of them attempt real validation,
// formatting, or inference: that behavior belongs to those beads.

// EnvConfig reads Config values directly from the process environment, with
// no validation. Replaced wholesale by internal/config in loam-0pj.2.
type EnvConfig struct{}

// NewEnvConfig constructs an EnvConfig.
func NewEnvConfig() *EnvConfig { return &EnvConfig{} }

// OutputFormat returns LOAM_OUTPUT_FORMAT, defaulting to "json" when unset.
func (c *EnvConfig) OutputFormat() string {
	if format := os.Getenv("LOAM_OUTPUT_FORMAT"); format != "" {
		return format
	}
	return "json"
}

// AgentName returns LOAM_AGENT_NAME.
func (c *EnvConfig) AgentName() string { return os.Getenv("LOAM_AGENT_NAME") }

// AgentID returns LOAM_AGENT_ID.
func (c *EnvConfig) AgentID() string { return os.Getenv("LOAM_AGENT_ID") }

// AgentRole returns LOAM_AGENT_ROLE.
func (c *EnvConfig) AgentRole() string { return os.Getenv("LOAM_AGENT_ROLE") }

// ServerURL returns LOAM_SERVER_URL.
func (c *EnvConfig) ServerURL() string { return os.Getenv("LOAM_SERVER_URL") }

// JSONEncoder always writes v as JSON to w, regardless of the configured
// output format. Replaced by the real json/yaml/xml/human encoder in
// loam-0pj.3.
type JSONEncoder struct{ w io.Writer }

// NewJSONEncoder constructs a JSONEncoder writing to w.
func NewJSONEncoder(w io.Writer) *JSONEncoder { return &JSONEncoder{w: w} }

// Encode writes v to the underlying writer as JSON.
func (e *JSONEncoder) Encode(v any) error { return json.NewEncoder(e.w).Encode(v) }

// CoarseErrorMapper maps any non-nil error to exit code 1. Replaced by the
// structured error/exit-code mapping in loam-0pj.4.
type CoarseErrorMapper struct{}

// NewCoarseErrorMapper constructs a CoarseErrorMapper.
func NewCoarseErrorMapper() *CoarseErrorMapper { return &CoarseErrorMapper{} }

// ExitCode returns 1 for any non-nil err, 0 otherwise.
func (m *CoarseErrorMapper) ExitCode(err error) int {
	if err == nil {
		return 0
	}
	return 1
}

// errWorkspaceUnresolved is returned by UnresolvedWorkspace: this stage has
// no inference logic, so repo/work-branch must always be given explicitly.
var errWorkspaceUnresolved = errors.New("workspace inference not implemented; pass repo/work-branch explicitly")

// UnresolvedWorkspace never infers a repo or work branch. Replaced by real
// workspace/.loam inference in loam-0pj.5.
type UnresolvedWorkspace struct{}

// NewUnresolvedWorkspace constructs an UnresolvedWorkspace.
func NewUnresolvedWorkspace() *UnresolvedWorkspace { return &UnresolvedWorkspace{} }

// ResolveRepo always fails: no inference exists yet.
func (w *UnresolvedWorkspace) ResolveRepo() (string, error) { return "", errWorkspaceUnresolved }

// ResolveWorkBranch always fails: no inference exists yet.
func (w *UnresolvedWorkspace) ResolveWorkBranch() (string, error) {
	return "", errWorkspaceUnresolved
}

// NoopConnectClient satisfies ConnectClient with no behavior. Replaced once
// the reshaped proto surface under internal/gen settles.
type NoopConnectClient struct{}

// NewNoopConnectClient constructs a NoopConnectClient.
func NewNoopConnectClient() *NoopConnectClient { return &NoopConnectClient{} }
