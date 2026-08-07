package cli

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTryHelp_WorkStart_UsageLine_IsTrue pins loam-hi5o.4's reported bug
// directly: `loam work start --help` used to print "Usage: loam work
// start [flags]", which is false -- work start takes two positional
// arguments (<repo> <from>) and defines no flags at all. This is
// acceptance criterion 1.
func TestTryHelp_WorkStart_UsageLine_IsTrue(t *testing.T) {
	t.Parallel()
	text, ok := TryHelp([]string{"work", "start", "--help"})
	require.True(t, ok)
	assert.Contains(t, text, "Usage: loam work start <repo> <from>")
	assert.NotContains(t, text, "[flags]", "work start defines no flags; its usage line must not claim it does")
}

// TestTryHelp_EveryLeaf_FlagShapeOnlyWhenFlagsDefined is acceptance
// criterion 2, widened past the one reported command: no leaf in
// commandTree() may have its usage line claim flags unless its own
// newFlags() FlagSet actually registers at least one.
//
// Since loam-hwru the usage line spells the flags out rather than printing
// a bare "[flags]" token, so this asserts on the usage LINE, not on the
// whole help text: the flag list further down renders every flag's name
// whether or not the usage line does, and a test searching the whole text
// for "--stat" would pass on a usage line that omitted it entirely. That
// is the same "what does the fixture make indistinguishable" trap this
// change has already hit twice.
func TestTryHelp_EveryLeaf_FlagShapeOnlyWhenFlagsDefined(t *testing.T) {
	t.Parallel()
	for leaf, fs := range leafFlagSets(commandTree()) {
		args := strings.Fields(leaf)
		args = append(args, "--help")
		text, ok := TryHelp(args)
		require.True(t, ok, "command %q", leaf)
		usage := strings.SplitN(text, "\n", 2)[0]
		if !fs.HasFlags() {
			assert.NotContains(t, usage, "--", "command %q defines no flags; its usage line must not claim any", leaf)
			assert.NotContains(t, usage, "[flags]", "command %q defines no flags; its usage line must not claim any", leaf)
			continue
		}
		fs.VisitAll(func(f *pflag.Flag) {
			assert.Contains(t, usage, "--"+f.Name, "command %q registers --%s, which must appear in its usage line, not only in the flag list below it", leaf, f.Name)
		})
	}
}

// instructionsSuggestionRe extracts the command line inside the backticks
// following "run " in renderLeafHelp's trailing pointer line, e.g.
// "run `loam instructions \"work start\"`" -> `loam instructions "work start"`.
var instructionsSuggestionRe = regexp.MustCompile("run `(loam instructions[^`]*)`")

// splitShellWords is a minimal shell-like tokenizer: it splits on
// whitespace outside double quotes and drops the quote characters
// themselves. It is deliberately not a general shell grammar (no escapes,
// no single quotes) -- help.go's quoteIfMulti only ever produces
// plain double-quoted words, so that is all this needs to undo faithfully
// to reproduce what a real shell would hand the CLI as argv.
func splitShellWords(s string) []string {
	var words []string
	var cur strings.Builder
	inQuotes := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
		case r == ' ' && !inQuotes:
			if cur.Len() > 0 {
				words = append(words, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		words = append(words, cur.String())
	}
	return words
}

// TestTryHelp_EveryLeaf_InstructionsSuggestion_IsRunnableVerbatim is
// acceptance criterion 3: every `loam instructions <command>` suggestion
// help prints must be runnable verbatim, quoting included. This walks
// every leaf's --help text, extracts the printed suggestion, tokenizes it
// the way a real shell would (splitShellWords), and feeds the result
// through the exact same argument parser runInstructions itself uses
// (parseInstructionsArgs) -- not a hand-rolled re-check of the same rule
// that could silently drift from it. loam-hi5o.4's own report is the
// regression this guards: `loam instructions work start`, copied literally
// from an earlier version of this help text, failed with "instructions
// takes at most one command argument" because the space-containing name
// was not quoted.
func TestTryHelp_EveryLeaf_InstructionsSuggestion_IsRunnableVerbatim(t *testing.T) {
	t.Parallel()
	for leaf := range leafFlagSets(commandTree()) {
		args := strings.Fields(leaf)
		args = append(args, "--help")
		text, ok := TryHelp(args)
		require.True(t, ok, "command %q", leaf)
		match := instructionsSuggestionRe.FindStringSubmatch(text)
		require.NotNil(t, match, "command %q: help text has no `loam instructions ...` suggestion:\n%s", leaf, text)
		tokens := splitShellWords(match[1])
		require.Equal(t, "loam", tokens[0])
		require.Equal(t, "instructions", tokens[1])
		_, err := parseInstructionsArgs(tokens[2:])
		assert.NoError(t, err, "command %q: suggested %q is not runnable verbatim through instructions' own argument parser", leaf, match[1])
	}
}

// leafFlagSets walks tree (see router_test.go's leafCommandKeys, which
// this mirrors) and returns every dispatchable leaf's full name mapped to
// its own FlagSet, built fresh from that leaf's newFlags constructor --
// the same one help.go's TryHelp renders from, with no Deps required.
func leafFlagSets(tree map[string]*command) map[string]*pflag.FlagSet {
	out := make(map[string]*pflag.FlagSet)
	for name, cmd := range tree {
		if cmd.subcommands == nil {
			out[name] = cmd.newFlags()
			continue
		}
		for sub, subcmd := range cmd.subcommands {
			out[name+" "+sub] = subcmd.newFlags()
		}
	}
	return out
}
