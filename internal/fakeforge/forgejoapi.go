package fakeforge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// This file is the fake's Forgejo-REST-shaped surface, as distinct from the
// /provider/* surface in provider.go. The two are not alternatives and
// neither is deprecated:
//
//   - /provider/* is the fake's own control-shaped REST API, consumed by
//     *fakeforge.Client, which satisfies forge.Provider directly. That is
//     how the acceptance layer and internal/forgesuite's fake leg reach
//     the fake: at the Provider seam, with the real Forgejo REST client
//     replaced wholesale.
//   - /api/v1/... below is consumed by the REAL *forge.Forgejo client,
//     unmodified, over real HTTP. It exists because production callers
//     cannot all be served at the Provider seam: internal/handler/credential's
//     SetUpstreamToken holds a host-agnostic *forge.Forgejo and calls
//     ValidateToken on it, and internal/mirrorsync's StorePRPoller (the
//     admin CloseWorkBranch path and the background PR-state poller) holds
//     one bound to a specific host and calls CreatePR/GetPRState/ClosePR/
//     FindOpenPR on it. Anything wanting to exercise those RPCs end to end
//     -- features/credentials.feature's token scenarios,
//     features/admin-proposals.feature's "Closing a work branch closes its
//     upstream PR", cmd/server's credential integration tests, a demo --
//     needs a server that answers Forgejo's own wire shape, not the fake's.
//
// # Scope: the PR lifecycle forge.Forgejo issues, and deliberately nothing more
//
// loam-7d2 first added this file with exactly one route --
// POST .../pulls, answering only ValidateToken's scope probe, 501 for any
// repo that genuinely exists -- because giving it real PR-creation
// semantics meant inventing wire behaviour nothing here had verified
// against a live Forgejo. loam-c8v (this revision) extends it to the rest
// of the pulls surface forge.Forgejo's own client code issues:
//
//	POST  /api/v1/repos/{owner}/{repo}/pulls        CreatePR (and ValidateToken's probe)
//	GET   /api/v1/repos/{owner}/{repo}/pulls/{n}     GetPRState
//	PATCH /api/v1/repos/{owner}/{repo}/pulls/{n}     ClosePR
//	GET   /api/v1/repos/{owner}/{repo}/pulls         FindOpenPR's paged list
//
// over the SAME prRegistry the /provider/* surface and the /control/*
// surface both already share, so a PR closed via one surface is observably
// closed via any of the others -- there is exactly one registry, never a
// second copy of PR state.
//
// What is still deliberately NOT modelled, and still answers honestly
// rather than with a fabricated success:
//
//   - Any PATCH body other than exactly {"state":"closed"}. Real Forgejo's
//     edit-PR endpoint can also change title, body, and target branch;
//     forge.Forgejo.ClosePR only ever sends the one field, so that is the
//     only edit this route implements. Anything else gets the same
//     honest 501 the whole file used to answer for every create.
//   - POST .../merge (Forgejo's merge endpoint). No production caller
//     issues it: CreatePR/GetPRState/ClosePR/FindOpenPR cover
//     forge.Provider's whole surface, and the fake's merge simulation
//     (a real three-way git merge, exercised by tests wanting to observe
//     a forge-side merge) is reached through the /control/merge-pr
//     control API instead, which mutates the same prRegistry this file
//     reads. There being no route here for it is not an oversight.
//   - A missing HEAD branch on CreatePR. Real Forgejo 9.0.3 answers that
//     with a 500 carrying a leaked git error (loam-9qu; re-confirmed
//     while writing loam-li0.9's contract suite, which excludes this case
//     from its shared table for exactly that reason). Mimicking it would
//     mean fabricating a plausible-looking git error string nothing here
//     has verified live. This route keeps the plain 404 provider.go's own
//     create-PR handler already uses for the same case, a documented,
//     pre-existing divergence rather than a new one.
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
// at most call sites are illustrative, NOT contractual: every consumer in
// internal/forge branches on the status code alone, plus the mere
// non-emptiness of message on a 404 (ValidateToken). The "BaseNotExist"
// and merged-PR messages are copied from a real instance (Forgejo 9.0.3,
// recorded in loam-2uy/loam-giq.8 and in cmd/server's credential
// integration test stub) but are still not asserted on by any production
// caller.
func writeForgejoError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, forgejoErrorEnvelope{Message: message, URL: forgejoSwaggerURL})
}

// forgejoRefWire is Forgejo's head/base branch object, of which
// forge.Forgejo reads only `ref`.
type forgejoRefWire struct {
	Ref string `json:"ref"`
}

// forgejoPullWire is the Forgejo pull-request response shape
// internal/forge/forgejo.go's own forgejoPullRequest decodes: html_url,
// number, state, merged, and the head/base refs FindOpenPR filters on
// client-side. Every one of CreatePR/GetPRState/ClosePR/FindOpenPR reads
// its response through this same struct shape in the real client, which
// is why every 2xx response below -- including the close (PATCH) response
// ClosePR itself never reads a field of -- must still carry a body
// decodable into it: doPullRequest unconditionally decodes the response
// body after checking the status, regardless of which operation asked for
// it, so an empty 204-style body here would fail ClosePR with a decode
// error instead of the success the status code promised.
type forgejoPullWire struct {
	HTMLURL string         `json:"html_url"`
	Number  int            `json:"number"`
	State   string         `json:"state"`
	Merged  bool           `json:"merged"`
	Head    forgejoRefWire `json:"head"`
	Base    forgejoRefWire `json:"base"`
}

// forgejoCreatePullRequest is the create-PR body forge.Forgejo.CreatePR
// encodes.
type forgejoCreatePullRequest struct {
	Head  string `json:"head"`
	Base  string `json:"base"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// forgejoPatchPullRequest is the edit-PR body; forge.Forgejo.ClosePR sends
// only {"state":"closed"}, the one edit this route models (see the package
// doc's "deliberately NOT modelled" list).
type forgejoPatchPullRequest struct {
	State string `json:"state"`
}

// wireState converts the fake's own three-value PR state ("open"/"closed"/
// "merged") into Forgejo's two-field wire encoding, the same fold
// forge.Forgejo.GetPRState reverses the other way
// (state=="closed" && merged -> "merged").
func wireState(state string) (string, bool) {
	if state == "merged" {
		return "closed", true
	}
	return state, false
}

// forgejoPullWireFor renders one recorded PR into the wire shape
// create/get/patch all share, using r.Host the same way the /provider/*
// surface's PR URLs already do.
func forgejoPullWireFor(r *http.Request, repo string, pr *prRecord) forgejoPullWire {
	state, merged := wireState(pr.state)
	return forgejoPullWire{
		HTMLURL: fmt.Sprintf("http://%s/%s/pulls/%d", r.Host, repo, pr.number),
		Number:  pr.number,
		State:   state,
		Merged:  merged,
		Head:    forgejoRefWire{Ref: pr.headBranch},
		Base:    forgejoRefWire{Ref: pr.targetBranch},
	}
}

// requireForgejoToken enforces "Authorization: token <t>", writing a 401
// Forgejo-shaped error and reporting false if the token is absent or
// unknown. This alone is what GET (read) operations require in real
// Forgejo; create/close additionally need requireForgejoWriteScope below.
func (s *Server) requireForgejoToken(w http.ResponseWriter, r *http.Request) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	if !ok || !s.hasToken(token) {
		writeForgejoError(w, http.StatusUnauthorized, "token does not exist")
		return "", false
	}
	return token, true
}

// requireForgejoWriteScope is requireForgejoToken plus the write:repository
// check real Forgejo's scope-enforcement middleware performs before
// create/edit pull-request calls -- the same predicate and 403 message
// handleForgejoCreatePull's probe has always used, factored out so
// handleForgejoPatchPull (close) enforces it identically. The scope
// predicate is tokenHasPRScope, exactly the one /provider/validate-token
// and ValidateToken's probe use, so every one of the fake's surfaces
// answers the same scope question the same way.
func (s *Server) requireForgejoWriteScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	token, ok := s.requireForgejoToken(w, r)
	if !ok {
		return "", false
	}
	if !s.tokenHasPRScope(token) {
		writeForgejoError(w, http.StatusForbidden, "token does not have at least one of required scope(s): [write:repository]")
		return "", false
	}
	return token, true
}

// forgejoRepoPath reassembles the "<owner>/<repo>" identifier from the
// path wildcards -- the same string repos.name/CreatePR's repo argument
// carries and s.repoDir/s.prs key their records under, which is what
// keeps one repo name coherent across the git surface, /provider/*, and
// this file.
func forgejoRepoPath(r *http.Request) string {
	return r.PathValue("owner") + "/" + r.PathValue("repo")
}

// handleForgejoCreatePull answers POST /api/v1/repos/{owner}/{repo}/pulls:
// both forge.Forgejo.ValidateToken's scope probe (a repo path picked never
// to exist) and forge.Forgejo.CreatePR's real pull-request creation
// (a repo that does exist).
//
// The order of the auth/scope checks against repo resolution is the
// contract, not an implementation detail. Forgejo runs its
// scope-enforcement middleware BEFORE resolving the owner or the repo
// (verified against 9.0.3 and documented at internal/forge/forgejo.go's
// probeOwner/probeRepo constants), which is the entire reason
// ValidateToken can probe a path picked never to exist and still read an
// unambiguous verdict off the response:
//
//   - 401, token absent/unknown → forge.ErrInvalidToken.
//   - 403, token known but missing the PR scope →
//     forge.ErrInsufficientScope.
//   - 404 WITH a non-empty message → the repo genuinely does not exist,
//     which is ValidateToken's expected probe outcome and CreatePR's
//     ErrRepoNotFound alike.
//
// Reordering these -- resolving the repo first, say -- would answer 404
// for an unknown token and silently validate anything.
func (s *Server) handleForgejoCreatePull(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireForgejoWriteScope(w, r); !ok {
		return
	}
	owner, repo := r.PathValue("owner"), forgejoRepoPath(r)
	repoDir := s.repoDir(repo)
	if err := s.requireRepo(repoDir); err != nil {
		writeForgejoError(w, http.StatusNotFound, "user redirect does not exist [name: "+owner+"]")
		return
	}
	// A missing or undecodable body yields the zero request and falls
	// through to the branch checks below, which then 404 on an empty
	// branch name, rather than a 400. This is not laxity:
	// forge.Forgejo.ValidateToken's probe already returned above (its
	// target repo never exists), so nothing that reaches here is that
	// probe; a real CreatePR call always sends a well-formed body.
	var req forgejoCreatePullRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	// The head and base checks below deliberately use different status
	// codes. Real Forgejo 9.0.3 folds a nonexistent base branch into the
	// same 404 class as a missing repo ({"message":"BaseNotExist"}), so
	// the base check matches that message. A nonexistent HEAD branch
	// instead 500s there with a leaked git error (loam-9qu) -- an
	// apparent upstream bug excluded from internal/forgesuite's shared
	// contract table for exactly that reason -- so the head check here
	// keeps the same plain 404 provider.go's /provider/create-pr handler
	// already answers for the identical case, rather than claiming a
	// parity that does not exist.
	if err := s.requireBranch(r.Context(), repoDir, req.Head); err != nil {
		writeForgejoError(w, http.StatusNotFound, "head branch \""+req.Head+"\" does not exist")
		return
	}
	if err := s.requireBranch(r.Context(), repoDir, req.Base); err != nil {
		writeForgejoError(w, http.StatusNotFound, "BaseNotExist")
		return
	}
	if existing, ok := s.prs.findOpen(repo, req.Head, req.Base); ok {
		// Real Forgejo's 409 message embeds the existing PR's INTERNAL id,
		// not its per-repo number (internal/forge/forgejo.go's
		// ErrDuplicatePR godoc) -- undocumented, unstructured text that no
		// caller parses. Using the per-repo number here is therefore not a
		// claim that it matches Forgejo's id; it only has to be present so
		// the message shape (and its non-emptiness) matches.
		writeForgejoError(w, http.StatusConflict, fmt.Sprintf("pull request already exists for these targets [id: %d]", existing.number))
		return
	}
	pr := s.prs.create(repo, req.Head, req.Base, req.Title, req.Body)
	writeJSON(w, http.StatusCreated, forgejoPullWireFor(r, repo, pr))
}

// handleForgejoGetPull answers GET /api/v1/repos/{owner}/{repo}/pulls/{n},
// the request forge.Forgejo.GetPRState issues. It intentionally does not
// distinguish "the repo does not exist" from "the repo exists but this PR
// number does not": real Forgejo 9.0.3 answers the identical generic 404
// for both (internal/forge/errors.go's ErrRepoNotFound godoc), which is
// why looking the PR straight up in the shared registry -- with no
// separate repo-existence check -- already matches that fold rather than
// having to reconstruct it.
func (s *Server) handleForgejoGetPull(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireForgejoToken(w, r); !ok {
		return
	}
	repo := forgejoRepoPath(r)
	number, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		writeForgejoError(w, http.StatusNotFound, "The target couldn't be found.")
		return
	}
	pr, ok := s.prs.get(repo, number)
	if !ok {
		writeForgejoError(w, http.StatusNotFound, "The target couldn't be found.")
		return
	}
	writeJSON(w, http.StatusOK, forgejoPullWireFor(r, repo, pr))
}

// handleForgejoPatchPull answers PATCH /api/v1/repos/{owner}/{repo}/pulls/{n},
// the request forge.Forgejo.ClosePR issues with body {"state":"closed"} --
// the only edit this route models (see the package doc). Any other body
// is answered honestly with the same 501 the whole file used to answer
// for every create, rather than a silently-accepted no-op.
//
// A PR the registry already has recorded as merged rejects the close with
// 412 Precondition Failed, state unchanged: verified against Forgejo
// 9.0.3, PATCH .../pulls/{merged} {"state":"closed"} returns 412 and
// leaves the PR untouched (internal/forge/errors.go's ErrPRAlreadyMerged
// godoc; the same guard control.go's ClosePR and provider.go's
// handleProviderClosePR already apply on their own surfaces).
func (s *Server) handleForgejoPatchPull(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireForgejoWriteScope(w, r); !ok {
		return
	}
	repo := forgejoRepoPath(r)
	number, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		writeForgejoError(w, http.StatusNotFound, "The target couldn't be found.")
		return
	}
	var req forgejoPatchPullRequest
	if decodeErr := json.NewDecoder(r.Body).Decode(&req); decodeErr != nil || req.State != "closed" {
		writeForgejoError(w, http.StatusNotImplemented,
			"fakeforge: this route models only {\"state\":\"closed\"}, the one edit forge.Forgejo.ClosePR sends")
		return
	}
	pr, ok := s.prs.get(repo, number)
	if !ok {
		writeForgejoError(w, http.StatusNotFound, "The target couldn't be found.")
		return
	}
	if pr.state == "merged" {
		writeForgejoError(w, http.StatusPreconditionFailed, "cannot change status of a merged pull request")
		return
	}
	s.prs.setState(repo, number, "closed")
	pr, _ = s.prs.get(repo, number)
	writeJSON(w, http.StatusOK, forgejoPullWireFor(r, repo, pr))
}

// forgejoListPageSize and default page are used only when the request omits
// limit/page, matching real Forgejo's own defaults for this endpoint.
const forgejoListDefaultLimit = 50

// handleForgejoListPulls answers GET /api/v1/repos/{owner}/{repo}/pulls,
// the paged list forge.Forgejo.FindOpenPR walks with state=open,
// limit=50, page=1,2,.... Verified empirically against Forgejo 9.0.3 (see
// FindOpenPR's own doc comment), that endpoint takes no head/base query
// filter, so filtering on those is the client's job, not this route's --
// exactly what FindOpenPR does. This handler filters only on state
// (default "open", matching Forgejo's own default when the query omits
// it) and paginates on limit/page, which is everything FindOpenPR's own
// request shape exercises.
//
// The loop-terminating property FindOpenPR relies on -- an EMPTY page
// means the end of the list -- holds here unconditionally: unlike real
// Forgejo, this in-memory registry has no server-side
// MAX_RESPONSE_ITEMS cap to silently shorten a page below the requested
// limit, so there is no equivalent of the "a full page might not be the
// last page" case FindOpenPR's own doc warns about. That is a genuine,
// narrower guarantee than the real provider offers, not a claim that this
// fake reproduces the cap.
func (s *Server) handleForgejoListPulls(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireForgejoToken(w, r); !ok {
		return
	}
	repo := forgejoRepoPath(r)
	if err := s.requireRepo(s.repoDir(repo)); err != nil {
		writeForgejoError(w, http.StatusNotFound, "The target couldn't be found.")
		return
	}
	want := r.URL.Query().Get("state")
	if want == "" {
		want = "open"
	}
	limit := forgejoQueryInt(r, "limit", forgejoListDefaultLimit)
	page := forgejoQueryInt(r, "page", 1)
	all := s.prs.list(repo)
	matched := make([]PullRequest, 0, len(all))
	for _, pr := range all {
		state, _ := wireState(pr.State)
		if want != "all" && state != want {
			continue
		}
		matched = append(matched, pr)
	}
	rows := make([]forgejoPullWire, 0)
	if start := (page - 1) * limit; start < len(matched) {
		end := min(start+limit, len(matched))
		for _, pr := range matched[start:end] {
			state, merged := wireState(pr.State)
			rows = append(rows, forgejoPullWire{
				HTMLURL: fmt.Sprintf("http://%s/%s/pulls/%d", r.Host, repo, pr.Number),
				Number:  pr.Number,
				State:   state,
				Merged:  merged,
				Head:    forgejoRefWire{Ref: pr.HeadBranch},
				Base:    forgejoRefWire{Ref: pr.TargetBranch},
			})
		}
	}
	writeJSON(w, http.StatusOK, rows)
}

// forgejoQueryInt reads a positive integer query parameter, falling back
// to def when absent, unparseable, or non-positive.
func forgejoQueryInt(r *http.Request, name string, def int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || v <= 0 {
		return def
	}
	return v
}
