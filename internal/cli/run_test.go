package cli

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_NoArgs_ExitsUsageAndEncodesStructuredError proves the top-level
// acceptance criterion: `loam` with no args exits 2 with a structured usage
// error written through the injected OutputEncoder.
func TestRun_NoArgs_ExitsUsageAndEncodesStructuredError(t *testing.T) {
	t.Parallel()
	var encoded errorPayload
	encoder := &OutputEncoderMock{EncodeFunc: func(v any) error {
		payload, ok := v.(errorPayload)
		require.True(t, ok, "Run must encode an errorPayload")
		encoded = payload
		return nil
	}}
	deps := NewDeps(testLogger(), &ConfigMock{}, encoder, &ErrorMapperMock{}, &WorkspaceResolverMock{}, fakeConnect{})
	router := NewRouter(deps)
	code := Run(t.Context(), router, nil)
	assert.Equal(t, 2, code)
	assert.Equal(t, "usage", encoded.Error.Code)
	assert.NotEmpty(t, encoded.Error.Message)
}

// TestRun_UnknownCommand_ExitsUsage proves an unknown command also exits 2
// with a structured usage error, independent of the injected ErrorMapper.
func TestRun_UnknownCommand_ExitsUsage(t *testing.T) {
	t.Parallel()
	errorMapperCalled := false
	encoder := &OutputEncoderMock{EncodeFunc: func(v any) error { return nil }}
	errMapper := &ErrorMapperMock{ExitCodeFunc: func(err error) int { errorMapperCalled = true; return 1 }}
	deps := NewDeps(testLogger(), &ConfigMock{}, encoder, errMapper, &WorkspaceResolverMock{}, fakeConnect{})
	router := NewRouter(deps)
	code := Run(t.Context(), router, []string{"bogus"})
	assert.Equal(t, 2, code)
	assert.False(t, errorMapperCalled, "usage errors must not be delegated to the injected ErrorMapper")
}

// TestRun_WrappedUsageError_StillExitsUsage proves Run uses errors.As (not
// a bare type assertion) to recognize a usageError: a later bead that
// wraps one with fmt.Errorf("...: %w", ...) for context must still exit 2
// via the fixed usage path, never fall through to the injected ErrorMapper.
func TestRun_WrappedUsageError_StillExitsUsage(t *testing.T) {
	t.Parallel()
	var encoded errorPayload
	encoder := &OutputEncoderMock{EncodeFunc: func(v any) error {
		payload, ok := v.(errorPayload)
		require.True(t, ok)
		encoded = payload
		return nil
	}}
	errMapperCalled := false
	errMapper := &ErrorMapperMock{ExitCodeFunc: func(err error) int { errMapperCalled = true; return 1 }}
	deps := NewDeps(testLogger(), &ConfigMock{}, encoder, errMapper, &WorkspaceResolverMock{}, fakeConnect{})
	router := &Router{deps: deps, commands: map[string]*command{
		"wrapped": {run: func(ctx context.Context, d *Deps, args []string) error {
			return fmt.Errorf("dispatching wrapped: %w", newUsageError("bad args"))
		}},
	}}
	code := Run(t.Context(), router, []string{"wrapped"})
	assert.Equal(t, 2, code)
	assert.False(t, errMapperCalled, "a wrapped usage error must still bypass the injected ErrorMapper")
	assert.Equal(t, "usage", encoded.Error.Code)
}

// TestRun_CommandError_DelegatesToErrorMapper proves a command handler's
// error (as opposed to a routing usageError) is encoded and its exit code
// comes from the injected ErrorMapper.
func TestRun_CommandError_DelegatesToErrorMapper(t *testing.T) {
	t.Parallel()
	var encoded errorPayload
	encoder := &OutputEncoderMock{EncodeFunc: func(v any) error {
		payload, ok := v.(errorPayload)
		require.True(t, ok)
		encoded = payload
		return nil
	}}
	errMapper := &ErrorMapperMock{ExitCodeFunc: func(err error) int {
		assert.ErrorIs(t, err, errNotImplemented)
		return 7
	}}
	deps := NewDeps(testLogger(), &ConfigMock{}, encoder, errMapper, &WorkspaceResolverMock{}, fakeConnect{})
	router := NewRouter(deps)
	code := Run(t.Context(), router, []string{"whoami"})
	assert.Equal(t, 7, code)
	assert.Equal(t, "internal", encoded.Error.Code)
}

// TestRun_Success_ReturnsZero proves a nil error from Dispatch exits 0
// without touching the encoder.
func TestRun_Success_ReturnsZero(t *testing.T) {
	t.Parallel()
	encodeCalled := false
	encoder := &OutputEncoderMock{EncodeFunc: func(v any) error { encodeCalled = true; return nil }}
	deps := NewDeps(testLogger(), &ConfigMock{}, encoder, &ErrorMapperMock{}, &WorkspaceResolverMock{}, fakeConnect{})
	router := &Router{deps: deps, commands: map[string]*command{
		"noop": {run: func(ctx context.Context, d *Deps, args []string) error { return nil }},
	}}
	code := Run(t.Context(), router, []string{"noop"})
	assert.Equal(t, 0, code)
	assert.False(t, encodeCalled, "success must not invoke the encoder")
}
