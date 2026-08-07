package cli

import (
	"fmt"
	"strings"
)

// diffStat is `work diff --stat`'s summary of a unified diff: which files
// changed and by how much.
//
// It is DERIVED from the very bytes of the patch the same call would
// otherwise return -- not fetched separately and not computed from a second
// source. That is deliberate: a diffstat obtained any other way could
// disagree with the patch, and two artifacts that can disagree about the
// same question are exactly the failure this bead exists to remove. This is
// a RENDERING of the diff, in the same sense `git diff --stat` is a
// rendering of `git diff`.
type diffStat struct {
	FilesChanged int `json:"files_changed"`
	Insertions   int `json:"insertions"`
	Deletions    int `json:"deletions"`
	// Truncated reports that the server capped the diff before this stat
	// was derived from it, so the counts below describe only the part that
	// arrived. See diffWasTruncated.
	Truncated bool           `json:"truncated"`
	Files     []diffStatFile `json:"files"`
}

// diffStatFile is one file's row in a diffStat. Binary files carry zero
// insertions and deletions -- git emits no per-line content for them -- and
// the flag is what keeps that from reading as "changed, but by nothing".
type diffStatFile struct {
	Path       string `json:"path"`
	Insertions int    `json:"insertions"`
	Deletions  int    `json:"deletions"`
	Binary     bool   `json:"binary"`
}

// diffTruncatedNeedle is the leading, invariant portion of the marker
// internal/gitdiff appends when its own maxDiffBytes cap binds
// (diffTruncatedMarkerFormat there: "\n... diff truncated at %d bytes; git
// produced more -- ..."). Matching the invariant prefix rather than the
// whole formatted string is what lets this survive a change to the byte
// count or to the advice that follows it.
const diffTruncatedNeedle = "\n... diff truncated at "

// diffTruncatedTailBytes bounds how much of the diff's END is searched for
// the marker. The marker is always the last thing in a truncated diff, so
// searching the tail alone is sufficient -- and it is what keeps a patch
// that merely CONTAINS that sentence somewhere in its own content (a change
// to internal/gitdiff's own source, say -- this is not hypothetical, that
// file is in this repository) from being misread as truncated.
const diffTruncatedTailBytes = 1024

// diffWasTruncated reports whether diff ends with internal/gitdiff's
// truncation marker. See diffTruncatedNeedle and diffTruncatedTailBytes for
// why it is a tail search for an invariant prefix rather than a whole-text
// match.
func diffWasTruncated(diff string) bool {
	tail := diff
	if len(tail) > diffTruncatedTailBytes {
		tail = tail[len(tail)-diffTruncatedTailBytes:]
	}
	return strings.Contains(tail, diffTruncatedNeedle)
}

// computeDiffStat parses a unified diff into a diffStat.
//
// The parse is anchored on `diff --git ` headers, so anything before the
// first one (and the truncation marker after the last hunk) contributes
// nothing. Within a file, +/- lines are counted only AFTER an `@@` hunk
// header has been seen, which is what keeps the `---`/`+++` file headers --
// themselves lines beginning with - and + -- from being counted as a
// deletion and an insertion of their own.
func computeDiffStat(diff string) diffStat {
	stat := diffStat{Files: []diffStatFile{}, Truncated: diffWasTruncated(diff)}
	current := -1
	inHunk := false
	for _, line := range strings.Split(diff, "\n") {
		if path, ok := strings.CutPrefix(line, "diff --git "); ok {
			stat.Files = append(stat.Files, diffStatFile{Path: pathFromDiffGitLine(path)})
			current = len(stat.Files) - 1
			inHunk = false
			continue
		}
		if current < 0 {
			continue
		}
		file := &stat.Files[current]
		switch {
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "GIT binary patch"):
			file.Binary = true
		case !inHunk && strings.HasPrefix(line, "+++ b/"):
			file.Path = trimGitHeaderPath(strings.TrimPrefix(line, "+++ b/"))
		case !inHunk && strings.HasPrefix(line, "rename to "):
			file.Path = strings.TrimPrefix(line, "rename to ")
		case inHunk && strings.HasPrefix(line, "+"):
			file.Insertions++
			stat.Insertions++
		case inHunk && strings.HasPrefix(line, "-"):
			file.Deletions++
			stat.Deletions++
		}
	}
	stat.FilesChanged = len(stat.Files)
	return stat
}

// trimGitHeaderPath strips the trailing TAB git appends to a `---`/`+++`
// header whose path contains a space.
//
// This is git's own disambiguation: those header lines are otherwise
// space-delimited (a `--- a/foo\t2026-08-07` form survives from the
// classic unified-diff timestamp field), so a path with a space in it is
// terminated with a tab to mark where it ends. Verified byte-for-byte
// against real git via `od -c`, and pinned by
// TestComputeDiffStat_RealGitOutput_SpacePathsCarryNoTrailingTab.
//
// Without this, the `+++ b/` override did not merely duplicate the other
// naming routes -- it OVERWROTE A CORRECT ANSWER WITH A WRONG ONE for
// every space-containing path, since pathFromDiffGitLine's equal-halves
// scan had already produced the untabbed name. `work diff --stat` reported
// those files with a trailing tab.
//
// Only the trailing tab is removed, never surrounding whitespace: a path
// may legitimately end in a space, and TrimSpace here would corrupt one to
// tidy the other.
func trimGitHeaderPath(path string) string {
	return strings.TrimSuffix(path, "\t")
}

// pathFromDiffGitLine recovers the changed path from the "a/<x> b/<y>"
// remainder of a `diff --git ` header. It is the FALLBACK naming route:
// computeDiffStat overrides it from `+++ b/<path>` or `rename to <path>`
// whenever the diff carries one, and those cover every textual change. This
// is what names a binary or mode-only change, which has neither.
//
// The remainder cannot simply be split on its first space: a path may
// contain spaces. The reliable structure is that a plain (non-rename)
// header repeats the same path twice, so the split is found by looking for
// the position where "a/<p> b/<p>" holds with identical halves. A rename
// header has different halves and falls through to the last " b/", which
// names the destination -- the same path `rename to` would have given.
func pathFromDiffGitLine(rest string) string {
	for i := 1; i < len(rest); i++ {
		if rest[i] != ' ' || !strings.HasPrefix(rest, "a/") || !strings.HasPrefix(rest[i+1:], "b/") {
			continue
		}
		if rest[2:i] == rest[i+3:] {
			return rest[i+3:]
		}
	}
	if idx := strings.LastIndex(rest, " b/"); idx >= 0 {
		return rest[idx+3:]
	}
	return rest
}

// humanDiffStat renders a diffStat the way `git diff --stat` renders its
// own: one line per file, then a summary. The per-file counts are written
// as explicit "+N -N" numbers rather than git's scaled +++--- bar, because
// the bar's scaling is lossy and this output exists to be CHECKED against a
// patch, not skimmed.
func humanDiffStat(stat diffStat) string {
	var b strings.Builder
	width := 0
	for _, file := range stat.Files {
		if len(file.Path) > width {
			width = len(file.Path)
		}
	}
	for _, file := range stat.Files {
		if file.Binary {
			fmt.Fprintf(&b, " %-*s | binary\n", width, file.Path)
			continue
		}
		fmt.Fprintf(&b, " %-*s | %d +%d -%d\n", width, file.Path, file.Insertions+file.Deletions, file.Insertions, file.Deletions)
	}
	fmt.Fprintf(&b, " %d file(s) changed, %d insertion(s)(+), %d deletion(s)(-)\n", stat.FilesChanged, stat.Insertions, stat.Deletions)
	if stat.Truncated {
		b.WriteString(" WARNING: the server truncated this diff; the counts above are partial\n")
	}
	return b.String()
}
