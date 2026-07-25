package handler_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"connectrpc.com/connect"
	"github.com/bobcob7/loam/internal/handler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestErrorMapper_ToConnectErr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		err      error
		wantCode connect.Code
	}{
		{name: "not found", err: fmt.Errorf("work branch wb-1: %w", handler.ErrNotFound), wantCode: connect.CodeNotFound},
		{name: "already exists", err: fmt.Errorf("repo acme/web: %w", handler.ErrAlreadyExists), wantCode: connect.CodeAlreadyExists},
		{name: "failed precondition", err: fmt.Errorf("repo acme/web: %w", handler.ErrFailedPrecondition), wantCode: connect.CodeFailedPrecondition},
		{name: "invalid argument", err: fmt.Errorf("field title: %w", handler.ErrInvalidArgument), wantCode: connect.CodeInvalidArgument},
		{name: "permission denied", err: fmt.Errorf("role author: %w", handler.ErrPermissionDenied), wantCode: connect.CodePermissionDenied},
		{name: "unmapped error", err: errors.New("boom: disk on fire"), wantCode: connect.CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mapper := handler.NewErrorMapper(slog.New(slog.NewJSONHandler(io.Discard, nil)))
			got := mapper.ToConnectErr(tt.err)
			require.NotNil(t, got)
			assert.Equal(t, tt.wantCode, got.Code())
		})
	}
}

// TestErrorMapper_ToConnectErr_LogsUnmappedError proves an unmapped error
// is logged before being collapsed to CodeInternal — not just given a
// generic code, but actually observed by the logger — so a silently
// swallowed failure would fail this test. This is the one test in the
// package that cannot use an io.Discard logger, since its entire point is
// to prove something was written to the log.
func TestErrorMapper_ToConnectErr_LogsUnmappedError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	mapper := handler.NewErrorMapper(slog.New(slog.NewJSONHandler(&buf, nil)))
	raw := errors.New("boom: disk on fire")
	got := mapper.ToConnectErr(raw)
	require.NotNil(t, got)
	assert.Equal(t, connect.CodeInternal, got.Code())
	assert.Contains(t, buf.String(), "disk on fire", "the raw error must be logged, not silently dropped")
	assert.NotContains(t, got.Message(), "disk on fire", "the raw error must not leak to the client over CodeInternal")
}
