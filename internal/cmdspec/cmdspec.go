// Package cmdspec is the single source of truth for one thing only: the
// argument shape of each `loam` command that lives outside its flags --
// positional arguments (docs/cli-spec.md's <...>/[...] convention) and,
// for the few commands that read a body from stdin, that fact. Two
// packages need this text and cannot import each other -- internal/cli
// renders it in `loam <command> --help` (help.go), and
// internal/handler/meta serves it in `loam instructions <command>`'s
// response (catalog.go) -- so it lives here instead, in a package with no
// dependency on either, to keep the two answers from drifting apart the
// way loam-hi5o.4 found them already had (a hardcoded "[flags]" in the
// CLI, and a catalog entry with no argument shape at all).
//
// This package used to exclude flag usage, on the stated grounds that
// "flags already have their own non-driftable source on each side (the
// CLI's pflag.FlagSet)". That premise held on ONE side. internal/cli does
// render real flags from the real FlagSet -- but internal/handler/meta has
// no pflag.FlagSet and no access to one, so `loam instructions <command>`
// carried no flag information at all, for any command. Since
// `instructions` is the surface an agent orients from (it is named first
// in the usage text this same package's consumer serves), a flag that
// exists only in `--help` is a flag an agent working from `instructions`
// never learns about.
//
// loam-hwru is the case that made that concrete: `loam work diff --stat`
// was added precisely because agents kept reaching for it, and an agent
// reading `instructions` would still have been told the command's shape is
// "[repo] [work-branch]" and gone back to piping 100KB of escaped JSON
// through jq. A fix for a discoverability bug that is itself undiscoverable
// is not a fix.
//
// So Flags below carries the flag SHAPE, and the duplication this package
// exists to prevent is closed the only way it can be across a boundary
// these two packages must not import each other over: by a test.
// internal/cli's TestFlags_CmdspecMatchesEveryRealFlagSet compares every
// entry against the flag names the real newFlags constructor registers, and
// fails on any disagreement in either direction -- an undocumented flag, a
// documented flag that does not exist, or an entry for a command with no
// flags at all.
package cmdspec

import "strings"

// Synopsis maps a command's full dispatch name -- a bare name for a
// top-level command ("clone"), or "<group> <subcommand>" for one nested
// under a group ("work start"), matching both internal/cli's commandTree()
// keys and internal/handler/meta's catalog names exactly -- to its
// positional-argument shape ONLY. An empty string means the command takes
// no positional arguments at all (e.g. "work list", which is flags-only).
//
// This deliberately holds NOTHING but the positional shape -- no flags
// (see the package doc comment) and no stdin note (see StdinNote below,
// kept separate on purpose): a renderer that needs to place a "[flags]"
// token between the positional shape and a trailing stdin note (as
// internal/cli/help.go's leafUsageLine does, matching docs/cli-spec.md's
// own ordering -- positional, then flags, then the parenthetical last)
// needs these as independent pieces, not one pre-joined string.
var Synopsis = map[string]string{
	"instructions": "[command]",
	"whoami":       "",
	"clone":        "<repo> <branch>",

	"work start":          "<repo> <from>",
	"work set":            "[repo] [work-branch]",
	"work request-review": "[repo] [work-branch]",
	"work list":           "",
	"work show":           "[repo] [work-branch]",
	"work diff":           "[repo] [work-branch]",
	"work comments":       "[repo] [work-branch]",
	"work verdicts":       "[repo] [work-branch]",
	"work comment":        "[repo] [work-branch]",
	"work reply":          "[repo] [work-branch]",
	"work verdict":        "[repo] [work-branch]",

	"graph def":        "<target>",
	"graph refs":       "<target>",
	"graph deps":       "<target>",
	"graph dependents": "<target>",
	"graph history":    "<target>",

	"search": "<query>",
}

// StdinNote maps a command's full dispatch name (same keying as Synopsis)
// to a short description of what it reads from stdin, for the handful of
// commands that do (checked against docs/cli-spec.md, not assumed: "work
// set" -- optional description, "work comment" -- required body unless
// --resolve/--discard alone, "work reply" -- required body; no other
// command reads stdin). Absent from this map (the zero value, "") means
// the command does not read stdin at all -- most commands, so this map is
// intentionally sparse rather than carrying an empty-string entry for
// every command the way Synopsis does.
//
// Stored as plain text, with no surrounding parentheses -- Compose (below)
// is the one place that adds them, so both renderers format this the same
// way rather than each hand-punctuating its own copy.
var StdinNote = map[string]string{
	"work set":     "description optional on stdin",
	"work comment": "body on stdin, required unless --resolve or --discard alone",
	"work reply":   "body required on stdin",
}

// Flags maps a command's full dispatch name (same keying as Synopsis) to
// its FLAG shape, spelled the way docs/cli-spec.md's own synopsis lines
// spell it ("[--title <title>]"). Absent from this map (the zero value,
// "") means the command registers no flags of its own -- so, like
// StdinNote and unlike Synopsis, this map is intentionally sparse.
//
// See the package doc comment for why flags live here at all, and for the
// test that keeps every entry honest against the real pflag.FlagSet. That
// test is not a nicety: this map is a hand-written second copy of
// information the CLI already holds, which is precisely the shape this
// package exists to eliminate, and the only thing that makes it acceptable
// is that a disagreement fails the build.
// Every entry below was CORRECTED by that test on first run rather than
// written correctly by hand -- it caught a missing `--edit` on "work
// comment" and three missing flags on each of the five graph subcommands,
// in the same commit that introduced the map. That is the argument for the
// test in one line: this map is exactly as trustworthy as the check behind
// it, and unchecked it would have shipped wrong on the day it was added.
var Flags = map[string]string{
	"whoami": "[--verify]",

	"work set":      "[--title <title>]",
	"work list":     "[--repo <repo>] [--author <id>] [--target <branch>] [--awaiting-review] [--state <state>] [--limit <n>]",
	"work diff":     "[--format <patch|stat>] [--stat] [--allow-unpushed]",
	"work comments": "[--staged]",
	"work comment":  "[--file <path>] [--line <n>] [--resolve <thread-id>] [--edit <staged-id>] [--discard <staged-id>] [--list]",
	// --thread and --outcome are NOT bracketed: both are required, and both
	// exit 2 with a usage error when omitted (commands_work_reply.go:55,
	// commands_work_verdict.go:106). docs/cli-spec.md spells them the same
	// way and marks them *(required)*. They were bracketed here when this
	// map was first written, which would have told an agent the commands
	// run without them. See TestFlags_CmdspecMatchesEveryRealFlagSet's doc
	// comment for why the drift test cannot catch this class.
	"work reply":   "--thread <thread-id>",
	"work verdict": "--outcome <approve|disapprove|neutral>",

	"graph def":        "[--repo <repo>] [--file <path>] [--limit <n>] [--all]",
	"graph refs":       "[--repo <repo>] [--file <path>] [--limit <n>] [--all]",
	"graph deps":       "[--repo <repo>] [--file <path>] [--limit <n>] [--all]",
	"graph dependents": "[--repo <repo>] [--file <path>] [--limit <n>] [--all]",
	"graph history":    "[--repo <repo>] [--file <path>] [--limit <n>] [--all]",

	"search": "[--repo <repo>] [--limit <n>] [--all]",
}

// Compose builds the single-string synopsis `loam instructions <command>`
// shows (internal/handler/meta/catalog.go's CommandInfo.Synopsis): the
// positional shape, then the flag shape, then -- when the command also
// reads from stdin -- a trailing parenthetical, in that order, the same
// order docs/cli-spec.md's own synopsis lines use (e.g. "`loam work set
// [repo] [work-branch] [--title <title>]` (optional description read from
// stdin)" -- the parenthetical trails the WHOLE synopsis, flags included,
// never sitting ahead of them). internal/cli/router.go's applySynopsis
// keeps the three as separate command fields rather than pre-composing
// them, so help.go's leafUsageLine can render them itself; this function
// is what the cross-package consistency test
// (internal/cli/synopsis_test.go) uses to build the equivalent value from
// those CLI-side fields before comparing it against the catalog's
// already-composed one, so both sides go through the identical formatting
// rule instead of hand-written concatenations that could diverge.
func Compose(synopsis, flags, stdinNote string) string {
	parts := make([]string, 0, 3)
	for _, part := range []string{synopsis, flags} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if stdinNote != "" {
		parts = append(parts, "("+stdinNote+")")
	}
	return strings.Join(parts, " ")
}
