// Package cmdspec is the single source of truth for one thing only: the
// positional-argument shape of each `loam` command (docs/cli-spec.md's
// <...>/[...] convention). Two packages need this text and cannot import
// each other -- internal/cli renders it in `loam <command> --help`
// (help.go), and internal/handler/meta serves it in `loam instructions
// <command>`'s response (catalog.go) -- so it lives here instead, in a
// package with no dependency on either, to keep the two answers from
// drifting apart the way loam-hi5o.4 found them already had (a hardcoded
// "[flags]" in the CLI, and a catalog entry with no argument shape at
// all).
//
// This deliberately excludes flag usage, but not stdin (see Synopsis'
// own doc comment for why stdin stays): flags already have their own
// non-driftable source on each side (the CLI's pflag.FlagSet, built by
// the same newFlags constructor its real handler parses with), so
// duplicating them here would recreate exactly the problem this package
// exists to avoid.
package cmdspec

// Synopsis maps a command's full dispatch name -- a bare name for a
// top-level command ("clone"), or "<group> <subcommand>" for one nested
// under a group ("work start"), matching both internal/cli's commandTree()
// keys and internal/handler/meta's catalog names exactly -- to its
// positional-argument shape. An empty string means the command takes no
// positional arguments at all (e.g. "work list", which is flags-only).
//
// A command that also reads its body/description from stdin (per
// docs/cli-spec.md: "work set", "work comment", "work reply" -- checked
// against the spec, not assumed; no other command reads stdin) carries
// that as a trailing parenthetical, the same way docs/cli-spec.md's own
// synopsis lines do. It belongs here, in the one positional-shape string,
// rather than as a second parallel map: stdin is part of the command's
// calling convention exactly like a positional argument is (an agent must
// know to supply it, and neither pflag's FlagSet nor a bare positional
// list says so), and a second map would just be one more place for this
// text to drift from the first.
var Synopsis = map[string]string{
	"instructions": "[command]",
	"whoami":       "",
	"clone":        "<repo> <branch>",

	"work start":          "<repo> <from>",
	"work set":            "[repo] [work-branch] (description optional on stdin)",
	"work request-review": "[repo] [work-branch]",
	"work list":           "",
	"work show":           "[repo] [work-branch]",
	"work diff":           "[repo] [work-branch]",
	"work comments":       "[repo] [work-branch]",
	"work verdicts":       "[repo] [work-branch]",
	"work comment":        "[repo] [work-branch] (body on stdin, required unless --resolve or --discard alone)",
	"work reply":          "[repo] [work-branch] (body required on stdin)",
	"work verdict":        "[repo] [work-branch]",

	"graph def":        "<target>",
	"graph refs":       "<target>",
	"graph deps":       "<target>",
	"graph dependents": "<target>",
	"graph history":    "<target>",

	"search": "<query>",
}
