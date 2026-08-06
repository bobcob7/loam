package forge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// Kind names a forge implementation NewProvider can resolve to. It is a
// small, closed set by design (docs/sync-spec.md → Provider Interface):
// adding a third forge means adding a case to KindForHost and to
// NewProvider, not opening this up to caller-supplied values.
type Kind string

const (
	// KindForgejo is the default kind: every host that does not match a
	// more specific pattern below resolves here — self-hosted forges
	// live at arbitrary domains, so there is no positive signal to
	// require before trusting one as Forgejo. This preserves prior
	// behaviour for essentially every already-enrolled host, but NOT
	// byte-for-byte for all of them: a self-hosted Forgejo whose host
	// happens to contain the substring "github" (e.g.
	// "github-mirror.internal.corp") now fails to resolve at all
	// instead of defaulting here — see KindForHost's doc comment for
	// why that narrow behaviour change is accepted deliberately rather
	// than an oversight.
	KindForgejo Kind = "forgejo"
	// KindGitHub is github.com and its REST-API alias api.github.com.
	// GitHub Enterprise Server (a customer's own domain) is out of
	// scope — see KindForHost.
	KindGitHub Kind = "github"
)

// githubHost and githubAPIHost are the two host spellings KindForHost
// recognizes as KindGitHub: the web/git host an operator enters (and the
// one repos.forge_host/credentials.host store, per internal/forgehost's
// canonicalization — loam-tmds.5), and the REST API host, in case an
// operator enters that one into the Credentials screen's Host field by
// mistake.
const (
	githubHost    = "github.com"
	githubAPIHost = "api.github.com"
)

// errUnsupportedForgeKind is returned by KindForHost/NewProvider for a
// host this package cannot -- or does not yet -- resolve to an
// implementation. Unexported, matching internal/forgehost's errInvalid
// precedent: no caller needs to match it distinctly yet (every current
// caller just propagates the wrapped, host-naming error up as a plain
// failure), and errors_sentinels_test.go's AST discovery guard treats
// every exported Err* var in this package as a forge-RESPONSE sentinel
// AllSentinels()/fakeforge must model — this is a resolution-time
// error, returned before any Provider is even constructed, never BY one
// of the seven Provider methods, so it deliberately does not join that
// list. If a caller later needs errors.Is against this specific case,
// export it then and add it to AllSentinels() in the same commit.
var errUnsupportedForgeKind = errors.New("forge: unsupported or unrecognized forge kind for host")

// KindForHost resolves host to the forge Kind NewProvider would
// construct, without constructing anything -- the "recorded decision"
// loam-tmds.1 asks for, made inspectable and independently testable.
//
// THE RULE, and why: a host is KindGitHub only for an EXACT match on
// github.com or its REST-API alias api.github.com. Every other host
// defaults to KindForgejo, exactly as this project behaved before this
// epic existed: self-hosted forges live at arbitrary domains, so there
// is no positive signal to require before trusting one, and requiring
// one would silently break every already-enrolled repo.
//
// One exception: a host that CONTAINS "github" but is neither exact
// alias fails loudly instead of defaulting to KindForgejo, naming the
// host. THIS IS A SUBSTRING HEURISTIC, NOT GITHUB ENTERPRISE SERVER
// DETECTION, and the difference matters: GitHub Enterprise Server
// installs at whatever hostname the customer chooses -- "git.acme.com",
// "source.corp.io", "scm.example.net" are all ordinary GHE hostnames,
// and every one of them still falls through to KindForgejo below,
// unmitigated, sending its token to a Forgejo-shaped API URL exactly
// the way loam-tmds.1's own notes call the worst failure mode here.
// This check only catches the minority of GHE installs an operator
// happened to name after the vendor (or a host some OTHER Forgejo
// operator happened to name that way, with no GitHub involved at all --
// seeKindForgejo's own doc comment on that cost). It exists because
// catching that minority loudly is strictly better than catching none
// of them, not because it is a general Enterprise Server detector -- see
// docs/sync-spec.md's Limits section for the operator-facing statement
// of exactly what this does and does not catch, and KindForgejo's own
// doc comment for the corresponding cost against a non-GitHub host.
//
// An empty host (used by the three call sites that bind gittransport's
// shared, host-agnostic credential converter -- see Resolver) is also
// rejected: NewProvider is never called with an empty host in this
// tree, only Resolver is, and Resolver never calls KindForHost at all
// (see its own doc comment), so reaching this branch would mean a new
// caller added a pre-repo or repo-bound call site without a real host
// in hand, which is exactly the "no repo row to read yet" trap
// loam-tmds.1's own notes describe -- failing here surfaces that
// mistake immediately rather than routing an anonymous request at
// "https:///api/v1/...".
func KindForHost(host string) (Kind, error) {
	bare := strings.ToLower(hostOf(strings.TrimSpace(host)))
	if bare == "" {
		return "", fmt.Errorf("resolving forge kind: host is empty: %w", errUnsupportedForgeKind)
	}
	if bare == githubHost || bare == githubAPIHost {
		return KindGitHub, nil
	}
	if strings.Contains(bare, "github") {
		return "", fmt.Errorf("resolving forge kind for host %q: looks like GitHub but is not %s or %s; GitHub Enterprise Server is not supported: %w", redactHost(host), githubHost, githubAPIHost, errUnsupportedForgeKind)
	}
	return KindForgejo, nil
}

// NewProvider resolves host to a Kind via KindForHost and constructs the
// Provider implementation for it, bound to host and token exactly as
// NewForgejo/NewGitHub document. This is the ONLY function outside a
// forge-specific file (forgejo.go, github.go) that names a concrete
// provider type (loam-tmds.1's AC1) -- every caller in cmd/server and
// internal/handler/repoadmin that has a genuine, per-call host in hand
// goes through this function; the two call sites that need one
// long-lived, host-agnostic value instead go through Resolver, below.
func NewProvider(host, token string, httpClient *http.Client, logger *slog.Logger) (Provider, error) {
	kind, err := KindForHost(host)
	if err != nil {
		return nil, err
	}
	switch kind {
	case KindForgejo:
		return NewForgejo(host, token, httpClient, logger), nil
	case KindGitHub:
		return NewGitHub(host, token, httpClient, logger), nil
	default:
		return nil, fmt.Errorf("resolving forge kind for host %q: %s support is not implemented: %w", redactHost(host), kind, errUnsupportedForgeKind)
	}
}

// Resolver adapts NewProvider to the two composition-root call sites
// that need ONE long-lived, host-agnostic value satisfying a narrow
// consumer interface across EVERY host a request happens to name:
// internal/handler/credential's tokenValidator (ValidateToken takes host
// as an explicit per-call argument, by the Provider interface's own
// design, since CredentialService validates a candidate token for
// whatever host an admin just typed) and internal/gittransport's
// gitCredentialConverter (GitCredentials needs no host at all -- see its
// method doc and gitCredentialsConvention below). Every other call site
// in this tree resolves once, per call, via NewProvider directly, since
// it already has a specific host in hand (a repo's forge_host, or an
// enrolment request's host argument).
type Resolver struct {
	httpClient *http.Client
	logger     *slog.Logger
}

// NewResolver constructs a Resolver using httpClient for every
// underlying request. httpClient must not be nil.
func NewResolver(httpClient *http.Client, logger *slog.Logger) *Resolver {
	return &Resolver{httpClient: httpClient, logger: logger}
}

// ValidateToken resolves host's Kind fresh on every call -- never
// pre-bound, since a single Resolver instance is deliberately reused
// across requests naming different hosts (e.g. one admin enrolling a
// Forgejo repo, the next enrolling a GitHub one) -- and delegates to
// that Kind's Provider.
func (r *Resolver) ValidateToken(ctx context.Context, host, token string) error {
	provider, err := NewProvider(host, token, r.httpClient, r.logger)
	if err != nil {
		return err
	}
	return provider.ValidateToken(ctx, host, token)
}

// GitCredentials needs no host, and therefore no resolution: every Kind
// this package supports shares the git-over-HTTPS convention
// gitCredentialsConvention implements -- see that function's doc comment
// for why GitHub's classic-PAT convention and Forgejo's happen to
// coincide.
func (r *Resolver) GitCredentials(_ context.Context, token string) (string, string, error) {
	return gitCredentialsConvention(token)
}
