// Package git implements the git smart-HTTP transport handler
// (docs/git-spec.md "Endpoint & Protocol", "Enforcement Mechanics";
// loam-ofg.16): GET .../info/refs?service=git-upload-pack|git-receive-pack
// and POST .../git-upload-pack, POST .../git-receive-pack under
// /git/<group>/<repo_name>.git, served by shelling out to stock
// `git upload-pack --stateless-rpc` / `git receive-pack --stateless-rpc`
// against the enrolled repo's bare mirror -- manual smart-HTTP framing,
// deliberately NOT the git-http-backend CGI internal/fakeforge uses for
// the upstream-forge fixture, per this bead's DESIGN note naming the two
// subcommands explicitly.
//
// Scope is transport only. Identity resolution (internal/httpauth) and
// capability/role authorization (internal/handler.GitRoleGate) both wrap
// this handler in the mux -- see cmd/server's wiring -- and are already
// done by the time ServeHTTP runs; this package's own responsibility
// toward that layering is the CRITICAL SEAM docs/git-spec.md's
// "Enforcement Mechanics" names: propagating the identity
// internal/httpauth already resolved into the request context onto the
// `git receive-pack` subprocess's environment
// (LOAM_AGENT_NAME/LOAM_AGENT_ID/LOAM_AGENT_ROLE, plus LOAM_REPO), so
// loam-ofg.18's pre-receive hook -- which inherits that environment --
// learns who is pushing. Writing the hook itself, and the
// receive.denyNonFastForwards/receive.denyDeletes config the hook and
// git rely on, is internal/mirrorreconcile's job (loam-ofg.19), not this
// package's.
package git

import (
	"context"

	"github.com/bobcob7/loam/internal/reposstore"
)

// RepoStore is the repo-enrollment seam this handler consumes to turn a
// requested "<group>/<repo_name>" path segment into either a genuinely
// enrolled repo (docs/git-spec.md: "upload-pack serves the whole mirror")
// or a 404 (docs/git-spec.md: "Repo not enrolled -> 404"). *reposstore.Store
// satisfies this directly (structurally) in production; tests drive a moq
// mock instead. Same single-method shape as internal/handler/repo.RepoStore's
// GetRepoByName -- this handler needs nothing else from the store.
type RepoStore interface {
	GetRepoByName(ctx context.Context, name string) (reposstore.Repo, error)
}

//go:generate go tool moq -out moq_test.go . RepoStore
