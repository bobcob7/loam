package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bobcob7/loam/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
)

// lockedBuffer makes a slog destination safe to read while the OpenTelemetry
// SDK might still be writing to it. That is not paranoia about this test: the
// handler installed below stays installed for the rest of this binary's life,
// so any later otel.Handle from any goroutine lands here.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRun_InstallsTheOTelErrorHandlerBeforeItCanFail guards the single wiring
// line, which is the part of loam-0jle that production actually depends on and
// the part no test in internal/telemetry can see: that package proves the
// handler WORKS, and this proves it is INSTALLED.
//
// It asserts through behaviour rather than through the type of
// otel.GetErrorHandler(), because the handler type is unexported in
// internal/telemetry and should stay that way -- and because "an error handed
// to otel.Handle comes out of cfg.Logger as JSON" is the property an operator
// has, while "the global is of type X" is an implementation detail that a
// refactor could legitimately change.
//
// The config is deliberately one that makes run() fail IMMEDIATELY, at
// telemetry.New's sample-ratio validation: no database, no listener, no
// container, and it reaches the assertion in microseconds. That the wiring
// survives on a failing path is itself the point -- a handler installed after
// telemetry.New would miss exactly the startup errors that explain why
// telemetry never came up.
func TestRun_InstallsTheOTelErrorHandlerBeforeItCanFail(t *testing.T) {
	// Not parallel: it mutates the process-wide OpenTelemetry error handler,
	// which is precisely the thing internal/telemetry refuses to do in its own
	// test binary. Here it is unavoidable and harmless -- nothing in this
	// package reads the handler, and the acceptance harness's own run() calls
	// set it anyway.
	captured := &lockedBuffer{}
	cfg := config.Config{
		Logger:          slog.New(slog.NewJSONHandler(captured, nil)),
		SyncInterval:    time.Minute,
		OTelEndpoint:    "http://127.0.0.1:1",
		OTelServiceName: "loam",
		// Out of range on purpose: this is what makes run() return before it
		// touches Postgres.
		OTelSampleRatio: 2,
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	t.Cleanup(cancel)
	err := run(ctx, cancel, cfg, nil)
	require.Error(t, err, "this fixture depends on run() failing at telemetry.New")
	require.ErrorContains(t, err, "initializing telemetry",
		"run() failed somewhere else, so it may not have reached the wiring line at all")

	sentinel := errors.New("probe error routed through otel.Handle")
	otel.Handle(sentinel)
	var routed map[string]any
	for line := range strings.SplitSeq(captured.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record),
			"cfg.Logger emitted a non-JSON line: %q", line)
		if value, ok := record["error"].(string); ok && strings.Contains(value, sentinel.Error()) {
			routed = record
		}
	}
	require.NotNil(t, routed,
		"otel.Handle did not reach cfg.Logger; run() is missing telemetry.InstallErrorHandler.\ncaptured:\n%s",
		captured.String())
	assert.Equal(t, "ERROR", routed["level"])
}
