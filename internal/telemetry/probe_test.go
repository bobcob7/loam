package telemetry

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestIsProbe_DefaultsToFalse is the property that keeps this marker from
// being a footgun: work that never asked to be treated as a probe is never
// treated as one. Everything in the tree that does NOT call WithProbe -- the
// sync scheduler, ingest, every RPC handler -- relies on this default, and it
// is the difference between an opt-in policy and a heuristic that silences
// whatever it happens to match.
func TestIsProbe_DefaultsToFalse(t *testing.T) {
	t.Parallel()
	assert.False(t, IsProbe(t.Context()), "an unmarked context must never look like a probe")
	assert.False(t, IsProbe(context.WithValue(t.Context(), struct{}{}, true)),
		"a value stored under someone else's key must not be mistaken for the marker")
}

// TestWithProbe_MarksTheContext is the round trip.
func TestWithProbe_MarksTheContext(t *testing.T) {
	t.Parallel()
	assert.True(t, IsProbe(WithProbe(t.Context())))
}

// TestWithProbe_SurvivesDerivation is what makes the marker usable where it
// is actually applied. internal/health marks the request context and then
// immediately derives a deadline from it, and pgx derives further contexts
// again before the tracer ever sees one. A marker that did not survive
// context.WithTimeout would type-check, pass a naive round-trip test, and do
// nothing whatsoever in production.
func TestWithProbe_SurvivesDerivation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(WithProbe(t.Context()), time.Minute)
	t.Cleanup(cancel)
	assert.True(t, IsProbe(ctx), "the marker must survive the deadline internal/health derives from it")
	assert.True(t, IsProbe(context.WithValue(ctx, struct{ other int }{}, "x")),
		"the marker must survive an unrelated value being layered on top")
}

// TestWithProbe_DoesNotLeakUpwards pins the direction of the marker. A
// context is a tree, so marking a derived context must not retroactively
// mark the parent that real work may still be running under.
func TestWithProbe_DoesNotLeakUpwards(t *testing.T) {
	t.Parallel()
	parent := t.Context()
	_ = WithProbe(parent)
	assert.False(t, IsProbe(parent), "marking a derived context must never mark the context it was derived from")
}
