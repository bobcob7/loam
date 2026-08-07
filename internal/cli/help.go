// Help resolution for `loam help`, `loam --help`/`-h`, and `loam <command>
// [<subcommand>] --help`/`-h` (docs/cli-spec.md -> Help). See loam-dc2v and
// loam-q0ek: help must never require any LOAM_* configuration, and it must
// never leak pflag's own internal ErrHelp sentinel text.
//
// TryHelp is deliberately independent of Deps and of the Router: it reads
// only argv and commandTree(), which is why cmd/loam/main.go calls it
// BEFORE cli.NewProductionDeps -- the seam loam-q0ek's own notes identify
// as the actual obstacle ("control never reaches Dispatch at all" on an
// unconfigured machine). Every leaf command's flags are available with no
// Deps through its `newFlags` constructor (router.go's command.newFlags),
// which is what lets `loam work start --help` render real flag usage
// without ever calling runWorkStart or building a Connect client.
package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/pflag"
)

// quoteIfMulti wraps name in double quotes if it contains a space (a group
// subcommand's full dispatch name, e.g. "work start"), and returns it
// unchanged otherwise. Every place this package tells an agent to run
// `loam instructions <name>` needs this: internal/handler/meta/catalog.go
// (and runInstructions, commands_root.go) name a group subcommand with a
// literal space, and instructions itself (commands_root.go's
// runInstructions) rejects more than one positional argument -- so an
// unquoted "work start" printed into a suggested command line would parse
// as two positionals and fail exactly the way loam-hi5o.4 found it did.
//
// fmt.Sprintf("%q", ...) is GO quoting, not SHELL quoting -- they coincide
// for every name in cmdspec today (plain ASCII words joined by single
// spaces), and were verified to coincide in a real shell, not just against
// help_synopsis_test.go's own tokenizer (which models what this function
// produces, not shell semantics in general). If a command name ever
// contained a character a shell treats specially inside double quotes --
// `$`, a backtick, a backslash -- %q would leave it unescaped and a real
// shell would expand it where Go's quoting rules would not. Every name
// here is a literal written by this codebase, never user input, so that
// day is a `commandTree()`/cmdspec edit away rather than an attacker-
// controlled one, but it is not automatically caught if it happens.
func quoteIfMulti(name string) string {
	if strings.Contains(name, " ") {
		return fmt.Sprintf("%q", name)
	}
	return name
}

// isHelpToken reports whether tok is one of the exact spellings
// pflag.FlagSet.Parse itself hardcodes as a help request even for a
// "help"/"h" flag no command defines (github.com/spf13/pflag's
// parseLongArg/parseSingleShortArg: an unrecognized long flag named
// "help", or an unrecognized short flag "h", both short-circuit to
// pflag.ErrHelp). No command in this package registers a "-h" shorthand or
// a "--help" long flag of its own, so matching these tokens here can never
// collide with a real one.
func isHelpToken(tok string) bool {
	return tok == "-h" || tok == "--help" || strings.HasPrefix(tok, "--help=")
}

// containsHelpToken reports whether any token in args is a help token (see
// isHelpToken) -- flags may appear anywhere among positionals
// (docs/cli-spec.md -> Argument Ordering), so a leaf command's `--help`
// is not necessarily args[0].
func containsHelpToken(args []string) bool {
	for _, a := range args {
		if isHelpToken(a) {
			return true
		}
	}
	return false
}

// TryHelp recognizes every documented help route from args (normally
// os.Args[1:]) alone, with no LOAM_* configuration read and no Deps built.
// ok is false for anything that is not a help route -- including an empty
// args, an unrecognized command name, or a group/subcommand name that does
// not exist -- in which case the caller should fall through to its normal
// dispatch (main.go: cli.NewProductionDeps + Router.Dispatch), unchanged.
func TryHelp(args []string) (text string, ok bool) {
	if len(args) == 0 {
		return "", false
	}
	tree := commandTree()
	if args[0] == "help" || isHelpToken(args[0]) {
		return renderTopLevelHelp(tree), true
	}
	cmd, known := tree[args[0]]
	if !known {
		return "", false
	}
	if cmd.subcommands != nil {
		return tryGroupHelp(args[0], cmd, args[1:])
	}
	return tryLeafHelp(args[0], cmd, args[1:])
}

// tryGroupHelp handles a group command (e.g. "work", "graph"): `loam work
// --help`/`-h`/`help` lists its subcommands; `loam work <sub> ...`
// recurses into that subcommand's own leaf help. A bare `loam work` with
// no further args and no help token is left alone (ok=false) -- that stays
// Dispatch's existing "work requires a subcommand" usage error, not an
// implicit help listing.
func tryGroupHelp(name string, group *command, rest []string) (string, bool) {
	if len(rest) > 0 && (rest[0] == "help" || isHelpToken(rest[0])) {
		return renderGroupHelp(name, group), true
	}
	if len(rest) == 0 {
		return "", false
	}
	sub, known := group.subcommands[rest[0]]
	if !known {
		return "", false
	}
	return tryLeafHelp(name+" "+rest[0], sub, rest[1:])
}

// leafUsageLine renders a leaf's "Usage: loam <name> ..." line, in
// exactly the order docs/cli-spec.md's own synopsis lines use (e.g. line
// 351: "`loam work set [repo] [work-branch] [--title <title>]` (optional
// description read from stdin)"): the command's real positional synopsis
// (from cmd.synopsis, see router.go's applySynopsis), then its flag shape
// if and only if fs actually registers at least one flag (loam-hi5o.4
// acceptance criterion 2 -- a flagless command like `work start` must
// never claim to take flags), then -- LAST, after flags, never before --
// a parenthetical stdin note (cmd.stdinNote) for the few commands that
// read one. Putting the note before the flags (an earlier version of this
// function did) made the printed line read as if the note were still
// positional arguments and left the flag token trailing prose, which is
// exactly the kind of un-copyable usage line loam-hi5o.4 exists to
// eliminate -- so the ordering here is load-bearing, not cosmetic.
//
// Since loam-hwru the flag shape is the command's REAL one (cmd.flags,
// from cmdspec.Flags) rather than a bare "[flags]" token. The token was
// not wrong, but it left the usage line un-copyable for precisely the
// commands worth copying, and it is the same text `loam instructions` now
// serves -- from the same map, kept honest against this FlagSet by
// TestFlags_CmdspecMatchesEveryRealFlagSet. fs still decides WHETHER any
// flag shape is printed, so a leaf registering no flags cannot claim one
// even if the map grew a stray entry.
func leafUsageLine(name, synopsis, flags, stdinNote string, fs *pflag.FlagSet) string {
	usage := "Usage: loam " + name
	if synopsis != "" {
		usage += " " + synopsis
	}
	if fs.HasFlags() {
		usage += " " + flagShape(flags)
	}
	if stdinNote != "" {
		usage += " (" + stdinNote + ")"
	}
	return usage
}

// flagShape returns the spelled-out flag shape, falling back to the
// generic "[flags]" token when cmdspec carries no entry. The fallback is
// unreachable in this binary -- the drift test requires an entry for every
// leaf whose FlagSet has flags -- and exists so that a leaf added without
// its cmdspec entry still prints a usage line that is merely vague rather
// than one that silently omits the flags it accepts.
func flagShape(flags string) string {
	if flags == "" {
		return "[flags]"
	}
	return flags
}

// tryLeafHelp handles one leaf command: help only when a help token
// appears among its remaining args, rendered from its own newFlags
// constructor (see router.go's command.newFlags) -- never from running the
// command's real handler.
func tryLeafHelp(name string, cmd *command, rest []string) (string, bool) {
	if !containsHelpToken(rest) {
		return "", false
	}
	return renderLeafHelp(name, cmd.summary, cmd.synopsis, cmd.flags, cmd.stdinNote, cmd.newFlags()), true
}

// renderTopLevelHelp lists every top-level command (and, for a group, its
// subcommands), from commandTree()'s own summaries -- no separate catalog
// to keep in sync (see router.go: "commandTree() already carries a
// per-command summary"). Unlike `loam instructions`, this listing is NOT
// filtered to the caller's role: it cannot be, since it runs with no
// server call and no identity yet resolved. `loam instructions` is named
// explicitly as the authority on which of these a given role may actually
// use.
func renderTopLevelHelp(tree map[string]*command) string {
	names := sortedKeys(tree)
	var b strings.Builder
	b.WriteString("loam -- agent-facing CLI for Loam work branches, review, and code intelligence.\n\n")
	b.WriteString("Usage: loam <command> [<subcommand>] [flags] [args]\n\n")
	b.WriteString("Commands:\n")
	for _, name := range names {
		cmd := tree[name]
		fmt.Fprintf(&b, "  %-14s %s\n", name, cmd.summary)
		for _, sub := range sortedKeys(cmd.subcommands) {
			fmt.Fprintf(&b, "    %-12s %s\n", sub, cmd.subcommands[sub].summary)
		}
	}
	b.WriteString("\nRun `loam <command> --help` (or `loam <command> <subcommand> --help`) for a command's flags.\n")
	b.WriteString("Run `loam instructions` for the commands your configured role may actually use.\n")
	return b.String()
}

// renderGroupHelp lists one group's subcommands (e.g. `loam work --help`).
func renderGroupHelp(name string, group *command) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Usage: loam %s <subcommand> [flags] [args]\n\n", name)
	b.WriteString(group.summary + "\n\nSubcommands:\n")
	for _, sub := range sortedKeys(group.subcommands) {
		fmt.Fprintf(&b, "  %-14s %s\n", sub, group.subcommands[sub].summary)
	}
	fmt.Fprintf(&b, "\nRun `loam %s <subcommand> --help` for a subcommand's flags.\n", name)
	return b.String()
}

// renderLeafHelp renders one leaf command's usage: its real positional
// synopsis (and stdin note, where one applies), its pflag-registered flags
// (if any), and a pointer to the authoritative source for the rest of its
// argument shape.
//
// The positional synopsis (e.g. "work set [repo] [work-branch]") and the
// stdin note come from cmdspec.Synopsis/cmdspec.StdinNote (via router.go's
// applySynopsis), the same maps internal/handler/meta/catalog.go reads its
// own copy from (via cmdspec.Compose) -- so the two cannot drift the way
// loam-hi5o.4 found a hardcoded "[flags]" already had (a command with zero
// flags claiming it took some). Flag usage itself is still rendered
// straight from fs, the same FlagSet the real handler parses with, for the
// same non-drift reason as before this changed.
func renderLeafHelp(name, summary, synopsis, flags, stdinNote string, fs *pflag.FlagSet) string {
	var b strings.Builder
	b.WriteString(leafUsageLine(name, synopsis, flags, stdinNote, fs) + "\n\n")
	b.WriteString(summary + "\n")
	if usage := fs.FlagUsagesWrapped(0); usage != "" {
		b.WriteString("\nFlags:\n" + usage)
	}
	fmt.Fprintf(&b, "\nSee docs/cli-spec.md, or run `loam instructions %s`, for this command's full argument shape.\n", quoteIfMulti(name))
	return b.String()
}

// sortedKeys returns m's keys in sorted order, for deterministic help
// output. Safe on a nil map (ranges zero times), so a leaf's absent
// subcommands need no separate nil check at each call site.
func sortedKeys(m map[string]*command) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
