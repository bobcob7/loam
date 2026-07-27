// Package mirrorpath is the single source of the bare-mirror path
// convention docs/server-spec.md's LOAM_DATA_DIR row pins: "bare mirrors
// under <dir>/mirrors/<group>/<repo_name>.git". Before this package
// existed the same one-line join was spelled twice, independently
// (cmd/server's mirrorPath and internal/mirrorsync.MirrorFetcher's
// mirrorDir) -- both now delegate to Dir here rather than keep their own
// copies, so a third caller (loam-ofg.16's git smart-HTTP handler) reuses
// it instead of adding a third spelling.
package mirrorpath

import "path/filepath"

// Dir derives an enrolled repo's bare-mirror path from dataDir
// (LOAM_DATA_DIR) and repoName. repoName is repos.name, already the
// "<group>/<repo_name>" string (docs/persistence-spec.md "Git mirrors":
// "path derived from repos.name"), so this is a single join, not a
// two-level split/join.
func Dir(dataDir, repoName string) string {
	return filepath.Join(dataDir, "mirrors", repoName+".git")
}
