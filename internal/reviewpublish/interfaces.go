package reviewpublish

import (
	"context"

	"github.com/bobcob7/loam/internal/workbranchstore"
)

//go:generate go tool moq -out moq_test.go . AnchorChecker

// AnchorChecker resolves a file's line count in wb's OWN ref (never the
// target's) at the mirror's current tip -- what Publish validates every
// staged comment's file/line anchor against before writing a single thread
// (loam-hi5o.15: `loam work comment --line 270` against a ~100-line file
// used to be accepted with no validation anywhere, publishing a comment
// anchored past the end of the file). Defined here at the consumer, per
// repo convention; *gitanchor.Checker satisfies it in production, wired in
// cmd/server/main.go. Tests drive a moq mock instead.
//
// FileLineCount is the only method this package needs -- not "does this
// anchor validate", because Publish must check the SAME file's count
// against every comment that names it without re-reading the blob once per
// comment, and because the caller-vs-internal distinction (a missing file
// is the caller's mistake; a missing mirror is an operational fault) is
// this package's own call to make in validateAnchors, not something an
// opaque bool could carry.
type AnchorChecker interface {
	// FileLineCount returns file's line count in wb's work-branch ref at
	// the mirror's current tip. It returns gitanchor.ErrFileNotFound when
	// file is not a blob there (missing entirely, a directory, or a
	// submodule gitlink), gitanchor.ErrRefMissing when the work branch's
	// own ref is absent from the mirror, and gitanchor.ErrMirrorMissing
	// when the mirror itself is absent or invalid on disk.
	FileLineCount(ctx context.Context, wb workbranchstore.WorkBranch, file string) (int, error)
}
