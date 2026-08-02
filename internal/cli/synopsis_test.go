package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/cmdspec"
	"github.com/bobcob7/loam/internal/handler/meta"
)

// synopsisByName walks tree (mirroring router_test.go's leafCommandKeys)
// and returns every dispatchable leaf's full name mapped to its FULL
// composed synopsis: cmd.synopsis and cmd.stdinNote (router.go's
// applySynopsis) joined by cmdspec.Compose, the exact same rule
// internal/handler/meta/catalog.go's newCatalog uses to build the value it
// puts in the catalog. Composing here, rather than comparing cmd.synopsis
// alone, is what makes the comparison in
// TestSynopsis_CommandTreeMatchesMetaCatalog apples-to-apples for the
// three commands that also read stdin (cmd.synopsis on its own is
// positional-only; the catalog's entry.synopsis is always the composed
// form, since the catalog has nowhere else to carry a stdin note).
func synopsisByName(tree map[string]*command) map[string]string {
	out := make(map[string]string)
	for name, cmd := range tree {
		if cmd.subcommands == nil {
			out[name] = cmdspec.Compose(cmd.synopsis, cmd.stdinNote)
			continue
		}
		for sub, subcmd := range cmd.subcommands {
			out[name+" "+sub] = cmdspec.Compose(subcmd.synopsis, subcmd.stdinNote)
		}
	}
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
