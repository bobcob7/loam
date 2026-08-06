package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Identities the staging tests share. stagingWorkspace (staging_test.go)
// infers exactly this repo and work branch from its fixed git lookup, so a
// test that omits the positionals resolves to the same staging key an
// explicit invocation would.
const (
	testRepo       = "bobcob7/doc-server"
	testWorkBranch = "wb-9c2f1a"
	testReviewer   = "ada-lovelace-7-reviewer"
)

// openTestStore opens a staging store the way a fresh CLI invocation would:
// a brand new workspace resolver over the same workspace root, holding no
// state carried over from any earlier store.
func openTestStore(t *testing.T, workspaceRoot, agent string) *stagingStore {
	t.Helper()
	store, err := openStagingStore(stagingWorkspace(workspaceRoot, agent), testRepo, testWorkBranch)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close()) })
	return store
}

// stagedIDs returns just the ids of a staged set, for assertions about
// allocation order.
func stagedIDs(items []stagedItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

// TestStagingStore_EmptyArea_ListsNothing pins the first-invocation case: a
// staging area with no staged.json yet is an empty set, not an error.
func TestStagingStore_EmptyArea_ListsNothing(t *testing.T) {
	t.Parallel()
	store := openTestStore(t, realTempDir(t), testReviewer)
	items, err := store.list()
	require.NoError(t, err)
	assert.Empty(t, items)
}

// TestStagingStore_ItemsSurviveAcrossInvocations is the persistence
// property: a staging store that lost items between process runs would be
// useless, so the item is written by one store and read back by a
// completely separate one built from scratch over the same workspace.
func TestStagingStore_ItemsSurviveAcrossInvocations(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	first, err := openTestStore(t, workspaceRoot, testReviewer).add(stagedItem{File: "auth.go", Line: 42, Body: "this leaks a token"})
	require.NoError(t, err)
	require.Equal(t, "s1", first.ID)
	items, err := openTestStore(t, workspaceRoot, testReviewer).list()
	require.NoError(t, err)
	require.Len(t, items, 1, "a staged item must still be there for the next invocation")
	assert.Equal(t, stagedItem{ID: "s1", File: "auth.go", Line: 42, Body: "this leaks a token"}, items[0])
}

// TestStagingStore_AllocatesSequentialIDsAndNeverReusesThem pins both
// halves of the id contract: ids are allocated in order within one
// (repo, work-branch, agent) area, and a discarded id is never handed out
// again — reuse would silently re-point a later --edit at a different
// comment than the agent read out of an earlier invocation.
func TestStagingStore_AllocatesSequentialIDsAndNeverReusesThem(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	for _, body := range []string{"one", "two", "three"} {
		_, err := openTestStore(t, workspaceRoot, testReviewer).add(stagedItem{Body: body})
		require.NoError(t, err)
	}
	items, err := openTestStore(t, workspaceRoot, testReviewer).list()
	require.NoError(t, err)
	require.Equal(t, []string{"s1", "s2", "s3"}, stagedIDs(items))
	_, err = openTestStore(t, workspaceRoot, testReviewer).discard("s3")
	require.NoError(t, err)
	added, err := openTestStore(t, workspaceRoot, testReviewer).add(stagedItem{Body: "four"})
	require.NoError(t, err)
	assert.Equal(t, "s4", added.ID, "the highest id was discarded, but reusing it would rename an existing comment")
}

// TestStagingStore_TamperedNextID_StillAllocatesAFreeID proves the
// allocation guard: staged.json is plain JSON in the agent's own workspace,
// so a next_id that has fallen behind the ids actually present is reachable
// by hand-editing. The next allocation must still be free rather than
// colliding with s7 and giving --edit two items to choose from.
func TestStagingStore_TamperedNextID_StillAllocatesAFreeID(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	ws := stagingWorkspace(workspaceRoot, testReviewer)
	area, err := ws.OpenStaging(testRepo, testWorkBranch)
	require.NoError(t, err)
	require.NoError(t, area.WriteFile(stagedFileName, []byte(`{"version":1,"next_id":1,"items":[{"id":"s7","body":"already staged"}]}`)))
	require.NoError(t, area.Close())
	added, err := openTestStore(t, workspaceRoot, testReviewer).add(stagedItem{Body: "next"})
	require.NoError(t, err)
	assert.Equal(t, "s8", added.ID)
	items, err := openTestStore(t, workspaceRoot, testReviewer).list()
	require.NoError(t, err)
	assert.Equal(t, []string{"s7", "s8"}, stagedIDs(items), "no two staged items may share an id")
}

// TestStagingStore_EditReplacesOnlyTheBody proves --edit's contract: the new
// body from stdin replaces the old one while the item keeps its id, its
// anchor, and its position in the set, so an agent's earlier `comments
// --staged` reading stays valid.
func TestStagingStore_EditReplacesOnlyTheBody(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	_, err := openTestStore(t, workspaceRoot, testReviewer).add(stagedItem{File: "auth.go", Line: 42, Body: "first"})
	require.NoError(t, err)
	_, err = openTestStore(t, workspaceRoot, testReviewer).add(stagedItem{Body: "second"})
	require.NoError(t, err)
	edited, err := openTestStore(t, workspaceRoot, testReviewer).edit("s1", "revised")
	require.NoError(t, err)
	assert.Equal(t, stagedItem{ID: "s1", File: "auth.go", Line: 42, Body: "revised"}, edited)
	items, err := openTestStore(t, workspaceRoot, testReviewer).list()
	require.NoError(t, err)
	assert.Equal(t, []string{"s1", "s2"}, stagedIDs(items), "editing must not reorder the set")
	assert.Equal(t, "second", items[1].Body, "editing one item must not touch another")
}

// TestStagingStore_DiscardRemovesOnlyThatItem proves --discard removes the
// named item and leaves the rest staged.
func TestStagingStore_DiscardRemovesOnlyThatItem(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	for _, body := range []string{"first", "second"} {
		_, err := openTestStore(t, workspaceRoot, testReviewer).add(stagedItem{Body: body})
		require.NoError(t, err)
	}
	removed, err := openTestStore(t, workspaceRoot, testReviewer).discard("s1")
	require.NoError(t, err)
	assert.Equal(t, "first", removed.Body, "discard reports the item it removed")
	items, err := openTestStore(t, workspaceRoot, testReviewer).list()
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "s2", items[0].ID)
}

// TestStagingStore_UnknownID_IsNotFound covers both operations that address
// an already-staged item: an id that is not staged is not_found (exit 3),
// not a silent no-op.
func TestStagingStore_UnknownID_IsNotFound(t *testing.T) {
	t.Parallel()
	tests := map[string]func(*stagingStore) error{
		"edit":    func(s *stagingStore) error { _, err := s.edit("s9", "body"); return err },
		"discard": func(s *stagingStore) error { _, err := s.discard("s9"); return err },
	}
	for name, op := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			workspaceRoot := realTempDir(t)
			_, err := openTestStore(t, workspaceRoot, testReviewer).add(stagedItem{Body: "staged"})
			require.NoError(t, err)
			err = op(openTestStore(t, workspaceRoot, testReviewer))
			require.Error(t, err)
			assert.ErrorIs(t, err, errNotFound)
			assert.Equal(t, 3, newErrorMapper().ExitCode(err))
		})
	}
}

// TestStagingStore_CorruptOrFutureFile_IsAnInternalError proves the store
// refuses to silently start over from an empty set when staged.json cannot
// be understood: doing so would discard review work the agent believes is
// staged. Neither case is the agent's usage mistake, so both are exit 1.
func TestStagingStore_CorruptOrFutureFile_IsAnInternalError(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"corrupt":        `{"version":1,"items":[`,
		"future version": `{"version":99,"next_id":1,"items":[]}`,
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			workspaceRoot := realTempDir(t)
			ws := stagingWorkspace(workspaceRoot, testReviewer)
			area, err := ws.OpenStaging(testRepo, testWorkBranch)
			require.NoError(t, err)
			require.NoError(t, area.WriteFile(stagedFileName, []byte(contents)))
			require.NoError(t, area.Close())
			_, err = openTestStore(t, workspaceRoot, testReviewer).list()
			require.Error(t, err)
			assert.ErrorIs(t, err, errStagingArea)
			assert.Equal(t, 1, newErrorMapper().ExitCode(err))
		})
	}
}

// TestStagingStore_ItemsAreKeyedPerAgent is the invisibility property at
// the store layer: two agents sharing one workspace, on the same repo and
// work branch, never see each other's staged items, because OpenStaging
// keys the directory by agent identifier as well.
func TestStagingStore_ItemsAreKeyedPerAgent(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	_, err := openTestStore(t, workspaceRoot, testReviewer).add(stagedItem{Body: "my private note"})
	require.NoError(t, err)
	mine, err := openTestStore(t, workspaceRoot, testReviewer).list()
	require.NoError(t, err)
	require.Len(t, mine, 1, "precondition: the item really is staged for its own agent")
	theirs, err := openTestStore(t, workspaceRoot, "alan-turing-4-reviewer").list()
	require.NoError(t, err)
	assert.Empty(t, theirs, "another agent in the same workspace must not see the staged item")
}

// TestStagingStore_ItemsAreKeyedPerWorkBranch proves the other two-thirds of
// the key: the same agent's staging on one work branch (or one repo) is not
// visible from another.
func TestStagingStore_ItemsAreKeyedPerWorkBranch(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	_, err := openTestStore(t, workspaceRoot, testReviewer).add(stagedItem{Body: "on wb-9c2f1a"})
	require.NoError(t, err)
	ws := stagingWorkspace(workspaceRoot, testReviewer)
	for name, key := range map[string][2]string{
		"other work branch": {testRepo, "wb-other"},
		"other repo":        {"bobcob7/other-repo", testWorkBranch},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store, err := openStagingStore(ws, key[0], key[1])
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, store.Close()) })
			items, err := store.list()
			require.NoError(t, err)
			assert.Empty(t, items)
		})
	}
}

// TestOpenStagingStore_InvalidKey_IsAUsageError proves a malformed key
// still classifies as exit 2 through the store, rather than being swallowed
// into an opaque failure — openStagingStore adds no error handling of its
// own, and this is what pins that it must not.
func TestOpenStagingStore_InvalidKey_IsAUsageError(t *testing.T) {
	t.Parallel()
	store, err := openStagingStore(stagingWorkspace(realTempDir(t), testReviewer), "../../etc", testWorkBranch)
	require.Error(t, err)
	assert.Nil(t, store)
	assert.ErrorIs(t, err, errInvalidStagingKey)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
}

// openTestStoreFor opens a staging store against an already-built
// workspace, for the tests that need to control the staging root and the
// legacy root independently.
func openTestStoreFor(t *testing.T, ws *workspace) *stagingStore {
	t.Helper()
	store, err := openStagingStore(ws, testRepo, testWorkBranch)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close()) })
	return store
}
