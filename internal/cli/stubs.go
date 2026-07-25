package cli

import (
	"errors"
	"io"
	"log/slog"
)

// The types below are placeholder implementations of the interfaces in
// interfaces.go. They exist only so main() has something concrete to
// construct and inject while loam-0pj.5 and loam-0pj.6 build the real
// workspace and Connect-client packages. Neither attempts real inference or
// RPC behavior: that belongs to those beads. They are unexported, and
// NewPlaceholderDeps is the single symbol main() needs from this file; a
// tracking bead (loam-qdr) deletes this whole file once .5 and .6 land.

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

// NewPlaceholderDeps builds a Deps from the real Config, OutputEncoder, and
// ErrorMapper collaborators, plus a WorkspaceResolver that never infers
// anything and an empty ConnectClient — loam-0pj.5 and loam-0pj.6 replace
// those two. It is the single symbol main() needs from this file.
//
// Config loading can fail (a missing or malformed required LOAM_* variable
// is a usage error, exit 2 per docs/cli-spec.md -> whoami). LOAM_OUTPUT_FORMAT
// is resolved independently of that validation — it never errors — so the
// failure is still reported in the right output format before this function
// returns the error: main() has no config yet at that point to pick an
// encoder from.
func NewPlaceholderDeps(logger *slog.Logger, out io.Writer) (*Deps, error) {
	encoder := newEncoder(resolveOutputFormat(), out)
	cfg, err := loadConfig()
	if err != nil {
		_ = encoder.Encode(errorPayload{Error: errorDetail{Code: codeUsage, Message: err.Error()}})
		return nil, err
	}
	return NewDeps(logger, cfg, encoder, newErrorMapper(), &unresolvedWorkspace{}, &noopConnectClient{}), nil
}
