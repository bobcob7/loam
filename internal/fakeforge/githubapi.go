package fakeforge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// This file is the fake's GitHub-REST-shaped surface (loam-tmds.4),
// the GitHub-dialect sibling of forgejoapi.go's Forgejo-REST-shaped
// one. Both live in this package rather than a new internal/fakegithub
// package: loam-tmds.4's own bead notes ask for that ratio to be
// checked before choosing, and it is lopsided here. Everything
// STATE-shaped -- the bare git repos on disk (git.go/seed.go), the
// shared prRegistry a PR lives in regardless of which wire dialect
// reads it, and the token registry (server.go's tokenScope) -- is
// already forge-agnostic and reused as-is, unchanged, by this file.
// Only the WIRE ENCODING differs: URL paths with no version prefix
// (GitHub's, unlike Forgejo's /api/v1), JSON field names, the 422
// validation-error shape, and the X-OAuth-Scopes header ValidateToken
// reads. That is a few hundred lines of routing and marshalling next to
// a shared state model an order of magnitude larger — a dialect on the
// same Server, not a reason to duplicate the state.
//
// Consumed by the REAL *forge.GitHub client, unmodified, exactly the
// way forgejoapi.go is consumed by the real *forge.Forgejo: this is
// what lets internal/forgesuite's GitHub-over-fake harness (loam-tmds.3)
// prove forge.GitHub's own request encoding and response decoding
// survive a real round trip, not merely that the fake can answer some
// shape a mock would also accept.
//
//	GET   /user                              ValidateToken's scope probe
//	POST  /repos/{owner}/{repo}/pulls        CreatePR
//	GET   /repos/{owner}/{repo}/pulls/{n}    GetPRState
//	PATCH /repos/{owner}/{repo}/pulls/{n}    ClosePR
//	GET   /repos/{owner}/{repo}/pulls        FindOpenPR (server-side
//	                                          head=owner:branch/base
//	                                          filtering, unlike Forgejo)
//
// # What this models, and what it deliberately does not
//
// The write-scope model reuses the SAME single-axis tokenScope
// server.go already has for Forgejo (AddToken/AddReadOnlyToken): a
// token either carries "repo" scope (full read/write) or it doesn't,
// exactly mirroring forge.GitHub's own requiredScope decision
// (github.go) to require "repo" unconditionally rather than accepting
// "public_repo". There is no modelling of fine-grained PAT permissions
// or App installation tokens here, matching forge.GitHub's own
// token-kind decision (classic PAT only) — a fake need not emulate a
// wire shape production never sends.
//
// ClosePR's already-merged case is modelled as an idempotent 200 with
// merged:true in the body, not a distinct rejection status: this bead
// could not confirm GitHub returns one (see forge/github.go's
// ClosePR doc comment for the citation gap), so the fake matches what
// forge.GitHub actually reads to detect the case — the response body —
// rather than inventing a status code neither side has verified.

// githubUserWire is the /user response body. Nothing in forge.GitHub
// reads a field of it — ValidateToken only reads the X-OAuth-Scopes
// header — but a real body shape is still served rather than an empty
// one, in case a future caller decodes it.
type githubUserWire struct {
	Login string `json:"login"`
}

// githubErrorWire is GitHub's generic error body ({"message":"..."}),
// used for 401/404/403 responses that carry no errors[] array.
type githubErrorWire struct {
	Message string `json:"message"`
}

// githubValidationErrorWire is GitHub's 422 validation-error shape
// (docs.github.com/en/rest/using-the-rest-api/troubleshooting-the-rest-
// api): a top-level message plus a structured errors[] array. The
// duplicate-PR case is matched on message text (Code=="custom", which
// GitHub's own docs define as "refer to the message property to
// diagnose the error" -- see forge.GitHub's githubIsDuplicatePR doc
// comment); the missing-base-branch case is matched structurally on
// Field=="base", Code=="invalid" (see githubIsMissingBaseBranch's doc
// comment) -- both fields are populated here so this fake can produce
// either shape faithfully.
type githubValidationErrorWire struct {
	Message string `json:"message"`
	Errors  []struct {
		Resource string `json:"resource"`
		Field    string `json:"field,omitempty"`
		Code     string `json:"code"`
		Message  string `json:"message,omitempty"`
	} `json:"errors,omitempty"`
}

// writeGitHubValidationError writes a GitHub-shaped 422 response with a
// single errors[] entry -- the field/code-addressable branch-validation
// shape (githubIsMissingBaseBranch), distinct from the message-text
// duplicate-PR shape handleGitHubCreatePull builds directly since that
// one needs a message, not a field/code pair.
func writeGitHubValidationError(w http.ResponseWriter, resource, field, code string) {
	writeJSON(w, http.StatusUnprocessableEntity, githubValidationErrorWire{
		Message: "Validation Failed",
		Errors: []struct {
			Resource string `json:"resource"`
			Field    string `json:"field,omitempty"`
			Code     string `json:"code"`
			Message  string `json:"message,omitempty"`
		}{{Resource: resource, Field: field, Code: code}},
	})
}

// githubPullWire mirrors forge/github.go's own githubPullWire: html_url,
// number, state ("open"/"closed" only), merged, and head/base ref
// names.
type githubPullWire struct {
	HTMLURL string           `json:"html_url"`
	Number  int              `json:"number"`
	State   string           `json:"state"`
	Merged  bool             `json:"merged"`
	Head    githubAPIRefWire `json:"head"`
	Base    githubAPIRefWire `json:"base"`
}

// githubAPIRefWire is GitHub's branch-name subset of the head/base
// object ({"ref": "..."}).
type githubAPIRefWire struct {
	Ref string `json:"ref"`
}

// writeGitHubError writes a GitHub-shaped {"message":"..."} error body.
func writeGitHubError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, githubErrorWire{Message: message})
}

// githubWireState converts the fake's own three-value PR state
// ("open"/"closed"/"merged") into GitHub's two-field encoding (state
// plus a separate merged bool) — the same fold forge.GitHub's
// GetPRState reverses the other way.
func githubWireState(state string) (string, bool) {
	if state == "merged" {
		return "closed", true
	}
	return state, false
}

// githubPullWireFor renders one recorded PR into GitHub's wire shape.
func githubPullWireFor(r *http.Request, repo string, pr *prRecord) githubPullWire {
	state, merged := githubWireState(pr.state)
	return githubPullWire{
		HTMLURL: fmt.Sprintf("http://%s/%s/pull/%d", r.Host, repo, pr.number),
		Number:  pr.number,
		State:   state,
		Merged:  merged,
		Head:    githubAPIRefWire{Ref: pr.headBranch},
		Base:    githubAPIRefWire{Ref: pr.targetBranch},
	}
}

// requireGitHubToken enforces "Authorization: token <t>" and reports the
// token, writing a 401 GitHub-shaped error if it is absent or unknown —
// what a GET (read) operation requires on real GitHub.
func (s *Server) requireGitHubToken(w http.ResponseWriter, r *http.Request) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	if !ok || !s.hasToken(token) {
		writeGitHubError(w, http.StatusUnauthorized, "Bad credentials")
		return "", false
	}
	return token, true
}

// requireGitHubWriteScope is requireGitHubToken plus the "repo" scope
// check forge.GitHub.ValidateToken itself requires, applied here to
// create/close so the fake's write-path behaviour matches what
// ValidateToken already promised: a token without "repo" scope can
// authenticate reads but is denied writes.
func (s *Server) requireGitHubWriteScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	token, ok := s.requireGitHubToken(w, r)
	if !ok {
		return "", false
	}
	if !s.tokenHasPRScope(token) {
		writeGitHubError(w, http.StatusForbidden, "Resource not accessible by personal access token")
		return "", false
	}
	return token, true
}

// handleGitHubUser answers GET /user, forge.GitHub.ValidateToken's scope
// probe: any authenticated token gets 200, with X-OAuth-Scopes carrying
// "repo" only for a full-access token — the same single-axis tokenScope
// model AddToken/AddReadOnlyToken already establish for the Forgejo
// surface.
func (s *Server) handleGitHubUser(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireGitHubToken(w, r)
	if !ok {
		return
	}
	scopes := "gist, notifications"
	if s.tokenHasPRScope(token) {
		scopes = "repo, gist, notifications"
	}
	w.Header().Set("X-OAuth-Scopes", scopes)
	writeJSON(w, http.StatusOK, githubUserWire{Login: "fakeforge-bot"})
}

// githubRepoPath reassembles "<owner>/<repo>" from the path wildcards,
// the same identifier forgejoRepoPath builds for the Forgejo surface —
// both key into the SAME s.repoDir/s.prs, so a repo seeded once is
// addressable through either dialect.
func githubRepoPath(r *http.Request) string {
	return r.PathValue("owner") + "/" + r.PathValue("repo")
}

// handleGitHubCreatePull answers POST /repos/{owner}/{repo}/pulls:
// forge.GitHub.CreatePR's real pull-request creation. Branch validation
// and repo-existence reuse the exact same helpers
// (s.requireRepo/s.requireBranch) the Forgejo surface uses over the
// same on-disk bare repos, so a repo seeded once behaves identically
// under either dialect's branch checks.
func (s *Server) handleGitHubCreatePull(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGitHubWriteScope(w, r); !ok {
		return
	}
	repo := githubRepoPath(r)
	repoDir := s.repoDir(repo)
	if err := s.requireRepo(repoDir); err != nil {
		writeGitHubError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		Head  string `json:"head"`
		Base  string `json:"base"`
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	// NEITHER branch check answers 404: POST /repos/{owner}/{repo}/pulls
	// documents ONLY 201/403/422 for this endpoint (docs.github.com/en/
	// rest/pulls/pulls#create-a-pull-request) -- 404 is not a status this
	// route can honestly return once the repo itself is known to exist
	// (s.requireRepo above already handled that case). A prior revision
	// of this file answered 404 here, reasoned as "the closest verified
	// sentinel," which a review correctly identified as circular: it
	// picked the one status GitHub's docs say this endpoint does NOT
	// return, because that status happened to be the one
	// forge.GitHub's doPullRequest already mapped to the sentinel the
	// contract wants -- which would have hidden a genuine gap in
	// doPullRequest's own classification instead of exercising it.
	//
	// The BASE case is now a confirmed response shape: a nonexistent
	// base branch answers 422 with an errors[] entry
	// {"resource":"PullRequest","field":"base","code":"invalid"}, which
	// forge.GitHub's githubIsMissingBaseBranch (github.go) now matches
	// structurally and maps to ErrRepoNotFound -- see that function's
	// own doc comment for why field/code, not message text, is the
	// right match here.
	//
	// The HEAD case is inferred by symmetry (the same errors[] shape,
	// field:"head"), NOT independently confirmed: this bead's excluded
	// from internal/forgesuite's shared contract table for the identical
	// reason Forgejo's own leaked-500 head case is (see that package's
	// doc comment), so nothing exercises this shape end to end.
	// forge.GitHub's doPullRequest deliberately does NOT match
	// field=="head" against anything -- it falls through to a generic
	// "unexpected status" error -- so this fake and the real client
	// agree on leaving this case unclassified rather than one of them
	// quietly guessing.
	if err := s.requireBranch(r.Context(), repoDir, req.Head); err != nil {
		writeGitHubValidationError(w, "PullRequest", "head", "invalid")
		return
	}
	if err := s.requireBranch(r.Context(), repoDir, req.Base); err != nil {
		writeGitHubValidationError(w, "PullRequest", "base", "invalid")
		return
	}
	if existing, ok := s.prs.findOpen(repo, req.Head, req.Base); ok {
		// The message text here is load-bearing, not illustrative:
		// forge.GitHub's githubIsDuplicatePR matches "pull request
		// already exists" case-insensitively against exactly this
		// field. Code=="custom" is what GitHub's own docs define as
		// "refer to the message property to diagnose the error" (see
		// githubErrorEnvelope's doc comment in forge/github.go), so
		// this is the documented shape, not an approximation of one.
		writeJSON(w, http.StatusUnprocessableEntity, githubValidationErrorWire{
			Message: "Validation Failed",
			Errors: []struct {
				Resource string `json:"resource"`
				Field    string `json:"field,omitempty"`
				Code     string `json:"code"`
				Message  string `json:"message,omitempty"`
			}{{Resource: "PullRequest", Code: "custom", Message: fmt.Sprintf("A pull request already exists for %s:%s (pr #%d).", strings.SplitN(repo, "/", 2)[0], req.Head, existing.number)}},
		})
		return
	}
	pr := s.prs.create(repo, req.Head, req.Base, req.Title, req.Body)
	writeJSON(w, http.StatusCreated, githubPullWireFor(r, repo, pr))
}

// handleGitHubGetPull answers GET /repos/{owner}/{repo}/pulls/{n},
// forge.GitHub.GetPRState's request.
func (s *Server) handleGitHubGetPull(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGitHubToken(w, r); !ok {
		return
	}
	repo := githubRepoPath(r)
	number, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		writeGitHubError(w, http.StatusNotFound, "Not Found")
		return
	}
	pr, ok := s.prs.get(repo, number)
	if !ok {
		writeGitHubError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, githubPullWireFor(r, repo, pr))
}

// handleGitHubPatchPull answers PATCH /repos/{owner}/{repo}/pulls/{n},
// forge.GitHub.ClosePR's request. A PR already recorded as merged is
// answered with an idempotent 200 whose body carries merged:true and
// state:"closed" — the merged PR is already in state=closed, so this
// models the documented endpoint's response shape as an idempotent
// success rather than inventing an undocumented rejection status; see
// this file's own package-level doc comment and forge/github.go's
// ClosePR doc comment for why.
func (s *Server) handleGitHubPatchPull(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGitHubWriteScope(w, r); !ok {
		return
	}
	repo := githubRepoPath(r)
	number, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		writeGitHubError(w, http.StatusNotFound, "Not Found")
		return
	}
	var req struct {
		State string `json:"state"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	pr, ok := s.prs.get(repo, number)
	if !ok {
		writeGitHubError(w, http.StatusNotFound, "Not Found")
		return
	}
	if req.State == "closed" && pr.state == "open" {
		s.prs.setState(repo, number, "closed")
		pr, _ = s.prs.get(repo, number)
	}
	writeJSON(w, http.StatusOK, githubPullWireFor(r, repo, pr))
}

// handleGitHubListPulls answers GET /repos/{owner}/{repo}/pulls,
// forge.GitHub.FindOpenPR's request. Unlike the Forgejo surface (which
// verifiably ignores head/base query parameters on real Forgejo — see
// forgejoapi.go), this filters server-side on state/head/base: GitHub's
// own docs require a head filter shaped "<owner>:<branch>" even for a
// same-repository PR, and forge.GitHub sends exactly that shape, so the
// fake must understand it to be a faithful double rather than a
// permissive one that would let a client-side filtering bug go
// unnoticed.
func (s *Server) handleGitHubListPulls(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireGitHubToken(w, r); !ok {
		return
	}
	repo := githubRepoPath(r)
	if err := s.requireRepo(s.repoDir(repo)); err != nil {
		writeGitHubError(w, http.StatusNotFound, "Not Found")
		return
	}
	wantState := r.URL.Query().Get("state")
	if wantState == "" {
		wantState = "open"
	}
	wantHead := r.URL.Query().Get("head")
	wantBase := r.URL.Query().Get("base")
	owner, _, _ := strings.Cut(repo, "/")
	rows := make([]githubPullWire, 0)
	for _, pr := range s.prs.list(repo) {
		state, _ := githubWireState(pr.State)
		if wantState != "all" && state != wantState {
			continue
		}
		if wantHead != "" && wantHead != owner+":"+pr.HeadBranch {
			continue
		}
		if wantBase != "" && wantBase != pr.TargetBranch {
			continue
		}
		st, merged := githubWireState(pr.State)
		rows = append(rows, githubPullWire{
			HTMLURL: fmt.Sprintf("http://%s/%s/pull/%d", r.Host, repo, pr.Number),
			Number:  pr.Number,
			State:   st,
			Merged:  merged,
			Head:    githubAPIRefWire{Ref: pr.HeadBranch},
			Base:    githubAPIRefWire{Ref: pr.TargetBranch},
		})
	}
	writeJSON(w, http.StatusOK, rows)
}
