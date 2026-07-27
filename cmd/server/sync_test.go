package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/credentialstore"
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
}

func (r *forgeAPIRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.path = req.URL.Path
	r.method = req.Method
	r.authorization = req.Header.Get("Authorization")
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
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
