package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

// --- scaffolding ---

// commentServer stands in for the server behind `work comment`: it answers
// the two READ rpcs the command makes (GetWorkBranch to check the work
// branch exists, ListComments to check a --resolve target), and records any
// call to a PUBLISHING rpc so a test can prove staging never reaches one.
//
// ListComments honours the request's page offset and reports a total, so a
// test can put the target thread beyond the first page.
type commentServer struct {
	client    *WorkBranchClientMock
	pageSize  int // 0 = one page containing every thread
	getCalls  int
	listCalls int
	published []string
}

// newCommentServer builds a server whose work branch exists and whose
// published threads are the ones given.
func newCommentServer(threads ...*loamv1.Thread) *commentServer {
	s := &commentServer{}
	s.client = &WorkBranchClientMock{
		GetWorkBranchFunc: func(context.Context, *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
			s.getCalls++
			return connect.NewResponse(&loamv1.GetWorkBranchResponse{WorkBranch: &loamv1.WorkBranch{
				Repo: testRepo, Name: testWorkBranch, Target: "main", State: loamv1.WorkBranchState_WORK_BRANCH_STATE_REVIEWABLE,
			}}), nil
		},
		ListCommentsFunc: func(_ context.Context, req *connect.Request[loamv1.ListCommentsRequest]) (*connect.Response[loamv1.ListCommentsResponse], error) {
			s.listCalls++
			return connect.NewResponse(&loamv1.ListCommentsResponse{
				Threads:  s.page(threads, int(req.Msg.GetPage().GetOffset())),
				PageInfo: &loamv1.PageInfo{Total: uint32(len(threads))},
			}), nil
		},
		SubmitVerdictFunc: func(context.Context, *connect.Request[loamv1.SubmitVerdictRequest]) (*connect.Response[loamv1.SubmitVerdictResponse], error) {
			s.published = append(s.published, "SubmitVerdict")
			return nil, errors.New("staging must not publish")
		},
		ReplyToThreadFunc: func(context.Context, *connect.Request[loamv1.ReplyToThreadRequest]) (*connect.Response[loamv1.ReplyToThreadResponse], error) {
			s.published = append(s.published, "ReplyToThread")
			return nil, errors.New("staging must not publish")
		},
	}
	return s
}

// page slices threads for the requested offset, honouring pageSize.
func (s *commentServer) page(threads []*loamv1.Thread, offset int) []*loamv1.Thread {
	if offset >= len(threads) {
		return nil
	}
	size := s.pageSize
	if size <= 0 {
		size = len(threads)
	}
	return threads[offset:min(offset+size, len(threads))]
}

// missingWorkBranchServer answers GetWorkBranch with NotFound — the shape a
// `work comment` against an unknown work branch sees.
func missingWorkBranchServer() *commentServer {
	s := newCommentServer()
	s.client.GetWorkBranchFunc = func(context.Context, *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
		s.getCalls++
		return nil, connect.NewError(connect.CodeNotFound, errors.New("work branch bobcob7/doc-server/wb-9c2f1a not found"))
	}
	return s
}

// publishedThread builds a thread opened by author (its first comment's
// author), optionally with later replies by other agents.
func publishedThread(id, author string, repliers ...string) *loamv1.Thread {
	comments := []*loamv1.Comment{{Author: author, Body: "opening comment"}}
	for _, replier := range repliers {
		comments = append(comments, &loamv1.Comment{Author: replier, Body: "a reply"})
	}
	return &loamv1.Thread{Id: id, Comments: comments}
}

// commentDeps wires a Deps for `work comment` against a REAL workspace
// rooted at workspaceRoot — the staging area is the thing under test, so it
// is a real contained directory rather than a mock.
func commentDeps(workspaceRoot, agent string, srv *commentServer, stdin string, encoded *any) *Deps {
	cfg := &ConfigMock{IdentifierFunc: func() string { return agent }}
	connectClient := &ConnectClientMock{WorkBranchFunc: func() WorkBranchClient { return srv.client }}
	encoder := &OutputEncoderMock{EncodeFunc: func(v any) error { *encoded = v; return nil }}
	return NewDeps(testLogger(), cfg, encoder, newErrorMapper(), stagingWorkspace(workspaceRoot, agent), connectClient, nil, strings.NewReader(stdin))
}

// runComment runs one `work comment` invocation exactly as a fresh process
// would: new deps, new workspace resolver, new staging store.
func runComment(t *testing.T, workspaceRoot, agent string, srv *commentServer, stdin string, args ...string) (any, error) {
	t.Helper()
	var encoded any
	err := runWorkComment(t.Context(), commentDeps(workspaceRoot, agent, srv, stdin, &encoded), args)
	return encoded, err
}

// explicitArgs is the positional pair every test below passes unless it is
// specifically exercising workspace inference.
func explicitArgs(rest ...string) []string {
	return append([]string{testRepo, testWorkBranch}, rest...)
}

// --- new thread ---

func TestRunWorkComment_AnchoredComment_StagesAndReportsALocalID(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	srv := newCommentServer()
	encoded, err := runComment(t, workspaceRoot, testReviewer, srv, "this leaks a token\n", explicitArgs("--file", "auth.go", "--line", "42")...)
	require.NoError(t, err)
	out, ok := encoded.(stagedCommentOutput)
	require.True(t, ok, "work comment must encode a stagedCommentOutput")
	assert.Equal(t, stagedCommentOutput{Staged: true, ID: "s1", File: "auth.go", Line: 42, Body: "this leaks a token"}, out)
	items, err := openTestStore(t, workspaceRoot, testReviewer).list()
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, stagedItem{ID: "s1", File: "auth.go", Line: 42, Body: "this leaks a token"}, items[0])
}

// TestRunWorkComment_TopLevelComment_OmitsAnchorFieldsInOutput pins the
// documented JSON shape (docs/cli-spec.md -> comment (add)) rather than just
// the Go struct: an unanchored comment must not report a phantom empty file
// or a line 0 an agent would parse as a real anchor.
func TestRunWorkComment_TopLevelComment_OmitsAnchorFieldsInOutput(t *testing.T) {
	t.Parallel()
	encoded, err := runComment(t, realTempDir(t), testReviewer, newCommentServer(), "a general remark", explicitArgs()...)
	require.NoError(t, err)
	raw, err := json.Marshal(encoded)
	require.NoError(t, err)
	assert.JSONEq(t, `{"staged":true,"id":"s1","body":"a general remark"}`, string(raw))
}

// TestRunWorkComment_ResolveOnly_NeedsNoBody covers the one invocation with
// no stdin at all that is still valid (docs/cli-spec.md: the body is
// "Required unless only --resolve or --discard is given").
func TestRunWorkComment_ResolveOnly_NeedsNoBody(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	srv := newCommentServer(publishedThread("t1", testReviewer))
	encoded, err := runComment(t, workspaceRoot, testReviewer, srv, "", explicitArgs("--resolve", "t1")...)
	require.NoError(t, err)
	assert.Equal(t, stagedCommentOutput{Staged: true, ID: "s1", Resolve: "t1"}, encoded)
	items, err := openTestStore(t, workspaceRoot, testReviewer).list()
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "t1", items[0].Resolve)
	assert.Empty(t, items[0].Body)
}

// TestRunWorkComment_ResolveAlone_BodyIsSilentlyIgnored used to be named
// TestRunWorkComment_ResolveWithBody_StagesBoth and proved a lone --resolve
// with a body piped on stdin attached both. loam-hi5o.6 narrows that
// deliberately (see the bead's NOTES): reading stdin unconditionally before
// knowing the mode is what made a lone --resolve or --discard hang forever
// on an un-redirected stdin, so a lone --resolve (no --file/--line) is now
// one of the two shapes that complete WITHOUT ever reading stdin (see
// commentModeSkipsBody). A body piped alongside it is therefore no longer
// attached to the resolve -- it is silently unread, not rejected either.
// --resolve combined with --file/--line is a genuinely new anchored
// comment, not a lone --resolve, and is unaffected: see
// TestRunWorkComment_ResolveWithAnchor_StillReadsAndAttachesBody below.
func TestRunWorkComment_ResolveAlone_BodyIsSilentlyIgnored(t *testing.T) {
	t.Parallel()
	srv := newCommentServer(publishedThread("t1", testReviewer))
	encoded, err := runComment(t, realTempDir(t), testReviewer, srv, "fixed, thanks", explicitArgs("--resolve", "t1")...)
	require.NoError(t, err)
	assert.Equal(t, stagedCommentOutput{Staged: true, ID: "s1", Resolve: "t1"}, encoded, "a body piped alongside a lone --resolve must be silently ignored, not attached")
}

// TestRunWorkComment_ResolveWithAnchor_StillReadsAndAttachesBody is
// acceptance criterion 3: --resolve combined with --file/--line is a
// genuinely new anchored comment, not the "lone --resolve" shape
// loam-hi5o.6 narrows away, so it must still read stdin and attach the body
// exactly as before -- the disambiguation this bead was careful to preserve.
func TestRunWorkComment_ResolveWithAnchor_StillReadsAndAttachesBody(t *testing.T) {
	t.Parallel()
	srv := newCommentServer(publishedThread("t1", testReviewer))
	encoded, err := runComment(t, realTempDir(t), testReviewer, srv, "fixed, thanks", explicitArgs("--resolve", "t1", "--file", "auth.go", "--line", "3")...)
	require.NoError(t, err)
	assert.Equal(t, stagedCommentOutput{Staged: true, ID: "s1", File: "auth.go", Line: 3, Body: "fixed, thanks", Resolve: "t1"}, encoded)
}

// TestRunWorkComment_ResolveTargetOnALaterPage_IsFound proves the thread
// lookup follows pagination: with a server page size of one, thread "t3"
// only appears on the third page, and reading just the first response would
// report exit 3 for a thread that exists.
func TestRunWorkComment_ResolveTargetOnALaterPage_IsFound(t *testing.T) {
	t.Parallel()
	srv := newCommentServer(
		publishedThread("t1", "alan-turing-4-reviewer"),
		publishedThread("t2", "alan-turing-4-reviewer"),
		publishedThread("t3", testReviewer),
	)
	srv.pageSize = 1
	encoded, err := runComment(t, realTempDir(t), testReviewer, srv, "", explicitArgs("--resolve", "t3")...)
	require.NoError(t, err)
	assert.Equal(t, stagedCommentOutput{Staged: true, ID: "s1", Resolve: "t3"}, encoded)
	assert.Equal(t, 3, srv.listCalls, "the lookup must page until the thread is found")
}

// --- edit and discard ---

func TestRunWorkComment_Edit_ReplacesBodyKeepingIDAndAnchor(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	srv := newCommentServer()
	_, err := runComment(t, workspaceRoot, testReviewer, srv, "first draft", explicitArgs("--file", "auth.go", "--line", "42")...)
	require.NoError(t, err)
	encoded, err := runComment(t, workspaceRoot, testReviewer, srv, "revised wording", explicitArgs("--edit", "s1")...)
	require.NoError(t, err)
	assert.Equal(t, stagedCommentOutput{Staged: true, ID: "s1", File: "auth.go", Line: 42, Body: "revised wording"}, encoded)
	items, err := openTestStore(t, workspaceRoot, testReviewer).list()
	require.NoError(t, err)
	require.Len(t, items, 1, "editing must not stage a second item")
	assert.Equal(t, "revised wording", items[0].Body)
}

func TestRunWorkComment_Discard_RemovesTheItemAndReportsItUnstaged(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	srv := newCommentServer()
	_, err := runComment(t, workspaceRoot, testReviewer, srv, "keep me", explicitArgs()...)
	require.NoError(t, err)
	_, err = runComment(t, workspaceRoot, testReviewer, srv, "drop me", explicitArgs()...)
	require.NoError(t, err)
	encoded, err := runComment(t, workspaceRoot, testReviewer, srv, "", explicitArgs("--discard", "s2")...)
	require.NoError(t, err)
	assert.Equal(t, stagedCommentOutput{Staged: false, ID: "s2", Body: "drop me"}, encoded)
	items, err := openTestStore(t, workspaceRoot, testReviewer).list()
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "keep me", items[0].Body)
}

// TestRunWorkComment_DiscardWithBody_BodyIsSilentlyIgnoredNotRejected used
// to be the "discard with a body" case in
// TestRunWorkComment_ConflictingModes_ExitTwoWithoutCallingServer, which
// expected exit 2 with no rpc made. loam-hi5o.6 narrows that (see the
// bead's NOTES): a lone --discard is now one of the two shapes that
// complete without ever reading stdin, so it can no longer detect -- and
// therefore can no longer reject -- a body piped alongside it. The
// discard now succeeds and the body is silently unread, not rejected.
func TestRunWorkComment_DiscardWithBody_BodyIsSilentlyIgnoredNotRejected(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	srv := newCommentServer()
	_, err := runComment(t, workspaceRoot, testReviewer, srv, "keep me", explicitArgs()...)
	require.NoError(t, err)
	encoded, err := runComment(t, workspaceRoot, testReviewer, srv, "an ignored body", explicitArgs("--discard", "s1")...)
	require.NoError(t, err)
	assert.Equal(t, stagedCommentOutput{Staged: false, ID: "s1", Body: "keep me"}, encoded)
	items, err := openTestStore(t, workspaceRoot, testReviewer).list()
	require.NoError(t, err)
	assert.Empty(t, items)
}

// --- stdin (loam-hi5o.6: a lone --discard/--resolve must not read stdin) ---

// blockingStdin returns a reader that never reaches EOF and never yields
// any bytes until the test closes it, standing in for an interactive or
// un-redirected stdin. strings.NewReader("") would pass the tests below
// vacuously even against the pre-fix bug, because it returns EOF instantly
// either way -- only a reader that genuinely blocks can tell "never
// touched stdin" apart from "touched it and got lucky".
func blockingStdin(t *testing.T) io.Reader {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	return pr
}

// runCommentAsync starts runWorkComment on its own goroutine and reports
// its result on the returned channel, so a caller can bound how long it
// waits without hanging the whole test binary the way the pre-fix bug hangs
// a real process. It takes no *testing.T, deliberately: if the call under
// test really does hang, this goroutine outlives the test (until the
// blocking reader above is closed by cleanup), and touching t from a
// goroutine after the test has finished is unsafe.
func runCommentAsync(ctx context.Context, workspaceRoot, agent string, srv *commentServer, stdin io.Reader, args []string) <-chan struct {
	encoded any
	err     error
} {
	out := make(chan struct {
		encoded any
		err     error
	}, 1)
	go func() {
		cfg := &ConfigMock{IdentifierFunc: func() string { return agent }}
		connectClient := &ConnectClientMock{WorkBranchFunc: func() WorkBranchClient { return srv.client }}
		var encoded any
		encoder := &OutputEncoderMock{EncodeFunc: func(v any) error { encoded = v; return nil }}
		deps := NewDeps(testLogger(), cfg, encoder, newErrorMapper(), stagingWorkspace(workspaceRoot, agent), connectClient, nil, stdin)
		err := runWorkComment(ctx, deps, args)
		out <- struct {
			encoded any
			err     error
		}{encoded, err}
	}()
	return out
}

// TestRunWorkComment_DiscardAlone_CompletesWithoutReadingStdin is
// acceptance criterion 1 for loam-hi5o.6: a lone --discard must complete
// even when stdin blocks forever, because it must never touch stdin at all.
func TestRunWorkComment_DiscardAlone_CompletesWithoutReadingStdin(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	srv := newCommentServer()
	_, err := runComment(t, workspaceRoot, testReviewer, srv, "keep me", explicitArgs()...)
	require.NoError(t, err)
	results := runCommentAsync(t.Context(), workspaceRoot, testReviewer, srv, blockingStdin(t), explicitArgs("--discard", "s1"))
	select {
	case r := <-results:
		require.NoError(t, r.err)
		assert.Equal(t, stagedCommentOutput{Staged: false, ID: "s1", Body: "keep me"}, r.encoded)
	case <-time.After(2 * time.Second):
		t.Fatal("work comment --discard blocked on stdin instead of completing without reading it")
	}
}

// TestRunWorkComment_ResolveAlone_CompletesWithoutReadingStdin is the
// --resolve half of criterion 1.
func TestRunWorkComment_ResolveAlone_CompletesWithoutReadingStdin(t *testing.T) {
	t.Parallel()
	srv := newCommentServer(publishedThread("t1", testReviewer))
	results := runCommentAsync(t.Context(), realTempDir(t), testReviewer, srv, blockingStdin(t), explicitArgs("--resolve", "t1"))
	select {
	case r := <-results:
		require.NoError(t, r.err)
		assert.Equal(t, stagedCommentOutput{Staged: true, ID: "s1", Resolve: "t1"}, r.encoded)
	case <-time.After(2 * time.Second):
		t.Fatal("work comment --resolve blocked on stdin instead of completing without reading it")
	}
}

// TestRunWorkComment_FlagOnlyModes_NilStdinDoesNotPanic is acceptance
// criterion 6: since a lone --discard or --resolve must never touch stdin,
// a nil deps.stdin -- which readStdin would nil-pointer-dereference on --
// must not panic in those two modes.
func TestRunWorkComment_FlagOnlyModes_NilStdinDoesNotPanic(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		args []string
		srv  func() *commentServer
	}{
		"discard alone": {explicitArgs("--discard", "s1"), func() *commentServer { return newCommentServer() }},
		"resolve alone": {explicitArgs("--resolve", "t1"), func() *commentServer { return newCommentServer(publishedThread("t1", testReviewer)) }},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			workspaceRoot := realTempDir(t)
			srv := tt.srv()
			if name == "discard alone" {
				_, err := runComment(t, workspaceRoot, testReviewer, srv, "keep me", explicitArgs()...)
				require.NoError(t, err)
			}
			cfg := &ConfigMock{IdentifierFunc: func() string { return testReviewer }}
			connectClient := &ConnectClientMock{WorkBranchFunc: func() WorkBranchClient { return srv.client }}
			var encoded any
			encoder := &OutputEncoderMock{EncodeFunc: func(v any) error { encoded = v; return nil }}
			deps := NewDeps(testLogger(), cfg, encoder, newErrorMapper(), stagingWorkspace(workspaceRoot, testReviewer), connectClient, nil, nil)
			var runErr error
			assert.NotPanics(t, func() { runErr = runWorkComment(t.Context(), deps, tt.args) })
			require.NoError(t, runErr)
			assert.NotNil(t, encoded)
		})
	}
}

// --- persistence ---

// TestRunWorkComment_StagedItemsAccumulateAcrossInvocations is the property
// that makes the staging area worth having: each invocation is a separate
// process with no shared state, so the second comment must find the first
// one already on disk and be given the next id.
func TestRunWorkComment_StagedItemsAccumulateAcrossInvocations(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	srv := newCommentServer()
	firstOut, err := runComment(t, workspaceRoot, testReviewer, srv, "first", explicitArgs()...)
	require.NoError(t, err)
	secondOut, err := runComment(t, workspaceRoot, testReviewer, srv, "second", explicitArgs("--file", "auth.go")...)
	require.NoError(t, err)
	assert.Equal(t, "s1", firstOut.(stagedCommentOutput).ID)
	assert.Equal(t, "s2", secondOut.(stagedCommentOutput).ID, "a second invocation must continue the first's id sequence, not restart it")
	items, err := openTestStore(t, workspaceRoot, testReviewer).list()
	require.NoError(t, err)
	require.Len(t, items, 2, "both invocations' items must still be staged")
	assert.Equal(t, []string{"first", "second"}, []string{items[0].Body, items[1].Body})
}

// --- invisibility ---

// TestRunWorkComment_StagingIsInvisible is the headline property. Staging
// is structurally incapable of publishing: the command's only rpcs are the
// two existence checks, no publishing rpc is ever invoked, and the item
// lands in a directory keyed by the caller's own agent identifier, so a
// different agent staging on the same repo and work branch sees nothing.
func TestRunWorkComment_StagingIsInvisible(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	srv := newCommentServer()
	_, err := runComment(t, workspaceRoot, testReviewer, srv, "my private note", explicitArgs()...)
	require.NoError(t, err)
	assert.Empty(t, srv.published, "staging a comment must not call any publishing rpc")
	assert.Equal(t, 1, srv.getCalls, "the only rpc for a plain staged comment is the work-branch existence check")
	assert.Zero(t, srv.listCalls)
	mine, err := openTestStore(t, workspaceRoot, testReviewer).list()
	require.NoError(t, err)
	require.Len(t, mine, 1, "precondition: the comment really is staged for its author")
	theirs, err := openTestStore(t, workspaceRoot, "alan-turing-4-reviewer").list()
	require.NoError(t, err)
	assert.Empty(t, theirs, "another agent in the same workspace must not see it")
}

// --- mutually exclusive modes (exit 2) ---

// TestRunWorkComment_ConflictingModes_ExitTwoWithoutCallingServer walks
// every combination docs/cli-spec.md rules out. Each must be rejected from
// the arguments alone: no rpc, and nothing left in the staging area.
//
// "discard with a body" used to live in this table and expected rejection.
// loam-hi5o.6 narrows that (see the bead's NOTES): a lone --discard is now
// one of the two shapes that complete without ever reading stdin, so a body
// piped alongside it can no longer be detected, and therefore can no longer
// be rejected -- it is silently ignored instead. That case moved to
// TestRunWorkComment_DiscardWithBody_BodyIsSilentlyIgnoredNotRejected below.
func TestRunWorkComment_ConflictingModes_ExitTwoWithoutCallingServer(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		stdin string
		args  []string
	}{
		"edit and discard together":     {"body", explicitArgs("--edit", "s1", "--discard", "s2")},
		"edit with an anchor":           {"body", explicitArgs("--edit", "s1", "--file", "auth.go")},
		"edit with a line":              {"body", explicitArgs("--edit", "s1", "--line", "42")},
		"edit with a resolve":           {"body", explicitArgs("--edit", "s1", "--resolve", "t1")},
		"discard with an anchor":        {"", explicitArgs("--discard", "s1", "--file", "auth.go")},
		"discard with a resolve":        {"", explicitArgs("--discard", "s1", "--resolve", "t1")},
		"edit with no body":             {"", explicitArgs("--edit", "s1")},
		"no body and no resolve":        {"", explicitArgs()},
		"blank stdin only":              {"\n", explicitArgs()},
		"line without file":             {"body", explicitArgs("--line", "42")},
		"anchor on a resolve-only":      {"", explicitArgs("--resolve", "t1", "--file", "auth.go")},
		"negative line":                 {"body", explicitArgs("--file", "auth.go", "--line", "-3")},
		"too many positional arguments": {"body", []string{testRepo, testWorkBranch, "extra"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			workspaceRoot := realTempDir(t)
			srv := newCommentServer(publishedThread("t1", testReviewer))
			encoded, err := runComment(t, workspaceRoot, testReviewer, srv, tt.stdin, tt.args...)
			require.Error(t, err)
			assert.Equal(t, 2, newErrorMapper().ExitCode(err))
			assert.Nil(t, encoded, "a rejected invocation must encode nothing")
			assert.Zero(t, srv.getCalls, "a usage error must be decided before any rpc")
			assert.Zero(t, srv.listCalls)
			items, err := openTestStore(t, workspaceRoot, testReviewer).list()
			require.NoError(t, err)
			assert.Empty(t, items, "a rejected invocation must stage nothing")
		})
	}
}

// --- not found (exit 3) ---

func TestRunWorkComment_UnknownWorkBranch_ExitsThreeWithoutStaging(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	srv := missingWorkBranchServer()
	encoded, err := runComment(t, workspaceRoot, testReviewer, srv, "a comment", explicitArgs()...)
	require.Error(t, err)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
	items, err := openTestStore(t, workspaceRoot, testReviewer).list()
	require.NoError(t, err)
	assert.Empty(t, items, "a comment on a work branch that does not exist must not be staged")
}

func TestRunWorkComment_UnknownThread_ExitsThree(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	srv := newCommentServer(publishedThread("t1", testReviewer))
	encoded, err := runComment(t, workspaceRoot, testReviewer, srv, "", explicitArgs("--resolve", "t404")...)
	require.Error(t, err)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
	ce := mapCommandError(err)
	require.NotNil(t, ce)
	assert.Equal(t, codeNotFound, ce.code)
	assert.Nil(t, encoded)
	items, err := openTestStore(t, workspaceRoot, testReviewer).list()
	require.NoError(t, err)
	assert.Empty(t, items)
}

// TestRunWorkComment_UnknownStagedID_ExitsThree covers both operations that
// address an already-staged item by id.
func TestRunWorkComment_UnknownStagedID_ExitsThree(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		stdin string
		args  []string
	}{
		"edit":    {"a revision", explicitArgs("--edit", "s9")},
		"discard": {"", explicitArgs("--discard", "s9")},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			workspaceRoot := realTempDir(t)
			srv := newCommentServer()
			_, err := runComment(t, workspaceRoot, testReviewer, srv, "staged already", explicitArgs()...)
			require.NoError(t, err)
			encoded, err := runComment(t, workspaceRoot, testReviewer, srv, tt.stdin, tt.args...)
			require.Error(t, err)
			assert.Equal(t, 3, newErrorMapper().ExitCode(err))
			assert.Nil(t, encoded)
			items, err := openTestStore(t, workspaceRoot, testReviewer).list()
			require.NoError(t, err)
			assert.Len(t, items, 1, "the existing staged item must be untouched")
		})
	}
}

// --- author-only resolve (exit 2) ---

// TestRunWorkComment_ResolvingAnotherAgentsThread_ExitsTwo is
// reviewing.feature's "Only the thread's author may resolve it": the thread
// exists, so this is not a not_found — it is a refusal, exit 2, with
// nothing staged.
func TestRunWorkComment_ResolvingAnotherAgentsThread_ExitsTwo(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	srv := newCommentServer(publishedThread("t1", "alan-turing-4-reviewer"))
	encoded, err := runComment(t, workspaceRoot, testReviewer, srv, "", explicitArgs("--resolve", "t1")...)
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	ce := mapCommandError(err)
	require.NotNil(t, ce)
	assert.Equal(t, codeUnauthorized, ce.code)
	assert.Contains(t, ce.Error(), "only a thread's author may resolve it")
	assert.Nil(t, encoded)
	items, err := openTestStore(t, workspaceRoot, testReviewer).list()
	require.NoError(t, err)
	assert.Empty(t, items, "a refused resolve must stage nothing, not even a body")
}

// TestRunWorkComment_ResolvingAThreadIOnlyRepliedTo_ExitsTwo pins WHICH
// comment identifies the author: the one that OPENED the thread. Having
// replied to someone else's thread must not confer the right to resolve it.
func TestRunWorkComment_ResolvingAThreadIOnlyRepliedTo_ExitsTwo(t *testing.T) {
	t.Parallel()
	srv := newCommentServer(publishedThread("t1", "alan-turing-4-reviewer", testReviewer))
	encoded, err := runComment(t, realTempDir(t), testReviewer, srv, "", explicitArgs("--resolve", "t1")...)
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
}

// TestRunWorkComment_ResolvingAnAuthorlessThread_ExitsTwo proves the guard
// fails closed: a thread with no comments has no identifiable author, so it
// is refused rather than treated as anyone's to resolve.
func TestRunWorkComment_ResolvingAnAuthorlessThread_ExitsTwo(t *testing.T) {
	t.Parallel()
	srv := newCommentServer(&loamv1.Thread{Id: "t1"})
	encoded, err := runComment(t, realTempDir(t), testReviewer, srv, "", explicitArgs("--resolve", "t1")...)
	require.Error(t, err)
	assert.Equal(t, 2, newErrorMapper().ExitCode(err))
	assert.Nil(t, encoded)
}

// --- identity resolution and dispatch ---

// TestRunWorkComment_InfersIdentityFromWorkspaceWhenPositionalsOmitted
// proves the [repo] [work-branch] convention holds here too: run from
// inside the clone, both are inferred, and the staged item lands under the
// SAME key an explicit invocation would have used.
func TestRunWorkComment_InfersIdentityFromWorkspaceWhenPositionalsOmitted(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	srv := newCommentServer()
	var captured *loamv1.GetWorkBranchRequest
	srv.client.GetWorkBranchFunc = func(_ context.Context, req *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error) {
		captured = req.Msg
		return connect.NewResponse(&loamv1.GetWorkBranchResponse{WorkBranch: &loamv1.WorkBranch{Repo: testRepo, Name: testWorkBranch}}), nil
	}
	_, err := runComment(t, workspaceRoot, testReviewer, srv, "an inferred comment")
	require.NoError(t, err)
	require.NotNil(t, captured)
	assert.Equal(t, testRepo, captured.GetRepo())
	assert.Equal(t, testWorkBranch, captured.GetWorkBranch())
	items, err := openTestStore(t, workspaceRoot, testReviewer).list()
	require.NoError(t, err)
	require.Len(t, items, 1, "the inferred key must be the same staging key the explicit one resolves to")
}

// TestRouterDispatch_WorkComment_ReachesRealHandler proves `loam work
// comment` reaches this bead's handler rather than the errNotImplemented
// stub it replaced.
func TestRouterDispatch_WorkComment_ReachesRealHandler(t *testing.T) {
	t.Parallel()
	workspaceRoot := realTempDir(t)
	var encoded any
	deps := commentDeps(workspaceRoot, testReviewer, newCommentServer(), "a routed comment", &encoded)
	err := NewRouter(deps).Dispatch(t.Context(), []string{"work", "comment", testRepo, testWorkBranch, "--file", "auth.go", "--line", "7"})
	require.NoError(t, err)
	assert.Equal(t, stagedCommentOutput{Staged: true, ID: "s1", File: "auth.go", Line: 7, Body: "a routed comment"}, encoded)
}

// --- staging area failures ---

// TestRunWorkComment_UnopenableStagingArea_PropagatesTheClassification
// proves the command does not swallow or reclassify a containment refusal
// from OpenStaging — the one failure that means a write would have escaped
// the workspace.
func TestRunWorkComment_UnopenableStagingArea_PropagatesTheClassification(t *testing.T) {
	t.Parallel()
	srv := newCommentServer()
	ws := &WorkspaceResolverMock{
		OpenStagingFunc: func(string, string) (StagingArea, error) { return nil, errStagingArea },
	}
	var encoded any
	cfg := &ConfigMock{IdentifierFunc: func() string { return testReviewer }}
	connectClient := &ConnectClientMock{WorkBranchFunc: func() WorkBranchClient { return srv.client }}
	encoder := &OutputEncoderMock{EncodeFunc: func(v any) error { encoded = v; return nil }}
	deps := NewDeps(testLogger(), cfg, encoder, newErrorMapper(), ws, connectClient, nil, strings.NewReader("a comment"))
	err := runWorkComment(t.Context(), deps, explicitArgs())
	require.ErrorIs(t, err, errStagingArea)
	assert.Nil(t, encoded)
}

// --- resolveCommentMode and commentModeSkipsBody (loam-hi5o.6) ---

// flagsFor builds a commentFlags with every field set explicitly, so a
// table-driven test can construct one per case without depending on
// newWorkCommentFlags' pflag defaults.
func flagsFor(file string, line int, resolve, edit, discard string) *commentFlags {
	return &commentFlags{file: &file, line: &line, resolve: &resolve, edit: &edit, discard: &discard}
}

// TestResolveCommentMode_TruthTable pins resolveCommentMode's full decision
// table directly (acceptance criterion 5) -- independent of whether a real
// caller would have read stdin to produce body, since resolveCommentMode's
// signature and behaviour are unchanged by loam-hi5o.6 (criterion 2): it is
// still the single authority for what a given (flags, body) pair means,
// called with the real body whenever runWorkComment actually reads one.
func TestResolveCommentMode_TruthTable(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		flags   *commentFlags
		body    string
		wantErr bool
		want    commentMode
	}{
		"edit and discard together is an error regardless of body": {
			flagsFor("", 0, "", "s1", "s2"), "body", true, commentModeNew,
		},
		"edit with an anchor is an error": {flagsFor("auth.go", 0, "", "s1", ""), "body", true, commentModeNew},
		"edit with a line is an error":    {flagsFor("", 42, "", "s1", ""), "body", true, commentModeNew},
		"edit with a resolve is an error": {flagsFor("", 0, "t1", "s1", ""), "body", true, commentModeNew},
		"edit alone with no body is an error": {
			flagsFor("", 0, "", "s1", ""), "", true, commentModeNew,
		},
		"edit alone with a body is commentModeEdit": {
			flagsFor("", 0, "", "s1", ""), "revised", false, commentModeEdit,
		},
		"discard with an anchor is an error": {flagsFor("auth.go", 0, "", "", "s1"), "", true, commentModeNew},
		"discard with a resolve is an error": {flagsFor("", 0, "t1", "", "s1"), "", true, commentModeNew},
		"discard alone with a body is an error": {
			flagsFor("", 0, "", "", "s1"), "a body", true, commentModeNew,
		},
		"discard alone with no body is commentModeDiscard": {
			flagsFor("", 0, "", "", "s1"), "", false, commentModeDiscard,
		},
		"negative line is an error":              {flagsFor("", -3, "", "", ""), "body", true, commentModeNew},
		"line without file is an error":          {flagsFor("", 42, "", "", ""), "body", true, commentModeNew},
		"no body and no resolve is an error":     {flagsFor("", 0, "", "", ""), "", true, commentModeNew},
		"anchor with no body is an error":        {flagsFor("auth.go", 0, "", "", ""), "", true, commentModeNew},
		"anchor on a resolve-only is an error":   {flagsFor("auth.go", 0, "t1", "", ""), "", true, commentModeNew},
		"top-level comment with a body is legal": {flagsFor("", 0, "", "", ""), "a comment", false, commentModeNew},
		"anchored comment with a body is legal":  {flagsFor("auth.go", 42, "", "", ""), "a comment", false, commentModeNew},
		"resolve-only with no body is legal":     {flagsFor("", 0, "t1", "", ""), "", false, commentModeNew},
		"resolve with a body stages both":        {flagsFor("", 0, "t1", "", ""), "fixed", false, commentModeNew},
		"resolve and anchor and body is legal":   {flagsFor("auth.go", 3, "t1", "", ""), "fixed", false, commentModeNew},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveCommentMode(tt.flags, tt.body)
			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, 2, newErrorMapper().ExitCode(err))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestCommentModeSkipsBody_MatchesTheNarrowedContract pins
// commentModeSkipsBody's cases (loam-hi5o.6): true only for a lone
// --discard (with or without a conflicting --edit) and a lone --resolve
// (no --file/--line), false everywhere else -- forcing body="" through
// resolveCommentMode for every "false" case here would change the outcome
// a real body produces, which is exactly why those must still read stdin.
func TestCommentModeSkipsBody_MatchesTheNarrowedContract(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		flags *commentFlags
		want  bool
	}{
		"discard alone":                   {flagsFor("", 0, "", "", "s1"), true},
		"discard with edit conflict":      {flagsFor("", 0, "", "s2", "s1"), true},
		"discard with an anchor":          {flagsFor("auth.go", 0, "", "", "s1"), true},
		"discard with a resolve":          {flagsFor("", 0, "t1", "", "s1"), true},
		"edit alone":                      {flagsFor("", 0, "", "s1", ""), false},
		"edit with an anchor":             {flagsFor("auth.go", 0, "", "s1", ""), false},
		"resolve alone":                   {flagsFor("", 0, "t1", "", ""), true},
		"resolve with a file":             {flagsFor("auth.go", 0, "t1", "", ""), false},
		"resolve with a line":             {flagsFor("", 3, "t1", "", ""), false},
		"resolve with file and line":      {flagsFor("auth.go", 3, "t1", "", ""), false},
		"top-level comment, no flags set": {flagsFor("", 0, "", "", ""), false},
		"anchored comment, no resolve":    {flagsFor("auth.go", 3, "", "", ""), false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, commentModeSkipsBody(tt.flags))
		})
	}
}
