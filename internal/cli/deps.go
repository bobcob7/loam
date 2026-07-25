package cli

import "log/slog"

// Deps bundles the collaborators command handlers are built against. main()
// constructs one Deps and injects it into the Router; nothing here is
// package-level mutable state.
type Deps struct {
	Logger    *slog.Logger
	Config    Config
	Encoder   OutputEncoder
	Errors    ErrorMapper
	Workspace WorkspaceResolver
	Connect   ConnectClient
}

// NewDeps constructs a Deps from its collaborators. Every field is required;
// callers (main(), tests) supply the concrete implementations.
func NewDeps(logger *slog.Logger, cfg Config, encoder OutputEncoder, errs ErrorMapper, workspace WorkspaceResolver, connect ConnectClient) *Deps {
	return &Deps{Logger: logger, Config: cfg, Encoder: encoder, Errors: errs, Workspace: workspace, Connect: connect}
}
