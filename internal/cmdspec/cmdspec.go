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
// This deliberately excludes flag usage: flags already have their own
// non-driftable source on each side (the CLI's pflag.FlagSet, built by
// the same newFlags constructor its real handler parses with), so
// duplicating them here would recreate exactly the problem this package
// exists to avoid.
package cmdspec

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

// Compose builds the single-string synopsis `loam instructions <command>`
// shows (internal/handler/meta/catalog.go's CommandInfo.Synopsis): the
// positional shape, then -- when the command also reads from stdin -- a
// trailing parenthetical, in that order, the same order docs/cli-spec.md's
// own synopsis lines use (e.g. "`loam work set [repo] [work-branch]
// [--title <title>]` (optional description read from stdin)" -- the
// parenthetical trails the WHOLE synopsis, flags included, never sitting
// ahead of them). internal/cli/router.go's applySynopsis keeps synopsis
// and stdinNote as separate command fields rather than pre-composing them,
// specifically so help.go's leafUsageLine can insert "[flags]" between the
// two; this function is what the cross-package consistency test
// (internal/cli/synopsis_test.go) uses to build the equivalent value from
// those two CLI-side fields before comparing it against the catalog's
// already-composed one, so both sides go through the identical formatting
// rule instead of two hand-written concatenations that could diverge.
func Compose(synopsis, stdinNote string) string {
	if stdinNote == "" {
		return synopsis
	}
	if synopsis == "" {
		return "(" + stdinNote + ")"
	}
	return synopsis + " (" + stdinNote + ")"
}
