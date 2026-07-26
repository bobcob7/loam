package cli

import (
	"io"
	"log/slog"

	"connectrpc.com/connect"
)

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

// NewErrorMapper builds the real ErrorMapper (see errormapper.go). Exported
// so main() can classify a Deps-construction failure from NewProductionDeps
// into an exit code — at that point no Deps exists yet to carry one.
func NewErrorMapper() ErrorMapper { return newErrorMapper() }

// NewProductionDeps builds the real Deps main() injects into the Router:
// loadConfig's validated LOAM_* configuration, the output encoder it
// selects, the real error mapper, the real workspace resolver (workspace.go
// -> NewWorkspaceResolver), and the real Connect clients (connect.go ->
// NewConnectClient) carrying the agent identity headers. httpClient is
// threaded through explicitly (main() passes http.DefaultClient) so tests
// can substitute one pointed at an httptest server.
//
// Building any of these can fail before a Deps exists to route the failure
// through — a missing/malformed required LOAM_* variable is a usage error
// (exit 2, see docs/cli-spec.md -> whoami); os.Getwd failing for the
// workspace resolver is not. Either way, the failure is still reported
// through the resolved output-format encoder (LOAM_OUTPUT_FORMAT never
// errors, so the encoder is available independent of the rest of config)
// before this returns the error for main() to classify via NewErrorMapper.
func NewProductionDeps(logger *slog.Logger, httpClient connect.HTTPClient, out io.Writer) (*Deps, error) {
	encoder := newEncoder(resolveOutputFormat(), out)
	cfg, err := loadConfig()
	if err != nil {
		return nil, reportConstructionError(encoder, err)
	}
	workspace, err := NewWorkspaceResolver(cfg)
	if err != nil {
		return nil, reportConstructionError(encoder, err)
	}
	connectClient, err := NewConnectClient(cfg, httpClient)
	if err != nil {
		return nil, reportConstructionError(encoder, err)
	}
	return NewDeps(logger, cfg, encoder, newErrorMapper(), workspace, connectClient), nil
}

// reportConstructionError encodes err through encoder in the same
// errorPayload shape Run uses for a command failure, classifying it via
// mapCommandError the same way (an unrecognized error is "internal", exit
// 1), and returns err unchanged for the caller to propagate.
func reportConstructionError(encoder OutputEncoder, err error) error {
	code := codeInternal
	if ce := mapCommandError(err); ce != nil {
		code = ce.code
	}
	_ = encoder.Encode(errorPayload{Error: errorDetail{Code: code, Message: err.Error()}})
	return err
}
