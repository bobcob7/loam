// Package cli implements the loam command tree: routing, per-command flag
// parsing, and the collaborator seams each command handler is built against.
// See docs/cli-spec.md for the full command surface.
package cli

import (
	"context"

	"connectrpc.com/connect"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

//go:generate go tool moq -out moq_test.go . Config OutputEncoder ErrorMapper WorkspaceResolver StagingArea gitLookup gitCloner ConnectClient WorkBranchClient RepoClient GraphClient SearchClient MetaClient

// Config exposes the LOAM_* environment configuration every command may
// need (see docs/cli-spec.md -> Environment Variables). Implemented by
// loadConfig in config.go; this package only depends on the small
// read-only surface below.
type Config interface {
	// OutputFormat returns the active output format: json, yaml, xml, or
	// human. Unrecognized values fall back to json.
	OutputFormat() string
	// AgentName returns the calling agent's configured name.
	AgentName() string
	// AgentID returns the calling agent's configured id.
	AgentID() string
	// AgentRole returns the calling agent's configured role.
	AgentRole() string
	// ServerURL returns the base URL of the Loam server: the Connect APIs
	// and the git smart-HTTP endpoint (clone composes
	// <ServerURL>/git/<group>/<repo>.git; there is no separate git URL).
	ServerURL() string
	// Identifier returns the resolved "<name>-<id>-<role>" identifier (see
	// docs/cli-spec.md -> Environment Variables), reused by whoami and by
	// the Connect identity headers.
	Identifier() string
}

// OutputEncoder writes a command's result, or a structured error, to stdout
// in the active output format. Implemented by the encoders in encoder.go.
type OutputEncoder interface {
	Encode(v any) error
}

// ErrorMapper maps a command handler's error to the CLI's coarse exit-code
// scheme (see docs/cli-spec.md -> Exit Codes & Errors: 0 success, 1
// unexpected internal error, 2 usage/authz/conflict/precondition, 3 not
// found). Implemented by cliErrorMapper in errormapper.go.
type ErrorMapper interface {
	ExitCode(err error) int
}

// WorkspaceResolver infers the repo and work-branch identifiers from the
// current working directory when a command omits them (see docs/cli-spec.md
// -> Workspace), and locates the local staging path for a work branch's
// unpublished comments (see docs/cli-spec.md -> comment (add), "Staging
// location"). Implemented by workspace in workspace.go.
type WorkspaceResolver interface {
	// ResolveRepo infers the enrolled repo identifier ("<group>/<repo_name>",
	// see docs/cli-spec.md -> clone) from the clone the caller is inside
	// (at any depth — cli-spec: "run from inside a repo directory"),
	// derived from that clone's origin remote. Returns an error when the
	// caller is not inside a clone, or the clone's origin remote is not
	// shaped like a Loam clone URL — resolveWorkBranchIdentity turns either
	// into a usage error (exit 2) unless an explicit argument was given.
	// It never falls back to the bare clone-directory name: that string is
	// not an identifier any RPC accepts.
	ResolveRepo() (string, error)
	// ResolveWorkBranch infers the work branch from the current git branch
	// checked out in the clone the caller is inside (at any depth). Returns
	// an error when the caller is not inside a clone, or the clone has no
	// current branch to report (e.g. a detached HEAD) — independent of
	// ResolveRepo, so a clone with an unparseable origin remote can still
	// resolve its work branch.
	ResolveWorkBranch() (string, error)
	// OpenStaging opens the local staging area for repo/workBranch, scoped
	// to the calling agent's identifier so distinct agents sharing a
	// workspace never collide (see docs/cli-spec.md -> "Staging location"),
	// creating the directory and any missing parents. It always lives under
	// the workspace root's .loam/ directory — the clone's parent, never
	// inside the clone itself, so it stays the same regardless of how deep
	// inside (or outside) a clone the caller is, and a reviewer who never
	// clones can still stage comments. Callers must Close the returned area.
	//
	// repo and workBranch come from CLI positionals (explicit, or inferred
	// from local git state — see resolveWorkBranchIdentity) and are never
	// trusted blindly: each is validated against an explicit allowed
	// character class before being joined onto the staging root, and the
	// composed path is verified to still be contained under it. repo may
	// legitimately nest ("<group>/<repo_name>"); workBranch may not. A key
	// that fails either check is rejected with a usage error (exit 2) —
	// never silently sanitized, since rewriting an attacker's input to
	// something "safe" would hide the attempt and could collide two
	// distinct keys onto the same path.
	//
	// Those key checks are lexical and cannot see symlinks. Containment is
	// enforced separately and structurally, by resolving every path
	// component through os.Root handles: a symlinked component leading
	// outside the staging root fails the open rather than relocating the
	// tree. Returning a handle rather than a path string is the point —
	// there is no raw staging path to write to unsafely.
	OpenStaging(repo, workBranch string) (StagingArea, error)
}

// StagingArea is a handle on one agent's local staging directory for one
// repo/work-branch pair — the caller's unpublished review comments (see
// docs/cli-spec.md -> comment (add), "Staging location"). Obtained from
// WorkspaceResolver.OpenStaging; implemented by stagingArea in staging.go.
//
// Its whole reason for existing is that it exposes no path. Every method
// takes a bare entry name resolved against an os.Root pinned to the
// staging directory, so no operation — including one whose name argument
// or on-disk target is a symlink planted after the area was opened — can
// touch a file outside it. A future writer cannot accidentally opt out of
// containment, because there is nothing to opt out with.
type StagingArea interface {
	// WriteFile creates or replaces the staged file name with data, owner-
	// readable only. name must be a single path segment; the file mode is
	// fixed rather than caller-supplied.
	WriteFile(name string, data []byte) error
	// ReadFile returns the contents of the staged file name. A missing file
	// reports an error satisfying errors.Is(err, os.ErrNotExist).
	ReadFile(name string) ([]byte, error)
	// Remove deletes the staged file name. A missing file reports an error
	// satisfying errors.Is(err, os.ErrNotExist).
	Remove(name string) error
	// Close releases the underlying directory handle.
	Close() error
}

// gitLookup resolves the git facts workspace inference depends on: the root
// of the working copy containing a directory (at any depth), that working
// copy's configured "origin" remote URL (which `loam clone` points at
// <LOAM_SERVER_URL>/git/<group>/<repo_name>.git — see docs/cli-spec.md ->
// clone), and its currently checked-out branch. Defined here, consumer-side,
// so workspace.go's tests can mock it instead of shelling out to a real git
// binary for every case; execGitLookup in workspace.go is the real
// implementation.
type gitLookup interface {
	// CloneRoot returns the top-level directory of the git working copy
	// containing dir — dir itself, or an ancestor of it (matching `git
	// rev-parse --show-toplevel`, which walks up from dir). err is non-nil
	// when dir is not inside any git working copy at all.
	CloneRoot(dir string) (string, error)
	// OriginURL returns the "origin" remote URL configured at cloneRoot
	// (matching `git remote get-url origin`). err is non-nil when there is
	// no such remote.
	OriginURL(cloneRoot string) (string, error)
	// CurrentBranch returns the branch checked out at cloneRoot. err is
	// non-nil when there is no named branch checked out (e.g. detached
	// HEAD).
	CurrentBranch(cloneRoot string) (string, error)
}

// gitCloner runs the git subprocess operations `loam clone` bootstraps (see
// docs/git-spec.md -> The CLI's Role): the initial single-branch `git
// clone` -- carrying the three Loam-Agent-* identity headers as clone-time
// config so even that first fetch is authorized -- plus the git-config
// write that sets author identity so plain `git push`/`git fetch` carry it
// from then on with no wrapper command. Defined here, consumer-side, so
// clone.go's tests can mock it instead of shelling out to a real git binary
// for every case; execGitCloner in clone.go is the real implementation.
type gitCloner interface {
	// Clone runs `git clone --branch branch --single-branch --config
	// http.extraHeader=<h> (once per entry in headers) url dest`
	// (docs/cli-spec.md -> clone: "single-branch clone -- a convenient
	// default shape, not an enforcement"; docs/git-spec.md -> Identity on
	// Git Operations). Passing headers at clone time (rather than writing
	// them into dest's config only after Clone returns) is required, not
	// cosmetic: git's own first request -- the upload-pack info/refs GET --
	// happens before dest exists at all, so config written afterward would
	// never reach it, and httpauth.Auth.GitIdentity 403s any /git/* request
	// missing them. err wraps the git process's combined output on
	// failure, so callers can surface a missing branch or transport
	// failure with the reason git itself gave.
	Clone(ctx context.Context, url, branch, dest string, headers []string) error
	// SetConfig runs `git -C dest config <key> <value>`, overwriting a
	// single-valued key. Used for user.name / user.email.
	SetConfig(ctx context.Context, dest, key, value string) error
}

// WorkBranchClient is the consumer-side seam for the WorkBranchService RPCs
// the work-branch commands call through (see docs/cli-spec.md -> Work
// Branches). Its method set mirrors loamv1connect.WorkBranchServiceClient
// exactly, so the real generated client satisfies it with no adapter.
type WorkBranchClient interface {
	CreateWorkBranch(context.Context, *connect.Request[loamv1.CreateWorkBranchRequest]) (*connect.Response[loamv1.CreateWorkBranchResponse], error)
	UpdateWorkBranch(context.Context, *connect.Request[loamv1.UpdateWorkBranchRequest]) (*connect.Response[loamv1.UpdateWorkBranchResponse], error)
	RequestReview(context.Context, *connect.Request[loamv1.RequestReviewRequest]) (*connect.Response[loamv1.RequestReviewResponse], error)
	ListWorkBranches(context.Context, *connect.Request[loamv1.ListWorkBranchesRequest]) (*connect.Response[loamv1.ListWorkBranchesResponse], error)
	GetWorkBranch(context.Context, *connect.Request[loamv1.GetWorkBranchRequest]) (*connect.Response[loamv1.GetWorkBranchResponse], error)
	GetWorkBranchDiff(context.Context, *connect.Request[loamv1.GetWorkBranchDiffRequest]) (*connect.Response[loamv1.GetWorkBranchDiffResponse], error)
	ListComments(context.Context, *connect.Request[loamv1.ListCommentsRequest]) (*connect.Response[loamv1.ListCommentsResponse], error)
	ListVerdicts(context.Context, *connect.Request[loamv1.ListVerdictsRequest]) (*connect.Response[loamv1.ListVerdictsResponse], error)
	SubmitVerdict(context.Context, *connect.Request[loamv1.SubmitVerdictRequest]) (*connect.Response[loamv1.SubmitVerdictResponse], error)
	ReplyToThread(context.Context, *connect.Request[loamv1.ReplyToThreadRequest]) (*connect.Response[loamv1.ReplyToThreadResponse], error)
}

// RepoClient is the consumer-side seam for the RepoService RPC `clone` calls
// through to resolve an enrolled repo (see docs/cli-spec.md -> clone).
type RepoClient interface {
	GetRepo(context.Context, *connect.Request[loamv1.GetRepoRequest]) (*connect.Response[loamv1.GetRepoResponse], error)
}

// GraphClient is the consumer-side seam for the GraphService RPC the `graph`
// subqueries call through (see docs/cli-spec.md -> Graph DB queries).
type GraphClient interface {
	Query(context.Context, *connect.Request[loamv1.QueryRequest]) (*connect.Response[loamv1.QueryResponse], error)
}

// SearchClient is the consumer-side seam for the SearchService RPC `search`
// calls through (see docs/cli-spec.md -> RAG queries).
type SearchClient interface {
	Search(context.Context, *connect.Request[loamv1.SearchRequest]) (*connect.Response[loamv1.SearchResponse], error)
}

// MetaClient is the consumer-side seam for the MetaService RPC
// `instructions` calls through (see docs/cli-spec.md -> instructions).
type MetaClient interface {
	GetInstructions(context.Context, *connect.Request[loamv1.GetInstructionsRequest]) (*connect.Response[loamv1.GetInstructionsResponse], error)
}

// ConnectClient bundles the per-service Connect client seams above, one
// accessor per loam.v1 service, so a single collaborator travels through
// Deps (see deps.go) while each command handler group still depends only on
// the narrow interface it actually calls. Implemented by connectClients in
// connect.go.
type ConnectClient interface {
	WorkBranch() WorkBranchClient
	Repo() RepoClient
	Graph() GraphClient
	Search() SearchClient
	Meta() MetaClient
}
