package cli

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/pflag"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

// The two ungated orientation commands, `instructions` and `whoami` (see
// docs/cli-spec.md -> instructions, whoami; docs/web-spec.md ->
// RoleService: "instructions and whoami are always available and
// ungated"), plus `clone`'s argument parsing.
//
// The two split cleanly along one line: `whoami` never touches the
// network, and `instructions` is nothing BUT a server call. That split is
// the reason they are separate commands at all (docs/cli-spec.md ->
// whoami: "Split out of instructions so identity can be fetched on its own
// without the larger payload"), and it is what lets an agent still report
// who it is when the server is down.

// --- instructions ---

// instructionsCommandOutput is one entry of `instructions`' command list
// (docs/cli-spec.md -> instructions -> Output: `{ "name": "work start",
// "summary": "Start a work branch from a target branch.", "synopsis":
// "<repo> <from>" }`). Synopsis carries the positional argument shape
// (loam-hi5o.4): before this field existed, `loam instructions "work
// start"` answered only the summary, restating the command's title rather
// than the question an agent asked it -- how to actually call the thing.
type instructionsCommandOutput struct {
	Name     string `json:"name"`
	Summary  string `json:"summary"`
	Synopsis string `json:"synopsis"`
}

// instructionsOutput is `instructions`' shape (docs/cli-spec.md ->
// instructions -> Output). Identity is deliberately absent -- that is
// `whoami`'s whole job -- so nothing here duplicates it.
type instructionsOutput struct {
	Usage            string                      `json:"usage"`
	Commands         []instructionsCommandOutput `json:"commands"`
	RoleInstructions string                      `json:"role_instructions"`
}

// instructionsCommandsFrom converts the proto command list into the output
// shape. The slice is always non-nil so an empty list still encodes as
// `[]` rather than `null`.
func instructionsCommandsFrom(commands []*loamv1.CommandInfo) []instructionsCommandOutput {
	rows := make([]instructionsCommandOutput, 0, len(commands))
	for _, c := range commands {
		rows = append(rows, instructionsCommandOutput{Name: c.GetName(), Summary: c.GetSummary(), Synopsis: c.GetSynopsis()})
	}
	return rows
}

// parseInstructionsArgs parses instructions' own positional argument:
// a bare newFlagSet (instructions takes no flags) with at most one
// positional token surviving. Factored out of runInstructions so
// help.go's `loam instructions <command>` suggestion can be checked
// against this exact function (loam-hi5o.4 acceptance criterion 3 --
// every suggestion help prints must be runnable verbatim) rather than a
// hand-rolled copy of the same check that could silently drift from it.
func parseInstructionsArgs(args []string) ([]string, error) {
	fs := newFlagSet("instructions")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return nil, err
	}
	if len(positional) > 1 {
		return nil, errors.New("instructions takes at most one command argument")
	}
	return positional, nil
}

// runInstructions implements `loam instructions [command]` (see
// docs/cli-spec.md -> instructions). No flags; the single optional
// argument names the command to fetch focused help for instead of the full
// orientation.
//
// Every field of the response comes from the server, INCLUDING the usage
// guide. docs/cli-spec.md describes this command as merging "a static
// usage guide built into the binary" with the server's role-specific text,
// but loam-ofg.11 put that static guide in the SERVER binary
// (internal/handler/meta/catalog.go -> usageText, whose doc comment quotes
// that same spec line as its rationale) and returns it in
// GetInstructionsResponse.usage. Keeping a second copy here would give an
// agent two usage guides free to drift apart, and the proto has no field
// to carry a client-side one in, so this renders exactly what the server
// sent. The command list is filtered to the caller's role server-side too,
// from the identity headers the Connect interceptor attaches (connect.go
// -> identityInterceptor); the CLI sends no role of its own and does no
// filtering, so it cannot widen what the caller is told it may do.
//
// Errors: a transport failure -- the "server is unreachable" case
// docs/cli-spec.md pins at exit 1 -- surfaces as a *connect.Error whose
// code (Unavailable/Unknown) classifyConnectError deliberately does not
// recognize, so mapCommandError returns nil and it exits 1 as an
// unexpected internal error. An unknown `command` argument is answered
// NotFound by the server (internal/handler/meta/meta.go -> GetInstructions)
// and so exits 3 -- which docs/cli-spec.md -> instructions -> Errors does
// not mention at all; that is the server's existing contract, not
// something invented here.
func runInstructions(ctx context.Context, deps *Deps, args []string) error {
	positional, err := parseInstructionsArgs(args)
	if err != nil {
		return newUsageError(err.Error())
	}
	req := &loamv1.GetInstructionsRequest{}
	if len(positional) == 1 {
		command := positional[0]
		req.Command = &command
	}
	resp, err := deps.connect.Meta().GetInstructions(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("fetching instructions: %w", err)
	}
	return deps.encoder.Encode(instructionsOutput{
		Usage:            resp.Msg.GetUsage(),
		Commands:         instructionsCommandsFrom(resp.Msg.GetCommands()),
		RoleInstructions: resp.Msg.GetRoleInstructions(),
	})
}

// --- whoami ---

// whoamiOutput is `whoami`'s shape (docs/cli-spec.md -> whoami -> Output).
//
// Identifier is reported ALONGSIDE the three parts rather than instead of
// them, and it is the full "<name>-<id>-<role>" string -- never the bare
// name. That distinction is the entire point of the field: loam-ppb was a
// P0 caused by treating the agent name as the identifier, and an agent
// reading this output to fill in a `--author` filter or to match a
// comment's author is exactly who that bug bites. Reporting both,
// unambiguously keyed, leaves nothing to guess about which string a given
// API wants.
//
// Verified is `omitempty` so it is absent from bare `whoami`'s JSON
// entirely, not merely false: loam-0pj.16 requires bare `whoami`'s output
// stay exactly as it was, and a caller must not be able to mistake "not
// checked" (`--verify` never ran) for "checked and failed" (which is
// instead a non-zero exit -- see runWhoami). It is only ever encoded true:
// a failed verification returns an error and never reaches the encode
// call, so this field, when present at all, always means the role
// resolved.
type whoamiOutput struct {
	Name       string `json:"name"`
	ID         string `json:"id"`
	Role       string `json:"role"`
	Identifier string `json:"identifier"`
	Verified   bool   `json:"verified,omitempty"`
}

// runWhoami implements `loam whoami` / `loam whoami --verify` (see
// docs/cli-spec.md -> whoami).
//
// Bare `whoami` is unchanged from before this flag existed: a pure render
// of the already-resolved configuration, no server call, enforced
// structurally by never reaching deps.connect on that path (see
// docs/cli-spec.md -> whoami: "Local only -- no server call", true of the
// DEFAULT, not of --verify). features/instructions.feature's "whoami works
// without contacting the server" points the CLI at a bound-then-closed
// port and depends on exactly this remaining true; --verify is opt-in
// specifically so that guarantee survives untouched.
//
// --verify additionally calls MetaService.GetInstructions -- the same RPC
// `instructions` already makes (runInstructions, above), reused rather than
// a new proto RPC because none is needed: an unresolvable role already
// fails that existing call. Its response body is discarded; only the error
// (or lack of one) is read. Two distinct failure modes follow from that one
// call, and conflating them would recreate the diagnostic problem this flag
// exists to solve (loam-0pj.16, loam-a8z):
//
//   - The role does not resolve server-side: rolestore.ErrNotFound at the
//     RoleStore seam is mapped to connect.CodePermissionDenied by loam-a8z's
//     handler-boundary fix, which classifyConnectError (errormapper.go)
//     turns into a newUnauthorizedError -- exit 2, docs/cli-spec.md's
//     authorization-denied class, deliberately not "not found" (a caller
//     must not distinguish real role names from typos by response code).
//   - The server is unreachable: an unclassified transport error
//     (connect.CodeUnavailable, or nothing *connect.Error at all) falls
//     through mapCommandError to the unexpected-internal-error class --
//     exit 1, identical to `instructions`' own "server is unreachable"
//     contract. That is deliberate, not an oversight: reusing
//     GetInstructions means --verify reuses that exact same exit-code
//     split for free, rather than inventing a third code no other command
//     uses.
//
// --verify checks deps.config.ServerURL() itself before touching
// deps.connect at all, rather than assuming a usable client exists: today
// NewProductionDeps (deps.go) always requires LOAM_SERVER_URL, so this
// check cannot fail in production, but that requirement is exactly what
// loam-dc2v is expected to loosen for bare `whoami` (identity alone needs
// no server), which would otherwise turn a missing LOAM_SERVER_URL into a
// nil-client panic here instead of the plain usage error this returns.
//
// The exit-2-on-missing-identity contract for a malformed environment is
// satisfied before this function can run at all, exactly as for bare
// whoami before this flag existed: loadConfig (config.go) rejects it and
// NewProductionDeps (deps.go) reports that as a usage error through the
// encoder before any Deps -- and therefore any dispatch -- exists.
// newWhoamiFlags builds the pflag.FlagSet for `loam whoami [--verify]`,
// plus the parsed --verify value. Factored out (rather than inlined in
// runWhoami, as it used to be) so router.go's commandTree() can build the
// same FlagSet with no Deps at all, for `loam whoami --help` (see
// router.go's command.newFlags and help.go's TryHelp).
func newWhoamiFlags() (*pflag.FlagSet, *bool) {
	fs := newFlagSet("whoami")
	verify := fs.Bool("verify", false, "confirm the configured role resolves on the server (makes a server call; the default is local only)")
	return fs, verify
}

func runWhoami(ctx context.Context, deps *Deps, args []string) error {
	fs, verify := newWhoamiFlags()
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if len(positional) > 0 {
		return newUsageError("whoami takes no arguments")
	}
	out := whoamiOutput{
		Name:       deps.config.AgentName(),
		ID:         deps.config.AgentID(),
		Role:       deps.config.AgentRole(),
		Identifier: deps.config.Identifier(),
	}
	if !*verify {
		return deps.encoder.Encode(out)
	}
	if deps.config.ServerURL() == "" {
		return newUsageError("whoami --verify requires LOAM_SERVER_URL to be configured")
	}
	if _, err := deps.connect.Meta().GetInstructions(ctx, connect.NewRequest(&loamv1.GetInstructionsRequest{})); err != nil {
		return fmt.Errorf("verifying role %s resolves on the server: %w", out.Role, err)
	}
	out.Verified = true
	return deps.encoder.Encode(out)
}

// --- clone ---

// runClone implements `loam clone <repo> <branch>` (see docs/cli-spec.md ->
// clone). No flags; both repo and branch are required — there is no
// default branch.
func runClone(ctx context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("clone")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if len(positional) != 2 {
		return newUsageError("clone requires exactly a repo and a branch argument")
	}
	return runCloneCommand(ctx, deps, positional[0], positional[1])
}
