package cli

import (
	"context"
	"fmt"

	"github.com/spf13/pflag"

	"github.com/bobcob7/loam/internal/cmdspec"
)

// handlerFunc implements one leaf command. args holds everything after the
// command's own name in the tree (e.g. for "work set a b --title x", the
// "set" handler receives ["a", "b", "--title", "x"]).
type handlerFunc func(ctx context.Context, deps *Deps, args []string) error

// command is one node in the tree: either a leaf with a handler, or a group
// with subcommands (never both).
//
// newFlags is set on every LEAF only (nil on a group like "work"/"graph",
// which has no flags of its own — dispatchGroup never parses any). It
// builds that leaf's pflag.FlagSet with no Deps at all, reusing exactly the
// registrations the real handler makes — either the already-factored
// newXFlags() helper most handlers already call (e.g. newWorkSetFlags,
// newGraphQueryFlags), or a bare newFlagSet(name) for the several leaves
// that take no flags beyond their positionals. help.go's TryHelp is the
// reason this exists: it renders `loam <command> --help`'s flag usage from
// this closure alone, entirely before main() ever builds a Deps (see
// loam-dc2v/loam-q0ek — help must never require LOAM_* configuration).
//
// synopsis and stdinNote are set on every LEAF only too, filled in by
// applySynopsis right after the tree literal below is built. They come
// from cmdspec.Synopsis and cmdspec.StdinNote respectively, keyed by the
// leaf's own full dispatch name -- the same package internal/handler/meta's
// catalog reads its copy from (loam-hi5o.4), so the two cannot drift the
// way a hardcoded "[flags]" and an absent catalog synopsis once did. They
// stay as two separate fields rather than one pre-joined string
// specifically so help.go's leafUsageLine can place a "[flags]" token
// between the positional shape and a trailing stdin note, matching
// docs/cli-spec.md's own ordering (cmdspec.Compose is the shared rule for
// joining them back together where a single string IS wanted, e.g. the
// catalog's synopsis field).
type command struct {
	summary     string
	run         handlerFunc
	newFlags    func() *pflag.FlagSet
	synopsis    string
	flags       string
	stdinNote   string
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
// docs/cli-spec.md). There is no commit or push command: after clone,
// source control is plain git, authorized server-side at receive time (see
// docs/git-spec.md).
func commandTree() map[string]*command {
	tree := map[string]*command{
		"instructions": {summary: "Role-specific instructions and CLI usage", run: runInstructions, newFlags: flaglessCommand("instructions")},
		"whoami":       {summary: "Report the calling agent's resolved identity", run: runWhoami, newFlags: func() *pflag.FlagSet { fs, _ := newWhoamiFlags(); return fs }},
		"clone":        {summary: "Clone an enrolled repo from the server", run: runClone, newFlags: flaglessCommand("clone")},
		"work": {summary: "Work branch operations", subcommands: map[string]*command{
			"start":          {summary: "Start a work branch", run: runWorkStart, newFlags: flaglessCommand("work start")},
			"set":            {summary: "Set a work branch's title/description", run: runWorkSet, newFlags: func() *pflag.FlagSet { fs, _ := newWorkSetFlags(); return fs }},
			"request-review": {summary: "Request review of a work branch", run: runWorkRequestReview, newFlags: flaglessCommand("work request-review")},
			"list":           {summary: "List work branches", run: runWorkList, newFlags: func() *pflag.FlagSet { fs, _ := newWorkListFlags(); return fs }},
			"show":           {summary: "Show a work branch's metadata", run: runWorkShow, newFlags: flaglessCommand("work show")},
			"diff":           {summary: "Show a work branch's diff", run: runWorkDiff, newFlags: func() *pflag.FlagSet { fs, _ := newWorkDiffFlags(); return fs }},
			"comments":       {summary: "Fetch comment threads or staged comments", run: runWorkComments, newFlags: func() *pflag.FlagSet { fs, _ := newWorkCommentsFlags(); return fs }},
			"verdicts":       {summary: "List verdicts on a work branch", run: runWorkVerdicts, newFlags: flaglessCommand("work verdicts")},
			"comment":        {summary: "Stage a review comment", run: runWorkComment, newFlags: func() *pflag.FlagSet { fs, _ := newWorkCommentFlags(); return fs }},
			"reply":          {summary: "Reply to a comment thread", run: runWorkReply, newFlags: func() *pflag.FlagSet { fs, _ := newWorkReplyFlags(); return fs }},
			"verdict":        {summary: "Publish staged comments as a verdict", run: runWorkVerdict, newFlags: func() *pflag.FlagSet { fs, _ := newWorkVerdictFlags(); return fs }},
		}},
		"graph": {summary: "Structural queries over the Tree-sitter graph", subcommands: map[string]*command{
			"def":        {summary: "Where a symbol is defined", run: runGraphDef, newFlags: graphQueryCommand("graph def")},
			"refs":       {summary: "Everywhere a symbol is referenced", run: runGraphRefs, newFlags: graphQueryCommand("graph refs")},
			"deps":       {summary: "What a target depends on", run: runGraphDeps, newFlags: graphQueryCommand("graph deps")},
			"dependents": {summary: "What depends on a target", run: runGraphDependents, newFlags: graphQueryCommand("graph dependents")},
			"history":    {summary: "A symbol's commit/ref history", run: runGraphHistory, newFlags: graphQueryCommand("graph history")},
		}},
		"search": {summary: "Natural-language semantic search", run: runSearch, newFlags: func() *pflag.FlagSet { fs, _, _, _ := newSearchFlags(); return fs }},
	}
	applySynopsis(tree)
	return tree
}

// applySynopsis fills in every leaf's synopsis, flags and stdinNote fields
// from cmdspec.Synopsis/cmdspec.Flags/cmdspec.StdinNote, keyed by the exact
// same name Dispatch and leafCommandKeys already use for that leaf -- a
// bare name at the top level, "<group> <sub>" nested under a group -- so
// there is no second hand-typed key anywhere in this package for that name
// to drift against.
func applySynopsis(tree map[string]*command) {
	for name, cmd := range tree {
		if cmd.subcommands == nil {
			cmd.synopsis = cmdspec.Synopsis[name]
			cmd.flags = cmdspec.Flags[name]
			cmd.stdinNote = cmdspec.StdinNote[name]
			continue
		}
		for sub, subcmd := range cmd.subcommands {
			full := name + " " + sub
			subcmd.synopsis = cmdspec.Synopsis[full]
			subcmd.flags = cmdspec.Flags[full]
			subcmd.stdinNote = cmdspec.StdinNote[full]
		}
	}
}

// flaglessCommand returns a command's newFlags constructor for a leaf that
// takes no flags beyond newFlagSet(name) itself -- every leaf whose real
// handler never registers anything beyond its own bare fs.
func flaglessCommand(name string) func() *pflag.FlagSet {
	return func() *pflag.FlagSet { return newFlagSet(name) }
}

// graphQueryCommand returns a command's newFlags constructor for one of the
// five `graph` subqueries, all of which share newGraphQueryFlags (see
// commands_graph.go).
func graphQueryCommand(name string) func() *pflag.FlagSet {
	return func() *pflag.FlagSet { fs, _, _, _, _ := newGraphQueryFlags(name); return fs }
}
