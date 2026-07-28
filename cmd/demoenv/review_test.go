package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// samplePublished is one `loam work comments` document in the exact shape
// demo:m4's second round produces: the reviewer's thread raised in round 1,
// carrying the author's two replies -- the second of which lands in round 2.
// That 1,1,2 sequence IS the demo's definition of done, so it is what these
// tests pin.
const samplePublished = `[
  {"id":"11111111-1111-4111-8111-111111111111","resolved":false,"file":"README.md","line":1,"round":1,
   "comments":[
     {"author":"grace-hopper-2-reviewer","body":"needs a heading","round":1},
     {"author":"ada-lovelace-1-author","body":"fixed","round":1},
     {"author":"ada-lovelace-1-author","body":"pushed a follow-up","round":2}]},
  {"id":"22222222-2222-4222-8222-222222222222","resolved":false,"file":"NOTES.md","round":1,
   "comments":[{"author":"grace-hopper-2-reviewer","body":"stray file","round":1}]}
]`

const sampleStaged = `[
  {"staged":true,"id":"01","file":"README.md","line":1,"body":"needs a heading"},
  {"staged":true,"id":"02","file":"NOTES.md","body":"stray file"}
]`

const sampleVerdicts = `[{"reviewer":"grace-hopper-2-reviewer","outcome":"approve","round":1,"stale":true}]`

const sampleWorkList = `{"truncated":false,"results":[
  {"repo":"bobcob7/doc-server","name":"wb-9c2f1a","target":"main","title":"t","author":"ada-lovelace-1-author","state":"reviewable"}]}`

func checkComments(t *testing.T, payload string, args ...string) error {
	t.Helper()
	return runCheckComments(args, strings.NewReader(payload), io.Discard)
}

func checkVerdicts(t *testing.T, payload string, args ...string) error {
	t.Helper()
	return runCheckVerdicts(args, strings.NewReader(payload), io.Discard)
}

func checkWorkList(t *testing.T, payload string, args ...string) error {
	t.Helper()
	return runCheckWorkList(args, strings.NewReader(payload), io.Discard)
}

func TestCheckComments_PublishedAllAssertionsHold(t *testing.T) {
	t.Parallel()
	require.NoError(t, checkComments(t, samplePublished,
		"-count", "2", "-want-files", "README.md,NOTES.md",
		"-want-bodies", "needs a heading,pushed a follow-up",
		"-select-file", "README.md", "-thread-round", "1",
		"-comment-rounds", "1,1,2",
		"-comment-authors", "grace-hopper-2-reviewer,ada-lovelace-1-author,ada-lovelace-1-author"))
}

// TestCheckComments_EmptyListIsAnAssertableCount is the "not visible until
// submitted" proof: an empty published listing must be assertable as
// exactly zero, which a grep for a comment body cannot express -- that grep
// finds nothing just as readily when the CLI crashed.
func TestCheckComments_EmptyListIsAnAssertableCount(t *testing.T) {
	t.Parallel()
	require.NoError(t, checkComments(t, `[]`, "-count", "0"))
	require.Error(t, checkComments(t, samplePublished, "-count", "0"))
}

// TestCheckComments_CountZeroIsNotUnset guards the reason unsetCount is -1:
// if zero doubled as "unset", the demo's headline assertion would silently
// become no assertion at all.
func TestCheckComments_CountZeroIsNotUnset(t *testing.T) {
	t.Parallel()
	err := checkComments(t, samplePublished, "-count", "0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1 assertion(s) failed")
}

func TestCheckComments_WrongCommentRoundsFails(t *testing.T) {
	t.Parallel()
	err := checkComments(t, samplePublished, "-select-file", "README.md", "-comment-rounds", "1,1,1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1 assertion(s) failed")
}

// TestCheckComments_CommentRoundsAreOrdered is why the rounds are compared
// as a sequence rather than as a set: "the round incremented" is a claim
// about which comment carries which round, not about the multiset of rounds
// present anywhere in the document.
func TestCheckComments_CommentRoundsAreOrdered(t *testing.T) {
	t.Parallel()
	require.Error(t, checkComments(t, samplePublished, "-select-file", "README.md", "-comment-rounds", "2,1,1"))
}

func TestCheckComments_ThreadRoundIsNotTheCommentRound(t *testing.T) {
	t.Parallel()
	// The thread was raised in round 1 even though it carries a round-2
	// comment; asserting the thread's round as 2 must fail.
	require.Error(t, checkComments(t, samplePublished, "-select-file", "README.md", "-thread-round", "2"))
}

func TestCheckComments_SelectionRequiresExactlyOneThread(t *testing.T) {
	t.Parallel()
	err := checkComments(t, samplePublished, "-select-file", "missing.md", "-thread-round", "1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1 assertion(s) failed")
}

func TestCheckComments_PerThreadAssertionsRequireSelection(t *testing.T) {
	t.Parallel()
	require.Error(t, checkComments(t, samplePublished, "-comment-rounds", "1,1,2"))
}

func TestCheckComments_StagedShape(t *testing.T) {
	t.Parallel()
	require.NoError(t, checkComments(t, sampleStaged, "-staged", "-count", "2",
		"-want-files", "README.md,NOTES.md", "-want-bodies", "stray file"))
	require.Error(t, checkComments(t, sampleStaged, "-staged", "-count", "1"))
	require.Error(t, checkComments(t, sampleStaged, "-staged", "-want-files", "absent.md"))
}

// TestCheckComments_StagedFalseIsARegression: `comments --staged` lists the
// staging area, so every row it returns is by definition still staged. A
// false there would mean the CLI listed something already published.
func TestCheckComments_StagedFalseIsARegression(t *testing.T) {
	t.Parallel()
	require.Error(t, checkComments(t, `[{"staged":false,"id":"01","file":"a","body":"b"}]`, "-staged"))
}

func TestCheckVerdicts_AllAssertionsHold(t *testing.T) {
	t.Parallel()
	require.NoError(t, checkVerdicts(t, sampleVerdicts, "-count", "1",
		"-reviewer", "grace-hopper-2-reviewer", "-outcome", "approve", "-round", "1", "-stale", "true"))
}

// TestCheckVerdicts_StaleFalseIsAssertable is why -stale is a string: the
// demo asserts stale=false BEFORE the second round opens and stale=true
// after, and a bool flag could not tell an explicit false from an unset one.
func TestCheckVerdicts_StaleFalseIsAssertable(t *testing.T) {
	t.Parallel()
	fresh := `[{"reviewer":"r","outcome":"approve","round":1,"stale":false}]`
	require.NoError(t, checkVerdicts(t, fresh, "-reviewer", "r", "-stale", "false"))
	require.Error(t, checkVerdicts(t, fresh, "-reviewer", "r", "-stale", "true"))
}

func TestCheckVerdicts_UnknownStaleValueFails(t *testing.T) {
	t.Parallel()
	require.Error(t, checkVerdicts(t, sampleVerdicts, "-reviewer", "grace-hopper-2-reviewer", "-stale", "yes"))
}

// TestCheckVerdicts_StaleIsAttributedToAReviewer: the row is addressed by
// reviewer, not by index, so a second agent's verdict can never satisfy an
// assertion about the first one's.
func TestCheckVerdicts_StaleIsAttributedToAReviewer(t *testing.T) {
	t.Parallel()
	two := `[{"reviewer":"other","outcome":"approve","round":2,"stale":false},
	         {"reviewer":"target","outcome":"approve","round":1,"stale":true}]`
	require.NoError(t, checkVerdicts(t, two, "-reviewer", "target", "-round", "1", "-stale", "true"))
	require.Error(t, checkVerdicts(t, two, "-reviewer", "target", "-stale", "false"))
	require.Error(t, checkVerdicts(t, two, "-reviewer", "absent"))
}

func TestCheckVerdicts_PerRowAssertionsRequireAReviewer(t *testing.T) {
	t.Parallel()
	require.Error(t, checkVerdicts(t, sampleVerdicts, "-stale", "true"))
}

func TestCheckWorkList_PresenceAndAbsence(t *testing.T) {
	t.Parallel()
	require.NoError(t, checkWorkList(t, sampleWorkList, "-count", "1",
		"-want-names", "wb-9c2f1a", "-want-state", "reviewable", "-want-repo", "bobcob7/doc-server"))
	require.Error(t, checkWorkList(t, sampleWorkList, "-deny-names", "wb-9c2f1a"))
	require.NoError(t, checkWorkList(t, `{"truncated":false,"results":[]}`,
		"-count", "0", "-deny-names", "wb-9c2f1a"))
}

func TestCheckWorkList_WrongStateFails(t *testing.T) {
	t.Parallel()
	require.Error(t, checkWorkList(t, sampleWorkList, "-want-state", "reviewed"))
}

func TestThreadID_PrintsOnlyTheID(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	require.NoError(t, runThreadID([]string{"-file", "NOTES.md"}, strings.NewReader(samplePublished), &out))
	assert.Equal(t, "22222222-2222-4222-8222-222222222222\n", out.String())
}

// TestThreadID_AmbiguousOrMissingAnchorFails: returning an empty string
// would send an empty --thread argument several steps downstream, where the
// resulting error would name the wrong culprit.
func TestThreadID_AmbiguousOrMissingAnchorFails(t *testing.T) {
	t.Parallel()
	require.Error(t, runThreadID([]string{"-file", "absent.md"}, strings.NewReader(samplePublished), io.Discard))
	dup := `[{"id":"a","file":"same.md","comments":[]},{"id":"b","file":"same.md","comments":[]}]`
	require.Error(t, runThreadID([]string{"-file", "same.md"}, strings.NewReader(dup), io.Discard))
	require.Error(t, runThreadID(nil, strings.NewReader(samplePublished), io.Discard))
}

func TestField_PrintsOnlyTheValue(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	doc := `{"repo":"bobcob7/doc-server","name":"wb-9c2f1a","target":"main","state":"draft"}`
	require.NoError(t, runField([]string{"-name", "name"}, strings.NewReader(doc), &out))
	assert.Equal(t, "wb-9c2f1a\n", out.String())
}

func TestField_AbsentEmptyOrNonStringFails(t *testing.T) {
	t.Parallel()
	require.Error(t, runField([]string{"-name", "absent"}, strings.NewReader(`{"name":"x"}`), io.Discard))
	require.Error(t, runField([]string{"-name", "name"}, strings.NewReader(`{"name":""}`), io.Discard))
	require.Error(t, runField([]string{"-name", "name"}, strings.NewReader(`{"name":7}`), io.Discard))
	require.Error(t, runField(nil, strings.NewReader(`{"name":"x"}`), io.Discard))
}
