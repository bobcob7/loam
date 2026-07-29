package meta

import (
	"slices"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/handler"
)

// usageText is the static CLI usage guide GetInstructions returns
// verbatim in every response's Usage field (docs/cli-spec.md ->
// instructions: "a static usage guide built into the binary (overall
// usage + the conventions above)"), merged with the caller's
// role-specific instructions and filtered command list.
const usageText = `Loam CLI: agents orient with "instructions" (this command) and "whoami" ` +
	`(local identity only, no server call), then drive everything else through the ` +
	`commands below. Commands identifying a work branch take <repo> and <work-branch> ` +
	`positional arguments; both are inferred when run from inside a clone. Output is ` +
	`JSON by default (LOAM_OUTPUT_FORMAT selects yaml/xml/human). Exit codes: 0 success, ` +
	`1 unexpected internal error, 2 usage/authorization/conflict/precondition failure, ` +
	`3 not found. After "clone", source control is plain git -- there are no "loam ` +
	`commit"/"loam push" commands; the server authorizes each push at receive time.`

// catalogEntry is one CLI command in the fixed catalog below. A zero-value
// Capability marks a command as ungated -- always included regardless of
// the caller's granted operations, matching docs/web-spec.md: "instructions
// and whoami are always available and ungated".
type catalogEntry struct {
	name       string
	summary    string
	capability handler.Capability
}

// commandCatalog is the full CLI command surface (docs/cli-spec.md ->
// Commands), each paired with the single capability that gates it
// (docs/web-spec.md -> RoleService's operation descriptions), or left
// ungated for instructions/whoami. "git.push" has no entry of its own: it
// gates plain `git push` at the smart-HTTP transport, not a `loam`
// subcommand (docs/cli-spec.md -> clone: "there are no loam commit or loam
// push commands").
var commandCatalog = []catalogEntry{
	{name: "instructions", summary: "Return role-specific instructions, available commands, and general CLI usage.", capability: ""},
	{name: "whoami", summary: "Report the calling agent's identity and role, resolved locally from the environment.", capability: ""},
	{name: "clone", summary: "Clone an enrolled repo at a branch and bootstrap it for plain git.", capability: handler.CapabilityGitClone},
	{name: "work start", summary: "Start a work branch from a target branch.", capability: handler.CapabilityWorkStart},
	{name: "work set", summary: "Set or update a work branch's title and/or description.", capability: handler.CapabilityWorkSet},
	{name: "work request-review", summary: "Request review of a work branch, opening a new review round.", capability: handler.CapabilityWorkRequestReview},
	{name: "work list", summary: "List work branches across all enrolled repos.", capability: handler.CapabilityWorkRead},
	{name: "work show", summary: "Return a work branch's metadata, title, description, and state.", capability: handler.CapabilityWorkRead},
	{name: "work diff", summary: "Return a work branch's diff against its target.", capability: handler.CapabilityWorkRead},
	{name: "work comments", summary: "Fetch a work branch's published comment threads, or the caller's staged comments.", capability: handler.CapabilityWorkRead},
	{name: "work verdicts", summary: "List the verdicts on a work branch, current and stale.", capability: handler.CapabilityWorkRead},
	{name: "work comment", summary: "Stage a review comment on a work branch locally, published later by verdict.", capability: handler.CapabilityWorkVerdict},
	{name: "work reply", summary: "Reply immediately to an existing comment thread.", capability: handler.CapabilityWorkReply},
	{name: "work verdict", summary: "Publish the caller's staged comments as a verdict with an outcome.", capability: handler.CapabilityWorkVerdict},
	{name: "graph", summary: "Run a structural query over the Tree-sitter code graph.", capability: handler.CapabilityGraphQuery},
	{name: "search", summary: "Run a natural-language semantic search over ingested docs/code.", capability: handler.CapabilitySearch},
}

// filterCommands returns the commandCatalog entries the caller may use:
// every ungated entry, plus every entry whose capability appears in
// granted. Order matches commandCatalog's declaration order.
func filterCommands(granted []handler.Capability) []*loamv1.CommandInfo {
	commands := make([]*loamv1.CommandInfo, 0, len(commandCatalog))
	for _, entry := range commandCatalog {
		if entry.capability != "" && !slices.Contains(granted, entry.capability) {
			continue
		}
		commands = append(commands, &loamv1.CommandInfo{Name: entry.name, Summary: entry.summary})
	}
	return commands
}

// findCommand looks up name within commands (the caller's already-filtered
// list, never the full catalog) so a command hidden by the capability
// filter is indistinguishable from one that does not exist at all --
// neither leaks which gated commands exist to a caller whose role cannot
// use them.
func findCommand(commands []*loamv1.CommandInfo, name string) (*loamv1.CommandInfo, bool) {
	for _, command := range commands {
		if command.GetName() == name {
			return command, true
		}
	}
	return nil, false
}
