package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/config"
	"github.com/bobcob7/loam/internal/credentialstore"
	"github.com/bobcob7/loam/internal/mirrorsync"
	"github.com/bobcob7/loam/internal/reposstore"
)

// syncTestLogger is this file's discard-everything logger, per the repo's
// test-logger convention.
func syncTestLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// TestValidateSyncInterval_RejectsNonPositiveDurations pins the guard that
// stands between a mistyped LOAM_SYNC_INTERVAL and a time.NewTicker panic
// in a binary with no recover() anywhere. internal/config parses the
// duration but range-checks nothing (loam-35b), so zero and negative both
// reach run() today.
func TestValidateSyncInterval_RejectsNonPositiveDurations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		interval time.Duration
		wantErr  bool
	}{
		{name: "zero is rejected: time.NewTicker panics on it", interval: 0, wantErr: true},
		{name: "negative is rejected: time.NewTicker panics on it too", interval: -5 * time.Minute, wantErr: true},
		{name: "the smallest positive duration is accepted", interval: time.Nanosecond, wantErr: false},
		{name: "the documented default is accepted", interval: 60 * time.Second, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateSyncInterval(tt.interval)
			if !tt.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorIs(t, err, errNonPositiveSyncInterval)
		})
	}
}

// TestSyncRunner_DrainsInFlightCyclesAfterRunReturns is the shutdown-drain
// invariant: serve's runner contract is "Run blocks until ctx is canceled
// AND every unit of work it already started has drained", and
// Scheduler.Run satisfies only the first half -- it returns the instant
// ctx is canceled while its per-repo cycle goroutines keep running. This
// asserts syncRunner supplies the second half by calling Shutdown, and
// that it does so AFTER the tick loop has returned, never before (draining
// while the loop can still start new cycles would drain nothing useful).
//
// The order slice, not timing, is what makes this deterministic: both
// entries are appended under the same mutex from the single goroutine
// syncRunner.Run occupies.
func TestSyncRunner_DrainsInFlightCyclesAfterRunReturns(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var order []string
	runner := syncRunner{
		run: func(ctx context.Context) {
			<-ctx.Done()
			mu.Lock()
			order = append(order, "run returned")
			mu.Unlock()
		},
		shutdown: func(context.Context) error {
			mu.Lock()
			order = append(order, "shutdown called")
			mu.Unlock()
			return nil
		},
		grace:  time.Second,
		logger: syncTestLogger(),
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.Run(ctx)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("syncRunner.Run did not return after ctx was canceled")
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"run returned", "shutdown called"}, order,
		"syncRunner.Run must drain in-flight cycles via Shutdown, and only once the tick loop has already returned")
}

// TestSyncRunner_DrainContextOutlivesTheCanceledRunContext pins the one
// subtle line in syncRunner.Run: the drain deadline is derived with
// context.WithoutCancel. By the time the drain starts, the ctx handed to
// Run is ALWAYS already canceled -- that is what made Run return -- so
// deriving the drain context from it directly would hand Shutdown an
// already-expired context, which returns immediately having waited for
// nothing. That is exactly the do-nothing shutdown Scheduler.Shutdown
// exists to prevent, and it is invisible from the outside: the process
// still exits 0, it just abandons every in-flight cycle.
func TestSyncRunner_DrainContextOutlivesTheCanceledRunContext(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var drainErr error
	var drainDeadline time.Time
	var hasDeadline bool
	runner := syncRunner{
		run: func(ctx context.Context) { <-ctx.Done() },
		shutdown: func(ctx context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			drainErr = ctx.Err()
			drainDeadline, hasDeadline = ctx.Deadline()
			return nil
		},
		grace:  30 * time.Second,
		logger: syncTestLogger(),
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	runner.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	require.NoError(t, drainErr, "the drain context must still be live: a canceled one makes Shutdown wait for nothing")
	require.True(t, hasDeadline, "the drain must be bounded by the shutdown grace period")
	assert.WithinDuration(t, time.Now().Add(30*time.Second), drainDeadline, 5*time.Second,
		"the drain deadline must come from the configured grace period")
}

// TestSyncRunner_LogsButDoesNotBlockWhenTheDrainTimesOut proves a
// scheduler that fails to drain within the grace period does not wedge
// shutdown: Run still returns, so serve's own bounded wait completes and
// the process exits.
func TestSyncRunner_LogsButDoesNotBlockWhenTheDrainTimesOut(t *testing.T) {
	t.Parallel()
	runner := syncRunner{
		run:      func(ctx context.Context) { <-ctx.Done() },
		shutdown: func(context.Context) error { return context.DeadlineExceeded },
		grace:    time.Millisecond,
		logger:   syncTestLogger(),
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.Run(ctx)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("syncRunner.Run must return even when the drain reports a timeout")
	}
}

// forgeAPIRecorder is a fake Forgejo REST endpoint recording the one
// request it receives and answering with a merged pull request.
type forgeAPIRecorder struct {
	mu            sync.Mutex
	path          string
	method        string
	authorization string
	// body is the decoded JSON request body, for the CreatePR call site
	// whose payload (head, base, title, body) is itself part of what has
	// to be pinned -- the PR body is where the attribution footer reaches
	// the wire.
	body map[string]any
}

func (r *forgeAPIRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.path = req.URL.Path
	r.method = req.Method
	r.authorization = req.Header.Get("Authorization")
	r.body = nil
	if req.Body != nil {
		var decoded map[string]any
		if err := json.NewDecoder(req.Body).Decode(&decoded); err == nil {
			r.body = decoded
		}
	}
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	// The list endpoint FindOpenPR pages through wants an ARRAY; every
	// single-PR endpoint wants an object. Both are served off the same
	// recorder, keyed on the path shape Forgejo's own API uses.
	if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/pulls") {
		_ = json.NewEncoder(w).Encode([]any{})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed", "merged": true})
}

// TestForgePRTracker_BindsEachCallToTheReposOwnHostAndToken is the whole
// reason forgePRTracker exists. A *forge.Forgejo is bound to one host and
// one token at construction, while StorePRPoller is a single object
// polling every enrolled repo -- so the host and token must be resolved
// per call from the repo's own row and that host's own stored credential.
// A shared, pre-bound provider would send one repo's token to another
// repo's forge (or, bound to the empty host, would target the literal URL
// "https:///api/v1/...").
//
// The assertion is on the request the forge actually received: its path
// proves the host binding (the request reached THIS server, at the repo
// row's forge_host), and its Authorization header proves the token came
// from the credential store rather than being empty or hardcoded.
func TestForgePRTracker_BindsEachCallToTheReposOwnHostAndToken(t *testing.T) {
	t.Parallel()
	recorder := &forgeAPIRecorder{}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)
	tracker := forgePRTracker{
		repos: &repoForgeLookupMock{GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			return reposstore.Repo{Name: name, ForgeHost: server.URL}, nil
		}},
		credentials: &forgeCredentialLookupMock{GetByHostFunc: func(_ context.Context, host string) (credentialstore.Credential, error) {
			return credentialstore.Credential{Host: host, Token: "tkn-from-the-credential-store"}, nil
		}},
		httpClient: server.Client(),
		logger:     syncTestLogger(),
	}
	state, err := tracker.GetPRState(t.Context(), "acme/widgets", 7)
	require.NoError(t, err)
	assert.Equal(t, "merged", state)
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	assert.Equal(t, "/api/v1/repos/acme/widgets/pulls/7", recorder.path,
		"the request must reach the forge host recorded on the repo's own row")
	assert.Equal(t, "token tkn-from-the-credential-store", recorder.authorization,
		"the request must carry the token the credential store returned for that host")
}

// TestForgePRTracker_ClosePR_BindsTheSameWayAndIssuesAPatch pins the
// second of the two pullRequestTracker methods, which the admin-close path
// reaches through StorePRPoller.
func TestForgePRTracker_ClosePR_BindsTheSameWayAndIssuesAPatch(t *testing.T) {
	t.Parallel()
	recorder := &forgeAPIRecorder{}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)
	tracker := forgePRTracker{
		repos: &repoForgeLookupMock{GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			return reposstore.Repo{Name: name, ForgeHost: server.URL}, nil
		}},
		credentials: &forgeCredentialLookupMock{GetByHostFunc: func(_ context.Context, host string) (credentialstore.Credential, error) {
			return credentialstore.Credential{Host: host, Token: "tkn"}, nil
		}},
		httpClient: server.Client(),
		logger:     syncTestLogger(),
	}
	require.NoError(t, tracker.ClosePR(t.Context(), "acme/widgets", 7))
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	assert.Equal(t, http.MethodPatch, recorder.method)
	assert.Equal(t, "/api/v1/repos/acme/widgets/pulls/7", recorder.path)
	assert.Equal(t, "token tkn", recorder.authorization)
}

// TestForgePRTracker_MissingCredentialIsReportedNotSwallowed proves an
// unresolvable credential surfaces as an error rather than degrading to an
// anonymous request. That distinction matters: an unauthenticated PR read
// against a private repo answers 404, which StorePRPoller reads as an
// unknown state and reports as a non-destructive no-op -- so a swallowed
// credential failure would look like a healthy poll forever.
func TestForgePRTracker_MissingCredentialIsReportedNotSwallowed(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("no credential for host")
	tracker := forgePRTracker{
		repos: &repoForgeLookupMock{GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			return reposstore.Repo{Name: name, ForgeHost: "forge.example.com"}, nil
		}},
		credentials: &forgeCredentialLookupMock{GetByHostFunc: func(context.Context, string) (credentialstore.Credential, error) {
			return credentialstore.Credential{}, wantErr
		}},
		httpClient: &http.Client{},
		logger:     syncTestLogger(),
	}
	_, err := tracker.GetPRState(t.Context(), "acme/widgets", 7)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Contains(t, err.Error(), "forge.example.com", "the error must name the host whose credential is missing")
}

// TestForgePRTracker_UnknownRepoIsReported proves a repo lookup failure
// aborts the call rather than producing a provider bound to a zero-value
// host.
func TestForgePRTracker_UnknownRepoIsReported(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("repo not found")
	credentialsCalled := false
	tracker := forgePRTracker{
		repos: &repoForgeLookupMock{GetRepoByNameFunc: func(context.Context, string) (reposstore.Repo, error) {
			return reposstore.Repo{}, wantErr
		}},
		credentials: &forgeCredentialLookupMock{GetByHostFunc: func(context.Context, string) (credentialstore.Credential, error) {
			credentialsCalled = true
			return credentialstore.Credential{}, nil
		}},
		httpClient: &http.Client{},
		logger:     syncTestLogger(),
	}
	err := tracker.ClosePR(t.Context(), "acme/missing", 7)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.False(t, credentialsCalled, "an unresolvable repo must not reach the credential store at all")
}

// TestForgePRTracker_CreatePR_BindsTheSameWayAndPostsTheBody proves
// proposal acceptance's forge call binds per repo exactly as the poller's
// two do. This is the seam loam-0do's finding applies to: a *forge.Forgejo
// carries one host and one token from construction, so a single shared
// provider would open one repo's pull request against another repo's
// forge, with that other forge's token.
//
// The body is asserted too, not just the binding: the PR body this engine
// sends is the only place the attribution footer reaches the outside
// world, so a wiring test that ignored it would leave the whole path from
// LOAM_PR_ATTRIBUTION to the wire unpinned at the composition root.
func TestForgePRTracker_CreatePR_BindsTheSameWayAndPostsTheBody(t *testing.T) {
	t.Parallel()
	recorder := &forgeAPIRecorder{}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)
	tracker := forgePRTracker{
		repos: &repoForgeLookupMock{GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			return reposstore.Repo{Name: name, ForgeHost: server.URL}, nil
		}},
		credentials: &forgeCredentialLookupMock{GetByHostFunc: func(_ context.Context, host string) (credentialstore.Credential, error) {
			return credentialstore.Credential{Host: host, Token: "tkn-create"}, nil
		}},
		httpClient: server.Client(),
		logger:     syncTestLogger(),
	}
	_, number, err := tracker.CreatePR(t.Context(), "acme/widgets", "loam/wb-9c2f1a", "main", "Add the widget", "body\n\n---\nProposed via Loam.")
	require.NoError(t, err)
	assert.Equal(t, 7, number)
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	assert.Equal(t, http.MethodPost, recorder.method)
	assert.Equal(t, "/api/v1/repos/acme/widgets/pulls", recorder.path,
		"the request must reach the forge host recorded on the repo's own row")
	assert.Equal(t, "token tkn-create", recorder.authorization,
		"the request must carry the token the credential store returned for that host")
	assert.Equal(t, "loam/wb-9c2f1a", recorder.body["head"])
	assert.Equal(t, "main", recorder.body["base"])
	assert.Equal(t, "body\n\n---\nProposed via Loam.", recorder.body["body"])
}

// TestForgePRTracker_FindOpenPR_BindsTheSameWay pins the adoption lookup's
// binding. It is the call that recovers an existing PR's number after a
// duplicate rejection, so sending it to the wrong forge would return a
// number from an unrelated repo.
func TestForgePRTracker_FindOpenPR_BindsTheSameWay(t *testing.T) {
	t.Parallel()
	recorder := &forgeAPIRecorder{}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)
	tracker := forgePRTracker{
		repos: &repoForgeLookupMock{GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			return reposstore.Repo{Name: name, ForgeHost: server.URL}, nil
		}},
		credentials: &forgeCredentialLookupMock{GetByHostFunc: func(_ context.Context, host string) (credentialstore.Credential, error) {
			return credentialstore.Credential{Host: host, Token: "tkn-find"}, nil
		}},
		httpClient: server.Client(),
		logger:     syncTestLogger(),
	}
	_, _, _, err := tracker.FindOpenPR(t.Context(), "acme/widgets", "loam/wb-9c2f1a", "main")
	require.NoError(t, err)
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	assert.Equal(t, http.MethodGet, recorder.method)
	assert.Equal(t, "/api/v1/repos/acme/widgets/pulls", recorder.path)
	assert.Equal(t, "token tkn-find", recorder.authorization)
}

// TestForgePRTracker_CreatePR_MissingCredentialIsReportedNotSwallowed
// proves the accept path refuses to fall back to an anonymous request the
// way the poll path does. It matters more here, not less: an anonymous
// POST would fail against the forge anyway, but reporting the credential
// problem as itself is what tells the admin why their accept did not open
// a PR.
func TestForgePRTracker_CreatePR_MissingCredentialIsReportedNotSwallowed(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("no credential for host")
	tracker := forgePRTracker{
		repos: &repoForgeLookupMock{GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			return reposstore.Repo{Name: name, ForgeHost: "forge.example.com"}, nil
		}},
		credentials: &forgeCredentialLookupMock{GetByHostFunc: func(context.Context, string) (credentialstore.Credential, error) {
			return credentialstore.Credential{}, wantErr
		}},
		httpClient: &http.Client{},
		logger:     syncTestLogger(),
	}
	_, _, err := tracker.CreatePR(t.Context(), "acme/widgets", "loam/wb-9c2f1a", "main", "t", "d")
	require.ErrorIs(t, err, wantErr)
	assert.Contains(t, err.Error(), "forge.example.com")
	_, _, _, err = tracker.FindOpenPR(t.Context(), "acme/widgets", "loam/wb-9c2f1a", "main")
	require.ErrorIs(t, err, wantErr)
}

// TestBuildProposalAccepter_WiresTheProductionGraph proves the
// composition-root constructor registerProposalService builds
// AcceptProposal's engine from actually assembles -- and, by compiling at all,
// that forgePRTracker satisfies mirrorsync's pullRequestOpener seam and
// *gittransport.Transport its upstreamRefPusher seam.
//
// It takes a nil pool deliberately: every collaborator here is constructed
// over the pool without touching it (gen.New, reposstore.NewStore,
// workbranchstore.New, credentialstore.New are all plain struct
// constructions), so this exercises the wiring without needing a database.
// The one thing that CAN fail is the encryptor, which is why the config
// carries a real key.
func TestBuildProposalAccepter_WiresTheProductionGraph(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		DataDir:       t.TempDir(),
		EncryptionKey: make([]byte, 32),
		PRAttribution: true,
		Logger:        syncTestLogger(),
	}
	accepter, err := buildProposalAccepter(cfg, nil, &http.Client{})
	require.NoError(t, err)
	assert.NotNil(t, accepter)
}

// TestBuildProposalAccepter_RejectsABadEncryptionKey proves a wiring
// failure fails startup rather than degrading to an engine that cannot
// decrypt the token its pushes need.
func TestBuildProposalAccepter_RejectsABadEncryptionKey(t *testing.T) {
	t.Parallel()
	cfg := config.Config{DataDir: t.TempDir(), EncryptionKey: []byte("too short"), Logger: syncTestLogger()}
	_, err := buildProposalAccepter(cfg, nil, &http.Client{})
	require.Error(t, err)
}

// syncCollaborators is one stand-in for all seven mirrorsync collaborator
// seams newSyncRunner takes, driving the scheduler through a real Mirror
// Sync cycle whose only observable step is the fetch.
//
// It is a hand-written fake rather than a moq mock for a mechanical
// reason, not a stylistic one: the interfaces it satisfies are declared in
// internal/mirrorsync, so their mocks are generated into that package's
// own moq_test.go and are unreachable from here, and generating a second
// copy into this package would need moq's -pkg flag, which this repo's Go
// standards rule out. It records no expectations and asserts nothing --
// every assertion lives in the tests below.
type syncCollaborators struct {
	repos []mirrorsync.RepoID
	// entered receives one value as each cycle enters Fetch, which is the
	// first step past the concurrency gate. It is buffered to hold every
	// repo so no cycle is ever blocked by the test failing to read.
	entered chan mirrorsync.RepoID
	// release gates Fetch's return. A test that closes it lets every
	// waiting cycle finish; a test that never closes it leaves them
	// waiting on ctx instead, which is what the production collaborators
	// do under a canceled context (exec.CommandContext, pgx, and
	// http.NewRequestWithContext all fail fast on one).
	release chan struct{}
}

func newSyncCollaborators(n int) *syncCollaborators {
	repos := make([]mirrorsync.RepoID, n)
	for i := range repos {
		repos[i] = mirrorsync.RepoID(fmt.Sprintf("acme/repo%d", i))
	}
	return &syncCollaborators{repos: repos, entered: make(chan mirrorsync.RepoID, n), release: make(chan struct{})}
}

func (c *syncCollaborators) ListRepos(context.Context) ([]mirrorsync.RepoID, error) {
	return c.repos, nil
}

func (c *syncCollaborators) Fetch(ctx context.Context, repo mirrorsync.RepoID) (mirrorsync.FetchResult, error) {
	c.entered <- repo
	select {
	case <-c.release:
	case <-ctx.Done():
	}
	return mirrorsync.FetchResult{}, nil
}

func (c *syncCollaborators) DetectAdvances(context.Context, mirrorsync.RepoID, mirrorsync.FetchResult) ([]mirrorsync.Advance, error) {
	return nil, nil
}

func (c *syncCollaborators) CheckMergeability(context.Context, mirrorsync.RepoID, []mirrorsync.Advance) error {
	return nil
}

func (c *syncCollaborators) EnqueueIngest(context.Context, mirrorsync.RepoID, []mirrorsync.Advance) (bool, error) {
	return false, nil
}

func (c *syncCollaborators) PollPRs(context.Context, mirrorsync.RepoID) error { return nil }

func (c *syncCollaborators) ReportSyncing(context.Context, mirrorsync.RepoID) error { return nil }

func (c *syncCollaborators) ReportIdle(context.Context, mirrorsync.RepoID, bool) error { return nil }

func (c *syncCollaborators) ReportError(context.Context, mirrorsync.RepoID, error, bool) error {
	return nil
}

// runSyncRunner starts r.Run on its own goroutine and returns a channel
// closed when it returns, so a test can assert both that a bound holds
// while cycles are in flight and that shutdown eventually completes.
func runSyncRunner(ctx context.Context, r runner) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()
	return done
}

// TestNewSyncRunner_BoundsConcurrentCyclesAtTheProductionDefault is
// loam-k1fb's whole point: not that mirrorsync offers a bound (loam-5v5
// proved that, in that package's own tests), but that the scheduler THIS
// BINARY builds is actually bounded. Before this bead, WithMaxConcurrentCycles
// existed and nothing outside internal/mirrorsync passed it, so
// cmd/server shipped the unbounded fan-out loam-5v5 was filed about:
// one goroutine per enrolled repo per tick, each driving a real git fetch.
//
// It drives the same function buildSyncScheduler returns from, so the
// bound is asserted as wiring rather than as an option value re-applied by
// the test: n = defaultMaxConcurrentCycles + 4 repos are enrolled, each
// cycle reports itself as it enters Fetch and then blocks, and exactly
// defaultMaxConcurrentCycles of them may be inside Fetch at once. The
// (k+1)th's absence is asserted with the same bounded-wait idiom
// internal/mirrorsync's own bound test uses, which is what makes "no more
// than k" provable rather than merely unobserved. Dropping the option from
// newSyncRunner, or setting the default to a non-positive value (a
// documented no-op that leaves New unbounded), each fail here.
func TestNewSyncRunner_BoundsConcurrentCyclesAtTheProductionDefault(t *testing.T) {
	t.Parallel()
	const extra = 4
	collaborators := newSyncCollaborators(defaultMaxConcurrentCycles + extra)
	ticks := make(chan time.Time)
	r := newSyncRunner(syncTestLogger(), ticks, collaborators, collaborators, collaborators, collaborators, collaborators, collaborators, collaborators, 5*time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runDone := runSyncRunner(ctx, r)
	ticks <- time.Now()
	for i := range defaultMaxConcurrentCycles {
		select {
		case <-collaborators.entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of the %d cycles the bound allows ever started", i, defaultMaxConcurrentCycles)
		}
	}
	select {
	case repo := <-collaborators.entered:
		t.Fatalf("a cycle (%s) started beyond the bound of %d before any slot was released -- cmd/server's scheduler is not bounded", repo, defaultMaxConcurrentCycles)
	case <-time.After(200 * time.Millisecond):
	}
	close(collaborators.release)
	for i := range extra {
		select {
		case <-collaborators.entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("releasing every slot did not let queued cycle %d proceed -- a freed slot was not reused", i)
		}
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the sync runner did not return after its context was canceled")
	}
}

// TestNewSyncRunner_DrainsEveryQueuedCycleWithinTheGracePeriod pins the
// half of the bound's cost that the shutdown path pays. Bounding the
// fan-out means a signalled process can have cycles QUEUED behind the
// gate, not just in flight, and mirrorsync's slot acquire is not
// context-aware: a queued cycle still acquires its slot and runs all five
// steps after cancellation rather than bailing out. That is safe only
// because every step observes the canceled context and fails immediately
// -- which is what this test pins, with a fake whose Fetch waits on
// ctx.Done exactly as exec.CommandContext, pgx, and
// http.NewRequestWithContext all do underneath the production
// collaborators.
//
// The assertion is that ALL n cycles ran (none was silently dropped) AND
// that the runner returned well inside its grace period. A drain that
// blew the grace would surface here as a short count: syncRunner.Run
// returns on the deadline either way, abandoning whatever is still queued.
func TestNewSyncRunner_DrainsEveryQueuedCycleWithinTheGracePeriod(t *testing.T) {
	t.Parallel()
	const extra = 4
	n := defaultMaxConcurrentCycles + extra
	collaborators := newSyncCollaborators(n)
	ticks := make(chan time.Time)
	r := newSyncRunner(syncTestLogger(), ticks, collaborators, collaborators, collaborators, collaborators, collaborators, collaborators, collaborators, 5*time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runDone := runSyncRunner(ctx, r)
	ticks <- time.Now()
	for i := range defaultMaxConcurrentCycles {
		select {
		case <-collaborators.entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of the %d cycles the bound allows ever started", i, defaultMaxConcurrentCycles)
		}
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the sync runner did not return after its context was canceled")
	}
	assert.Len(t, collaborators.entered, extra,
		"every cycle queued behind the bound must still run -- and finish -- inside the shutdown grace period")
}

// TestDefaultMaxConcurrentCycles_IsPositive guards the one value that
// would make every other assertion in this file vacuous.
// mirrorsync.WithMaxConcurrentCycles documents n <= 0 as a NO-OP that
// leaves New's default unbounded, so a default of 0 -- or a negative one
// from a botched edit -- would not fail loudly at startup or anywhere
// else. It would simply restore the unbounded fan-out this bead exists to
// remove, silently.
func TestDefaultMaxConcurrentCycles_IsPositive(t *testing.T) {
	t.Parallel()
	assert.Positive(t, defaultMaxConcurrentCycles,
		"a non-positive bound is a documented no-op in mirrorsync.WithMaxConcurrentCycles: it silently ships an UNBOUNDED scheduler")
}
