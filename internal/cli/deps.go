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
	cloner      gitCloner
	stdin       io.Reader
}

// NewDeps constructs a Deps from its collaborators. Every field is required
// for main(), which always supplies NewProductionDeps' concrete
// implementations. cloner is the one exception in tests: it is only
// exercised by `loam clone` (see clone.go), so tests that never dispatch
// clone may safely pass nil for it, as several in this package's own test
// files do. stdin is the same kind of exception for `loam work set` (see
// commands_work.go's readStdin) -- tests that never dispatch it may also
// pass nil.
func NewDeps(logger *slog.Logger, cfg Config, encoder OutputEncoder, errorMapper ErrorMapper, workspace WorkspaceResolver, connect ConnectClient, cloner gitCloner, stdin io.Reader) *Deps {
	return &Deps{logger: logger, config: cfg, encoder: encoder, errorMapper: errorMapper, workspace: workspace, connect: connect, cloner: cloner, stdin: stdin}
}

// NewErrorMapper builds the real ErrorMapper (see errormapper.go). Exported
// so main() can classify a Deps-construction failure from NewProductionDeps
// into an exit code — at that point no Deps exists yet to carry one.
func NewErrorMapper() ErrorMapper { return newErrorMapper() }

// NewProductionDeps builds the real Deps main() injects into the Router:
// loadConfig's validated LOAM_* configuration, the output encoder it
// selects, the real error mapper, the real workspace resolver (workspace.go
// -> newWorkspaceResolver), and the real Connect clients (connect.go ->
// newConnectClient) carrying the agent identity headers. httpClient and in
// are threaded through explicitly (main() passes http.DefaultClient and
// os.Stdin) so tests can substitute either one -- an httptest server for
// httpClient, a fixed reader for in, which `loam work set` reads an
// optional description from (see commands_work.go's readStdin).
//
// Building any of these can fail before a Deps exists to route the failure
// through — a missing/malformed required LOAM_* variable is a usage error
// (exit 2, see docs/cli-spec.md -> whoami); os.Getwd failing for the
// workspace resolver is not. Either way, the failure is still reported
// through the resolved output-format encoder (LOAM_OUTPUT_FORMAT never
// errors, so the encoder is available independent of the rest of config)
// before this returns the error for main() to classify via NewErrorMapper.
func NewProductionDeps(logger *slog.Logger, httpClient connect.HTTPClient, out io.Writer, in io.Reader) (*Deps, error) {
	encoder := newEncoder(resolveOutputFormat(), out)
	cfg, err := loadConfig()
	if err != nil {
		return nil, reportConstructionError(encoder, err)
	}
	workspace, err := newWorkspaceResolver(cfg)
	if err != nil {
		return nil, reportConstructionError(encoder, err)
	}
	connectClient, err := newConnectClient(cfg, httpClient)
	if err != nil {
		return nil, reportConstructionError(encoder, err)
	}
	return NewDeps(logger, cfg, encoder, newErrorMapper(), workspace, connectClient, newGitCloner(), in), nil
}

// reportConstructionError encodes err through encoder in the same
// errorPayload shape Run uses for a command failure, classifying it via
// mapCommandError the same way (an unrecognized error is "internal", exit
// 1), and returns err unchanged for the caller to propagate. The message
// comes from the classified *cliError when there is one (see run.go's
// identical rationale: a raw *connect.Error's own Error() prepends its
// code, which would duplicate errorDetail.Code).
func reportConstructionError(encoder OutputEncoder, err error) error {
	code := codeInternal
	message := err.Error()
	if ce := mapCommandError(err); ce != nil {
		code = ce.code
		message = ce.Error()
	}
	_ = encoder.Encode(errorPayload{Error: errorDetail{Code: code, Message: message}})
	return err
}
