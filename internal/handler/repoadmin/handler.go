package repoadmin

import (
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	adminv1 "github.com/bobcob7/loam/internal/gen/loam/admin/v1"
	"github.com/bobcob7/loam/internal/gen/loam/admin/v1/adminv1connect"
	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/reposstore"
)

// Handler implements adminv1connect.RepoAdminServiceHandler.
type Handler struct {
	dataDir      string
	store        repoStore
	workBranches workBranchLister
	credentials  credentialResolver
	checker      upstreamChecker
	cloner       cloner
	reconcile    mirrorReconciler
	ingest       ingestEnqueuer
	jobs         jobLister
	deleter      repoDeleter
	errors       *handler.ErrorMapper
	logger       *slog.Logger
}

// compile-time assertion that Handler satisfies the generated interface.
var _ adminv1connect.RepoAdminServiceHandler = (*Handler)(nil)

// New builds a Handler. dataDir is LOAM_DATA_DIR, the root EnrollRepo
// derives each repo's bare-mirror path under (internal/mirrorpath.Dir).
func New(
	dataDir string,
	store repoStore,
	workBranches workBranchLister,
	credentials credentialResolver,
	checker upstreamChecker,
	cloner cloner,
	reconcile mirrorReconciler,
	ingestEnqueuer ingestEnqueuer,
	jobs jobLister,
	deleter repoDeleter,
	errors *handler.ErrorMapper,
	logger *slog.Logger,
) *Handler {
	return &Handler{
		dataDir:      dataDir,
		store:        store,
		workBranches: workBranches,
		credentials:  credentials,
		checker:      checker,
		cloner:       cloner,
		reconcile:    reconcile,
		ingest:       ingestEnqueuer,
		jobs:         jobs,
		deleter:      deleter,
		errors:       errors,
		logger:       logger,
	}
}

// repoSegmentPattern is the same allowlist internal/handler/git's
// validRepoName uses for a filesystem-joined identifier segment (that
// package's own doc comment cites loam-hs5's settled reasoning): it must
// start with an alphanumeric and contain only alphanumerics, '.', '_', or
// '-'. That makes '.', '..', and empty segments impossible BY
// CONSTRUCTION, never by blacklisting known-bad substrings. This is
// necessarily a second copy, not a shared import: internal/handler/git's
// validRepoName is unexported, and the two call sites validate at
// different moments for different reasons (a URL-path segment already
// claimed by an existing enrollment there; a freshly URL-derived
// candidate identifier here, before any row exists) -- see EnrollRepo's
// own doc comment for why this bead is the write path repos.name needs a
// constraint at (loam-ofg.16's review: "..%2f traversal reaching a
// filesystem path because repos.name has NO CHECK constraint and the
// enroll path -- this bead -- did not exist to constrain it").
var repoSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validRepoName reports whether name is exactly two '/'-delimited
// segments (docs/persistence-spec.md's "<group>/<repo_name>"
// convention), each matching repoSegmentPattern. It REJECTS anything
// else -- including a traversal segment like ".." or an empty segment --
// rather than sanitizing it: silently rewriting an invalid identifier
// into some other path the caller cannot predict is worse than a clean
// rejection.
func validRepoName(name string) bool {
	segments := strings.Split(name, "/")
	if len(segments) != 2 {
		return false
	}
	for _, segment := range segments {
		if !repoSegmentPattern.MatchString(segment) {
			return false
		}
	}
	return true
}

// deriveRepoIdentity parses upstreamURL into the forge host EnrollRepo
// resolves credentials and CheckRepo against, and a candidate
// "<group>/<repo_name>" identifier derived from the URL's path
// (docs/web-spec.md -> RepoAdminService: "EnrollRepo ... the server
// derives the <group>/<repo_name> identifier"). It only parses; the
// caller must still run the result through validRepoName before trusting
// it as a filesystem-joined or database identifier -- this function
// accepts any syntactically valid http(s) URL, including one whose path
// has the wrong shape.
func deriveRepoIdentity(upstreamURL string) (host, name string, err error) {
	u, err := url.Parse(upstreamURL)
	if err != nil {
		return "", "", fmt.Errorf("parsing upstream url %s: %w", upstreamURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", fmt.Errorf("upstream url %s: scheme must be http or https", upstreamURL)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("upstream url %s: missing host", upstreamURL)
	}
	path := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
	return u.Host, path, nil
}

// stringOrEmpty dereferences s, or returns "" for a nil pointer -- the
// convention every EnrolledRepo/SyncStatus proto conversion in this
// package uses for reposstore's nullable *string/*time.Time fields, none
// of which have a proto-side "unset" representation other than "".
func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// timeOrEmpty formats t as RFC 3339, or returns "" for a nil pointer --
// the SyncStatus.last_synced_at / IngestJob timestamp convention pinned
// in the proto itself (proto/loam/admin/v1/repo_admin.proto:
// last_synced_at's "RFC 3339 timestamp of the last successful sync; empty
// if never", started_at/finished_at's "empty until they occur").
func timeOrEmpty(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

// syncStateToProto maps a reposstore.SyncState string to its proto enum,
// UNSPECIFIED for anything unrecognized (defensive: the DB's CHECK
// constraint should make that unreachable in production).
func syncStateToProto(s string) adminv1.SyncState {
	switch reposstore.SyncState(s) {
	case reposstore.SyncStateIdle:
		return adminv1.SyncState_SYNC_STATE_IDLE
	case reposstore.SyncStateSyncing:
		return adminv1.SyncState_SYNC_STATE_SYNCING
	case reposstore.SyncStateError:
		return adminv1.SyncState_SYNC_STATE_ERROR
	default:
		return adminv1.SyncState_SYNC_STATE_UNSPECIFIED
	}
}

// pageParams converts a loam.v1.Page request field to the (limit, offset)
// pair reposstore/ingest's list methods take. page may be nil (an unset
// request field): loamv1.Page's generated GetLimit/GetOffset are nil-safe
// (protoc-gen-go always generates a nil-receiver check), so this needs no
// separate nil branch; a non-positive limit is passed straight through as
// 0, which the store layer itself substitutes its own default for.
func pageParams(page *loamv1.Page) (limit, offset int32) {
	return int32(page.GetLimit()), int32(page.GetOffset())
}
