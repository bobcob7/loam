package meta

import (
	"slices"

	"github.com/bobcob7/loam/internal/cmdspec"
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
//
// synopsis is filled in by newCatalog (not written out by hand alongside
// name/summary below) from cmdspec.Synopsis, keyed by name -- the same map
// internal/cli/router.go reads its own copy from for `loam <command>
// --help` -- so the CLI's and the server's idea of a command's positional
// argument shape cannot drift apart (loam-hi5o.4).
type catalogEntry struct {
	name       string
	summary    string
	synopsis   string
	capability handler.Capability
}

// commandCatalog is the full CLI command surface (docs/cli-spec.md ->
// Commands), each paired with the single capability that gates it
// (docs/web-spec.md -> RoleService's operation descriptions), or left
// ungated for instructions/whoami. "git.push" has no entry of its own: it
// gates plain `git push` at the smart-HTTP transport, not a `loam`
// subcommand (docs/cli-spec.md -> clone: "there are no loam commit or loam
// push commands").
//
// One entry per DISPATCHABLE command -- matching internal/cli/router.go's
// commandTree() leaves exactly, name for name (loam-hi5o.4's cross-package
// drift test, internal/cli/synopsis_test.go, enforces this both
// directions). Graph's
// five subqueries (def/refs/deps/dependents/history) are listed
// separately here for that reason, even though docs/cli-spec.md -> Graph
// DB queries covers them in one prose section: `loam graph def` and `loam
// graph refs` are two different runnable commands with two different
// synopses, the same way `work start` and `work set` already were two
// catalog entries before this comment existed.
var commandCatalog = newCatalog([]catalogEntry{
	{name: "instructions", summary: "Return role-specific instructions, available commands, and general CLI usage.", capability: ""},
	{name: "whoami", summary: "Report the calling agent's identity and role, resolved locally from the environment.", capability: ""},
	{name: "clone", summary: "Clone an enrolled repo at a branch and bootstrap it for plain git.", capability: handler.CapabilityGitClone},
	{name: "work start", summary: "Start a work branch from a target branch.", capability: handler.CapabilityWorkStart},
	{name: "work set", summary: "Set or update a work branch's title and/or description (optional description read from stdin).", capability: handler.CapabilityWorkSet},
	{name: "work request-review", summary: "Request review of a work branch, opening a new review round.", capability: handler.CapabilityWorkRequestReview},
	{name: "work list", summary: "List work branches across all enrolled repos.", capability: handler.CapabilityWorkRead},
	{name: "work show", summary: "Return a work branch's metadata, title, description, and state.", capability: handler.CapabilityWorkRead},
	{name: "work diff", summary: "Return a work branch's diff against its target.", capability: handler.CapabilityWorkRead},
	{name: "work comments", summary: "Fetch a work branch's published comment threads, or the caller's staged comments.", capability: handler.CapabilityWorkRead},
	{name: "work verdicts", summary: "List the verdicts on a work branch, current and stale.", capability: handler.CapabilityWorkRead},
	{name: "work comment", summary: "Stage a review comment on a work branch locally, published later by verdict (body required on stdin unless --resolve or --discard alone).", capability: handler.CapabilityWorkVerdict},
	{name: "work reply", summary: "Reply immediately to an existing comment thread (body required on stdin).", capability: handler.CapabilityWorkReply},
	{name: "work verdict", summary: "Publish the caller's staged comments as a verdict with an outcome.", capability: handler.CapabilityWorkVerdict},
	{name: "graph def", summary: "Find where a symbol is defined in the Tree-sitter code graph.", capability: handler.CapabilityGraphQuery},
	{name: "graph refs", summary: "Find everywhere a symbol is referenced in the Tree-sitter code graph.", capability: handler.CapabilityGraphQuery},
	{name: "graph deps", summary: "Find what a target depends on in the Tree-sitter code graph.", capability: handler.CapabilityGraphQuery},
	{name: "graph dependents", summary: "Find what depends on a target in the Tree-sitter code graph (blast radius).", capability: handler.CapabilityGraphQuery},
	{name: "graph history", summary: "Return a symbol's commit/ref history in the Tree-sitter code graph.", capability: handler.CapabilityGraphQuery},
	{name: "search", summary: "Run a natural-language semantic search over ingested docs/code.", capability: handler.CapabilitySearch},
})

// newCatalog fills in each entry's synopsis from cmdspec.Synopsis and
// cmdspec.StdinNote, keyed by its own name and joined by cmdspec.Compose
// (the same composition rule internal/cli/synopsis_test.go's
// cross-package check uses to build the equivalent value from the CLI
// side) -- so the literal above only ever writes name, summary, and
// capability by hand, and the argument shape comes from the one shared
// source instead of a second hand-typed copy next to it.
func newCatalog(entries []catalogEntry) []catalogEntry {
	for i := range entries {
		entries[i].synopsis = cmdspec.Compose(cmdspec.Synopsis[entries[i].name], cmdspec.StdinNote[entries[i].name])
	}
	return entries
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
		commands = append(commands, &loamv1.CommandInfo{Name: entry.name, Summary: entry.summary, Synopsis: entry.synopsis})
	}
	return commands
}

// CatalogEntry is an exported view of one commandCatalog row: name,
// summary, synopsis, AND capability (handler.Capability, itself already
// exported -- an empty value means ungated, exactly as catalogEntry's own
// doc comment describes). Capability was originally left off this type as
// an "internal gating detail" a caller outside this package would have no
// use for; loam-hi5o.4's review round proved that wrong the hard way --
// cmd/server's acceptance suite needed exactly this pairing to predict
// which commands a role should see, hand-maintained it in a third copy
// (acceptanceCommandCapability), and that copy went stale the moment this
// file's graph entry split into five, breaking four scenarios no build-time
// check could have caught. Exposing Capability here lets that map be
// DISCOVERED instead of restated.
type CatalogEntry struct {
	Name       string
	Summary    string
	Synopsis   string
	Capability handler.Capability
}

// AllCommands returns the FULL command catalog, unfiltered by capability
// -- unlike filterCommands (used by GetInstructions itself), which always
// scopes to one caller's granted operations. This exists for
// introspection across the package boundary: internal/cli's cross-package
// synopsis-consistency test (loam-hi5o.4) discovers this package's command
// names and synopses independently of internal/cli/router.go's
// commandTree() and asserts the two agree for every command; cmd/server's
// acceptance suite (acceptance_roles_test.go's acceptanceCommandCapability)
// discovers the name-to-capability pairing from here too, rather than
// trusting a hand-maintained copy on either side to have been kept in
// sync.
func AllCommands() []CatalogEntry {
	entries := make([]CatalogEntry, 0, len(commandCatalog))
	for _, entry := range commandCatalog {
		entries = append(entries, CatalogEntry{Name: entry.name, Summary: entry.summary, Synopsis: entry.synopsis, Capability: entry.capability})
	}
	return entries
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
