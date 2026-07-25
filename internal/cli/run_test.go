package cli

import (
	"context"
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
	deps := NewDeps(testLogger(), &ConfigMock{}, encoder, &ErrorMapperMock{}, &WorkspaceResolverMock{}, &NoopConnectClient{})
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
	deps := NewDeps(testLogger(), &ConfigMock{}, encoder, errMapper, &WorkspaceResolverMock{}, &NoopConnectClient{})
	router := NewRouter(deps)
	code := Run(t.Context(), router, []string{"bogus"})
	assert.Equal(t, 2, code)
	assert.False(t, errorMapperCalled, "usage errors must not be delegated to the injected ErrorMapper")
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
	deps := NewDeps(testLogger(), &ConfigMock{}, encoder, errMapper, &WorkspaceResolverMock{}, &NoopConnectClient{})
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
	deps := NewDeps(testLogger(), &ConfigMock{}, encoder, &ErrorMapperMock{}, &WorkspaceResolverMock{}, &NoopConnectClient{})
	router := &Router{deps: deps, commands: map[string]*command{
		"noop": {run: func(ctx context.Context, d *Deps, args []string) error { return nil }},
	}}
	code := Run(t.Context(), router, []string{"noop"})
	assert.Equal(t, 0, code)
	assert.False(t, encodeCalled, "success must not invoke the encoder")
}
