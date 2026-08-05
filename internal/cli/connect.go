package cli

import (
	"context"

	"connectrpc.com/connect"

	"github.com/bobcob7/loam/internal/gen/loam/v1/loamv1connect"
)

// Header names the CLI attaches to every outbound Connect request, carrying
// the LOAM_AGENT_* identity (see docs/cli-spec.md -> Environment
// Variables: "the LOAM_AGENT_* values travel as the request headers
// Loam-Agent-Name, Loam-Agent-Id, and Loam-Agent-Role"). These MUST match
// internal/httpauth/middleware.go's headerAgentName/headerAgentID/
// headerAgentRole EXACTLY: agentIdentityFromHeaders there requires all
// three or treats the identity as entirely absent, and the server's
// fail-closed rule on /loam.v1.* (loam-gcg) then rejects the request with a
// 401 that gives no hint the cause was a header-name typo here. There is no
// shared constant to import — httpauth's are deliberately unexported, only
// its own middleware should name the wire format — so this package pins its
// own copy; connect_test.go's TestIdentityInterceptor_HeadersMatchHTTPAuth
// proves the two stay in sync by exercising httpauth's real CLI() wrapper
// end-to-end over an httptest server.
const (
	headerAgentName = "Loam-Agent-Name"
	headerAgentID   = "Loam-Agent-Id"
	headerAgentRole = "Loam-Agent-Role"
)

// identityInterceptor attaches the three Loam-Agent-* headers to every
// outbound unary request, from cfg's already-validated identity fields.
// newConnectClient refuses to construct a client at all when one of them is
// empty (see its doc comment), so this never sends a partial set — a
// partial set is exactly what the server's fail-closed rule (loam-gcg)
// treats as no identity at all, 401ing with a message that gives no hint
// the actual cause was a missing/blank LOAM_AGENT_* variable.
func identityInterceptor(cfg Config) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set(headerAgentName, cfg.AgentName())
			req.Header().Set(headerAgentID, cfg.AgentID())
			req.Header().Set(headerAgentRole, cfg.AgentRole())
			return next(ctx, req)
		}
	})
}

// connectClients implements ConnectClient over the real generated
// loamv1connect clients.
type connectClients struct {
	workBranch loamv1connect.WorkBranchServiceClient
	repo       loamv1connect.RepoServiceClient
	graph      loamv1connect.GraphServiceClient
	search     loamv1connect.SearchServiceClient
	meta       loamv1connect.MetaServiceClient
}

// WorkBranch returns the WorkBranchService seam.
func (c *connectClients) WorkBranch() WorkBranchClient { return c.workBranch }

// Repo returns the RepoService seam.
func (c *connectClients) Repo() RepoClient { return c.repo }

// Graph returns the GraphService seam.
func (c *connectClients) Graph() GraphClient { return c.graph }

// Search returns the SearchService seam.
func (c *connectClients) Search() SearchClient { return c.search }

// Meta returns the MetaService seam.
func (c *connectClients) Meta() MetaClient { return c.meta }

// newConnectClient builds the real ConnectClient targeting cfg.ServerURL()
// for every loam.v1 service, with identityInterceptor attaching the agent
// identity headers to every outbound request. It fails closed — before any
// request is ever sent — if any LOAM_AGENT_* identity field is empty:
// every config loader in config.go already guarantees this for the values
// it produces -- loadOrchestratorConfig by construction, since its identity
// is compile-time constants -- but this is a second, defensive check at the
// one other place a Config could reach here (e.g. a hand-built test
// double), because the alternative is a
// request with an incomplete header set silently reaching the server and
// coming back as a confusing 401 (see docs/cli-spec.md's fail-closed note
// for loam-gcg) instead of a clear local usage error. Unexported: only
// deps.go's NewProductionDeps and this package's own tests call it —
// cmd/loam imports NewProductionDeps, NewErrorMapper, NewRouter, and Run
// only.
func newConnectClient(cfg Config, httpClient connect.HTTPClient) (ConnectClient, error) {
	if cfg.AgentName() == "" || cfg.AgentID() == "" || cfg.AgentRole() == "" {
		return nil, newUsageCLIError("LOAM_AGENT_NAME, LOAM_AGENT_ID, and LOAM_AGENT_ROLE must all be set to make server requests", errMissingEnv)
	}
	baseURL := cfg.ServerURL()
	opts := []connect.ClientOption{connect.WithInterceptors(identityInterceptor(cfg))}
	return &connectClients{
		workBranch: loamv1connect.NewWorkBranchServiceClient(httpClient, baseURL, opts...),
		repo:       loamv1connect.NewRepoServiceClient(httpClient, baseURL, opts...),
		graph:      loamv1connect.NewGraphServiceClient(httpClient, baseURL, opts...),
		search:     loamv1connect.NewSearchServiceClient(httpClient, baseURL, opts...),
		meta:       loamv1connect.NewMetaServiceClient(httpClient, baseURL, opts...),
	}, nil
}
