package repoadmin

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"connectrpc.com/connect"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/gittransport"
	"github.com/bobcob7/loam/internal/handler"
)

// headRefPrefix is the refs/heads/ prefix ls-remote reports branch refs
// under; ProbeRepo strips it to report the bare branch name the enroll
// form's picker and indexed_branch field both expect (matching how
// EnrolledRepo/EnrollRepoRequest carry bare branch names throughout).
const headRefPrefix = "refs/heads/"

// ProbeRepo lists upstreamURL's branches and current HEAD before
// enrollment, via an authenticated `git ls-remote --symref`
// (docs/web-spec.md -> RepoAdminService "ProbeRepo": "an authenticated
// ls-remote against the upstream using the URL's host credential").
// Read-only: it never creates a repos row, and EnrollRepo's own CheckRepo
// (read + write probes) remains the authoritative gate before an actual
// clone is attempted (docs/web-spec.md's own note on this).
func (h *Handler) ProbeRepo(ctx context.Context, req *connect.Request[adminv1.ProbeRepoRequest]) (*connect.Response[adminv1.ProbeRepoResponse], error) {
	upstreamURL := req.Msg.GetUpstreamUrl()
	if upstreamURL == "" {
		return nil, h.errors.ToConnectErr(fmt.Errorf("probe repo: empty upstream_url: %w", handler.ErrInvalidArgument))
	}
	u, err := url.Parse(upstreamURL)
	// upstream_url is deliberately never interpolated into an error
	// message below this point (loam-ra1k): this handles the raw string
	// straight back to whoever submitted it, and on the parse-failure
	// branch specifically, *url.Error's own Error() renders as
	// `parse "<raw url>": <reason>`, so even wrapping err via %w would
	// leak it -- err is discarded on purpose, not %w-wrapped.
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, h.errors.ToConnectErr(fmt.Errorf("probe repo: upstream_url is not a valid http(s) URL: %w", handler.ErrInvalidArgument))
	}
	// u.User != nil rejects an upstream URL carrying embedded credentials
	// (user:token@host) before it ever reaches an RPC error string or a
	// git subprocess. gittransport.Transport (h.cloner below) rejects the
	// same shape via this identical sentinel, but only after this method
	// has already derived host and called credentials.GetByHost -- this
	// is the cheaper, earlier fail-fast the credential never needs to
	// survive past.
	if u.User != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("probe repo: %w: %w", gittransport.ErrUpstreamURLHasUserinfo, handler.ErrInvalidArgument))
	}
	// host must be derived exactly like EnrollRepo's deriveRepoIdentity
	// (forgeHostOf, handler.go): both resolve the same credentials.host
	// row for the same forge, and for a plaintext-HTTP forge that row is
	// keyed by the scheme-qualified form, not upstreamURL's bare u.Host
	// (loam-4kz).
	host := forgeHostOf(u)
	// h.cloner.LsRemote (gittransport.Transport) resolves the credential
	// itself before running the git subprocess; this call is purely to
	// fail fast with a clear "no credential configured" message, rather
	// than a generic git-subprocess failure, when the host was never
	// enrolled with a token at all.
	if _, err := h.credentials.GetByHost(ctx, host); err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("probe repo: no usable credential for host %s: %w: %w", host, err, handler.ErrFailedPrecondition))
	}
	out, err := h.cloner.LsRemote(ctx, host, upstreamURL)
	if err != nil {
		// host, not upstreamURL, per this file's leading comment on the
		// parse-validation branch above (loam-ra1k): even though this
		// point is unreachable with a userinfo-bearing upstreamURL now,
		// nothing here should rely on that ordering to stay leak-free.
		return nil, h.errors.ToConnectErr(fmt.Errorf("probe repo %s: %w: %w", host, err, handler.ErrFailedPrecondition))
	}
	branches, head, err := parseLsRemote(out)
	if err != nil {
		return nil, h.errors.ToConnectErr(fmt.Errorf("probe repo %s: parsing ls-remote output: %w", host, err))
	}
	return connect.NewResponse(&adminv1.ProbeRepoResponse{Branches: branches, Head: head}), nil
}

// parseLsRemote parses `git ls-remote --symref` output into a sorted,
// de-duplicated branch name list (refs/heads/* entries, prefix stripped)
// plus the HEAD symref's target branch name, from its
// "ref: refs/heads/<name>\tHEAD" line. A malformed or ref-less output
// (no symref line, or no refs/heads/* lines at all) is not itself an
// error here -- an empty upstream repo genuinely has no branches yet --
// so this never returns an error except a real scan failure.
func parseLsRemote(out []byte) (branches []string, head string, err error) {
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "ref: ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				head = strings.TrimPrefix(fields[1], headRefPrefix)
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		ref := fields[1]
		if !strings.HasPrefix(ref, headRefPrefix) {
			continue
		}
		name := strings.TrimPrefix(ref, headRefPrefix)
		if seen[name] {
			continue
		}
		seen[name] = true
		branches = append(branches, name)
	}
	if err := scanner.Err(); err != nil {
		return nil, "", err
	}
	sort.Strings(branches)
	return branches, head, nil
}
