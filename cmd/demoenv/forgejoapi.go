package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/bobcob7/loam/internal/fakeforge"
	"github.com/bobcob7/loam/internal/forge"
)

// forgejoAPI is a Forgejo-REST-shaped pull-request surface mounted in
// FRONT of an internal/fakeforge Server, and it exists because demo:m5 is
// the first demo whose subject is the forge REST API rather than git.
//
// The acceptance suite never needs this: it substitutes a
// *fakeforge.Client (the fake's own /provider/* REST shape) for the
// production forge.Provider, so the real *forge.Forgejo client is never
// on the path there. demo:m5 drives the REAL compiled bin/server, whose
// composition root builds a *forge.Forgejo (cmd/server/sync.go's
// forgePRTracker) and speaks Forgejo's actual endpoints:
//
//	POST   /api/v1/repos/<owner>/<repo>/pulls        CreatePR
//	GET    /api/v1/repos/<owner>/<repo>/pulls/<n>    GetPRState
//	PATCH  /api/v1/repos/<owner>/<repo>/pulls/<n>    ClosePR
//	GET    /api/v1/repos/<owner>/<repo>/pulls?...    FindOpenPR
//
// internal/fakeforge serves none of those paths, so without this shim the
// demo's first AcceptProposal would fail inside CreatePR with a 404 and
// the whole milestone would be unobservable through the shipped binaries.
//
// This is demo:m3's embedder decision applied to the forge REST surface,
// for the same reason: fake the FAR side, drive the real NEAR side. The
// production client's request encoding, its 404/401/403/409/412
// classification, its html_url/number/state/merged decoding, and its
// client-side head/base filtering in FindOpenPR are all exercised for
// real; only the server answering them is a fake. Substituting the whole
// provider (what the acceptance suite does) would delete forge.Forgejo
// from the demo entirely.
//
// It is a translator, NOT a second PR registry. Numbering, the
// open/merged/closed state machine, and the real git merge behind
// /control/merge-pr all stay in internal/fakeforge, reached here through
// a *fakeforge.Client bound to the same Server. The only state this type
// keeps of its own is what it was ASKED to create -- title, body, head,
// base, per PR number -- because the fake's provider surface has no
// read-back for those fields and Forgejo's list/get responses carry them.
// That memo is the shim being the far side, not a shadow copy of it: it
// is written once, at create, and never diverges because nothing else can
// write it.
//
// It lives in cmd/demoenv rather than in internal/fakeforge on purpose.
// internal/fakeforge is a shared test double with its own contract suite
// (loam-li0.9) pinning the fake and real Forgejo providers against ONE
// table of behaviours; adding a second, differently-shaped surface to it
// would need that whole contract extended to say which surface each row
// applies to. Demo support is not a shared contract, so it stays with the
// demo that needs it.
type forgejoAPI struct {
	inner  http.Handler
	client *fakeforge.Client
	token  string
	logger *slog.Logger
	mux    *http.ServeMux

	mu      sync.Mutex
	created map[string][]*forgejoPRMemo
}

// forgejoPRMemo is what one CreatePR asked for, recorded so the list and
// get responses can carry the fields the fake's provider surface does not
// return. State is deliberately NOT held here: it is read live from the
// fake on every request, so a /control/merge-pr performed behind this
// shim's back is reflected immediately.
type forgejoPRMemo struct {
	Number int
	URL    string
	Head   string
	Base   string
	Title  string
	Body   string
}

// newForgejoAPI wraps inner (a *fakeforge.Server) with the Forgejo REST
// pull-request surface, talking to that same server's provider REST API
// through baseURL with token. baseURL must be the externally reachable
// URL the server is about to be served on, which is why callers construct
// this only after their listener has bound.
func newForgejoAPI(inner http.Handler, baseURL, token string, logger *slog.Logger) *forgejoAPI {
	api := &forgejoAPI{
		inner:   inner,
		client:  fakeforge.NewClient(baseURL, token),
		token:   token,
		logger:  logger,
		created: make(map[string][]*forgejoPRMemo),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/repos/{owner}/{repo}/pulls", api.handleCreate)
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/pulls", api.handleList)
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/pulls/{index}", api.handleGet)
	mux.HandleFunc("PATCH /api/v1/repos/{owner}/{repo}/pulls/{index}", api.handlePatch)
	// Everything this shim does not claim -- the git smart-HTTP surface,
	// /provider/*, and the whole /control/* test API the demo scripts its
	// upstream events with -- falls through to the fake forge untouched.
	mux.Handle("/", inner)
	api.mux = mux
	return api
}

// ServeHTTP implements http.Handler.
func (a *forgejoAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mux.ServeHTTP(w, r)
}

// forgejoPRWire is the response shape forge.Forgejo's own
// forgejoPullRequest decodes, plus the title/body fields its list
// response carries and this demo asserts the attribution footer on.
type forgejoPRWire struct {
	Number  int              `json:"number"`
	HTMLURL string           `json:"html_url"`
	State   string           `json:"state"`
	Merged  bool             `json:"merged"`
	Title   string           `json:"title"`
	Body    string           `json:"body"`
	Head    forgejoRefWire   `json:"head"`
	Base    forgejoRefWire   `json:"base"`
	Labels  []map[string]any `json:"labels"`
}

// forgejoRefWire is Forgejo's head/base branch object, of which
// forge.Forgejo reads only `ref`.
type forgejoRefWire struct {
	Ref string `json:"ref"`
}

// forgejoCreateRequest is Forgejo's create-PR body, which
// forge.Forgejo.CreatePR encodes verbatim.
type forgejoCreateRequest struct {
	Head  string `json:"head"`
	Base  string `json:"base"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// forgejoPatchRequest is the edit-PR body; forge.Forgejo.ClosePR sends
// only {"state":"closed"}.
type forgejoPatchRequest struct {
	State string `json:"state"`
}

// requireToken enforces Forgejo's "Authorization: token <token>" header.
// A request without it is a 401, which is what makes
// forge.Forgejo.ValidateToken's ErrInvalidToken classification reachable
// against this shim rather than only against a real instance.
func (a *forgejoAPI) requireToken(w http.ResponseWriter, r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	if !ok || token != a.token {
		a.writeError(w, http.StatusUnauthorized, "token does not authenticate")
		return false
	}
	return true
}

// repoOf reassembles the "<owner>/<repo>" identifier from the path
// wildcards. It is the same string Loam holds in repos.name and passes to
// forge.Provider.CreatePR, and the same string internal/fakeforge stores
// the bare repo under, which is what makes demo:m5's one repo name work
// unchanged through the DB, the git URL, and this API.
func repoOf(r *http.Request) string {
	return r.PathValue("owner") + "/" + r.PathValue("repo")
}

func (a *forgejoAPI) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !a.requireToken(w, r) {
		return
	}
	repo := repoOf(r)
	// A missing or undecodable body yields the zero request and falls
	// through to the repo lookup below rather than a 400. That is not
	// laxity: forge.Forgejo.ValidateToken POSTs this very endpoint with NO
	// body at all, against a repo path picked never to exist, and reads
	// the resulting 404-with-an-error-envelope as "the token
	// authenticates and carries write:repository". Real Forgejo answers
	// that probe from its scope middleware, before the body is read, so
	// rejecting an empty body here would make the shim answer a
	// classification real Forgejo never answers.
	var req forgejoCreateRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	url, number, err := a.client.CreatePR(r.Context(), repo, req.Head, req.Base, req.Title, req.Body)
	if err != nil {
		a.writeForgeError(w, r, "creating pull request on "+repo, err)
		return
	}
	memo := &forgejoPRMemo{Number: number, URL: url, Head: req.Head, Base: req.Base, Title: req.Title, Body: req.Body}
	a.mu.Lock()
	a.created[repo] = append(a.created[repo], memo)
	a.mu.Unlock()
	a.logger.Info("demoenv forgejo api: opened pull request", "repo", repo, "number", number, "head", req.Head, "base", req.Base)
	a.writeJSON(w, http.StatusCreated, a.wire(memo, "open", false))
}

func (a *forgejoAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	if !a.requireToken(w, r) {
		return
	}
	repo := repoOf(r)
	number, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		a.writeError(w, http.StatusNotFound, "no pull request "+r.PathValue("index"))
		return
	}
	state, err := a.client.GetPRState(r.Context(), repo, number)
	if err != nil {
		a.writeForgeError(w, r, fmt.Sprintf("reading pull request %s#%d", repo, number), err)
		return
	}
	memo := a.memo(repo, number)
	if memo == nil {
		memo = &forgejoPRMemo{Number: number}
	}
	wireState, merged := forgejoStateWire(state)
	a.writeJSON(w, http.StatusOK, a.wire(memo, wireState, merged))
}

func (a *forgejoAPI) handlePatch(w http.ResponseWriter, r *http.Request) {
	if !a.requireToken(w, r) {
		return
	}
	repo := repoOf(r)
	number, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		a.writeError(w, http.StatusNotFound, "no pull request "+r.PathValue("index"))
		return
	}
	var req forgejoPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeError(w, http.StatusBadRequest, "undecodable edit body")
		return
	}
	if req.State != "closed" {
		a.writeError(w, http.StatusBadRequest, "only {\"state\":\"closed\"} is supported by this demo shim")
		return
	}
	if err := a.client.ClosePR(r.Context(), repo, number); err != nil {
		a.writeForgeError(w, r, fmt.Sprintf("closing pull request %s#%d", repo, number), err)
		return
	}
	memo := a.memo(repo, number)
	if memo == nil {
		memo = &forgejoPRMemo{Number: number}
	}
	a.writeJSON(w, http.StatusOK, a.wire(memo, "closed", false))
}

// handleList answers Forgejo's list-pulls endpoint. state=open (what
// forge.Forgejo.FindOpenPR requests) filters to open PRs; state=all
// returns every pull request ever opened on the repo regardless of state,
// which is the endpoint demo:m5 asserts "exactly one PR number was ever
// created" against -- a claim state=open cannot make once the PR merges.
//
// Every row's state is read live from the fake per request rather than
// from the memo, so a PR merged through /control/merge-pr shows as merged
// here without this shim being told.
func (a *forgejoAPI) handleList(w http.ResponseWriter, r *http.Request) {
	if !a.requireToken(w, r) {
		return
	}
	repo := repoOf(r)
	want := r.URL.Query().Get("state")
	if want == "" {
		want = "open"
	}
	limit, page := intQuery(r, "limit", 50), intQuery(r, "page", 1)
	if page < 1 {
		page = 1
	}
	rows := make([]forgejoPRWire, 0, limit)
	for _, memo := range a.memos(repo) {
		state, err := a.client.GetPRState(r.Context(), repo, memo.Number)
		if err != nil {
			a.writeForgeError(w, r, fmt.Sprintf("listing pull requests on %s", repo), err)
			return
		}
		wireState, merged := forgejoStateWire(state)
		if want != "all" && state != want {
			continue
		}
		rows = append(rows, a.wire(memo, wireState, merged))
	}
	start := (page - 1) * limit
	if start >= len(rows) {
		a.writeJSON(w, http.StatusOK, []forgejoPRWire{})
		return
	}
	end := min(start+limit, len(rows))
	a.writeJSON(w, http.StatusOK, rows[start:end])
}

// memo returns the recorded create for repo#number, or nil.
func (a *forgejoAPI) memo(repo string, number int) *forgejoPRMemo {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, m := range a.created[repo] {
		if m.Number == number {
			return m
		}
	}
	return nil
}

// memos returns repo's recorded creates, ascending by number, as a copy so
// the caller never holds the lock while making network calls.
func (a *forgejoAPI) memos(repo string) []*forgejoPRMemo {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]*forgejoPRMemo(nil), a.created[repo]...)
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// wire renders one memo plus a live state into Forgejo's response shape.
func (a *forgejoAPI) wire(memo *forgejoPRMemo, state string, merged bool) forgejoPRWire {
	return forgejoPRWire{
		Number:  memo.Number,
		HTMLURL: memo.URL,
		State:   state,
		Merged:  merged,
		Title:   memo.Title,
		Body:    memo.Body,
		Head:    forgejoRefWire{Ref: memo.Head},
		Base:    forgejoRefWire{Ref: memo.Base},
		Labels:  []map[string]any{},
	}
}

// forgejoStateWire converts forge.Provider's three-value state into
// Forgejo's own two-field encoding, which forge.Forgejo.GetPRState then
// folds back the other way ("closed" + merged -> "merged"). Round-tripping
// through the real encoding rather than shortcutting is the point: it is
// what keeps GetPRState's own fold on the demo's path.
func forgejoStateWire(state string) (string, bool) {
	if state == "merged" {
		return "closed", true
	}
	return state, false
}

// intQuery reads a positive integer query parameter, falling back to def.
func intQuery(r *http.Request, name string, def int) int {
	value, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || value <= 0 {
		return def
	}
	return value
}

// writeForgeError maps a forge sentinel reconstructed by *fakeforge.Client
// back to the HTTP status real Forgejo answers with, so the production
// client's own classification (doPullRequest's status switch) is what
// decides the error class rather than this shim asserting one.
func (a *forgejoAPI) writeForgeError(w http.ResponseWriter, r *http.Request, context string, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, forge.ErrRepoNotFound):
		status = http.StatusNotFound
	case errors.Is(err, forge.ErrDuplicatePR):
		status = http.StatusConflict
	case errors.Is(err, forge.ErrPRAlreadyMerged):
		status = http.StatusPreconditionFailed
	case errors.Is(err, forge.ErrInvalidToken):
		status = http.StatusUnauthorized
	case errors.Is(err, forge.ErrInsufficientScope), errors.Is(err, forge.ErrNoWriteAccess):
		status = http.StatusForbidden
	}
	a.logger.Warn("demoenv forgejo api: request failed", "path", r.URL.Path, "status", status, "error", err)
	a.writeError(w, status, context+": "+err.Error())
}

// writeError emits Forgejo's standard error envelope. Its `message` field
// is load-bearing, not decoration: forge.Forgejo.ValidateToken treats a
// 404 WITHOUT one as unclassifiable rather than as success, precisely so a
// wrong host cannot validate a token.
func (a *forgejoAPI) writeError(w http.ResponseWriter, status int, message string) {
	a.writeJSON(w, status, map[string]string{
		"message": message,
		"url":     "https://forgejo.example.invalid/api/swagger",
	})
}

func (a *forgejoAPI) writeJSON(w http.ResponseWriter, status int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "encoding response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}
