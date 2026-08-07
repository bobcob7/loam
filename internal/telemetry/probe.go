package telemetry

import "context"

// probeKey is the private context key carrying the probe marker. It is a
// zero-width struct type rather than a string so nothing outside this
// package can collide with it or set it without going through WithProbe.
type probeKey struct{}

// WithProbe marks ctx as belonging to a HEALTH PROBE: work performed only to
// answer "is this process still able to serve", on a fixed short interval,
// by an orchestrator that will never read a trace.
//
// # WHAT INSTRUMENTATION IS BEING TOLD
//
// Not "do not instrument this". The marker says the SUCCESSFUL case is
// uninteresting because it is identical thousands of times a day, and the
// FAILING case is the entire reason the probe exists. Instrumentation that
// honours it should therefore stay silent on success and still emit on
// failure. internal/db's queryTracer is the first honourer and does exactly
// that, building the failure span retrospectively.
//
// That distinction is the whole point, and getting it wrong in the obvious
// direction -- reading this as a blanket "skip" -- would make things worse
// than not fixing anything: a pool that has gone bad would then show up as
// NOTHING, and absence is unusable as an alarm. Anything that cannot
// preserve the failure signal should ignore this marker entirely rather
// than honour it halfway.
//
// # WHY THE CALLER DECIDES, AND NOT THE TRACER
//
// The alternative loam-om77 considered was to have internal/db infer it:
// skip any operation that has no parent span and no sqlc query name. That
// heuristic is wrong on both halves. "No parent span" is equally true of
// the sync scheduler and of ingest, which are legitimate background work
// whose root traces are the ONLY record of them -- inferring would silence
// those too. And "no query name" is a property of hand-written SQL, not of
// health checking; internal/db/migrations' schema check is unheadered and
// so is a future one-off admin query. Only the health handler knows it is a
// health handler, so only the health handler gets to say so.
//
// It also puts the decision beside the one already made at the RPC layer:
// internal/server's RegisterUnauthenticated does not instrument /healthz or
// /readyz for the same reason, and these are now visibly one policy rather
// than two unrelated omissions.
//
// # SCOPE
//
// This is a context VALUE, so it is intra-process only: it does not
// propagate over the wire, and it is deliberately not part of the
// OpenTelemetry trace state. That is correct -- it is a local decision
// about local noise, not a sampling instruction to hand a peer. It also
// means a probe marker cannot escape into work the probe merely triggered
// on another goroutine unless that goroutine was handed this exact context.
//
// A marked context must NOT be reused for real work. The health handler
// derives it per request from the incoming *http.Request's context and
// discards it when the response is written.
func WithProbe(ctx context.Context) context.Context {
	return context.WithValue(ctx, probeKey{}, true)
}

// IsProbe reports whether ctx was marked by WithProbe. Instrumentation
// calls it at the point where it would otherwise start a span.
func IsProbe(ctx context.Context) bool {
	marked, _ := ctx.Value(probeKey{}).(bool)
	return marked
}
