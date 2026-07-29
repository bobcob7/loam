package cli

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

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
// (docs/cli-spec.md -> instructions -> Output: `{ "name": "work list",
// "summary": "List work branches" }`).
type instructionsCommandOutput struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
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
		rows = append(rows, instructionsCommandOutput{Name: c.GetName(), Summary: c.GetSummary()})
	}
	return rows
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
	fs := newFlagSet("instructions")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if len(positional) > 1 {
		return newUsageError("instructions takes at most one command argument")
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
type whoamiOutput struct {
	Name       string `json:"name"`
	ID         string `json:"id"`
	Role       string `json:"role"`
	Identifier string `json:"identifier"`
}

// runWhoami implements `loam whoami` (see docs/cli-spec.md -> whoami). No
// arguments, no flags.
//
// It takes no context because it makes no call that could use one: this is
// a pure render of the already-resolved configuration, and the "Local only
// -- no server call" promise in docs/cli-spec.md is enforced structurally,
// by there being no client reachable from here, rather than by a comment
// asking future edits not to add one.
//
// The exit-2-on-missing-identity contract is satisfied before this
// function can run at all: loadConfig (config.go) rejects a missing or
// malformed LOAM_AGENT_* variable, and NewProductionDeps (deps.go) reports
// that as a usage error through the encoder and returns before any Deps --
// and therefore any dispatch -- exists. Re-validating here would be a
// second copy of that rule, free to diverge from the one that actually
// runs first; the right place to prove it is an end-to-end test of the
// real binary, which cmd/loam/main_test.go has.
func runWhoami(_ context.Context, deps *Deps, args []string) error {
	fs := newFlagSet("whoami")
	positional, err := parseCommandArgs(fs, args)
	if err != nil {
		return newUsageError(err.Error())
	}
	if len(positional) > 0 {
		return newUsageError("whoami takes no arguments")
	}
	return deps.encoder.Encode(whoamiOutput{
		Name:       deps.config.AgentName(),
		ID:         deps.config.AgentID(),
		Role:       deps.config.AgentRole(),
		Identifier: deps.config.Identifier(),
	})
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
