package mirrorsync

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// allRefsRefspec is the wildcard positive refspec MirrorFetcher pairs with
// a negative exclusion per registered work-branch ref (docs/sync-spec.md
// -> Mirror Sync step 1). The leading '+' is NOT what makes a diverged or
// force-pushed upstream ref win here: gittransport.Transport.Fetch already
// passes the git-level --force flag unconditionally, which (per
// git-fetch(1) -f/--force) overrides the non-fast-forward check for every
// refspec regardless of a per-refspec '+' -- dropping the '+' leaves every
// integration test in this package green; only the refspec-string unit
// tests catch it. The '+' is kept anyway because it is what
// docs/sync-spec.md's DESIGN mandates verbatim and it is exactly the
// refspec `git clone --mirror` itself uses, so intent stays correct even
// if Transport ever stops passing --force: belt-and-suspenders notation,
// not the forcing mechanism. The sharper unconditional flag actually in
// play is --prune, which is safe to run unconditionally here only because
// git scopes pruning to the refspec's own destination namespace -- a ref
// a negative exclusion removes from the refspec is never a prune
// candidate either, exactly as it is never a fetch candidate.
const allRefsRefspec = "+refs/*:refs/*"

// MirrorFetcher is the production Fetcher (docs/sync-spec.md -> Mirror
// Sync step 1; docs/git-spec.md -> Ref Policy "Work-branch refs"; owned by
// bead giq.2): a forced, pruning fetch of every upstream ref into a repo's
// bare mirror, upstream-wins, with every currently registered work-branch
// ref excluded from the refspec so a same-named upstream branch can never
// clobber one of Loam's own. The exclusion list is recomputed from
// work_branches immediately before every call to Fetch, since branches
// come and go between sync ticks.
//
// MirrorFetcher builds refspecs and turns gittransport's returned
// --porcelain output back into a FetchResult; it never shells out to git
// itself -- that stays inside upstream (in production,
// *gittransport.Transport), the seam that owns credential injection,
// config isolation, and secret scrubbing (loam-giq.3).
type MirrorFetcher struct {
	dataDir  string
	upstream upstreamRefFetcher
	repos    repoResolver
}

// NewMirrorFetcher builds a MirrorFetcher rooted at dataDir (LOAM_DATA_DIR;
// docs/server-spec.md: "bare mirrors under
// <dir>/mirrors/<group>/<repo_name>.git"), running fetches through
// upstream (in production, *gittransport.Transport) and resolving each
// repo's fetch coordinates and current work-branch names through repos
// (in production, StoreRepoResolver).
func NewMirrorFetcher(dataDir string, upstream upstreamRefFetcher, repos repoResolver) *MirrorFetcher {
	return &MirrorFetcher{dataDir: dataDir, upstream: upstream, repos: repos}
}

// Fetch satisfies Fetcher: it resolves repo's forge host, upstream URL,
// and currently registered work-branch names, builds the refspec set,
// runs the fetch through upstream, and parses the returned --porcelain
// output into a FetchResult.
func (f *MirrorFetcher) Fetch(ctx context.Context, repo RepoID) (FetchResult, error) {
	host, upstreamURL, workBranches, err := f.repos.ResolveRepo(ctx, repo)
	if err != nil {
		return FetchResult{}, fmt.Errorf("resolving repo %s for mirror fetch: %w", repo, err)
	}
	refspecs := buildFetchRefspecs(workBranches)
	out, err := f.upstream.Fetch(ctx, host, f.mirrorDir(repo), upstreamURL, refspecs)
	if err != nil {
		return FetchResult{}, fmt.Errorf("mirror-fetching repo %s: %w", repo, err)
	}
	refs, err := parsePorcelainFetch(out)
	if err != nil {
		return FetchResult{}, fmt.Errorf("parsing mirror-fetch output for repo %s: %w", repo, err)
	}
	return FetchResult{Refs: refs}, nil
}

// mirrorDir derives repo's bare-mirror path, mirroring cmd/server's own
// mirrorPath convention (docs/server-spec.md: "bare mirrors under
// <dir>/mirrors/<group>/<repo_name>.git") -- RepoID is repos.name, already
// the "<group>/<repo_name>" string that convention joins.
func (f *MirrorFetcher) mirrorDir(repo RepoID) string {
	return filepath.Join(f.dataDir, "mirrors", string(repo)+".git")
}

// buildFetchRefspecs returns the wildcard positive refspec plus one
// negative exclusion, "^refs/heads/<name>", per name in workBranches
// (docs/git-spec.md -> Ref Policy: "Work-branch refs -- refs/heads/<name>
// where <name> is a registered work branch"). Excluding a ref this way
// means the fetch never considers it for this invocation at all -- not
// "fetch then restore" -- so it survives both an upstream force-push of a
// same-named branch and an upstream deletion, since neither ever reaches
// this ref. Tags and every other upstream ref are covered by the wildcard
// unconditionally: docs/sync-spec.md's Mirror Sync step 1 says "fetch all
// upstream refs", with no carve-out for tags or any refs/pull/*-style
// namespace, and none appears anywhere else in docs/sync-spec.md or
// docs/git-spec.md either.
func buildFetchRefspecs(workBranches []string) []string {
	refspecs := make([]string, 0, len(workBranches)+1)
	refspecs = append(refspecs, allRefsRefspec)
	for _, name := range workBranches {
		refspecs = append(refspecs, "^refs/heads/"+name)
	}
	return refspecs
}

// porcelainFlags is the fixed set of single-character status flags
// git-fetch(1) OUTPUT documents for --porcelain: ' ' (fast-forward), '+'
// (forced), '-' (pruned), 't' (tag update), '*' (new ref), '!' (rejected),
// and '=' (up to date, --verbose only). A line whose first byte is not one
// of these is not a porcelain ref line at all.
var porcelainFlags = map[byte]bool{' ': true, '+': true, '-': true, 't': true, '*': true, '!': true, '=': true}

// zeroOID reports whether s is a git all-zero object id -- the
// deleted/absent sentinel in `git fetch --porcelain` output -- regardless
// of the hash algorithm's digest length (40 hex chars for SHA-1, 64 for
// SHA-256).
func zeroOID(s string) bool {
	return s != "" && strings.Count(s, "0") == len(s)
}

// isHexOID reports whether s is a plausible git object id: lowercase hex,
// 40 chars (SHA-1) or 64 chars (SHA-256). This also accepts the all-zero
// sentinel, since "0"*40/"0"*64 is valid hex.
func isHexOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// parsePorcelainFetchLine parses one line of `git fetch --porcelain`
// output (git-fetch(1) OUTPUT: "<flag> <old-object-id> <new-object-id>
// <local-reference>") into a RefUpdate, reporting ok=false when line does
// not conform to that exact shape: flag is one of porcelainFlags, both
// object-id fields are plausible hex-or-zero, and the ref is rooted at
// "refs/". The flag character itself is not retained in the result -- once
// Fetch has already returned a nil error, every flag this function accepts
// denotes a successful update, and pruned/created rows are already
// distinguishable from the SHA fields alone (an all-zero old id means a
// new ref, an all-zero new id means a prune) without needing the flag too.
func parsePorcelainFetchLine(line string) (RefUpdate, bool) {
	if len(line) < 2 || line[1] != ' ' || !porcelainFlags[line[0]] {
		return RefUpdate{}, false
	}
	fields := strings.Fields(line[1:])
	if len(fields) != 3 {
		return RefUpdate{}, false
	}
	oldSHA, newSHA, ref := fields[0], fields[1], fields[2]
	if !isHexOID(oldSHA) || !isHexOID(newSHA) || !strings.HasPrefix(ref, "refs/") {
		return RefUpdate{}, false
	}
	if zeroOID(oldSHA) {
		oldSHA = ""
	}
	if zeroOID(newSHA) {
		newSHA = ""
	}
	return RefUpdate{Ref: ref, OldSHA: oldSHA, NewSHA: newSHA}, true
}

// parsePorcelainFetch turns `git fetch --porcelain` output into
// RefUpdates, one per conforming line (see parsePorcelainFetchLine). Lines
// that do not conform are silently skipped rather than fabricated into a
// RefUpdate or treated as a fatal parse error: Transport.run returns
// cmd.CombinedOutput() (internal/gittransport/transport.go), so stdout and
// stderr are interleaved in out, and a benign git warning on stderr (e.g.
// "warning: redirecting to https://.../foo.git/" on a redirected upstream
// URL, or fetch's own "From <url>" summary line) is a real, common shape
// here -- neither a ref update to report nor a reason to fail the whole
// sync cycle into sync_state=error.
func parsePorcelainFetch(out []byte) ([]RefUpdate, error) {
	var refs []RefUpdate
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		ref, ok := parsePorcelainFetchLine(line)
		if !ok {
			continue
		}
		refs = append(refs, ref)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning fetch --porcelain output: %w", err)
	}
	return refs, nil
}
