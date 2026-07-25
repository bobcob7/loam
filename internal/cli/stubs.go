package cli

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
)

// The types below are placeholder implementations of the interfaces in
// interfaces.go. They exist only so main() has something concrete to
// construct and inject while loam-0pj.2 through loam-0pj.5 build the real
// packages (internal/config, internal/output, an error-mapping package, and
// a workspace package respectively). None of them attempt real validation,
// formatting, or inference: that behavior belongs to those beads. They are
// unexported, and NewPlaceholderDeps is the single symbol main() needs from
// this file, so nothing outside this package can come to depend on a
// placeholder; a tracking bead deletes this whole file once .2-.5 land.

// envConfig reads Config values directly from the process environment, with
// no required-variable validation. Replaced wholesale by internal/config.
type envConfig struct{}

// OutputFormat returns LOAM_OUTPUT_FORMAT, defaulting to "json" when unset.
func (c *envConfig) OutputFormat() string {
	if format := os.Getenv("LOAM_OUTPUT_FORMAT"); format != "" {
		return format
	}
	return "json"
}

// AgentName returns LOAM_AGENT_NAME.
func (c *envConfig) AgentName() string { return os.Getenv("LOAM_AGENT_NAME") }

// AgentID returns LOAM_AGENT_ID.
func (c *envConfig) AgentID() string { return os.Getenv("LOAM_AGENT_ID") }

// AgentRole returns LOAM_AGENT_ROLE.
func (c *envConfig) AgentRole() string { return os.Getenv("LOAM_AGENT_ROLE") }

// ServerURL returns LOAM_SERVER_URL.
func (c *envConfig) ServerURL() string { return os.Getenv("LOAM_SERVER_URL") }

// jsonEncoder always writes v as JSON to w, regardless of the configured
// output format. Replaced by the real json/yaml/xml/human encoder.
type jsonEncoder struct{ w io.Writer }

// Encode writes v to the underlying writer as JSON.
func (e *jsonEncoder) Encode(v any) error { return json.NewEncoder(e.w).Encode(v) }

// coarseErrorMapper maps any non-nil error to exit code 1, collapsing the
// spec's 1/2/3 exit-code scheme. Replaced by the structured error/exit-code
// mapping bead.
type coarseErrorMapper struct{}

// ExitCode returns 1 for any non-nil err, 0 otherwise.
func (m *coarseErrorMapper) ExitCode(err error) int {
	if err == nil {
		return 0
	}
	return 1
}

// errWorkspaceUnresolved is returned by unresolvedWorkspace: this stage has
// no inference logic, so repo/work-branch must always be given explicitly.
var errWorkspaceUnresolved = errors.New("workspace inference not implemented; pass repo/work-branch explicitly")

// unresolvedWorkspace never infers a repo or work branch. Replaced by real
// workspace/.loam inference.
type unresolvedWorkspace struct{}

// ResolveRepo always fails: no inference exists yet.
func (w *unresolvedWorkspace) ResolveRepo() (string, error) { return "", errWorkspaceUnresolved }

// ResolveWorkBranch always fails: no inference exists yet.
func (w *unresolvedWorkspace) ResolveWorkBranch() (string, error) {
	return "", errWorkspaceUnresolved
}

// noopConnectClient satisfies ConnectClient with no behavior. Replaced once
// the reshaped proto surface under internal/gen settles.
type noopConnectClient struct{}

// NewPlaceholderDeps builds a Deps from placeholder collaborators: an
// env-var Config with no required-variable validation, a JSON-only
// OutputEncoder, an ErrorMapper that collapses every error to exit 1, a
// WorkspaceResolver that never infers anything, and an empty ConnectClient.
// It is the single symbol main() needs from this file; loam-0pj.2 through
// loam-0pj.5 replace it with real constructor injection of internal/config,
// internal/output, etc.
func NewPlaceholderDeps(logger *slog.Logger, out io.Writer) *Deps {
	return NewDeps(logger, &envConfig{}, &jsonEncoder{w: out}, &coarseErrorMapper{}, &unresolvedWorkspace{}, &noopConnectClient{})
}
