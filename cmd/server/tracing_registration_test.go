package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bobcob7/loam/internal/ingest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// The two Connect service groups docs/web-spec.md -> Hosting & Routing
// defines, and the only two proto packages this repository declares. Any
// service in either must be registered by buildRouter AND must be
// constructed with router.RPCOptions(); see the test below.
const (
	cliProtoPackage   = "loam.v1"
	adminProtoPackage = "loam.admin.v1"
)

// declaredProcedure is one probe: the first method of one declared
// service, plus which auth regime reaches it.
type declaredProcedure struct {
	service protoreflect.FullName
	method  protoreflect.Name
	admin   bool
}

func (p declaredProcedure) path() string {
	return "/" + string(p.service) + "/" + string(p.method)
}

// spanName is what otelconnect names the span for this procedure --
// the Connect procedure with the leading slash trimmed.
func (p declaredProcedure) spanName() string {
	return string(p.service) + "/" + string(p.method)
}

// declaredProcedures walks the protobuf global registry for every service
// this binary links in under loam.v1 or loam.admin.v1, and returns one
// probe per service.
//
// The registry, not a hand-written list, is what makes the test below
// EXHAUSTIVE: a service added to proto/ and wired into buildRouter without
// router.RPCOptions() appears here automatically and fails, whereas a
// hand-written list would have to be remembered -- which is precisely the
// thing being guarded against.
func declaredProcedures(t *testing.T) []declaredProcedure {
	t.Helper()
	var procedures []declaredProcedure
	protoregistry.GlobalFiles.RangeFiles(func(file protoreflect.FileDescriptor) bool {
		pkg := string(file.Package())
		if pkg != cliProtoPackage && pkg != adminProtoPackage {
			return true
		}
		services := file.Services()
		for i := range services.Len() {
			service := services.Get(i)
			require.Positive(t, service.Methods().Len(),
				"service %s declares no methods; this probe assumes at least one", service.FullName())
			procedures = append(procedures, declaredProcedure{
				service: service.FullName(),
				method:  service.Methods().Get(0).Name(),
				admin:   pkg == adminProtoPackage,
			})
		}
		return true
	})
	sort.Slice(procedures, func(i, j int) bool { return procedures[i].service < procedures[j].service })
	return procedures
}

// TestBuildRouter_EveryDeclaredServiceIsTraced closes the one hole
// buildRouter's doc comment names: connect interceptors are handler
// CONSTRUCTION options, so unlike the auth wrappers the Router cannot apply
// its RPC instrumentation for the caller, and a generated constructor
// called without router.RPCOptions()... routes and authenticates correctly
// while producing no span. Nothing in internal/server can see that -- the
// forgotten call site is in this file's package.
//
// The registry walk makes the check exhaustive without any new Router API,
// without Docker and in well under a second. It is the "a comment is not a
// guard" correction to the trade buildRouter's doc comment describes:
// dropping router.RPCOptions()... from ANY of the nine constructors fails
// here, naming the procedure that lost its span.
//
// Handler outcomes are deliberately irrelevant. Every probe runs against an
// unreachable pool and an unreachable embedder, so every RPC fails -- and
// the span is still recorded, because otelconnect ends its span whatever
// the handler returns (internal/server's TestRPCSpan_ErrorStatusIsRecorded
// establishes that directly). Asserting on spans rather than on responses
// is what keeps this test from needing a database.
func TestBuildRouter_EveryDeclaredServiceIsTraced(t *testing.T) {
	t.Parallel()
	procedures := declaredProcedures(t)
	// A floor, not an exact count: it catches a registry walk that silently
	// matched nothing (which would make every assertion below vacuous),
	// while a new service added by a later bead raises the count and is
	// checked by the set comparison rather than churning this line.
	require.GreaterOrEqual(t, len(procedures), 9,
		"expected at least the nine services this repo declares; got %d -- registry walk broken?", len(procedures))
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	// context.WithoutCancel: t.Context() is already cancelled by the time
	// cleanups run (loam-fyh5), and a cancelled context makes Shutdown
	// return "context canceled".
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.WithoutCancel(t.Context()))) })
	cfg := testConfigForRouter()
	// registerSearchService returns early -- registering NOTHING -- unless
	// an embedder is configured, so testConfigForRouter's zero values would
	// silently drop loam.v1.SearchService from the router entirely and this
	// test would report it as untraced. Pointing it at a closed port keeps
	// the registration branch taken while keeping the test container-free;
	// the embedder is never reached, because the capability check fails
	// against the unreachable pool first.
	cfg.EmbedderURL = "http://127.0.0.1:1"
	cfg.EmbedderModel = "nomic-embed-text"
	pool := unreachablePool(t)
	ingestPool := ingest.NewPool(cfg.Logger, pool, nil, 1)
	router := buildRouter(cfg, pool, ingestPool, "", provider)
	srv := httptest.NewServer(router.Handler())
	t.Cleanup(srv.Close)
	client := &http.Client{Transport: newIsolatedTransport(t), Timeout: 10 * time.Second}
	for _, procedure := range procedures {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			srv.URL+procedure.path(), strings.NewReader("{}"))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		if procedure.admin {
			request.SetBasicAuth(cfg.AdminUser, cfg.AdminPassword)
		} else {
			request.Header.Set("Loam-Agent-Name", "ada-lovelace")
			request.Header.Set("Loam-Agent-Id", "7")
			request.Header.Set("Loam-Agent-Role", "author")
		}
		response, err := client.Do(request)
		require.NoError(t, err, procedure.path())
		require.NoError(t, response.Body.Close())
		// The status is not asserted: every one of these fails against the
		// unreachable pool, and which failure it is belongs to the handler
		// beads, not here. A 404 WOULD matter -- it would mean the service
		// is not registered at all -- and it is caught below, because an
		// unregistered service produces no span either.
	}
	recorded := make(map[string]bool, len(recorder.Ended()))
	for _, span := range recorder.Ended() {
		recorded[span.Name()] = true
	}
	var untraced []string
	for _, procedure := range procedures {
		if !recorded[procedure.spanName()] {
			untraced = append(untraced, fmt.Sprintf(
				"no span for %s -- its constructor in buildRouter is missing router.RPCOptions()..., or the service is not registered at all",
				procedure.spanName()))
		}
	}
	assert.Empty(t, untraced, "every declared %s / %s service must be traced:\n%s",
		cliProtoPackage, adminProtoPackage, strings.Join(untraced, "\n"))
	assert.Len(t, recorder.Ended(), len(procedures),
		"expected exactly one span per probed procedure")
}
