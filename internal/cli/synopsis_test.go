package cli

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/cmdspec"
	"github.com/bobcob7/loam/internal/handler/meta"
)

// synopsisByName walks tree (mirroring router_test.go's leafCommandKeys)
// and returns every dispatchable leaf's full name mapped to its FULL
// composed synopsis: cmd.synopsis, cmd.flags and cmd.stdinNote (router.go's
// applySynopsis) joined by cmdspec.Compose, the exact same rule
// internal/handler/meta/catalog.go's newCatalog uses to build the value it
// puts in the catalog. Composing here, rather than comparing cmd.synopsis
// alone, is what makes the comparison in
// TestSynopsis_CommandTreeMatchesMetaCatalog apples-to-apples for the
// commands that also take flags or read stdin (cmd.synopsis on its own is
// positional-only; the catalog's entry.synopsis is always the composed
// form, since the catalog has nowhere else to carry either).
func synopsisByName(tree map[string]*command) map[string]string {
	out := make(map[string]string)
	for name, cmd := range tree {
		if cmd.subcommands == nil {
			out[name] = cmdspec.Compose(cmd.synopsis, cmd.flags, cmd.stdinNote)
			continue
		}
		for sub, subcmd := range cmd.subcommands {
			out[name+" "+sub] = cmdspec.Compose(subcmd.synopsis, subcmd.flags, subcmd.stdinNote)
		}
	}
	return out
}

// flagNamesIn extracts the long flag names ("--stat" -> "stat") from a
// cmdspec.Flags value. It is deliberately a dumb scan for "--" prefixes
// rather than a parser for the whole "[--x <y>]" grammar: what needs
// checking is the NAME SET, and a scan that understood the grammar could
// only agree or disagree with the grammar the map was hand-written in --
// which is the thing under test.
func flagNamesIn(shape string) map[string]bool {
	names := make(map[string]bool)
	for _, tok := range strings.FieldsFunc(shape, func(r rune) bool { return r == ' ' || r == '[' || r == ']' || r == '<' || r == '>' }) {
		if after, ok := strings.CutPrefix(tok, "--"); ok && after != "" {
			names[after] = true
		}
	}
	return names
}

// flagNamesRegisteredBy returns the long flag names a leaf's real
// pflag.FlagSet registers -- the same constructor its real handler parses
// with (router.go's command.newFlags).
func flagNamesRegisteredBy(fs *pflag.FlagSet) map[string]bool {
	names := make(map[string]bool)
	fs.VisitAll(func(f *pflag.Flag) { names[f.Name] = true })
	return names
}

// TestFlags_CmdspecMatchesEveryRealFlagSet is what makes cmdspec.Flags
// acceptable at all.
//
// cmdspec's whole reason for existing is that a hand-written second copy of
// a command's shape drifts from the real one, and Flags IS a hand-written
// second copy -- of information the pflag.FlagSet already holds exactly.
// The package could not simply read the FlagSet, because
// internal/handler/meta (which serves `loam instructions`, the surface an
// agent orients from) has no FlagSet and must not import internal/cli. So
// the duplication is closed here instead, by failing the build on any
// disagreement in either direction:
//
//   - a flag the CLI registers that cmdspec does not document -- which is
//     the loam-hwru failure exactly: `--stat` existed in --help and was
//     invisible to every agent reading `instructions`;
//   - a flag cmdspec documents that the CLI does not register, i.e. a
//     command an agent would be told to run and that would exit 2;
//   - an entry for a leaf with no flags at all, or a missing entry for a
//     leaf that has them.
//
// ONLY NAMES ARE COMPARED, and a reader who trusts this test needs to know
// what that leaves uncovered, because the gap is not obvious and has
// already bitten once.
//
// Not covered: whether a flag is REQUIRED or OPTIONAL. Bracketing is a
// convention in the map's text; pflag has no notion of it, since a required
// flag is enforced by the handler's own check after parsing (e.g.
// commands_work_reply.go's `if *thread == ""`), never by the FlagSet. So
// `[--thread <thread-id>]` and `--thread <thread-id>` are indistinguishable
// here, and both were shipped bracketed-but-required in this map's first
// version -- telling an agent the command runs without them, when it exits
// 2. The check for that is docs/cli-spec.md, read by a human.
//
// Also not covered: the value spelling ("<patch|stat>"), which is prose
// aimed at a reader and has no machine-checkable counterpart, and default
// values.
//
// What IS covered is the name set -- the part an agent copies into a
// command line, and the part whose being wrong costs a round-trip.
func TestFlags_CmdspecMatchesEveryRealFlagSet(t *testing.T) {
	t.Parallel()
	tree := commandTree()
	leaves := leafCommandKeys(tree)
	for name := range cmdspec.Flags {
		assert.True(t, leaves[name], "internal/cmdspec.Flags names %q, which is not a dispatchable leaf in commandTree()", name)
	}
	forEachLeaf(tree, func(name string, cmd *command) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			registered := flagNamesRegisteredBy(cmd.newFlags())
			documented := flagNamesIn(cmdspec.Flags[name])
			if len(registered) == 0 {
				assert.Empty(t, cmdspec.Flags[name], "%q registers no flags, so internal/cmdspec.Flags must have no entry for it", name)
				return
			}
			require.NotEmpty(t, cmdspec.Flags[name], "%q registers flags (%v) but internal/cmdspec.Flags has no entry, so `loam instructions %q` would not mention them", name, sortedFlagNames(registered), name)
			assert.Equal(t, sortedFlagNames(registered), sortedFlagNames(documented), "the flags %q registers and the flags internal/cmdspec.Flags documents for it disagree", name)
		})
	})
}

// forEachLeaf calls fn for every dispatchable leaf in tree, keyed the way
// Dispatch keys it.
func forEachLeaf(tree map[string]*command, fn func(name string, cmd *command)) {
	for name, cmd := range tree {
		if cmd.subcommands == nil {
			fn(name, cmd)
			continue
		}
		for sub, subcmd := range cmd.subcommands {
			fn(name+" "+sub, subcmd)
		}
	}
}

// sortedFlagNames renders a name set as a sorted slice, so a failure
// reports which flags differ rather than that two maps are unequal.
func sortedFlagNames(names map[string]bool) []string {
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestSynopsis_CommandTreeMatchesMetaCatalog is loam-hi5o.4 acceptance
// criterion 5: a test that fails if commandTree()'s synopsis and
// internal/handler/meta's catalog synopsis disagree for any command.
//
// What this test actually catches, checked by mutation during review
// (loam-hi5o.4's first review round) rather than assumed: NAME-SET
// disagreement in either direction -- a command commandTree() dispatches
// but the catalog does not list (or vice versa) -- and a wrong SYNOPSIS
// VALUE reached by a path that skips cmdspec (e.g. router.go's
// applySynopsis assigning some other string after reading cmdspec.Synopsis
// correctly). It does NOT catch a literal string written directly into a
// commandCatalog entry's synopsis field in catalog.go, because newCatalog
// overwrites that field unconditionally from cmdspec on every call -- so
// that particular bypass is structurally impossible, not merely
// undetected, and needs no test. The name-set check is the one that would
// have caught this bead's own review-round regression: cmd/server's
// acceptance suite still mapped "graph" to a capability after this file's
// catalog split "graph" into five commands, and the equivalent
// name-presence question is exactly what this test asks of commandTree()
// and the catalog directly.
//
// Both sides discover their command sets independently -- leafCommandKeys
// walks commandTree() itself; meta.AllCommands() walks meta's own catalog
// var -- rather than iterating a hand-written list of names, which is
// exactly the failure mode this project has already been bitten by once
// (router_test.go's commandImplementationProofs doc comment, and now this
// bead's own review round one package over in cmd/server).
func TestSynopsis_CommandTreeMatchesMetaCatalog(t *testing.T) {
	t.Parallel()
	cliSynopsis := synopsisByName(commandTree())
	catalog := meta.AllCommands()
	seen := make(map[string]bool, len(catalog))
	for _, entry := range catalog {
		seen[entry.Name] = true
		want, ok := cliSynopsis[entry.Name]
		require.True(t, ok, "meta catalog names %q, which is not a dispatchable leaf in internal/cli's commandTree()", entry.Name)
		assert.Equal(t, want, entry.Synopsis, "synopsis for %q disagrees between commandTree() (internal/cli) and the meta catalog (internal/handler/meta)", entry.Name)
	}
	for name := range cliSynopsis {
		assert.True(t, seen[name], "commandTree() dispatches %q, which has no entry in the meta catalog (internal/handler/meta)", name)
	}
}

// TestCommandTree_EveryLeafHasACmdspecEntry guards the one way the shared
// table itself could go stale without TestSynopsis_CommandTreeMatchesMetaCatalog
// noticing: a new leaf added to commandTree() with no matching key in
// cmdspec.Synopsis at all would read back "" on both sides (the zero value
// of a missing map entry), which would still "agree" with a catalog that
// makes the identical mistake -- silently, since both sides read the same
// map the same way. This checks presence (comma-ok), not just a non-empty
// value, since a genuinely flag-only command (e.g. "work list") is
// correctly "" and must stay that way.
func TestCommandTree_EveryLeafHasACmdspecEntry(t *testing.T) {
	t.Parallel()
	leaves := leafCommandKeys(commandTree())
	for leaf := range leaves {
		_, ok := cmdspec.Synopsis[leaf]
		assert.True(t, ok, "command %q is dispatchable but internal/cmdspec.Synopsis has no entry for it", leaf)
	}
	for name := range cmdspec.Synopsis {
		assert.True(t, leaves[name], "internal/cmdspec.Synopsis names %q, which is not a dispatchable leaf in commandTree()", name)
	}
	// cmdspec.StdinNote is intentionally sparse (most commands read no
	// stdin), so only the orphan direction applies: every key it DOES have
	// must name a real leaf, catching a typo'd command name the same way
	// the two checks above do for Synopsis.
	for name := range cmdspec.StdinNote {
		assert.True(t, leaves[name], "internal/cmdspec.StdinNote names %q, which is not a dispatchable leaf in commandTree()", name)
	}
}
