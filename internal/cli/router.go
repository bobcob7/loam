package cli

import (
	"context"
	"fmt"
)

// handlerFunc implements one leaf command. args holds everything after the
// command's own name in the tree (e.g. for "work set a b --title x", the
// "set" handler receives ["a", "b", "--title", "x"]).
type handlerFunc func(ctx context.Context, deps *Deps, args []string) error

// command is one node in the tree: either a leaf with a handler, or a group
// with subcommands (never both).
type command struct {
	summary     string
	run         handlerFunc
	subcommands map[string]*command
}

// Router dispatches argv to the command tree defined in
// docs/cli-spec.md. The tree is built once, in commandTree, so it has a
// single source of truth.
type Router struct {
	deps     *Deps
	commands map[string]*command
}

// NewRouter builds the full command tree wired against deps.
func NewRouter(deps *Deps) *Router {
	return &Router{deps: deps, commands: commandTree()}
}

// Dispatch resolves args against the command tree and invokes the matching
// handler. An empty argv, an unknown top-level command, or an unknown
// subcommand all produce a *usageError.
func (rt *Router) Dispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return newUsageError("no command given; run `loam instructions` for usage")
	}
	name := args[0]
	cmd, ok := rt.commands[name]
	if !ok {
		return newUsageError(fmt.Sprintf("unknown command %q", name))
	}
	if cmd.subcommands == nil {
		return cmd.run(ctx, rt.deps, args[1:])
	}
	return dispatchGroup(ctx, rt.deps, name, cmd, args[1:])
}

// dispatchGroup resolves the subcommand of a group node (e.g. "work",
// "graph") and invokes it.
func dispatchGroup(ctx context.Context, deps *Deps, groupName string, group *command, args []string) error {
	if len(args) == 0 {
		return newUsageError(fmt.Sprintf("%s requires a subcommand", groupName))
	}
	sub, ok := group.subcommands[args[0]]
	if !ok {
		return newUsageError(fmt.Sprintf("unknown %s subcommand %q", groupName, args[0]))
	}
	return sub.run(ctx, deps, args[1:])
}

// commandTree is the single definition of the loam command surface (see
// docs/cli-spec.md, as corrected by loam-0pj.1's NOTES: commit and push are
// removed; graph gains --file/--limit and work list gains --limit).
func commandTree() map[string]*command {
	return map[string]*command{
		"instructions": {summary: "Role-specific instructions and CLI usage", run: runInstructions},
		"whoami":       {summary: "Report the calling agent's resolved identity", run: runWhoami},
		"clone":        {summary: "Clone an enrolled repo from the server", run: runClone},
		"work": {summary: "Work branch operations", subcommands: map[string]*command{
			"start":          {summary: "Start a work branch", run: runWorkStart},
			"set":            {summary: "Set a work branch's title/description", run: runWorkSet},
			"request-review": {summary: "Request review of a work branch", run: runWorkRequestReview},
			"list":           {summary: "List work branches", run: runWorkList},
			"show":           {summary: "Show a work branch's metadata", run: runWorkShow},
			"diff":           {summary: "Show a work branch's diff", run: runWorkDiff},
			"comments":       {summary: "Fetch comment threads or staged comments", run: runWorkComments},
			"verdicts":       {summary: "List verdicts on a work branch", run: runWorkVerdicts},
			"comment":        {summary: "Stage a review comment", run: runWorkComment},
			"reply":          {summary: "Reply to a comment thread", run: runWorkReply},
			"verdict":        {summary: "Publish staged comments as a verdict", run: runWorkVerdict},
		}},
		"graph": {summary: "Structural queries over the Tree-sitter graph", subcommands: map[string]*command{
			"def":        {summary: "Where a symbol is defined", run: runGraphDef},
			"refs":       {summary: "Everywhere a symbol is referenced", run: runGraphRefs},
			"deps":       {summary: "What a target depends on", run: runGraphDeps},
			"dependents": {summary: "What depends on a target", run: runGraphDependents},
			"history":    {summary: "A symbol's commit/ref history", run: runGraphHistory},
		}},
		"search": {summary: "Natural-language semantic search", run: runSearch},
	}
}
