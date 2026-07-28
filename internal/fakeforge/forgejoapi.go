package fakeforge

import (
	"net/http"
	"strings"
)

// This file is the fake's ONE Forgejo-REST-shaped surface, as distinct
// from the /provider/* surface in provider.go. The two are not
// alternatives and neither is deprecated:
//
//   - /provider/* is the fake's own control-shaped REST API, consumed by
//     *fakeforge.Client, which satisfies forge.Provider directly. That is
//     how the acceptance layer and internal/forgesuite's fake leg reach
//     the fake: at the Provider seam, with the real Forgejo REST client
//     replaced wholesale.
//   - /api/v1/... below is consumed by the REAL *forge.Forgejo client,
//     unmodified, over real HTTP. It exists because ONE production caller
//     cannot be served at the Provider seam: internal/handler/credential's
//     SetUpstreamToken holds a host-agnostic *forge.Forgejo and calls
//     ValidateToken on it, so anything wanting to exercise that RPC end to
//     end -- features/credentials.feature's "Setting a token for a forge
//     host" and "A rejected token is reported", cmd/server's credential
//     integration tests, a demo -- needs a server that answers Forgejo's
//     own wire shape, not the fake's.
//
// # Scope: the scope probe, and deliberately nothing more
//
// Only the endpoint ValidateToken actually issues is implemented, with
// only the status codes it actually consumes (internal/forge/forgejo.go →
// ValidateToken). It is NOT a general Forgejo pulls API: a POST naming a
// repo that EXISTS on this fake is answered 501, never a fabricated
// success, so no caller can mistake this surface for a PR-creation route
// (that is /provider/create-pr, and giving this one PR-creation semantics
// too would mean inventing wire behaviour for CreatePR's duplicate,
// missing-branch and merged classes that nothing here has verified against
// a real Forgejo). See loam-c8v.
//
// # Why forgejoErrorEnvelope is not errorEnvelope
//
// httpjson.go's errorEnvelope is {"error","code"} -- the fake's OWN wire
// contract, whose Code field is what Client reconstructs sentinels from.
// Forgejo's is {"message","url"}, and ValidateToken specifically requires
// a NON-EMPTY message on a 404 before it will read that 404 as success
// (a bare 404 is treated as unclassifiable, because an unauthenticated
// request or a wrong host produces one too). Reusing the fake's envelope
// here would therefore turn every successful validation into an error,
// and the fields are genuinely different contracts, not a duplication.

// forgejoSwaggerURL is the "url" field Forgejo attaches to its error
// bodies. Nothing consumes it -- ValidateToken reads only "message" --
// but including it keeps the body the shape an operator or a future
// client would recognise.
const forgejoSwaggerURL = "https://forgejo.example.invalid/api/swagger"

// forgejoErrorEnvelope is Forgejo's standard JSON error body, the shape
// internal/forge/forgejo.go decodes into its own forgejoErrorEnvelope.
type forgejoErrorEnvelope struct {
	Message string `json:"message"`
	URL     string `json:"url,omitempty"`
}

// writeForgejoError writes a Forgejo-shaped error body. The message texts
// at the call sites are illustrative, NOT contractual: ValidateToken
// branches on the status code alone, plus the mere non-emptiness of
// message on a 404. Only the 403 text is copied from a real instance
// (Forgejo 9.0.3, recorded in loam-2uy and in cmd/server's credential
// integration test stub), and even that is matched by no assertion here.
func writeForgejoError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, forgejoErrorEnvelope{Message: message, URL: forgejoSwaggerURL})
}

// handleForgejoCreatePull answers POST /api/v1/repos/{owner}/{repo}/pulls,
// the request forge.Forgejo.ValidateToken issues as its scope probe.
//
// The order of the three checks is the contract, not an implementation
// detail. Forgejo runs its scope-enforcement middleware BEFORE resolving
// the owner or the repo (verified against 9.0.3 and documented at
// internal/forge/forgejo.go's probeOwner/probeRepo constants), which is
// the entire reason ValidateToken can probe a path picked never to exist
// and still read an unambiguous verdict off the response:
//
//   - 401, token absent/unknown → forge.ErrInvalidToken.
//   - 403, token known but missing the PR scope →
//     forge.ErrInsufficientScope.
//   - 404 WITH a non-empty message → success. The repo genuinely not
//     existing is the expected outcome of a probe, and only a token that
//     got past both middlewares reaches it.
//
// Reordering these -- resolving the repo first, say -- would answer 404
// for an unknown token and silently validate anything.
//
// The scope predicate is tokenHasPRScope, exactly the one
// /provider/validate-token uses, so both of the fake's surfaces answer
// the same question the same way. That deliberately inherits the fake's
// canPush/canPR split, whose divergence from Forgejo 9.0.3 (which gates
// both on write:repository) is filed as loam-2uy; making THIS surface
// alone consult canPush would have created a second, unfiled divergence
// between the fake's own two surfaces.
func (s *Server) handleForgejoCreatePull(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	if !ok || !s.hasToken(token) {
		writeForgejoError(w, http.StatusUnauthorized, "token does not exist")
		return
	}
	if !s.tokenHasPRScope(token) {
		writeForgejoError(w, http.StatusForbidden, "token does not have at least one of required scope(s): [write:repository]")
		return
	}
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	if err := s.requireRepo(s.repoDir(owner + "/" + repo)); err != nil {
		writeForgejoError(w, http.StatusNotFound, "user redirect does not exist [name: "+owner+"]")
		return
	}
	writeForgejoError(w, http.StatusNotImplemented,
		"fakeforge: this Forgejo-shaped route implements ValidateToken's scope probe only; use POST /provider/create-pr to open a pull request on the fake")
}
