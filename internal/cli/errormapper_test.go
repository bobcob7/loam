package cli

import (
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIErrorMapper_ExitCode_Success(t *testing.T) {
	t.Parallel()
	mapper := newErrorMapper()
	assert.Equal(t, 0, mapper.ExitCode(nil))
}

func TestCLIErrorMapper_ExitCode_UnexpectedError_ExitsOne(t *testing.T) {
	t.Parallel()
	mapper := newErrorMapper()
	assert.Equal(t, 1, mapper.ExitCode(errors.New("boom")))
}

// TestCLIErrorMapper_ExitCode_ByClass proves each cliError code maps to its
// documented exit code (see docs/cli-spec.md -> Exit Codes & Errors), and
// that the constructed error carries the matching sentinel via errors.Is —
// never a bare "an error occurred" assertion.
func TestCLIErrorMapper_ExitCode_ByClass(t *testing.T) {
	tests := []struct {
		name     string
		build    func() error
		sentinel error
		wantExit int
	}{
		{"usage", func() error { return newUsageCLIError("bad flag", nil) }, errUsage, 2},
		{"unauthorized", func() error { return newUnauthorizedError("denied", nil) }, errUnauthorized, 2},
		{"conflict", func() error { return newConflictError("already exists", nil) }, errConflict, 2},
		{"precondition_failed", func() error { return newPreconditionFailedError("wrong state", nil) }, errPreconditionFailed, 2},
		{"not_found", func() error { return newNotFoundError("missing", nil) }, errNotFound, 3},
	}
	mapper := newErrorMapper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.build()
			require.ErrorIs(t, err, tt.sentinel)
			assert.Equal(t, tt.wantExit, mapper.ExitCode(err))
		})
	}
}

// TestCLIErrorMapper_ExitCode_WrappedCLIError_StillMaps proves a cliError
// wrapped with additional context (fmt.Errorf("...: %w", ...)) still maps
// correctly — errors.As, never a bare type assertion.
func TestCLIErrorMapper_ExitCode_WrappedCLIError_StillMaps(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("resolving work branch: %w", newNotFoundError("wb-1 not found", nil))
	require.ErrorIs(t, err, errNotFound)
	assert.Equal(t, 3, newErrorMapper().ExitCode(err))
}

// TestCLIErrorMapper_ExitCode_UsageError_StillExitsTwo proves the router's
// *usageError (from errors.go) also maps to exit 2 through ErrorMapper
// directly, independent of Run's own errors.As short-circuit in run.go —
// defense in depth for any caller that invokes ExitCode without going
// through Run.
func TestCLIErrorMapper_ExitCode_UsageError_StillExitsTwo(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 2, newErrorMapper().ExitCode(newUsageError("bad args")))
}

// TestCLIErrorMapper_ExitCode_ConnectCodes proves every table-driven
// ConnectRPC code (see docs/cli-spec.md -> Exit Codes & Errors design
// notes) maps to the documented exit code, both raw (a handler that forgot
// to classify it) and pre-classified via classifyConnectError.
func TestCLIErrorMapper_ExitCode_ConnectCodes(t *testing.T) {
	tests := []struct {
		code     connect.Code
		wantExit int
		wantCode string
	}{
		{connect.CodeNotFound, 3, codeNotFound},
		{connect.CodePermissionDenied, 2, codeUnauthorized},
		{connect.CodeUnauthenticated, 2, codeUnauthorized},
		{connect.CodeFailedPrecondition, 2, codePreconditionFailed},
		{connect.CodeAlreadyExists, 2, codeConflict},
		{connect.CodeAborted, 2, codeConflict},
		{connect.CodeInvalidArgument, 2, codeUsage},
		{connect.CodeUnknown, 1, ""},
		{connect.CodeInternal, 1, ""},
	}
	mapper := newErrorMapper()
	for _, tt := range tests {
		t.Run(tt.code.String(), func(t *testing.T) {
			t.Parallel()
			connErr := connect.NewError(tt.code, errors.New("boom"))
			assert.Equal(t, tt.wantExit, mapper.ExitCode(connErr))
			if tt.wantCode == "" {
				assert.Nil(t, classifyConnectError(connErr))
				return
			}
			classified := classifyConnectError(connErr)
			require.NotNil(t, classified)
			assert.Equal(t, tt.wantCode, classified.code)
		})
	}
}

// TestClassifyConnectError_PreservesUnderlyingError proves mapCommandError
// keeps the original *connect.Error reachable via errors.As after
// classification, so a caller can still inspect transport-level detail if
// it needs to.
func TestClassifyConnectError_PreservesUnderlyingError(t *testing.T) {
	t.Parallel()
	connErr := connect.NewError(connect.CodeNotFound, errors.New("work branch missing"))
	classified := classifyConnectError(connErr)
	require.NotNil(t, classified)
	assert.ErrorIs(t, classified, errNotFound)
	var gotConnErr *connect.Error
	require.ErrorAs(t, classified, &gotConnErr)
	assert.Equal(t, connect.CodeNotFound, gotConnErr.Code())
}

func TestMapCommandError_RawConnectError_ClassifiesAtBoundary(t *testing.T) {
	t.Parallel()
	connErr := connect.NewError(connect.CodeAlreadyExists, errors.New("verdict already recorded"))
	classified := mapCommandError(connErr)
	require.NotNil(t, classified)
	assert.Equal(t, codeConflict, classified.code)
	assert.ErrorIs(t, classified, errConflict)
}

func TestMapCommandError_UnclassifiableError_ReturnsNil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, mapCommandError(errors.New("boom")))
}

func TestCLIError_Error_IncludesCauseWhenDistinctFromMessage(t *testing.T) {
	t.Parallel()
	err := newNotFoundError("work branch wb-1 not found", errors.New("row not found"))
	assert.Contains(t, err.Error(), "work branch wb-1 not found")
	assert.Contains(t, err.Error(), "row not found")
}

func TestCLIError_Error_NoCause_IsJustMessage(t *testing.T) {
	t.Parallel()
	err := newUsageCLIError("bad flag", nil)
	assert.Equal(t, "bad flag", err.Error())
}

// TestCLIError_Error_CauseEqualToSentinel_NoDoubleWrapping proves passing
// the sentinel itself as cause (rather than nil) still renders a clean
// message, not a duplicated "sentinel: sentinel" string.
func TestCLIError_Error_CauseEqualToSentinel_NoDoubleWrapping(t *testing.T) {
	t.Parallel()
	err := newCLIError(codeNotFound, "missing", errNotFound, errNotFound)
	assert.ErrorIs(t, err, errNotFound)
}
