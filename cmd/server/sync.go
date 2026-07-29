package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bobcob7/loam/internal/config"
	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/crypto"
	"github.com/bobcob7/loam/internal/db/gen"
	"github.com/bobcob7/loam/internal/forge"
	"github.com/bobcob7/loam/internal/gitmergetree"
	"github.com/bobcob7/loam/internal/gittransport"
	"github.com/bobcob7/loam/internal/ingest"
	"github.com/bobcob7/loam/internal/mirrorsync"
	"github.com/bobcob7/loam/internal/mirrorsync/state"
	"github.com/bobcob7/loam/internal/reposstore"
	"github.com/bobcob7/loam/internal/workbranchstore"
)

// errNonPositiveSyncInterval is returned by validateSyncInterval for a
// LOAM_SYNC_INTERVAL of zero or less. It is a startup error rather than a
// clamp-to-a-default because either value is a genuine operator mistake
// with no sensible interpretation: a zero interval is not "sync as fast as
// possible" (time.NewTicker rejects it outright) and a negative one is not
// "never sync".
var errNonPositiveSyncInterval = errors.New("LOAM_SYNC_INTERVAL must be greater than zero")

// validateSyncInterval rejects a non-positive LOAM_SYNC_INTERVAL before
// run() ever builds the sync ticker.
//
// This guard is load-bearing, not defensive decoration. internal/config
// parses LOAM_SYNC_INTERVAL with time.ParseDuration and range-checks
// nothing (config/env.go's parseDurationEnv), so "0s" and "-5m" both load
// cleanly today; time.NewTicker panics on any non-positive duration; and
// nothing on this binary's startup path installs a recover() (loam-337
// added one, but it guards a claimed ingest job inside ingest.Pool, which
// is started long after this value is consumed and would never see this
// panic). Without this check, a single
// mistyped environment variable turns startup into a panic-and-stack-trace
// crash instead of a one-line configuration error. Checking here -- at the
// consumer, before anything else in run() runs -- also means the process
// fails before it opens a database connection, creates a mirror, or binds
// a port.
//
// loam-35b owns the fuller fix (the same range check inside
// internal/config, alongside LOAM_INGEST_WORKERS, with its own sentinel
// and a documented constraint column in docs/server-spec.md's
// Configuration table). This is deliberately not that: it is the narrowest
// guard that makes the value this file consumes safe to consume, and it
// stays correct -- merely redundant -- once loam-35b lands its own.
func validateSyncInterval(interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("%w (got %s)", errNonPositiveSyncInterval, interval)
	}
	return nil
}

// syncRunner adapts a *mirrorsync.Scheduler to serve's runner contract:
// "Run blocks until ctx is canceled AND every unit of work it already
// started has drained" (interfaces.go). Scheduler.Run alone does not
// satisfy that second half -- it returns the instant ctx is canceled while
// the per-repo cycle goroutines it started keep running -- so this type
// pairs it with Scheduler.Shutdown, which is exactly the drain seam
// loam-ofg.21 added for this call site. serve therefore needs no special
// case for the scheduler: it starts and waits on it exactly as it does the
// ingest pool and the policy socket.
//
// It holds the scheduler's Run and Shutdown METHOD VALUES, never the
// *mirrorsync.Scheduler itself, and that is loam-f75 ("never call
// Scheduler.Run and Scheduler.Tick on the same Scheduler") satisfied by
// construction rather than by convention: newSyncRunner below is the
// only function in this package that ever holds the Scheduler value, it
// holds it as a local, and it returns a plain runner. No other code in
// this binary -- production or test -- has a reference through which Tick
// could be reached, so there is no comment to obey and no rule a later
// change could silently break. The acceptance harness reaches the opposite
// end of the same constraint from its own side (see
// acceptance_harness_test.go's newSyncHarness).
type syncRunner struct {
	run      func(ctx context.Context)
	shutdown func(ctx context.Context) error
	grace    time.Duration
	logger   *slog.Logger
}

// Run implements runner: it runs the scheduler's tick loop until ctx is
// canceled (or the tick channel closes), then drains whatever cycles were
// still in flight at that moment, bounded by grace.
//
// The drain context is derived with context.WithoutCancel: by the time
// this line is reached the ctx passed in is already canceled, so deriving
// the drain deadline from it would produce an already-expired context and
// the "drain" would return instantly, waiting for nothing -- which is the
// same do-nothing shutdown that made Scheduler.Shutdown necessary in the
// first place.
//
// Note what this drain does and does not do, per Scheduler.Shutdown's own
// contract: it waits for in-flight cycles, it never kills them. Their
// underlying work does observe the canceled ctx (git subprocesses run
// under exec.CommandContext, Postgres calls take the same ctx), so in
// practice they unwind quickly with a cancellation error and the repo is
// picked up again on the next process's first tick -- the crash-recovery
// path docs/server-spec.md's Shutdown contract already describes.
func (s syncRunner) Run(ctx context.Context) {
	s.run(ctx)
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.grace)
	defer cancel()
	if err := s.shutdown(drainCtx); err != nil {
		s.logger.Warn("sync scheduler did not drain within the shutdown grace period", "grace", s.grace, "error", err)
	}
}

// defaultMaxConcurrentCycles bounds how many repo sync cycles this binary
// runs at once, across every enrolled repo combined -- the cap loam-5v5
// added the mechanism for (mirrorsync.WithMaxConcurrentCycles) and
// loam-k1fb applies here, at the composition root. It is a package-level
// default with the Option reserved for tests, exactly the shape
// defaultShutdownGrace (serve.go) uses, and for the same reason: an option
// that only tests ever pass bounds nothing in production, which is what
// left tick's one-goroutine-per-enrolled-repo fan-out live in the shipped
// binary after the mechanism landed.
//
// Why 32 -- neither a smaller "obviously safe" number nor a much larger
// one:
//
//   - It is a ceiling on concurrently held OS resources, which is the
//     thing actually being bounded. Every in-flight cycle owns a git fetch
//     subprocess pair (git plus its git-remote-https helper) and the pipes
//     os/exec holds for it, one or more forge HTTPS connections, and a pgx
//     pool connection during each of its store-backed steps. Thirty-two of
//     those sit comfortably inside a container's usual 1024 open-file
//     limit and inside darwin's stingier 256 for local runs. The unbounded
//     behaviour this replaces put no ceiling on any of them, and the "few
//     thousand enrolled repos" loam-5v5 names exhausts all three at once.
//   - It sits above the pgx pool's own default MaxConns
//     (pgxpool.ParseConfig defaults it to max(4, NumCPU) and internal/db's
//     NewPool does not override it), so the DB-bound steps still saturate
//     that pool rather than being throttled below it -- but not so far
//     above that dozens of cycles pile up waiting on Acquire.
//   - It keeps a full sweep inside the tick interval at the enrollment
//     scale that motivated the bound. The common per-repo cycle is a no-op
//     incremental fetch -- one round trip, no pack, a few hundred
//     milliseconds -- so a sweep costs roughly ceil(N/32) of those: ~10s
//     at 1,000 repos and ~50s at 5,000, both inside LOAM_SYNC_INTERVAL's
//     60s default. Past that the sweep degrades gracefully rather than
//     piling up (tick's tryStart skips a repo whose previous cycle is
//     still queued or running), but the effective interval does stretch --
//     the honest cost of any bound, and the reason this one is not
//     smaller.
//
// Shutdown is deliberately NOT an argument for keeping this number large,
// despite how the arithmetic looks: the cycles still queued behind the
// bound when the process is signalled do not each cost a full cycle before
// the drain completes. They inherit the already-canceled context, and
// every production collaborator honours it -- git runs under
// exec.CommandContext (internal/gittransport, internal/gitmergetree), the
// forge client builds every request with http.NewRequestWithContext
// (internal/forge), and pgx fails a canceled query immediately -- so a
// queued cycle unwinds through its five steps in microseconds, not
// seconds. What defaultShutdownGrace has to cover is therefore about one
// in-flight wave, near enough independent of this value.
//
// What this number deliberately does not solve: a cycle that HANGS holds
// its slot indefinitely, so enough simultaneously hung repos starve every
// other repo -- head-of-line blocking that a smaller bound makes worse and
// a larger one only postpones (hung slots accumulate; they are never
// reclaimed). The fix for that is a timeout on the calls that can hang --
// loam-1kl, the forge REST client having none -- not a bigger constant
// here.
const defaultMaxConcurrentCycles = 32

// newSyncRunner builds the *mirrorsync.Scheduler over already-constructed
// collaborators and wraps it as a runner, applying the production bound
// (defaultMaxConcurrentCycles) on the way through. Its parameters mirror
// mirrorsync.New's own, in the same order, plus the shutdown grace period
// syncRunner drains under.
//
// It exists as its own function so the bound is testable as WIRING rather
// than as an option value: buildSyncScheduler below needs a live
// *pgxpool.Pool to construct its seven collaborators, so no unit test can
// reach the Scheduler it builds, and a test that merely re-applied
// WithMaxConcurrentCycles itself would pass just as happily if this
// binary passed nothing at all. Everything between here and serve's
// background tier -- the bound, the Run/Shutdown pairing, the grace period
// -- is exercised through this one function instead (sync_test.go).
//
// The Scheduler value stays a local here and only its method values
// escape, which is where syncRunner's doc comment's loam-f75 claim is
// actually enforced.
func newSyncRunner(logger *slog.Logger, ticks <-chan time.Time, repos mirrorsync.RepoLister, fetcher mirrorsync.Fetcher, advances mirrorsync.AdvanceDetector, mergeability mirrorsync.MergeabilityChecker, enqueuer mirrorsync.IngestEnqueuer, prPoller mirrorsync.PRPoller, reporter mirrorsync.SyncStateReporter, grace time.Duration) runner {
	scheduler := mirrorsync.New(
		logger,
		ticks,
		repos,
		fetcher,
		advances,
		mergeability,
		enqueuer,
		prPoller,
		reporter,
		mirrorsync.WithMaxConcurrentCycles(defaultMaxConcurrentCycles),
	)
	return syncRunner{run: scheduler.Run, shutdown: scheduler.Shutdown, grace: grace, logger: logger}
}

// buildSyncScheduler constructs the production mirrorsync.Scheduler and
// every one of its seven collaborators over pool and ingestPool -- the
// same live Postgres connection and ingest worker pool run() already
// built, never a second, divergent instance -- and returns it as a plain
// runner for serve's background tier.
//
// The graph is deliberately identical to the one
// cmd/server/acceptance_harness_test.go's newSyncHarness builds (loam-a16
// wired that harness to mirror this function). Two differences are real
// and intended, both of them substitutions of a test double for its
// production counterpart, not structural: the harness points its
// gittransport.Transport and its forge surface at the shared fakeforge
// instance with one static token, while this function resolves each repo's
// own credential from the encrypted credential store (via
// gittransport.Transport itself for git, and via forgePRTracker below for
// the forge REST surface). Everything else -- constructor order,
// arguments, and which store backs which seam -- matches one for one,
// including the concurrency bound: the harness passes the same
// defaultMaxConcurrentCycles this function does, so an acceptance scenario
// never exercises a fan-out shape production cannot produce.
//
// ticks is the trigger seam. run() passes a real time.Ticker's channel;
// the interval is validated by validateSyncInterval before the ticker is
// ever constructed. The Scheduler itself is constructed by newSyncRunner
// above, which is where the bound is applied.
//
// A failure here fails startup rather than degrading to a stand-in, the
// same choice buildIngestOrchestrator makes and for the same reason: a
// scheduler wired over a broken collaborator would drive every enrolled
// repo into sync_state='error' on every tick, which is materially worse
// than not booting.
func buildSyncScheduler(cfg config.Config, pool *pgxpool.Pool, ingestPool *ingest.Pool, ticks <-chan time.Time, grace time.Duration) (runner, error) {
	encryptor, err := crypto.NewEncryptor(cfg.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("building encryptor: %w", err)
	}
	repos := reposstore.NewStore(gen.New(pool), cfg.Logger)
	workBranches := workbranchstore.New(gen.New(pool), cfg.Logger)
	credentials := credentialstore.New(pool, encryptor, cfg.Logger)
	httpClient := &http.Client{}
	// One host-agnostic *forge.Forgejo for gitCredentialConverter, exactly
	// as registerRepoAdminService builds it and for the reason documented
	// there: GitCredentials' token-as-password convention is the same for
	// every Forgejo host, so it needs no binding. The forge REST surface
	// the PR poller reads DOES need binding, which is what forgePRTracker
	// below exists for.
	transport := gittransport.New(credentials, forge.NewForgejo("", "", httpClient, cfg.Logger), cfg.Logger)
	resolver := mirrorsync.NewStoreRepoResolver(repos, workBranches)
	fetcher := mirrorsync.NewMirrorFetcher(cfg.DataDir, transport, resolver)
	advances := mirrorsync.NewStoreAdvanceDetector(repos, repos, workBranches)
	mergeability := mirrorsync.NewStoreMergeabilityChecker(cfg.DataDir, repos, workBranches, gitmergetree.New(cfg.Logger), workBranches)
	enqueuer := mirrorsync.NewStoreIngestEnqueuer(repos, repos, ingestPool)
	tracker := forgePRTracker{repos: repos, credentials: credentials, httpClient: httpClient, logger: cfg.Logger}
	prPoller := mirrorsync.NewStorePRPoller(cfg.DataDir, cfg.Logger, repos, workBranches, workBranches, tracker, transport)
	return newSyncRunner(
		cfg.Logger,
		ticks,
		mirrorsync.NewStoreRepoLister(repos),
		fetcher,
		advances,
		mergeability,
		enqueuer,
		prPoller,
		state.New(pool),
		grace,
	), nil
}

// buildProposalAccepter constructs the production
// *mirrorsync.StoreProposalAccepter -- Proposal Acceptance's push +
// CreatePR + record leg (docs/sync-spec.md -> Proposal Acceptance) -- over
// pool, wired exactly as buildSyncScheduler wires the collaborators it
// shares (the same reposstore/workbranchstore over the same live pool, the
// same gittransport.Transport resolving each host's credential per
// invocation, and the same per-repo forgePRTracker for the forge REST
// surface).
//
// This is the composition-root half of the seam that makes
// work_branches.upstream_pr_number a column anything actually writes: no
// other code in this tree writes it, and it is the whole poll set of
// mirrorsync.StorePRPoller (Mirror Sync step 5), so until an accept runs
// that step polls nothing.
//
// It is a constructor, not a runner: acceptance is synchronous and
// admin-triggered (docs/sync-spec.md: "The RPC is synchronous and
// idempotent by construction"), so nothing in serve's background tier
// starts it. Its caller is the AcceptProposal RPC handler --
// ProposalService (loam-ofg.14), registered by main.go's
// registerProposalService -- and it keeps the production wiring (per-repo
// forge binding, attribution from config, mirror root from LOAM_DATA_DIR)
// in one place next to the sync graph it mirrors rather than re-derived
// inside a handler.
func buildProposalAccepter(cfg config.Config, pool *pgxpool.Pool, httpClient *http.Client) (*mirrorsync.StoreProposalAccepter, error) {
	encryptor, err := crypto.NewEncryptor(cfg.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("building encryptor: %w", err)
	}
	repos := reposstore.NewStore(gen.New(pool), cfg.Logger)
	workBranches := workbranchstore.New(gen.New(pool), cfg.Logger)
	credentials := credentialstore.New(pool, encryptor, cfg.Logger)
	transport := gittransport.New(credentials, forge.NewForgejo("", "", httpClient, cfg.Logger), cfg.Logger)
	tracker := forgePRTracker{repos: repos, credentials: credentials, httpClient: httpClient, logger: cfg.Logger}
	return mirrorsync.NewStoreProposalAccepter(cfg.DataDir, cfg.Logger, cfg.PRAttribution, repos, workBranches, workBranches, tracker, transport), nil
}

// buildUpstreamPRCloser constructs the *mirrorsync.StorePRPoller the admin
// CloseWorkBranch path reaches for ClosePRAndCleanup -- the "Loam opened
// it, Loam closes it" half of docs/web-spec.md -> ProposalService -- over
// the same collaborators buildSyncScheduler wires its own poller over.
//
// It is a SECOND StorePRPoller instance, not the scheduler's, and that is
// safe by the type's own construction: StorePRPoller holds no mutable
// state of its own (dataDir, logger, and five seams, all set once at
// construction), so two instances over the same stores are
// indistinguishable from one. Returning the scheduler's instance instead
// would mean buildSyncScheduler handing out a reference to an object whose
// Run/Tick discipline it currently guarantees by never letting the
// Scheduler value escape (see syncRunner's doc comment); a separate
// instance keeps that property untouched.
//
// Only ClosePRAndCleanup is called through it. PollPRs on this instance is
// never invoked -- the scheduler's own poller owns the cycle.
func buildUpstreamPRCloser(cfg config.Config, pool *pgxpool.Pool, httpClient *http.Client) (*mirrorsync.StorePRPoller, error) {
	encryptor, err := crypto.NewEncryptor(cfg.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("building encryptor: %w", err)
	}
	repos := reposstore.NewStore(gen.New(pool), cfg.Logger)
	workBranches := workbranchstore.New(gen.New(pool), cfg.Logger)
	credentials := credentialstore.New(pool, encryptor, cfg.Logger)
	transport := gittransport.New(credentials, forge.NewForgejo("", "", httpClient, cfg.Logger), cfg.Logger)
	tracker := forgePRTracker{repos: repos, credentials: credentials, httpClient: httpClient, logger: cfg.Logger}
	return mirrorsync.NewStorePRPoller(cfg.DataDir, cfg.Logger, repos, workBranches, workBranches, tracker, transport), nil
}

// forgePRTracker is the production forge REST surface Mirror Sync step 5
// reads PR state through, and proposal acceptance opens a pull request
// through: mirrorsync's pullRequestTracker and pullRequestOpener seams,
// both satisfied by resolving each call's repo to its OWN forge host and
// token and building a single-use *forge.Forgejo bound to that pair.
//
// A per-call instance is required, not an optimisation deferred: a
// *forge.Forgejo is bound to one host and one token at construction
// (forge.NewForgejo's doc comment), while StorePRPoller is one object
// polling every enrolled repo, and different repos can be enrolled against
// different forge hosts with different credentials. A single shared
// instance would have to be bound to some arbitrary host -- or to the
// empty host, which makes every request target the literal URL
// "https:///api/v1/..." -- and would silently send one repo's token to
// another repo's forge. repoadmin.ForgeChecker already reached this same
// conclusion for CheckRepo and builds per call for the same reason; this
// is that pattern applied to the poller's own two calls.
//
// It is defined here, at the composition root, rather than in
// internal/mirrorsync: it is the join of three things this binary owns and
// that package deliberately does not import -- the repos store, the
// encrypted credential store, and the concrete forge implementation.
type forgePRTracker struct {
	repos       repoForgeLookup
	credentials forgeCredentialLookup
	httpClient  *http.Client
	logger      *slog.Logger
}

// GetPRState implements mirrorsync's pullRequestTracker.
func (t forgePRTracker) GetPRState(ctx context.Context, repo string, prNumber int) (string, error) {
	provider, err := t.provider(ctx, repo)
	if err != nil {
		return "", err
	}
	return provider.GetPRState(ctx, repo, prNumber)
}

// ClosePR implements mirrorsync's pullRequestTracker.
func (t forgePRTracker) ClosePR(ctx context.Context, repo string, prNumber int) error {
	provider, err := t.provider(ctx, repo)
	if err != nil {
		return err
	}
	return provider.ClosePR(ctx, repo, prNumber)
}

// CreatePR implements mirrorsync's pullRequestOpener. It binds per call
// exactly as GetPRState and ClosePR do -- the binding is a property of
// *forge.Forgejo, not of which operation is being performed, so an
// acceptance for a repo on one forge can never be sent with another
// forge's token.
func (t forgePRTracker) CreatePR(ctx context.Context, repo, headBranch, targetBranch, title, description string) (string, int, error) {
	provider, err := t.provider(ctx, repo)
	if err != nil {
		return "", 0, err
	}
	return provider.CreatePR(ctx, repo, headBranch, targetBranch, title, description)
}

// FindOpenPR implements mirrorsync's pullRequestOpener -- the lookup the
// accept engine adopts an already-existing PR through when CreatePR
// answers forge.ErrDuplicatePR.
func (t forgePRTracker) FindOpenPR(ctx context.Context, repo, headBranch, targetBranch string) (string, int, bool, error) {
	provider, err := t.provider(ctx, repo)
	if err != nil {
		return "", 0, false, err
	}
	return provider.FindOpenPR(ctx, repo, headBranch, targetBranch)
}

// provider resolves repo's enrolled forge host and that host's stored
// token, and returns a *forge.Forgejo bound to both. Failures are
// returned, never swallowed into an unauthenticated provider: an
// unauthenticated PR read against a private repo answers 404, which
// StorePRPoller would report as an unknown state rather than as the
// credential problem it actually is.
func (t forgePRTracker) provider(ctx context.Context, repo string) (*forge.Forgejo, error) {
	row, err := t.repos.GetRepoByName(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("resolving repo %s for forge access: %w", repo, err)
	}
	credential, err := t.credentials.GetByHost(ctx, row.ForgeHost)
	if err != nil {
		return nil, fmt.Errorf("resolving credential for forge host %s: %w", row.ForgeHost, err)
	}
	return forge.NewForgejo(row.ForgeHost, credential.Token, t.httpClient, t.logger), nil
}
