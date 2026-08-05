package server

import (
	"context"
	"testing"

	"github.com/bobcob7/loam/internal/httpauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// TestAnnotateSpanWithCaller_Anonymous is the one case the httptest round
// trips in tracing_test.go CANNOT reach, and the reason it lives here.
// internal/httpauth rejects a request carrying neither a valid admin
// credential nor all three Loam-Agent-* headers with a 401 before the
// handler runs, so no request through the real Router arrives at an
// interceptor without one of the two. The behaviour still has to be pinned:
// callerKindAnonymous exists precisely so that if that ever changes, the
// traces say so instead of showing an attribute-less span that looks like
// broken instrumentation.
func TestAnnotateSpanWithCaller_Anonymous(t *testing.T) {
	t.Parallel()
	recorded := annotate(t, t.Context())
	assert.Equal(t, callerKindAnonymous, recorded[attrCallerKind])
	// Absent, not "": an empty-string attribute is indistinguishable from
	// an agent whose name genuinely is empty.
	assert.NotContains(t, recorded, attrAgentName)
	assert.NotContains(t, recorded, attrAgentID)
	assert.NotContains(t, recorded, attrAgentRole)
	assert.NotContains(t, recorded, attrAgentIdentifier)
}

// TestAnnotateSpanWithCaller_IdentityWins covers the ordering the Router
// never produces either: httpauth.CLI sets EITHER the admin marker or an
// identity, never both. If a future wrapper ever set both, an agent
// identity is the more specific fact and must be what the span reports.
func TestAnnotateSpanWithCaller_IdentityWins(t *testing.T) {
	t.Parallel()
	ctx := httpauth.WithIdentity(httpauth.WithAdmin(t.Context()),
		httpauth.Identity{Name: "ada", ID: "7", Role: "reviewer"})
	recorded := annotate(t, ctx)
	assert.Equal(t, callerKindAgent, recorded[attrCallerKind])
	assert.Equal(t, "ada-7-reviewer", recorded[attrAgentIdentifier])
}

// TestAnnotateSpanWithCaller_EmptyFieldsAreRecordedAsGiven pins that this
// package never invents an identity and never second-guesses one: httpauth
// returns ok=false unless all three headers are present, so a half-filled
// Identity can only reach a span if something deliberately put one on the
// context, and the honest record of that is the half-filled value.
func TestAnnotateSpanWithCaller_EmptyFieldsAreRecordedAsGiven(t *testing.T) {
	t.Parallel()
	ctx := httpauth.WithIdentity(t.Context(), httpauth.Identity{Name: "ada"})
	recorded := annotate(t, ctx)
	// httpauth put an Identity on the context, so it is an agent; this
	// package does not second-guess that decision, it reports it.
	assert.Equal(t, callerKindAgent, recorded[attrCallerKind])
	assert.Equal(t, "ada", recorded[attrAgentName])
	assert.Equal(t, "ada--", recorded[attrAgentIdentifier])
}

// TestAnnotateSpanWithCaller_NonRecordingSpanIsUntouched proves the
// disabled path stays free: with telemetry off the current span is
// upstream's non-recording no-op, and annotateSpanWithCaller must return
// without building any attribute rather than relying on the SDK to discard
// them.
func TestAnnotateSpanWithCaller_NonRecordingSpanIsUntouched(t *testing.T) {
	t.Parallel()
	ctx, span := tracenoop.NewTracerProvider().Tracer("test").Start(t.Context(), "noop")
	defer span.End()
	require.False(t, span.IsRecording())
	// The assertion is that this does not panic and does not reach a
	// recording span; there is nothing to read back off a no-op span.
	annotateSpanWithCaller(ctx)
}

// annotate starts a real recorded span, runs the annotation against ctx,
// ends it, and returns the attributes that actually landed on the exported
// span -- not the arguments the code passed, which is what a mocked span
// would have measured.
func annotate(t *testing.T, ctx context.Context) map[attribute.Key]string {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.WithoutCancel(ctx))) })
	spanCtx, span := provider.Tracer("test").Start(ctx, "test-span")
	annotateSpanWithCaller(spanCtx)
	span.End()
	ended := recorder.Ended()
	require.Len(t, ended, 1)
	out := make(map[attribute.Key]string, len(ended[0].Attributes()))
	for _, kv := range ended[0].Attributes() {
		out[kv.Key] = kv.Value.Emit()
	}
	return out
}
