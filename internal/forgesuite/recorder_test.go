package forgesuite

import (
	"context"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobcob7/loam/internal/forge"
)

// callRecorder records which forge.Provider methods the contract actually
// invoked. Cases run in parallel, so it is mutex-guarded.
type callRecorder struct {
	mu   sync.Mutex
	seen map[string]int
}

func (r *callRecorder) record(method string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = make(map[string]int)
	}
	r.seen[method]++
}

func (r *callRecorder) methods() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.seen))
	for name := range r.seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// recordingProvider is the only way a contract case reaches the
// implementation under test, and the reason this suite cannot silently skip
// a Provider method.
//
// It implements forge.Provider BY HAND — an explicit method per interface
// method, holding `inner` in a named field rather than embedding
// forge.Provider. That distinction is the whole mechanism: an embedded
// interface would promote any method added to Provider straight through,
// keeping the compile-time assertion below green while the new method
// stayed unrecorded and untested. With no embedding, a Provider that grows
// an eighth method makes this file stop compiling until someone writes the
// wrapper, and the wrapper is what makes the runtime guard in
// assertEveryProviderMethodExercised able to see whether any case calls it.
// (internal/mirrorsync/production_assertions.go is the in-repo precedent
// for converting a prose inventory into a compiler-checked one.)
type recordingProvider struct {
	rec   *callRecorder
	inner forge.Provider
}

// Ensure *recordingProvider satisfies forge.Provider at compile time. If
// this line starts failing, forge.Provider grew a method: write its wrapper
// below and a contract case for it — do not reach for an embedded
// forge.Provider to make the error go away, that would defeat the guard.
var _ forge.Provider = (*recordingProvider)(nil)

func newRecordingProvider(rec *callRecorder, inner forge.Provider) *recordingProvider {
	return &recordingProvider{rec: rec, inner: inner}
}

func (p *recordingProvider) ValidateToken(ctx context.Context, host, token string) error {
	p.rec.record("ValidateToken")
	return p.inner.ValidateToken(ctx, host, token)
}

func (p *recordingProvider) CheckRepo(ctx context.Context, upstreamURL string) error {
	p.rec.record("CheckRepo")
	return p.inner.CheckRepo(ctx, upstreamURL)
}

func (p *recordingProvider) CreatePR(ctx context.Context, repo, headBranch, targetBranch, title, description string) (string, int, error) {
	p.rec.record("CreatePR")
	return p.inner.CreatePR(ctx, repo, headBranch, targetBranch, title, description)
}

func (p *recordingProvider) GetPRState(ctx context.Context, repo string, prNumber int) (string, error) {
	p.rec.record("GetPRState")
	return p.inner.GetPRState(ctx, repo, prNumber)
}

func (p *recordingProvider) ClosePR(ctx context.Context, repo string, prNumber int) error {
	p.rec.record("ClosePR")
	return p.inner.ClosePR(ctx, repo, prNumber)
}

func (p *recordingProvider) GitCredentials(ctx context.Context, token string) (string, string, error) {
	p.rec.record("GitCredentials")
	return p.inner.GitCredentials(ctx, token)
}

func (p *recordingProvider) FindOpenPR(ctx context.Context, repo, headBranch, targetBranch string) (string, int, bool, error) {
	p.rec.record("FindOpenPR")
	return p.inner.FindOpenPR(ctx, repo, headBranch, targetBranch)
}

// providerMethodNames discovers forge.Provider's real method set from the
// interface type itself, rather than trusting a hand-kept list here. The
// list is what rots: internal/forge's own
// TestAllSentinelsDiscoversEveryExportedErrVar exists because a hand-copied
// sentinel list was blind by construction to a sentinel added after the
// copy, and a hand-copied method list would be blind the same way.
func providerMethodNames() []string {
	typ := reflect.TypeOf((*forge.Provider)(nil)).Elem()
	names := make([]string, 0, typ.NumMethod())
	for i := range typ.NumMethod() {
		names = append(names, typ.Method(i).Name)
	}
	sort.Strings(names)
	return names
}

// assertEveryProviderMethodExercised is the runtime half of the
// completeness guard, run from Run's cleanup — i.e. after every parallel
// case has finished. It fails when a Provider method exists but no case
// called it, which is the state a new Provider method lands in once its
// recordingProvider wrapper is written and nothing else is.
//
// It checks the reverse direction too: a recorded name that is not a
// Provider method means a wrapper above records under a misspelled name, in
// which case the real method's coverage would be credited to a ghost.
func assertEveryProviderMethodExercised(t *testing.T, rec *callRecorder) {
	t.Helper()
	want := providerMethodNames()
	require.NotEmpty(t, want, "reflect found no methods on forge.Provider, which means this guard is looking at the wrong type, not that Provider is empty")
	got := rec.methods()
	seen := make(map[string]bool, len(got))
	for _, name := range got {
		seen[name] = true
	}
	for _, name := range want {
		assert.True(t, seen[name],
			"forge.Provider.%s is never exercised by the contract suite (called methods: %v) — a Provider method with no shared case means the fake is licensed for behaviour nothing ever compared against real Forgejo; add a case to contractCases", name, got)
	}
	known := make(map[string]bool, len(want))
	for _, name := range want {
		known[name] = true
	}
	for _, name := range got {
		assert.True(t, known[name],
			"recordingProvider recorded %q, which is not a method on forge.Provider (methods: %v) — a wrapper is recording under the wrong name, so some real method's coverage is being credited to a ghost", name, want)
	}
}

// TestProviderMethodNamesMatchTheRecordingWrapper is the completeness
// guard's own guard, and it runs in the ordinary `go test ./...` gate
// whether or not either leg does. providerMethodNames reflects over the
// interface; this test confirms recordingProvider (the compile-time half)
// covers exactly that set, so the two halves cannot drift apart — e.g. a
// wrapper deleted along with the interface method it wrapped, leaving the
// runtime guard checking a smaller population than it thinks.
func TestProviderMethodNamesMatchTheRecordingWrapper(t *testing.T) {
	t.Parallel()
	wrapperMethods := make([]string, 0)
	typ := reflect.TypeOf((*recordingProvider)(nil))
	for i := range typ.NumMethod() {
		wrapperMethods = append(wrapperMethods, typ.Method(i).Name)
	}
	sort.Strings(wrapperMethods)
	assert.Equal(t, providerMethodNames(), wrapperMethods,
		"recordingProvider must have exactly one wrapper per forge.Provider method: an extra method here records calls nothing in the contract can make, and a missing one cannot compile")
}
