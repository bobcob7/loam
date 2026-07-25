package cli

import "log/slog"

// Deps bundles the collaborators command handlers are built against. main()
// constructs one Deps and injects it into the Router; nothing here is
// package-level mutable state. Fields are unexported: nothing outside this
// package reads them directly, it only ever holds an opaque *Deps.
type Deps struct {
	logger      *slog.Logger
	config      Config
	encoder     OutputEncoder
	errorMapper ErrorMapper
	workspace   WorkspaceResolver
	connect     ConnectClient
}

// NewDeps constructs a Deps from its collaborators. Every field is required;
// callers (main(), tests) supply the concrete implementations.
func NewDeps(logger *slog.Logger, cfg Config, encoder OutputEncoder, errorMapper ErrorMapper, workspace WorkspaceResolver, connect ConnectClient) *Deps {
	return &Deps{logger: logger, config: cfg, encoder: encoder, errorMapper: errorMapper, workspace: workspace, connect: connect}
}
