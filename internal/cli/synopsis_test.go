package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/cmdspec"
	"github.com/bobcob7/loam/internal/handler/meta"
)

// synopsisByName walks tree (mirroring router_test.go's leafCommandKeys)
// and returns every dispatchable leaf's full name mapped to its synopsis
// field, as set by router.go's applySynopsis.
func synopsisByName(tree map[string]*command) map[string]string {
	out := make(map[string]string)
	for name, cmd := range tree {
		if cmd.subcommands == nil {
			out[name] = cmd.synopsis
			continue
		}
		for sub, subcmd := range cmd.subcommands {
			out[name+" "+sub] = subcmd.synopsis
		}
	}
	return out
}

// TestSynopsis_CommandTreeMatchesMetaCatalog is loam-hi5o.4 acceptance
// criterion 5: a test that fails if commandTree()'s synopsis and
// internal/handler/meta's catalog synopsis disagree for any command.
//
// Both sides already derive their synopsis from the one shared
// internal/cmdspec.Synopsis table (router.go's applySynopsis,
// internal/handler/meta/catalog.go's newCatalog), which is the "derive
// both from ONE source" option loam-hi5o.4 asks to prefer over a
// hand-maintained comparison list -- so in the normal case this test can
// only fail if someone bypasses that shared source (a literal string
// written directly into a commandTree() or commandCatalog entry instead
// of going through cmdspec). It still earns its place per loam-hi5o.4's
// own instructions ("the cross-package test is mandatory, not optional"
// whenever a single source is used, so the guard survives someone
// reaching for a shortcut later) -- and, more importantly, it is written
// to DISCOVER both command sets independently (leafCommandKeys walks
// commandTree() itself; meta.AllCommands() walks meta's own catalog var)
// rather than iterating a hand-written list of names, which is exactly
// the failure mode this project has already been bitten by once
// (router_test.go's commandImplementationProofs doc comment).
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
	for leaf := range leafCommandKeys(commandTree()) {
		_, ok := cmdspec.Synopsis[leaf]
		assert.True(t, ok, "command %q is dispatchable but internal/cmdspec.Synopsis has no entry for it", leaf)
	}
	for name := range cmdspec.Synopsis {
		assert.True(t, leafCommandKeys(commandTree())[name], "internal/cmdspec.Synopsis names %q, which is not a dispatchable leaf in commandTree()", name)
	}
}
