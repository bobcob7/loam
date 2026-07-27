// Package mirrorpath is the single source of the bare-mirror path
// convention docs/server-spec.md's LOAM_DATA_DIR row pins: "bare mirrors
// under <dir>/mirrors/<group>/<repo_name>.git". Before this package
// existed the same one-line join was spelled twice, independently
// (cmd/server's mirrorPath and internal/mirrorsync.MirrorFetcher's
// mirrorDir) -- both now delegate to Dir here rather than keep their own
// copies, so a third caller (loam-ofg.16's git smart-HTTP handler) reuses
// it instead of adding a third spelling.
package mirrorpath

import (
	"errors"
	"fmt"
	"path/filepath"
)

// errNotAMirrorPath is returned by DataDir when its input does not have the
// "<dataDir>/mirrors/<group>/<repo_name>.git" shape Dir produces.
var errNotAMirrorPath = errors.New("not a mirror path under a mirrors/ directory")

// Dir derives an enrolled repo's bare-mirror path from dataDir
// (LOAM_DATA_DIR) and repoName. repoName is repos.name, already the
// "<group>/<repo_name>" string (docs/persistence-spec.md "Git mirrors":
// "path derived from repos.name"), so this is a single join, not a
// two-level split/join.
func Dir(dataDir, repoName string) string {
	return filepath.Join(dataDir, "mirrors", repoName+".git")
}

// DataDir inverts Dir: given a bare mirror's own absolute path (a hook
// process's cwd, per git's own contract of running hooks with the bare
// repository as the working directory -- verified empirically against real
// git 2.x during loam-ofg.18's research, since docs/git-spec.md itself
// never states this), it recovers dataDir (LOAM_DATA_DIR) by walking up
// exactly the three path components Dir always inserts below dataDir:
// "<repo_name>.git", "<group>", "mirrors". This only round-trips correctly
// for a repoName with exactly one '/' (the "<group>/<repo_name>" shape
// internal/handler/git's validRepoName already enforces at every enrollment
// boundary in this tree), so a mirrorDir with any other shape -- in
// particular one whose grandparent directory is not literally named
// "mirrors" -- is rejected rather than silently returning a wrong
// directory.  loam-ofg.18's hook client uses this because the hook's own
// process environment carries no LOAM_DATA_DIR (internal/handler/git's
// serveRPC builds the receive-pack subprocess's environment explicitly,
// see gitCommand's own doc comment, and does not include it); the hook's
// cwd is the only reliable way it learns where the policy socket
// (<dataDir>/hook.sock) lives.
func DataDir(mirrorDir string) (string, error) {
	clean := filepath.Clean(mirrorDir)
	if filepath.Ext(clean) != ".git" {
		return "", fmt.Errorf("deriving data dir from mirror path %s: %w", mirrorDir, errNotAMirrorPath)
	}
	groupDir := filepath.Dir(clean)
	mirrorsDir := filepath.Dir(groupDir)
	if filepath.Base(mirrorsDir) != "mirrors" {
		return "", fmt.Errorf("deriving data dir from mirror path %s: %w", mirrorDir, errNotAMirrorPath)
	}
	dataDir := filepath.Dir(mirrorsDir)
	if dataDir == mirrorsDir {
		return "", fmt.Errorf("deriving data dir from mirror path %s: %w", mirrorDir, errNotAMirrorPath)
	}
	return dataDir, nil
}
