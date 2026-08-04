package repoadmin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/bobcob7/loam/internal/forge"
)

// ForgeChecker adapts forge.NewProvider to this package's upstreamChecker
// seam, satisfying it structurally. Unlike gittransport.Transport's
// gitCredentialConverter (a single, host-agnostic *forge.Resolver built
// once at composition-root time, since GitCredentials' convention is the
// same for every Kind this package resolves to), CheckRepo enforces that
// upstreamURL's host matches the bound Provider instance's OWN bound
// host (forge/forgejo_git.go's CheckRepo doc comment), so a fresh
// instance bound to host+token -- of whichever Kind host resolves to,
// loam-tmds.1's selection seam -- must be built per call. This is
// EnrollRepo's only use of this adapter, never reused or cached across
// repos: enrolment is the pre-repo case loam-tmds.1's own notes call
// out, so host and token arrive here as explicit RPC arguments, not from
// a repos row.
type ForgeChecker struct {
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// CheckRepo builds a single-use forge.Provider bound to host+token (of
// whichever Kind host resolves to) and delegates to its CheckRepo,
// satisfying upstreamChecker. A host that resolves to no known Kind
// (forge.NewProvider's own resolution error, via forge.KindForHost)
// fails here, before any network probe, naming the unresolvable host --
// never silently falling back to Forgejo.
func (f ForgeChecker) CheckRepo(ctx context.Context, host, token, upstreamURL string) error {
	provider, err := forge.NewProvider(host, token, f.HTTPClient, f.Logger)
	if err != nil {
		return fmt.Errorf("checking repo at %s: %w", host, err)
	}
	return provider.CheckRepo(ctx, upstreamURL)
}
