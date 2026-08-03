package reviewpublish

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/gitanchor"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// These exercise validateAnchors directly, against a mocked AnchorChecker,
// with no pool and no Postgres -- the previous session's own recorded
// intent: a fast, container-free test of the line-arithmetic boundaries
// that loam-hi5o.15 is about, distinct from integration_test.go's
// end-to-end proof that a rejection rolls back everything Publish wrote.
// validateAnchors never touches p.pool or p.logger, so a bare
// &Publisher{anchors: mock} is enough to call it.

// ptr32 is a tiny helper so table rows can take an int32 literal directly.
func ptr32(n int32) *int32 { return &n }

// TestValidateAnchors_LineBoundaries is loam-hi5o.15's acceptance criteria
// 1, 2 and 6 in one table, deliberately using a NON-round file length (53,
// not 100 or 50) so a mutant that is off by one, or that treats the file
// length itself as a round number, cannot pass by coincidence. Every
// boundary the bead's own instructions name is here: length (accept),
// length+1 (reject), 1 (accept), 0 (reject), and a negative line (reject,
// reachable via the proto uint32->int32 overflow validateAnchors' own doc
// comment describes).
func TestValidateAnchors_LineBoundaries(t *testing.T) {
	t.Parallel()
	const fileLen = 53
	for _, tc := range []struct {
		name    string
		line    int32
		wantErr error
	}{
		{name: "exactly the file's length is accepted", line: fileLen, wantErr: nil},
		{name: "one beyond the file's length is rejected", line: fileLen + 1, wantErr: ErrAnchorLineOutOfRange},
		{name: "the first line is accepted", line: 1, wantErr: nil},
		{name: "line zero is rejected", line: 0, wantErr: ErrAnchorLineInvalid},
		{name: "a negative line is rejected", line: -1, wantErr: ErrAnchorLineInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			file := "f.go"
			anchors := &AnchorCheckerMock{
				FileLineCountFunc: func(context.Context, workbranchstore.WorkBranch, string) (int, error) {
					return fileLen, nil
				},
			}
			p := &Publisher{anchors: anchors}
			line := tc.line
			err := p.validateAnchors(t.Context(), workbranchstore.WorkBranch{}, []NewComment{{File: &file, Line: &line, Body: "x"}})
			if tc.wantErr == nil {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// TestValidateAnchors_EmptyFile_OnlyLineOneIsRejected covers the empty-file
// boundary the bead's instructions call out by name: an empty file has
// zero lines, so even --line 1 must be rejected as out of range, not
// accepted as "the first line of nothing."
func TestValidateAnchors_EmptyFile_OnlyLineOneIsRejected(t *testing.T) {
	t.Parallel()
	file := "empty.txt"
	anchors := &AnchorCheckerMock{
		FileLineCountFunc: func(context.Context, workbranchstore.WorkBranch, string) (int, error) {
			return 0, nil
		},
	}
	p := &Publisher{anchors: anchors}
	line := int32(1)
	err := p.validateAnchors(t.Context(), workbranchstore.WorkBranch{}, []NewComment{{File: &file, Line: &line, Body: "x"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAnchorLineOutOfRange)
}

// TestValidateAnchors_FileNotFound_ClassifiedAsErrAnchorFileNotFound proves
// classifyAnchorErr's translation of gitanchor.ErrFileNotFound into this
// package's own ErrAnchorFileNotFound, without a real mirror -- criterion 3.
func TestValidateAnchors_FileNotFound_ClassifiedAsErrAnchorFileNotFound(t *testing.T) {
	t.Parallel()
	file := "never-committed.go"
	anchors := &AnchorCheckerMock{
		FileLineCountFunc: func(context.Context, workbranchstore.WorkBranch, string) (int, error) {
			return 0, gitanchor.ErrFileNotFound
		},
	}
	p := &Publisher{anchors: anchors}
	err := p.validateAnchors(t.Context(), workbranchstore.WorkBranch{}, []NewComment{{File: &file, Body: "x"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAnchorFileNotFound)
}

// TestValidateAnchors_OtherAnchorCheckerFailure_PassedThroughUnclassified
// proves an AnchorChecker failure that is NEITHER a file-not-found NOR a
// caller mistake (gitanchor.ErrMirrorMissing, an operational fault) is
// wrapped with the file for context but left as its own error, not folded
// into either ErrAnchorFileNotFound or ErrAnchorLineOutOfRange -- so the
// handler's own mapping still sees ErrMirrorMissing and can classify it
// correctly instead of reporting a caller mistake that was really an
// operational fault.
func TestValidateAnchors_OtherAnchorCheckerFailure_PassedThroughUnclassified(t *testing.T) {
	t.Parallel()
	file := "f.go"
	anchors := &AnchorCheckerMock{
		FileLineCountFunc: func(context.Context, workbranchstore.WorkBranch, string) (int, error) {
			return 0, gitanchor.ErrMirrorMissing
		},
	}
	p := &Publisher{anchors: anchors}
	err := p.validateAnchors(t.Context(), workbranchstore.WorkBranch{}, []NewComment{{File: &file, Body: "x"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, gitanchor.ErrMirrorMissing)
	assert.NotErrorIs(t, err, ErrAnchorFileNotFound, "a missing mirror is an operational fault, not the caller's mistake")
}

// TestValidateAnchors_ZeroLine_NeverConsultsTheMirror proves the <= 0 check
// runs before any AnchorChecker call: FileLineCountFunc is left nil, which
// moq panics on if invoked, so a regression that reordered the checks would
// fail this test on the panic rather than merely on a wrong error.
func TestValidateAnchors_ZeroLine_NeverConsultsTheMirror(t *testing.T) {
	t.Parallel()
	file := "f.go"
	p := &Publisher{anchors: &AnchorCheckerMock{}}
	line := int32(0)
	err := p.validateAnchors(t.Context(), workbranchstore.WorkBranch{}, []NewComment{{File: &file, Line: &line, Body: "x"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAnchorLineInvalid)
}

// TestValidateAnchors_UnanchoredComment_NeverConsultsTheMirror proves a
// top-level thread (File == nil) is skipped outright: FileLineCountFunc is
// left nil, which would panic if validateAnchors ever called it for this
// comment.
func TestValidateAnchors_UnanchoredComment_NeverConsultsTheMirror(t *testing.T) {
	t.Parallel()
	p := &Publisher{anchors: &AnchorCheckerMock{}}
	err := p.validateAnchors(t.Context(), workbranchstore.WorkBranch{}, []NewComment{{Body: "no file, no line"}})
	assert.NoError(t, err)
}

// TestValidateAnchors_WholeFileComment_ValidatesFileButNotLine proves a
// File set with Line nil only needs the file to exist -- the mock is
// consulted (proving the file itself is checked) but no line comparison
// ever runs, regardless of the count it returns.
func TestValidateAnchors_WholeFileComment_ValidatesFileButNotLine(t *testing.T) {
	t.Parallel()
	file := "f.go"
	calls := 0
	anchors := &AnchorCheckerMock{
		FileLineCountFunc: func(context.Context, workbranchstore.WorkBranch, string) (int, error) {
			calls++
			return 0, nil
		},
	}
	p := &Publisher{anchors: anchors}
	err := p.validateAnchors(t.Context(), workbranchstore.WorkBranch{}, []NewComment{{File: &file, Body: "whole file"}})
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "the file itself must still be checked")
}

// TestValidateAnchors_SameFileAcrossComments_CountsOncePerFile proves the
// per-call cache: several comments naming the SAME file cost exactly one
// AnchorChecker.FileLineCount call, not one per comment, matching
// validateAnchors' own doc comment about a real review's typical batch.
func TestValidateAnchors_SameFileAcrossComments_CountsOncePerFile(t *testing.T) {
	t.Parallel()
	file := "f.go"
	calls := 0
	anchors := &AnchorCheckerMock{
		FileLineCountFunc: func(context.Context, workbranchstore.WorkBranch, string) (int, error) {
			calls++
			return 10, nil
		},
	}
	p := &Publisher{anchors: anchors}
	line1, line2, line3 := int32(1), int32(5), int32(10)
	err := p.validateAnchors(t.Context(), workbranchstore.WorkBranch{}, []NewComment{
		{File: &file, Line: &line1, Body: "a"},
		{File: &file, Line: &line2, Body: "b"},
		{File: &file, Line: &line3, Body: "c"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "the same file's line count must be looked up once per Publish call, not once per comment")
}

// TestValidateAnchors_SecondCommentOnSameFile_CacheHitStillRangeChecked is
// this bead's own failure mode, reproduced directly: `--line 270` against a
// ~100-line file is exactly what a SECOND comment on the same file looks
// like once the first comment has already populated lineCounts. Every other
// out-of-range test in this file (and integration_test.go's
// TestPublish_AnchorOneBeyondBoundary_RejectsWholeVerdict_NothingPersists)
// puts its bad anchor on a file no earlier comment named, so the range
// check only ever ran on a cache MISS there -- an `if ok { continue }`
// short-circuit added to the cache-hit branch would compile, leave every
// other test in this package and the integration suite green, and
// reproduce the published bug for exactly the ordinary shape of a real
// review: several comments on one file.
func TestValidateAnchors_SecondCommentOnSameFile_CacheHitStillRangeChecked(t *testing.T) {
	t.Parallel()
	file := "f.go"
	anchors := &AnchorCheckerMock{
		FileLineCountFunc: func(context.Context, workbranchstore.WorkBranch, string) (int, error) {
			return 100, nil
		},
	}
	p := &Publisher{anchors: anchors}
	validLine := int32(1)
	badLine := int32(270)
	err := p.validateAnchors(t.Context(), workbranchstore.WorkBranch{}, []NewComment{
		{File: &file, Line: &validLine, Body: "a cache-populating first comment"},
		{File: &file, Line: &badLine, Body: "a cache-HIT second comment, still out of range"},
	})
	require.Error(t, err, "the second comment's cache-hit lookup must still be range-checked, not skipped")
	assert.ErrorIs(t, err, ErrAnchorLineOutOfRange)
	assert.Contains(t, err.Error(), "100 line", "the error must still name the file's actual length on a cache hit")
}

// TestValidateAnchors_SeveralBadAnchors_AllReportedInOneCall proves
// validateAnchors does not stop at the first bad anchor: three comments,
// each wrong in a DIFFERENT way (a zero line, a line beyond the file's
// length, and a file that does not exist at the tip), must all surface in
// the one error this call returns -- not just whichever one happened to be
// checked first. Without this, a reviewer with several wrong anchors in one
// batch learns about them one `work verdict` round trip at a time.
func TestValidateAnchors_SeveralBadAnchors_AllReportedInOneCall(t *testing.T) {
	t.Parallel()
	goodFile, outOfRangeFile, missingFile := "good.go", "toolong.go", "gone.go"
	anchors := &AnchorCheckerMock{
		FileLineCountFunc: func(_ context.Context, _ workbranchstore.WorkBranch, file string) (int, error) {
			switch file {
			case outOfRangeFile:
				return 10, nil
			case missingFile:
				return 0, gitanchor.ErrFileNotFound
			default:
				t.Fatalf("unexpected FileLineCount(%q)", file)
				return 0, nil
			}
		},
	}
	p := &Publisher{anchors: anchors}
	zeroLine := int32(0)
	tooFarLine := int32(999)
	err := p.validateAnchors(t.Context(), workbranchstore.WorkBranch{}, []NewComment{
		{File: &goodFile, Line: &zeroLine, Body: "a zero line"},
		{File: &outOfRangeFile, Line: &tooFarLine, Body: "way past the end"},
		{File: &missingFile, Body: "a file that isn't there"},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAnchorLineInvalid, "the zero-line comment's own failure must survive")
	assert.ErrorIs(t, err, ErrAnchorLineOutOfRange, "the out-of-range comment's own failure must survive")
	assert.ErrorIs(t, err, ErrAnchorFileNotFound, "the missing-file comment's own failure must survive")
	assert.Contains(t, err.Error(), goodFile)
	assert.Contains(t, err.Error(), outOfRangeFile)
	assert.Contains(t, err.Error(), missingFile)
}
