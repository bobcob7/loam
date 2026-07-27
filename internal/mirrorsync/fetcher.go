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
// -> Mirror Sync step 1). gittransport.Transport.Fetch already runs every
// refspec it is given with --prune --force, so the leading '+' here is
// what makes a diverged or force-pushed upstream ref win outright rather
// than being rejected as a non-fast-forward.
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

// zeroOID reports whether s is a git all-zero object id -- the
// deleted/absent sentinel in `git fetch --porcelain` output -- regardless
// of the hash algorithm's digest length (40 hex chars for SHA-1, 64 for
// SHA-256).
func zeroOID(s string) bool {
	return s != "" && strings.Count(s, "0") == len(s)
}

// parsePorcelainFetch turns `git fetch --porcelain` output (git-fetch(1)
// OUTPUT: one line per ref, "<flag> <old-object-id> <new-object-id>
// <local-reference>") into RefUpdates. The flag character itself is not
// retained -- ' ' (fast-forward), '+' (forced), '-' (pruned), '*' (new),
// and 't' (tag update) are all successful updates once Fetch has already
// returned a nil error, and pruned/created rows are already
// distinguishable from the SHA fields alone (an all-zero old id means a
// new ref, an all-zero new id means a prune) without needing the flag
// too. A rejected ('!') line cannot appear here: Fetch surfaces any git
// failure as an error before this ever runs.
func parsePorcelainFetch(out []byte) ([]RefUpdate, error) {
	var refs []RefUpdate
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return nil, fmt.Errorf("unparseable fetch --porcelain line: %q", line)
		}
		ref := fields[len(fields)-1]
		newSHA := fields[len(fields)-2]
		oldSHA := fields[len(fields)-3]
		if zeroOID(oldSHA) {
			oldSHA = ""
		}
		if zeroOID(newSHA) {
			newSHA = ""
		}
		refs = append(refs, RefUpdate{Ref: ref, OldSHA: oldSHA, NewSHA: newSHA})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning fetch --porcelain output: %w", err)
	}
	return refs, nil
}
