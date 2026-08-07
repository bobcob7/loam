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
	gitRefs     gitRefs
}

// NewDeps constructs a Deps from its collaborators. Every field is required
// for main(), which always supplies NewProductionDeps' concrete
// implementations. cloner is the one exception in tests: it is only
// exercised by `loam clone` (see clone.go), so tests that never dispatch
// clone may safely pass nil for it, as several in this package's own test
// files do. stdin is the same kind of exception for the two commands that
// read a body from it, `loam work set` and `loam work comment` (see
// commands_work.go's readStdin) -- tests that never dispatch either may
// also pass nil.
//
// refs is a third such exception, used by `clone`, `work diff` and `work
// show`. What a nil refs means differs by command, and the difference is
// stated here rather than generalized because loam-hwru is precisely about
// not asserting a uniformity that does not hold:
//
//   - `clone` FAILS. A clone with no base ref is the state that bead was
//     filed about, so it cannot be produced quietly.
//   - `work diff` reports refs_error and a "not checked" local_check. Its
//     artifact is the thing being identified, so an unidentifiable one has
//     to say so.
//   - `work show` omits target_sha/head_sha SILENTLY. Its own answer is
//     complete without them, and there is no nil refs in the binary to
//     report on -- see NewProductionDeps, which always supplies the real
//     one. Were that guard ever reachable in production it would be the
//     wrong behaviour and would need a refs_error like `work diff`'s; it
//     is written the way it is so this package's many existing `work show`
//     tests need not each construct a git double.
func NewDeps(logger *slog.Logger, cfg Config, encoder OutputEncoder, errorMapper ErrorMapper, workspace WorkspaceResolver, connect ConnectClient, cloner gitCloner, stdin io.Reader, refs gitRefs) *Deps {
	return &Deps{logger: logger, config: cfg, encoder: encoder, errorMapper: errorMapper, workspace: workspace, connect: connect, cloner: cloner, stdin: stdin, gitRefs: refs}
}

// NewErrorMapper builds the real ErrorMapper (see errormapper.go). Exported
// so main() can classify a Deps-construction failure from NewProductionDeps
// into an exit code — at that point no Deps exists yet to carry one.
func NewErrorMapper() ErrorMapper { return newErrorMapper() }

// NewProductionDeps builds the real Deps main() injects into the Router:
// its validated LOAM_* configuration, the output encoder it selects, the
// real error mapper, the real workspace resolver (workspace.go ->
// newWorkspaceResolver), and the real Connect clients (connect.go ->
// newConnectClient) carrying the agent identity headers. httpClient and in
// are threaded through explicitly (main() passes http.DefaultClient and
// os.Stdin) so tests can substitute either one -- an httptest server for
// httpClient, a fixed reader for in, which `loam work set` reads an
// optional description from (see commands_work.go's readStdin).
//
// args is the command line about to be dispatched (main() passes
// os.Args[1:], after its own cli.TryHelp check has already ruled out a
// help route -- see help.go). It decides which config-loading strategy to
// use via configForArgs, which OWNS that rule.
//
// This comment deliberately does not restate which commands need which
// variables. It used to, and the census read "every other command still
// needs the full four-variable config loadConfig requires" -- true when it
// was written, false the moment `instructions` became a second exception
// (loam-hi5o.31), and false in the more damaging way: configForArgs used to
// point back HERE as the authoritative account of itself, so the two
// cross-referenced each other and the stale one sounded like the summary.
// That pointer is gone -- see configForArgs, which now owns its own rule
// outright.
// Naming the rule's home instead of its contents is what stops the next
// command-specific relaxation from repeating that.
//
// What does not change, and is the reason the strategy is chosen HERE
// rather than inside config.go: this is "require each variable where it is
// actually used" applied to WHEN AND HOW Deps itself is built. Reordering
// loadConfig's requireX calls alone cannot achieve it, because
// NewProductionDeps used to call loadConfig unconditionally before
// Router.Dispatch ever saw args at all.
//
// Building any of these can fail before a Deps exists to route the failure
// through — a missing/malformed required LOAM_* variable is a usage error
// (exit 2, see docs/cli-spec.md -> whoami); os.Getwd failing for the
// workspace resolver is not. Either way, the failure is still reported
// through the resolved output-format encoder (LOAM_OUTPUT_FORMAT never
// errors, so the encoder is available independent of the rest of config)
// before this returns the error for main() to classify via NewErrorMapper.
//
// The Connect client is built only when ServerURL() is non-empty. For
// every command but `whoami` that is always true -- whichever loader
// configForArgs picked required LOAM_SERVER_URL -- so this changes nothing
// for them; for `whoami` without LOAM_SERVER_URL
// it leaves deps.connect nil, which is safe because bare whoami never
// touches it and `--verify` checks ServerURL() itself first (see
// runWhoami's doc comment in commands_root.go).
func NewProductionDeps(logger *slog.Logger, httpClient connect.HTTPClient, out io.Writer, in io.Reader, args []string) (*Deps, error) {
	encoder := newEncoder(resolveOutputFormat(), out)
	cfg, err := configForArgs(args)
	if err != nil {
		return nil, reportConstructionError(encoder, err)
	}
	workspace, err := newWorkspaceResolver(cfg)
	if err != nil {
		return nil, reportConstructionError(encoder, err)
	}
	var connectClient ConnectClient
	if cfg.ServerURL() != "" {
		connectClient, err = newConnectClient(cfg, httpClient)
		if err != nil {
			return nil, reportConstructionError(encoder, err)
		}
	}
	return NewDeps(logger, cfg, encoder, newErrorMapper(), workspace, connectClient, newGitCloner(), in, newGitRefs()), nil
}

// configForArgs picks the config-loading strategy from the top-level
// command about to run. NewProductionDeps' doc comment covers why that
// choice is made during Deps CONSTRUCTION at all; which command gets which
// loader is here, and only here, so the two no longer point at each other.
//
// Three cases, and everything not named below -- including an empty or
// unrecognized args, which will go on to fail Dispatch's own routing
// checks exactly as before -- still goes through the full four-variable
// loadConfig, unchanged from before loam-dc2v.
//
//   - `whoami` gets the relaxed, identity-only loader: identity IS the
//     environment it needs, and a server it does not talk to must not gate
//     it (loam-dc2v defect 3). It deliberately does NOT take the identity
//     defaults below: whoami reports the identity an operator CONFIGURED,
//     and answering it with a defaulted synthetic one would make
//     "misconfigured" indistinguishable from "deliberately left at the
//     defaults" in the one command whose whole job is diagnosing that.
//   - `instructions` gets the built-in DEFAULT VALUE of the three
//     LOAM_AGENT_* variables when none of them is set (loam-hi5o.31): the
//     well-known orchestrator identity, so an orchestrator that configured
//     nothing but LOAM_SERVER_URL can still ask what its job is. This is a
//     defaulted identity, not a missing one -- the request carries a real
//     identity over the ordinary authenticated path either way. With any
//     LOAM_AGENT_* set it takes the loadConfig branch below and behaves
//     exactly as it always has; see identityDefaultsApply for why the three
//     default together rather than one at a time.
//   - everything else: loadConfig.
//
// This supersedes loam-hi5o.3's decision that `instructions` must not run
// without a configured identity, but only narrowly, and its reasoning is
// preserved rather than discarded: that bead's concern was an agent reading an
// UNFILTERED command list as its own permissions. Nothing here returns an
// unfiltered list. The response is the orchestrator role's own
// capability-filtered commands and instructions -- a real, narrow role
// holding graph.query and search and no work-branch capability -- produced
// by the same server-side filter every other role's answer goes through
// (internal/handler/meta's filterCommands). `loam help` remains the
// unfiltered listing, and remains the one route that needs no environment
// at all.
func configForArgs(args []string) (*envConfig, error) {
	if len(args) == 0 {
		return loadConfig()
	}
	if args[0] == "whoami" {
		return loadIdentityConfig()
	}
	if args[0] == "instructions" && identityDefaultsApply() {
		return loadOrchestratorConfig()
	}
	return loadConfig()
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
