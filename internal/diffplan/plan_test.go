package diffplan

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/mirrorpath"
)

// testLogger is the slog.Logger every test's Planner is built with:
// io.Discard, so tests stay quiet regardless of what Plan logs internally.
func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// sampleVersions is a fixed, valid Versions value most tests use as both
// stored and current, so only the field under test actually varies.
func sampleVersions() Versions {
	return Versions{Grammar: "tree-sitter-go@0.25.0", Pipeline: "v1", EmbeddingModel: "nomic-embed-text"}
}

// TestPlan_Incremental_ClassifiesAddModifyDeleteRename_UnchangedFileNeverAppears
// is the acceptance-criteria test: a diff with add/modify/delete/rename
// yields correct drop vs reparse classification, and a file untouched by
// the second commit never appears in either list. The rename target path
// contains a space (a real, if easy to mishandle, filename shape) to prove
// the tab-delimited rename record ("R100\told\tnew", three fields) parses
// correctly rather than silently dropping or truncating it.
func TestPlan_Incremental_ClassifiesAddModifyDeleteRename_UnchangedFileNeverAppears(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "keep.txt", "untouched\n", "init: keep")
	writeAndCommit(t, src, "modify_me.txt", "v1\n", "init: modify_me")
	writeAndCommit(t, src, "delete_me.txt", "bye\n", "init: delete_me")
	writeAndCommit(t, src, "old name.txt", "renamed\n", "init: old name")
	oldSHA := runGit(t, src, "rev-parse", "HEAD")
	writeAndCommit(t, src, "add_me.txt", "hello\n", "add_me")
	writeAndCommit(t, src, "modify_me.txt", "v2\n", "modify modify_me")
	removeAndCommit(t, src, "delete_me.txt", "delete delete_me")
	renameAndCommit(t, src, "old name.txt", "new name with space.txt", "rename old name")
	newSHA := runGit(t, src, "rev-parse", "HEAD")
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	bareCloneInto(t, src, mirrorDir)
	p := New(testLogger())
	req := Request{
		OldRef:          oldSHA,
		NewRef:          newSHA,
		RequestedKind:   ingest.KindIncremental,
		StoredVersions:  versionsPtr(sampleVersions()),
		CurrentVersions: sampleVersions(),
	}
	plan, err := p.Plan(t.Context(), mirrorDir, req)
	require.NoError(t, err)
	assert.Equal(t, ingest.KindIncremental, plan.Kind)
	assert.Empty(t, plan.Reason)
	assert.ElementsMatch(t, []string{"add_me.txt", "modify_me.txt", "new name with space.txt"}, plan.ReparseFiles)
	assert.ElementsMatch(t, []string{"delete_me.txt", "old name.txt"}, plan.DropFiles)
	assert.NotContains(t, plan.ReparseFiles, "keep.txt")
	assert.NotContains(t, plan.DropFiles, "keep.txt")
}

// versionsPtr is a small helper so call sites can take the address of a
// Versions value literal inline.
func versionsPtr(v Versions) *Versions { return &v }

// TestPlan_NoChanges_ReturnsEmptyIncrementalPlan proves an old_ref/new_ref
// pair with an identical tree (nothing changed) is a valid, empty
// incremental Plan, not an error and not a spurious escalation.
func TestPlan_NoChanges_ReturnsEmptyIncrementalPlan(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "f.txt", "one\n", "init")
	sha := runGit(t, src, "rev-parse", "HEAD")
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	bareCloneInto(t, src, mirrorDir)
	p := New(testLogger())
	req := Request{
		OldRef: sha, NewRef: sha, RequestedKind: ingest.KindIncremental,
		StoredVersions: versionsPtr(sampleVersions()), CurrentVersions: sampleVersions(),
	}
	plan, err := p.Plan(t.Context(), mirrorDir, req)
	require.NoError(t, err)
	assert.Equal(t, ingest.KindIncremental, plan.Kind)
	assert.Empty(t, plan.DropFiles)
	assert.Empty(t, plan.ReparseFiles)
}

// TestBuildIncrementalPlan_CopyStatus_TreatedLikeRename is a whitebox test:
// real `git diff --name-status` never emits a C (copy) record unless
// --find-copies-harder is passed (verified empirically against real git
// 2.50.1: plain `git diff --name-status`, and even `-C` alone without
// --find-copies-harder, both report a copied-then-unrelatedly-added file as
// a plain "A", never "C"), and this package never passes that flag -- so a
// real C record cannot be produced through Planner.Plan for this test to
// drive end to end. Exercising buildIncrementalPlan directly proves the
// classification this package's DESIGN calls for regardless: a copy is
// handled exactly like a rename (drop the old path, reparse the new one),
// defensively, in case a future change ever enables copy detection.
func TestBuildIncrementalPlan_CopyStatus_TreatedLikeRename(t *testing.T) {
	t.Parallel()
	changes := []fileChange{{status: 'C', oldPath: "orig.txt", newPath: "copy.txt"}}
	plan := buildIncrementalPlan(changes)
	assert.Equal(t, []string{"orig.txt"}, plan.DropFiles)
	assert.Equal(t, []string{"copy.txt"}, plan.ReparseFiles)
}

// TestPlan_RequestedFull_ReturnsWholeTreeReparse_NoDropFiles_NoEscalationReason
// proves a caller-requested KindFull is honored directly: the whole tree at
// NewRef becomes ReparseFiles, DropFiles stays nil (a full plan's drop is
// repo-scoped, not file-scoped -- see Plan's own doc comment), and Reason
// is empty because this was not an escalation, it is what was asked for.
func TestPlan_RequestedFull_ReturnsWholeTreeReparse_NoDropFiles_NoEscalationReason(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "a.txt", "1\n", "a")
	writeAndCommit(t, src, "dir/b.txt", "2\n", "b")
	sha := runGit(t, src, "rev-parse", "HEAD")
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	bareCloneInto(t, src, mirrorDir)
	p := New(testLogger())
	req := Request{OldRef: "", NewRef: sha, RequestedKind: ingest.KindFull, CurrentVersions: sampleVersions()}
	plan, err := p.Plan(t.Context(), mirrorDir, req)
	require.NoError(t, err)
	assert.Equal(t, ingest.KindFull, plan.Kind)
	assert.Empty(t, plan.Reason)
	assert.Nil(t, plan.DropFiles)
	assert.ElementsMatch(t, []string{"a.txt", "dir/b.txt"}, plan.ReparseFiles)
}

// TestPlan_FirstIngest_NoOldRef_EscalatesToFullWithWholeTree proves the
// "first ingest of a repo" full-rebuild trigger (docs/ingestion-spec.md
// "Incremental Build" -> "Full rebuild"): an incremental request with no
// OldRef is detected via Request.OldRef == "" and escalated, never
// treated as (say) an empty diff.
func TestPlan_FirstIngest_NoOldRef_EscalatesToFullWithWholeTree(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "a.txt", "1\n", "a")
	sha := runGit(t, src, "rev-parse", "HEAD")
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	bareCloneInto(t, src, mirrorDir)
	p := New(testLogger())
	req := Request{OldRef: "", NewRef: sha, RequestedKind: ingest.KindIncremental, CurrentVersions: sampleVersions()}
	plan, err := p.Plan(t.Context(), mirrorDir, req)
	require.NoError(t, err)
	assert.Equal(t, ingest.KindFull, plan.Kind)
	assert.Contains(t, plan.Reason, "first ingest")
	assert.ElementsMatch(t, []string{"a.txt"}, plan.ReparseFiles)
}

// TestPlan_VersionMismatch_EscalatesToFull proves the "grammar/pipeline
// version bump, or an embedding-model change" full-rebuild trigger
// (docs/ingestion-spec.md "Incremental Build" -> "Full rebuild"): stored
// versions differing from current versions escalates even though OldRef is
// present and would otherwise diff cleanly.
func TestPlan_VersionMismatch_EscalatesToFull(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "a.txt", "1\n", "a")
	oldSHA := runGit(t, src, "rev-parse", "HEAD")
	writeAndCommit(t, src, "a.txt", "2\n", "a2")
	newSHA := runGit(t, src, "rev-parse", "HEAD")
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	bareCloneInto(t, src, mirrorDir)
	p := New(testLogger())
	stored := sampleVersions()
	stored.EmbeddingModel = "mxbai-embed-large"
	req := Request{
		OldRef: oldSHA, NewRef: newSHA, RequestedKind: ingest.KindIncremental,
		StoredVersions: &stored, CurrentVersions: sampleVersions(),
	}
	plan, err := p.Plan(t.Context(), mirrorDir, req)
	require.NoError(t, err)
	assert.Equal(t, ingest.KindFull, plan.Kind)
	assert.Contains(t, plan.Reason, "differ from current")
}

// TestPlan_NoStoredVersions_EscalatesToFull proves a nil StoredVersions
// (never recorded) is treated the same as a mismatch, per Request.
// StoredVersions' own doc comment -- Plan cannot certify incremental reuse
// is safe without a recorded baseline to compare against.
func TestPlan_NoStoredVersions_EscalatesToFull(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "a.txt", "1\n", "a")
	oldSHA := runGit(t, src, "rev-parse", "HEAD")
	writeAndCommit(t, src, "a.txt", "2\n", "a2")
	newSHA := runGit(t, src, "rev-parse", "HEAD")
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	bareCloneInto(t, src, mirrorDir)
	p := New(testLogger())
	req := Request{
		OldRef: oldSHA, NewRef: newSHA, RequestedKind: ingest.KindIncremental,
		StoredVersions: nil, CurrentVersions: sampleVersions(),
	}
	plan, err := p.Plan(t.Context(), mirrorDir, req)
	require.NoError(t, err)
	assert.Equal(t, ingest.KindFull, plan.Kind)
	assert.Contains(t, plan.Reason, "no stored")
}

// TestPlan_UnrelatedHistories_NoMergeBase_EscalatesToFull proves the "no
// valid diff base" full-rebuild trigger for the unrelated-histories shape:
// old and new share no common ancestor at all (an orphan branch, standing
// in for a force-pushed history rewrite -- docs/ingestion-spec.md
// "Incremental Build" -> "Full rebuild": "no valid diff base (force-push,
// history rewrite, shallow/reset ref)"). Asserting the FULL Plan's whole
// tree (not an empty incremental diff, which real `git diff old..new`
// WOULD produce for unrelated histories -- verified empirically that the
// two-dot form succeeds without a merge base, unlike three-dot) is exactly
// what would catch a mutant that dropped the merge-base check and let an
// ordinary incremental diff through instead.
func TestPlan_UnrelatedHistories_NoMergeBase_EscalatesToFull(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "a.txt", "1\n", "a")
	oldSHA := runGit(t, src, "rev-parse", "HEAD")
	runGit(t, src, "checkout", "--quiet", "--orphan", "unrelated")
	runGit(t, src, "rm", "-rf", "--quiet", ".")
	writeAndCommit(t, src, "b.txt", "unrelated\n", "unrelated init")
	newSHA := runGit(t, src, "rev-parse", "HEAD")
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	bareCloneInto(t, src, mirrorDir)
	p := New(testLogger())
	req := Request{
		OldRef: oldSHA, NewRef: newSHA, RequestedKind: ingest.KindIncremental,
		StoredVersions: versionsPtr(sampleVersions()), CurrentVersions: sampleVersions(),
	}
	plan, err := p.Plan(t.Context(), mirrorDir, req)
	require.NoError(t, err)
	assert.Equal(t, ingest.KindFull, plan.Kind)
	assert.Contains(t, plan.Reason, "no valid diff base")
	assert.ElementsMatch(t, []string{"b.txt"}, plan.ReparseFiles, "a real full rebuild must list the FULL new tree, not an empty/partial incremental diff")
}

// TestPlan_OldRefUnresolvable_EscalatesToFull proves the other "no valid
// diff base" shape: OldRef no longer resolves to any object in the mirror
// at all -- standing in for a commit pruned after loam-giq.2's forced,
// pruning mirror fetch following an upstream force-push (the object can be
// gone entirely, not merely unreachable-but-present). A well-formed but
// never-committed SHA is used rather than a real prune, which would need an
// actual `git gc --prune=now` cycle to reproduce reliably in a test.
func TestPlan_OldRefUnresolvable_EscalatesToFull(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "a.txt", "1\n", "a")
	newSHA := runGit(t, src, "rev-parse", "HEAD")
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	bareCloneInto(t, src, mirrorDir)
	p := New(testLogger())
	const neverCommittedSHA = "0123456789abcdef0123456789abcdef01234567"
	req := Request{
		OldRef: neverCommittedSHA, NewRef: newSHA, RequestedKind: ingest.KindIncremental,
		StoredVersions: versionsPtr(sampleVersions()), CurrentVersions: sampleVersions(),
	}
	plan, err := p.Plan(t.Context(), mirrorDir, req)
	require.NoError(t, err)
	assert.Equal(t, ingest.KindFull, plan.Kind)
	assert.Contains(t, plan.Reason, "no valid diff base")
}

// TestPlan_NewRefMissing_ReturnsErrRefMissing proves NewRef itself failing
// to resolve is a hard error (ErrRefMissing), never silently escalated the
// way OldRef's unresolvability is -- Request.NewRef's doc comment: this
// signals a caller/environment fault (the live mirror tip resolution the
// Orchestrator is responsible for went wrong), not a routine ingest
// condition.
func TestPlan_NewRefMissing_ReturnsErrRefMissing(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "a.txt", "1\n", "a")
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	bareCloneInto(t, src, mirrorDir)
	p := New(testLogger())
	req := Request{OldRef: "", NewRef: "no-such-ref", RequestedKind: ingest.KindIncremental, CurrentVersions: sampleVersions()}
	_, err := p.Plan(t.Context(), mirrorDir, req)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRefMissing)
}

// TestPlan_MirrorMissingOnDisk_ReturnsErrMirrorMissing proves a repo whose
// bare mirror does not exist on disk fails as ErrMirrorMissing -- a hard
// operational fault, since nothing (incremental or full) can be planned
// without a real mirror.
func TestPlan_MirrorMissingOnDisk_ReturnsErrMirrorMissing(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir() // no mirrors/ directory ever created under it
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	p := New(testLogger())
	req := Request{OldRef: "", NewRef: "main", RequestedKind: ingest.KindIncremental, CurrentVersions: sampleVersions()}
	_, err := p.Plan(t.Context(), mirrorDir, req)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMirrorMissing)
}

// TestPlan_UsesGitDirNotDashC_UpwardDiscoveryHazard reproduces, against
// real git, the `-C`-vs-`--git-dir` hazard internal/gitdiff's own test of
// the same name documents (loam-ofg.19's review): given a directory that
// EXISTS but is not itself a valid git repository, `-C` chdirs into it and
// walks UP looking for an enclosing repository, silently operating on
// whatever it finds instead of failing. outer is a real, unrelated repo one
// level above the "mirror" path with a DIFFERENT file at its tip than the
// fixture actually being diffed -- so a mutant swapping --git-dir for -C
// anywhere in this package's run() would SUCCEED with outer's wrong answer
// (a plausible-looking whole-tree listing containing WRONG_REPO_MARKER.txt)
// instead of this package's correct ErrMirrorMissing, which is exactly why
// this must be caught by an assertion on the returned error, not a panic.
func TestPlan_UsesGitDirNotDashC_UpwardDiscoveryHazard(t *testing.T) {
	t.Parallel()
	outer := t.TempDir()
	newWorkingRepo(t, outer)
	writeAndCommit(t, outer, "WRONG_REPO_MARKER.txt", "outer content\n", "outer init")
	mirrorDir := filepath.Join(outer, "mirrors", "acme", "widgets.git")
	require.NoError(t, os.MkdirAll(mirrorDir, 0o755))
	p := New(testLogger())
	req := Request{OldRef: "", NewRef: "main", RequestedKind: ingest.KindFull, CurrentVersions: sampleVersions()}
	plan, err := p.Plan(t.Context(), mirrorDir, req)
	require.Error(t, err, "a correct --git-dir addressing must fail on a non-repository path instead of silently escaping to the enclosing repo")
	assert.ErrorIs(t, err, ErrMirrorMissing)
	assert.Empty(t, plan.ReparseFiles)
	assert.NotContains(t, plan.ReparseFiles, "WRONG_REPO_MARKER.txt")
}

// TestPlan_TooManyChangedFiles_EscalatesToFull proves the "too many changed
// files to be worth incrementalising" trigger this package adds beyond
// docs/ingestion-spec.md's own list (see package doc comment: not
// spec-pinned, this package's own judgment call). maxIncrementalChanges is
// overridden down to a small number -- a package-level var exactly so a
// test can do this instead of materializing thousands of real fixture
// files, the same technique internal/gitdiff.maxDiffBytes uses.
func TestPlan_TooManyChangedFiles_EscalatesToFull(t *testing.T) {
	old := maxIncrementalChanges
	maxIncrementalChanges = 2
	t.Cleanup(func() { maxIncrementalChanges = old })
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "a.txt", "1\n", "a")
	oldSHA := runGit(t, src, "rev-parse", "HEAD")
	writeAndCommit(t, src, "b.txt", "1\n", "b")
	writeAndCommit(t, src, "c.txt", "1\n", "c")
	writeAndCommit(t, src, "d.txt", "1\n", "d")
	newSHA := runGit(t, src, "rev-parse", "HEAD")
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	bareCloneInto(t, src, mirrorDir)
	p := New(testLogger())
	req := Request{
		OldRef: oldSHA, NewRef: newSHA, RequestedKind: ingest.KindIncremental,
		StoredVersions: versionsPtr(sampleVersions()), CurrentVersions: sampleVersions(),
	}
	plan, err := p.Plan(t.Context(), mirrorDir, req)
	require.NoError(t, err)
	assert.Equal(t, ingest.KindFull, plan.Kind)
	assert.Contains(t, plan.Reason, "exceeds incremental threshold")
}

// TestPlan_FilenameWithNewline_ParsedCorrectly_RequiresDashZ proves -z is
// load-bearing, not decorative: a real filename containing a literal
// newline byte (a real, if rare, possibility on both Linux and macOS
// filesystems -- only '/' and NUL are actually forbidden in a path
// component) must still be parsed as an ordinary added file, in an
// ordinary incremental Plan.
//
// The exact failure mode a "drop -z" mutant hits here, verified empirically
// against real git 2.50.1 rather than assumed: without -z, git does NOT
// simply emit the raw newline byte into name-status's tab/newline-delimited
// text (which really would shift every subsequent record, as this
// package's own diffNameStatus doc comment describes) -- it instead
// C-quotes the whole path in double quotes with the newline escaped to a
// literal two-character "\n" ('git diff --name-status' output, e.g.
// `A\t"weird\nname.txt"`), REGARDLESS of core.quotepath (which only
// affects non-ASCII bytes, not control characters). This package's parser
// never unescapes that quoting, so consuming the non-z form here would
// mean parseNameStatusZ receives a status line with no NUL separators at
// all: bytes.Split on a separator that never occurs returns the entire
// blob as a single field, which fails this package's own field-count
// validation and gets classified as unparseable -- observable here as
// Kind flipping from the correct KindIncremental to KindFull, not a panic
// and not a silently-wrong path list (ls-tree, unaffected by this mutation,
// would still correctly list the real file in a full plan's ReparseFiles,
// so asserting Kind is what actually catches the mutation).
func TestPlan_FilenameWithNewline_ParsedCorrectly_RequiresDashZ(t *testing.T) {
	t.Parallel()
	const weirdName = "weird\nname.txt"
	src := t.TempDir()
	newWorkingRepo(t, src)
	writeAndCommit(t, src, "a.txt", "1\n", "init")
	oldSHA := runGit(t, src, "rev-parse", "HEAD")
	full := filepath.Join(src, weirdName)
	require.NoError(t, os.WriteFile(full, []byte("content\n"), 0o644))
	runGit(t, src, "add", "-A")
	runGit(t, src, "commit", "--quiet", "-m", "add weird name")
	newSHA := runGit(t, src, "rev-parse", "HEAD")
	dataDir := t.TempDir()
	mirrorDir := mirrorpath.Dir(dataDir, "acme/widgets")
	bareCloneInto(t, src, mirrorDir)
	p := New(testLogger())
	req := Request{
		OldRef: oldSHA, NewRef: newSHA, RequestedKind: ingest.KindIncremental,
		StoredVersions: versionsPtr(sampleVersions()), CurrentVersions: sampleVersions(),
	}
	plan, err := p.Plan(t.Context(), mirrorDir, req)
	require.NoError(t, err)
	assert.Equal(t, ingest.KindIncremental, plan.Kind, "a correctly-parsed diff over this fixture is an ordinary incremental change, not a fallback")
	assert.Contains(t, plan.ReparseFiles, weirdName)
}

// TestParseNameStatusZ_UnrecognizedStatus_ReturnsUnparseableError is a
// whitebox test of the defensive default case: a status letter this
// package does not know how to classify (e.g. 'U', unmerged -- which
// cannot occur in a two-commit `git diff` but is a real name-status letter
// git documents) is reported as an error rather than silently ignored or
// guessed at, so Plan's caller (diffNameStatus) can turn it into a
// full-rebuild escalation instead of corrupting the incremental Plan.
func TestParseNameStatusZ_UnrecognizedStatus_ReturnsUnparseableError(t *testing.T) {
	t.Parallel()
	input := []byte("U\x00mystery.txt\x00")
	_, err := parseNameStatusZ(input)
	require.Error(t, err)
	assert.ErrorIs(t, err, errUnparseableStatus)
}

// TestParseNameStatusZ_RenameMissingNewPathField_ReturnsUnparseableError
// whitebox-proves the three-field rename record's validation: a truncated
// stream ending right after a rename status's old path (no new path field
// at all) is reported as unparseable, not read past the end of the fields
// slice or silently treated as a two-field delete.
func TestParseNameStatusZ_RenameMissingNewPathField_ReturnsUnparseableError(t *testing.T) {
	t.Parallel()
	input := []byte("R100\x00old.txt\x00")
	_, err := parseNameStatusZ(input)
	require.Error(t, err)
	assert.ErrorIs(t, err, errUnparseableStatus)
}

// TestParseNameStatusZ_Empty_ReturnsNoChangesNoError proves an empty diff's
// raw output (git prints nothing at all, not even a lone NUL) parses to a
// nil, error-free change list.
func TestParseNameStatusZ_Empty_ReturnsNoChangesNoError(t *testing.T) {
	t.Parallel()
	changes, err := parseNameStatusZ(nil)
	require.NoError(t, err)
	assert.Empty(t, changes)
}
