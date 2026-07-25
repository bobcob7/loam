// Package cli implements the loam command tree: routing, per-command flag
// parsing, and the collaborator seams each command handler is built against.
// See docs/cli-spec.md for the full command surface.
package cli

//go:generate go tool moq -out moq_test.go . Config OutputEncoder ErrorMapper WorkspaceResolver

// Config exposes the LOAM_* environment configuration every command may
// need (see docs/cli-spec.md -> Environment Variables). Implemented by a
// later bead (loam-0pj.2); this package only depends on the small read-only
// surface below.
type Config interface {
	// OutputFormat returns the active output format: json, yaml, xml, or
	// human. Unrecognized values fall back to json.
	OutputFormat() string
	// AgentName returns the calling agent's configured name.
	AgentName() string
	// AgentID returns the calling agent's configured id.
	AgentID() string
	// AgentRole returns the calling agent's configured role.
	AgentRole() string
	// ServerURL returns the base URL of the Loam server: the Connect APIs
	// and the git smart-HTTP endpoint (clone composes
	// <ServerURL>/git/<group>/<repo>.git; there is no separate git URL).
	ServerURL() string
}

// OutputEncoder writes a command's result, or a structured error, to stdout
// in the active output format. Implemented by a later bead (loam-0pj.3).
type OutputEncoder interface {
	Encode(v any) error
}

// ErrorMapper maps a command handler's error to the CLI's coarse exit-code
// scheme (see docs/cli-spec.md -> Exit Codes & Errors: 0 success, 1
// unexpected internal error, 2 usage/authz/conflict/precondition, 3 not
// found). Implemented by a later bead (loam-0pj.4).
type ErrorMapper interface {
	ExitCode(err error) int
}

// WorkspaceResolver infers the repo and work-branch identifiers from the
// current working directory when a command omits them (see docs/cli-spec.md
// -> Workspace). Implemented by a later bead (loam-0pj.5).
type WorkspaceResolver interface {
	ResolveRepo() (string, error)
	ResolveWorkBranch() (string, error)
}

// ConnectClient is a placeholder seam for the Connect RPC clients command
// handlers will eventually call through (work branches, graph, search,
// repo/clone). It is intentionally empty: the proto surface under
// internal/gen is being reshaped concurrently by another bead, so this
// package must not import it yet. A later bead replaces this with the real
// generated Connect client interfaces, defined here where they are
// consumed.
type ConnectClient interface{}
