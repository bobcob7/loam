package repo_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	loamv1 "github.com/bobcob7/loam/internal/gen/loam/v1"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/bobcob7/loam/internal/handler/repo"
	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/bobcob7/loam/internal/reposstore"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func adminCtx(t *testing.T) context.Context {
	t.Helper()
	return httpauth.WithAdmin(t.Context())
}

func agentCtx(t *testing.T, role string) context.Context {
	t.Helper()
	return httpauth.WithIdentity(t.Context(), httpauth.Identity{Name: "ada-lovelace", ID: "7", Role: role})
}

// fixedRoleStore is a hand-written handler.RoleStore fake returning a fixed
// capability set for any role, since internal/handler's moq-generated mock
// lives in its own package's moq_test.go and is unreachable from this
// external test package (repo_test only imports handler as a normal,
// non-test dependency).
type fixedRoleStore struct {
	capabilities []handler.Capability
}

func (s fixedRoleStore) RoleCapabilities(context.Context, string) ([]handler.Capability, error) {
	return s.capabilities, nil
}

// newHandler wires a repo.Handler over store with a capability checker
// backed by roleCaps (the operations RequireCapability's role store
// resolves for any agent role) and an ErrorMapper that logs to buf so
// tests can assert on the logged line for unmapped errors.
func newHandler(store repo.RepoStore, roleCaps []handler.Capability, buf *bytes.Buffer) *repo.Handler {
	checker := handler.NewCapabilityChecker(fixedRoleStore{capabilities: roleCaps})
	mapper := handler.NewErrorMapper(slog.New(slog.NewJSONHandler(buf, nil)))
	return repo.New(store, checker, mapper, testLogger())
}

// TestGetRepo_AgentLackingGitClone_Denied proves an agent whose role does
// not grant git.clone is rejected with CodePermissionDenied before the
// store is ever consulted -- the requireCapability gate loam-ofg.4
// establishes and this bead's DESIGN note requires ("gated by
// loam-ofg.4's requireCapability with git.clone").
func TestGetRepo_AgentLackingGitClone_Denied(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	storeCalled := false
	// Both store methods are fully configured (not left nil) so that a
	// mutation which drops or short-circuits the capability gate lets
	// execution fall all the way through to a real, successful store
	// round-trip -- surfacing as a clean assertion failure below, never a
	// nil-func panic that would obscure what's actually being proved.
	store := &repo.RepoStoreMock{
		GetRepoByNameFunc: func(_ context.Context, _ string) (reposstore.Repo, error) {
			storeCalled = true
			return reposstore.Repo{ID: uuid.New(), Name: "bobcob7/doc-server"}, nil
		},
		ListTargetBranchesFunc: func(_ context.Context, _ uuid.UUID) ([]reposstore.TargetBranch, error) {
			storeCalled = true
			return nil, nil
		},
	}
	h := newHandler(store, []handler.Capability{handler.CapabilityWorkRead}, &buf)
	_, err := h.GetRepo(agentCtx(t, "reviewer-without-clone"), connect.NewRequest(&loamv1.GetRepoRequest{Repo: "bobcob7/doc-server"}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodePermissionDenied, connectErr.Code())
	assert.False(t, storeCalled, "the repo store must not be consulted when the capability gate denies the caller")
}

// TestGetRepo_AdminBypassesCapabilityGate proves admin basic-auth reaches
// the store as a superuser, per CapabilityChecker's documented bypass.
func TestGetRepo_AdminBypassesCapabilityGate(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	store := &repo.RepoStoreMock{
		GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			assert.Equal(t, "bobcob7/doc-server", name)
			return reposstore.Repo{ID: repoID, Name: name}, nil
		},
		ListTargetBranchesFunc: func(_ context.Context, id uuid.UUID) ([]reposstore.TargetBranch, error) {
			assert.Equal(t, repoID, id)
			return nil, nil
		},
	}
	h := newHandler(store, nil, &buf)
	resp, err := h.GetRepo(adminCtx(t), connect.NewRequest(&loamv1.GetRepoRequest{Repo: "bobcob7/doc-server"}))
	require.NoError(t, err)
	assert.Equal(t, "bobcob7/doc-server", resp.Msg.GetRepo())
}

// TestGetRepo_UnenrolledRepo_ReturnsCodeNotFound is the acceptance-critical
// case: a repo genuinely not enrolled must map to connect.CodeNotFound --
// exactly what `loam clone`'s classifyConnectError turns into exit 3
// (docs/cli-spec.md -> clone: "exit 3 if the repo is not enrolled"), and
// what this bead must make DISTINGUISHABLE from the "service not
// registered" fallback that used to answer every RepoService request.
func TestGetRepo_UnenrolledRepo_ReturnsCodeNotFound(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	store := &repo.RepoStoreMock{
		GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			return reposstore.Repo{}, fmt.Errorf("getting repo %s: %w", name, reposstore.ErrNotFound)
		},
	}
	h := newHandler(store, []handler.Capability{handler.CapabilityGitClone}, &buf)
	_, err := h.GetRepo(agentCtx(t, "author"), connect.NewRequest(&loamv1.GetRepoRequest{Repo: "bobcob7/ghost-repo"}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodeNotFound, connectErr.Code(), "an unenrolled repo must be CodeNotFound, the real reason -- not a generic error a client cannot classify")
	assert.Empty(t, buf.String(), "a mapped domain error (not found) must not be logged as an unmapped one")
}

// TestGetRepo_StoreFailure_MapsToInternalAndLogs proves an unclassified
// store error (anything other than ErrNotFound) is neither silently
// swallowed nor misreported as not-found: it becomes CodeInternal and is
// logged, per ErrorMapper.ToConnectErr's documented contract.
func TestGetRepo_StoreFailure_MapsToInternalAndLogs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	dbErr := errors.New("connection reset by peer")
	store := &repo.RepoStoreMock{
		GetRepoByNameFunc: func(_ context.Context, _ string) (reposstore.Repo, error) {
			return reposstore.Repo{}, dbErr
		},
	}
	h := newHandler(store, []handler.Capability{handler.CapabilityGitClone}, &buf)
	_, err := h.GetRepo(agentCtx(t, "author"), connect.NewRequest(&loamv1.GetRepoRequest{Repo: "bobcob7/doc-server"}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodeInternal, connectErr.Code())
	assert.Contains(t, buf.String(), "connection reset by peer", "the unmapped error must be logged, not silently dropped")
}

// TestGetRepo_EmptyRepoName_ReturnsInvalidArgument proves an empty repo
// identifier is rejected before the store is ever consulted.
func TestGetRepo_EmptyRepoName_ReturnsInvalidArgument(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	storeCalled := false
	store := &repo.RepoStoreMock{
		GetRepoByNameFunc: func(_ context.Context, _ string) (reposstore.Repo, error) {
			storeCalled = true
			return reposstore.Repo{}, nil
		},
	}
	h := newHandler(store, []handler.Capability{handler.CapabilityGitClone}, &buf)
	_, err := h.GetRepo(agentCtx(t, "author"), connect.NewRequest(&loamv1.GetRepoRequest{Repo: ""}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodeInvalidArgument, connectErr.Code())
	assert.False(t, storeCalled)
}

// TestGetRepo_Success_ReturnsTargetBranches proves the happy path
// translates reposstore.TargetBranch rows into the response's bare branch
// name list, in the store's returned order.
func TestGetRepo_Success_ReturnsTargetBranches(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	store := &repo.RepoStoreMock{
		GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			return reposstore.Repo{ID: repoID, Name: name}, nil
		},
		ListTargetBranchesFunc: func(_ context.Context, id uuid.UUID) ([]reposstore.TargetBranch, error) {
			require.Equal(t, repoID, id)
			return []reposstore.TargetBranch{{RepoID: id, Branch: "main"}, {RepoID: id, Branch: "release"}}, nil
		},
	}
	h := newHandler(store, []handler.Capability{handler.CapabilityGitClone}, &buf)
	resp, err := h.GetRepo(agentCtx(t, "author"), connect.NewRequest(&loamv1.GetRepoRequest{Repo: "bobcob7/doc-server"}))
	require.NoError(t, err)
	assert.Equal(t, "bobcob7/doc-server", resp.Msg.GetRepo())
	assert.Equal(t, []string{"main", "release"}, resp.Msg.GetTargetBranches())
}

// TestGetRepo_ListTargetBranchesFailure_MapsToInternal proves a failure in
// the second store call (after a successful repo lookup) still maps and
// logs correctly -- not swallowed just because the first call succeeded.
func TestGetRepo_ListTargetBranchesFailure_MapsToInternal(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	repoID := uuid.New()
	dbErr := errors.New("target branches query failed")
	store := &repo.RepoStoreMock{
		GetRepoByNameFunc: func(_ context.Context, name string) (reposstore.Repo, error) {
			return reposstore.Repo{ID: repoID, Name: name}, nil
		},
		ListTargetBranchesFunc: func(_ context.Context, _ uuid.UUID) ([]reposstore.TargetBranch, error) {
			return nil, dbErr
		},
	}
	h := newHandler(store, []handler.Capability{handler.CapabilityGitClone}, &buf)
	_, err := h.GetRepo(agentCtx(t, "author"), connect.NewRequest(&loamv1.GetRepoRequest{Repo: "bobcob7/doc-server"}))
	require.Error(t, err)
	var connectErr *connect.Error
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodeInternal, connectErr.Code())
	assert.Contains(t, buf.String(), "target branches query failed")
}
