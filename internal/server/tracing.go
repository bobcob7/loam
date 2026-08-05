package server

import (
	"context"
	"fmt"
	"slices"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"github.com/bobcob7/loam/internal/httpauth"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Span attribute keys for the caller behind an RPC. They are namespaced
// under loam. rather than reusing semconv's enduser.*/user.* because the
// subject here is an AGENT identity asserted by three trusted headers
// (internal/httpauth), not an authenticated end user; borrowing the
// semconv key would claim an authentication property loam does not have.
//
// CARDINALITY, and why these are SPAN attributes and nothing else:
//
//   - loam.agent.role is bounded by the roles table. It is the only field
//     here that would ever be safe as a METRIC label.
//   - loam.agent.name and loam.agent.id are effectively unbounded — every
//     agent run in this project gets a fresh pair, and they accumulate
//     forever. As span attributes that is free: spans are per-request and
//     head-sampled, so an unbounded attribute costs bytes on the spans that
//     survive sampling and nothing else. As metric labels the same two
//     fields are a new time series per agent, retained for the whole
//     retention window, which is how a Prometheus-compatible backend falls
//     over.
//
// No metric in this package carries any of them; see rpcOptions for why
// this package emits no RPC metrics at all this round.
const (
	attrCallerKind      = attribute.Key("loam.caller.kind")
	attrAgentName       = attribute.Key("loam.agent.name")
	attrAgentID         = attribute.Key("loam.agent.id")
	attrAgentRole       = attribute.Key("loam.agent.role")
	attrAgentIdentifier = attribute.Key("loam.agent.identifier")
)

// Values for attrCallerKind. This attribute is ALWAYS set on an RPC span,
// including when there is no agent identity to report, and it is bounded to
// these three strings.
//
// It exists because of the deliberate choice made in annotateSpanWithCaller:
// a request with no identity records NO loam.agent.* attributes at all
// rather than recording them as "". Absence is a queryable state in every
// trace backend, whereas an empty string is indistinguishable from an agent
// whose name genuinely is empty and forces every query to carry a `!= ""`
// clause. The cost of absence on its own is ambiguity — "no agent
// attributes" could mean an admin superuser, an unidentified caller, or
// instrumentation that silently stopped working — so callerKind is recorded
// unconditionally to name which of the three it was.
const (
	// callerKindAgent: the request carried all three Loam-Agent-* headers
	// and httpauth.CLI or httpauth.GitIdentity resolved them.
	callerKindAgent = "agent"
	// callerKindAdmin: the request presented valid admin basic auth.
	// httpauth's AdminOnly, and the admin branch of CLI, deliberately set
	// NO agent identity alongside the admin marker, so there is nothing
	// further to record — an admin is a superuser, not an agent.
	callerKindAdmin = "admin"
	// callerKindAnonymous: neither. No path this Router registers can
	// produce it today — httpauth.CLI 401s a request with neither
	// credential and AdminOnly 401s anything but valid basic auth, both
	// BEFORE the handler and therefore before any interceptor here runs.
	// It is recorded anyway so that if that ever stops being true (a new
	// auth regime, a handler mounted outside these wrappers) it shows up
	// as a value in the traces rather than as an absence that looks like
	// broken instrumentation.
	callerKindAnonymous = "anonymous"
)

// rpcOptions builds the connect.HandlerOption set every /loam.v1.* and
// /loam.admin.v1.* handler in this process is constructed with. A Router
// builds it exactly once in New and hands it out through RPCOptions.
//
// # Why the instrumentation is a connect interceptor and not HTTP middleware
//
// The obvious-looking alternative — wrap Handler()'s mux in otelhttp, one
// line, no call-site changes — is wrong here for a specific reason: it
// would trace /healthz and /readyz. Those are polled on a tight liveness
// interval, so tracing them buries real traffic in noise and spends
// collector budget on the two requests that carry the least information in
// the system. Putting the instrumentation at the connect layer makes that
// exemption STRUCTURAL rather than a path-string exclusion list: the health
// endpoints are plain http.Handlers registered through
// RegisterUnauthenticated, they are not connect handlers, they never see
// these options, and there is therefore no code path by which they could
// acquire a span. It also leaves untouched the property that
// cmd/server/main.go documents as "the only such exemption" and
// TestRouter_RegisterUnauthenticated_RejectsNonHealthPattern enforces —
// which an HTTP-level wrapper would have had to special-case by path, the
// exact shape loam-gcg refused for auth.
//
// The cost of that choice is that connect interceptors are handler
// CONSTRUCTION options; connect-go exposes no seam for attaching one to an
// already-built http.Handler. So the Router cannot inject them inside
// RegisterCLI/RegisterAdmin the way it injects the auth wrappers, and each
// generated constructor in cmd/server/main.go passes RPCOptions()...
// explicitly. That is a fact about connect-go's API, not a design
// preference: the Router still OWNS the instrumentation (one construction,
// one policy, one place to change it) and no service implementation
// contains a span.
//
// # No RPC metrics this round
//
// otelconnect emits five instruments per RPC alongside its span, and this
// call disables all of them with WithoutMetrics.
//
// The load-bearing reason is CARDINALITY. otelconnect v0.9.0's
// addRequestAttributes -> addAddressAttributes puts net.peer.name AND
// net.peer.port into the attribute slice, and that same slice becomes the
// attribute.NewSet passed to all five Record calls — so on the server side
// each client's EPHEMERAL TCP PORT becomes its own time series, retained for
// the whole retention window. That is a cardinality explosion strictly worse
// than the agent name/id this file refuses to put on metrics, and it is
// demonstrated, not inferred: with a global MeterProvider installed, one RPC
// through this Router records five instruments carrying a live
// net.peer.port.
//
// WHAT A LATER BEAD CANNOT DO, so it is not attempted here. The obvious
// remedy — WithoutServerPeerAttributes, keeping the peer attributes on the
// span — DOES NOT EXIST in v0.9.0. That option is implemented as
// WithAttributeFilter, documented as applying to "all metrics and trace
// attributes", and in the unary path the trace attribute set is derived from
// the same filtered slice. There is no per-signal seam. So the real menu for
// the metrics bead is: keep peer attributes on spans and accept a series per
// client port, or drop them from BOTH signals. Nor can role be added back:
// AttributeFilter is func(Spec, KeyValue) bool — keep or drop, never add —
// so loam.agent.role cannot become a metric label through this interceptor
// at any setting.
//
// A second, weaker reason: nothing would receive these instruments today.
// otelconnect captures otel.GetMeterProvider() at construction and
// internal/telemetry deliberately installs no OpenTelemetry global, so they
// would look wired and record nothing. That is a CHOICE rather than an
// absence — internal/telemetry does build a real MeterProvider and exposes
// it; rpcOptions simply is not given one — which is exactly why it cannot
// carry the argument on its own. If a later bead passes that provider in,
// the cardinality reason above is what still stands.
func rpcOptions(tracerProvider trace.TracerProvider) ([]connect.HandlerOption, error) {
	if tracerProvider == nil {
		tracerProvider = tracenoop.NewTracerProvider()
	}
	// WithTracerProvider is the whole reason internal/telemetry can get
	// away with installing no otel global: the provider is injected here
	// by the composition root, exactly as *slog.Logger is everywhere else
	// in this tree.
	otelInterceptor, err := otelconnect.NewInterceptor(
		otelconnect.WithTracerProvider(tracerProvider),
		otelconnect.WithoutMetrics(),
	)
	if err != nil {
		return nil, fmt.Errorf("building otelconnect interceptor: %w", err)
	}
	// Order matters and is not arbitrary. connect-go applies handler
	// interceptors so that the FIRST is outermost, so otelInterceptor runs
	// first and starts the span, and callerAttributeInterceptor runs inside
	// it with that span already on the context. Swapping them would leave
	// the identity interceptor annotating whatever span happened to be on
	// the inbound context — in practice none — and silently record nothing.
	// TestRPCSpan_RecordsCallerIdentity fails if they are reordered.
	return []connect.HandlerOption{
		connect.WithInterceptors(otelInterceptor, callerAttributeInterceptor{}),
	}, nil
}

// callerAttributeInterceptor annotates the span otelconnect started with
// the caller behind the request (see attrCallerKind). It creates no span of
// its own and NEVER touches the span's NAME: otelconnect names the span
// after the RPC procedure ("loam.v1.RepoService/GetRepo"), and that is what
// makes traces aggregatable — a span named after the caller would produce
// one distinct operation per agent and make "p99 of GetRepo" unanswerable.
// Identity belongs in attributes, which are filterable, and nowhere else.
// Do not "improve" this by folding the agent into the name.
type callerAttributeInterceptor struct{}

var _ connect.Interceptor = callerAttributeInterceptor{}

func (callerAttributeInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, request connect.AnyRequest) (connect.AnyResponse, error) {
		annotateSpanWithCaller(ctx)
		return next(ctx, request)
	}
}

// WrapStreamingClient is a no-op: this interceptor is only ever installed
// as a HandlerOption, so the client half is unreachable. It is present
// because connect.Interceptor requires it.
func (callerAttributeInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler annotates streaming RPCs the same way as unary ones.
// loam declares no streaming methods today (every proto/loam/**/*.proto RPC
// is unary), so this is dead code that stays correct by construction rather
// than a gap that would have to be noticed later.
func (callerAttributeInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		annotateSpanWithCaller(ctx)
		return next(ctx, conn)
	}
}

// annotateSpanWithCaller records who made the request onto the current span.
// It reads the identity from the CONTEXT rather than from request headers on
// purpose: internal/httpauth is the single place that decides whether a set
// of Loam-Agent-* headers is complete enough to trust, and re-reading the
// raw headers here would let a span disagree with the auth decision the rest
// of the request was made under.
//
// The IsRecording guard keeps the disabled path free — with telemetry off
// the span is upstream's non-recording no-op, and this returns before
// building any attribute.
func annotateSpanWithCaller(ctx context.Context) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	identity, ok := httpauth.IdentityFromContext(ctx)
	if !ok {
		kind := callerKindAnonymous
		if httpauth.IsAdmin(ctx) {
			kind = callerKindAdmin
		}
		span.SetAttributes(attrCallerKind.String(kind))
		return
	}
	// loam.agent.identifier duplicates the other three joined by "-". It is
	// recorded anyway because that joined form is the one a human actually
	// has in hand: it is what `loam whoami` prints (Identity.Identifier)
	// and what appears in review threads, so "show me everything
	// ada-7-reviewer did" is one attribute match instead of a three-way
	// conjunction the querier has to know to write.
	span.SetAttributes(
		attrCallerKind.String(callerKindAgent),
		attrAgentName.String(identity.Name),
		attrAgentID.String(identity.ID),
		attrAgentRole.String(identity.Role),
		attrAgentIdentifier.String(identity.Identifier()),
	)
}

// RPCOptions returns the connect.HandlerOption set every generated
// /loam.v1.* and /loam.admin.v1.* handler constructor in this process must
// be given:
//
//	router.RegisterCLI(loamv1connect.NewRepoServiceHandler(impl, router.RPCOptions()...))
//
// A handler constructed without them still routes and still gets its auth
// wrapper; it simply produces no span. Health handlers are registered
// through RegisterUnauthenticated and are not connect handlers, so they
// never take these options — see rpcOptions for why that exemption is
// structural rather than a path check.
//
// The returned slice is a copy: a caller doing append(router.RPCOptions(),
// extra...) must not be able to write into the Router's own backing array
// and change what every other handler is built with.
func (rt *Router) RPCOptions() []connect.HandlerOption {
	return slices.Clone(rt.rpcOptions)
}
