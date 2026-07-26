// Package cli implements the loam command tree: routing, per-command flag
// parsing, and the collaborator seams each command handler is built against.
// See docs/cli-spec.md for the full command surface.
package cli

import (
	"context"

	"connectrpc.com/connect"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
)

//go:generate go tool moq -out moq_test.go . Config OutputEncoder ErrorMapper WorkspaceResolver gitBranchLookup ConnectClient WorkBranchClient RepoClient GraphClient SearchClient MetaClient

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
	// ResolveRepo infers the enrolled repo identifier from the current
	// working directory: the directory name, when the caller is inside a
	// clone (see docs/cli-spec.md -> Workspace). Returns an error when the
	// caller is not inside a clone — resolveWorkBranchIdentity turns that
	// into a usage error (exit 2) unless an explicit argument was given.
	ResolveRepo() (string, error)
	// ResolveWorkBranch infers the work branch from the current git branch
	// checked out in the clone the caller is inside. Returns an error under
	// the same condition as ResolveRepo (not inside a clone), or when the
	// clone has no current branch to report (e.g. a detached HEAD).
	ResolveWorkBranch() (string, error)
	// StagingPath returns the local staging path for repo/workBranch,
	// scoped to the calling agent's identifier so distinct agents sharing a
	// workspace never collide (see docs/cli-spec.md -> "Staging location").
	// It lives under the workspace's .loam/ directory, outside any clone,
	// so a reviewer who never clones can still stage comments.
	StagingPath(repo, workBranch string) string
}

// gitBranchLookup resolves the git branch checked out at a directory, the
// seam workspace inference uses to detect whether the caller is inside a
// clone at all (see docs/cli-spec.md -> Workspace). Defined here,
// consumer-side, so workspace.go's tests can mock it instead of shelling
// out to a real git binary for every case; execGitBranchLookup in
// workspace.go is the real implementation.
type gitBranchLookup interface {
	// CurrentBranch returns the checked-out branch name for the git working
	// copy rooted exactly at dir. err is non-nil when dir is not the root
	// of a git working copy (no dir/.git) or has no named branch checked
	// out (e.g. detached HEAD) — either way, the signal that dir is not "a
	// clone" for inference purposes.
	CurrentBranch(dir string) (string, error)
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
