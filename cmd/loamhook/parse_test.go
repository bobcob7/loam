package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/hooksocket"
)

// TestParseUpdates_OneLinePerUpdate proves the "<old> <new> <ref>" per
// line, whole-push-in-one-invocation shape docs/git-spec.md "Enforcement
// Mechanics" describes.
func TestParseUpdates_OneLinePerUpdate(t *testing.T) {
	t.Parallel()
	in := strings.NewReader(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/heads/wb-one\n" +
			"cccccccccccccccccccccccccccccccccccccccc dddddddddddddddddddddddddddddddddddddddd refs/heads/wb-two\n",
	)
	updates, err := parseUpdates(in)
	require.NoError(t, err)
	assert.Equal(t, []hooksocket.RefUpdateWire{
		{OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("b", 40), Ref: "refs/heads/wb-one"},
		{OldSHA: strings.Repeat("c", 40), NewSHA: strings.Repeat("d", 40), Ref: "refs/heads/wb-two"},
	}, updates)
}

// TestParseUpdates_BlankLinesSkipped proves a trailing blank line (or one
// in the middle) is tolerated rather than misparsed.
func TestParseUpdates_BlankLinesSkipped(t *testing.T) {
	t.Parallel()
	in := strings.NewReader("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/heads/wb-one\n\n")
	updates, err := parseUpdates(in)
	require.NoError(t, err)
	assert.Len(t, updates, 1)
}

// TestParseUpdates_EmptyStdinIsNoUpdates proves an empty push (git never
// actually sends this, but the parser must not choke on it) yields a nil
// slice, not an error.
func TestParseUpdates_EmptyStdinIsNoUpdates(t *testing.T) {
	t.Parallel()
	updates, err := parseUpdates(strings.NewReader(""))
	require.NoError(t, err)
	assert.Empty(t, updates)
}

// TestParseUpdates_MalformedLineErrors proves a line that does not split
// into exactly three fields is reported as an error -- the caller's
// fail-closed responsibility, not this function's -- rather than silently
// dropped or partially parsed.
func TestParseUpdates_MalformedLineErrors(t *testing.T) {
	t.Parallel()
	in := strings.NewReader("only two\n")
	_, err := parseUpdates(in)
	assert.Error(t, err)
}
