package hooksocket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bobcob7/loam/internal/refpolicy"
)

// defaultConnDeadline bounds how long the SERVER side waits, per
// connection, for a request to fully arrive and a response to be fully
// written -- defense in depth against a stalled or misbehaving client
// tying up a handler goroutine forever. This is independent of, and
// unrelated to, client.go's DialTimeout/RPCTimeout, which bound the HOOK
// CLIENT's own patience for the same round trip; a real production
// deployment relies on both sides having a bounded wait, not just one.
const defaultConnDeadline = 5 * time.Second

// Server is the unix-socket listener docs/server-spec.md's Process Model
// names ("Policy socket (<data>/hook.sock)"), serving pre-receive ref-
// policy decisions by wrapping internal/refpolicy.EvaluatePush. Construct
// with Listen, which binds the socket synchronously -- so a bind failure
// is reported at server startup, before cmd/server ever starts its HTTP
// listener, per docs/server-spec.md Startup step 5's ordering ("git pushes
// are never accepted while the policy socket is down") -- then run its
// Accept loop via Run(ctx), which satisfies cmd/server's own runner
// interface (Run blocks until ctx is canceled AND every in-flight
// connection has been handled, matching ingest.Pool.Run's own contract
// that interface's doc comment describes).
type Server struct {
	listener     net.Listener
	store        WorkBranchStore
	onAccept     refpolicy.PostAcceptFunc
	logger       *slog.Logger
	connDeadline time.Duration
}

// Listen binds a unix socket at socketPath, removing any stale socket file
// left behind by a prior crash first: net.Listen("unix", ...) otherwise
// fails "address already in use" against a leftover, unconnectable socket
// file on disk, since the filesystem entry itself (not a live listener)
// is what collides. store resolves rule 1/2/3's Postgres lookup (see
// WorkBranchStore's own doc comment); onAccept is loam-giq.6's exposed
// catch-up-detection seam, nil until that bead lands.
func Listen(socketPath string, store WorkBranchStore, onAccept refpolicy.PostAcceptFunc, logger *slog.Logger) (*Server, error) {
	return listen(socketPath, store, onAccept, logger, defaultConnDeadline)
}

// listen is Listen's fully-parameterized form: this package's own tests
// call it directly with a short connDeadline so a test proving the
// server-side deadline actually fires does not have to wait out
// defaultConnDeadline's full production duration. Production (Listen)
// always passes defaultConnDeadline.
func listen(socketPath string, store WorkBranchStore, onAccept refpolicy.PostAcceptFunc, logger *slog.Logger, connDeadline time.Duration) (*Server, error) {
	if err := os.RemoveAll(socketPath); err != nil {
		return nil, fmt.Errorf("removing stale policy socket %s: %w", socketPath, err)
	}
	listener, err := bindUnixSocket(socketPath)
	if err != nil {
		return nil, fmt.Errorf("binding policy socket %s: %w", socketPath, err)
	}
	return &Server{listener: listener, store: store, onAccept: onAccept, logger: logger, connDeadline: connDeadline}, nil
}

// bindUnixSocket binds socketPath, working around the sun_path length
// limit unix domain sockets are subject to (~104 bytes on macOS/BSD, ~108
// on Linux, including the null terminator): a sufficiently long
// LOAM_DATA_DIR -- observed for real against this exact function, via a
// deeply-nested t.TempDir() LOAM_DATA_DIR in cmd/server's own
// main_integration_test.go harness, which made every test in that file
// fail with "server ... never became ready" once this package's Listen
// call was wired into cmd/server's Startup -- makes a direct net.Listen
// fail with "bind: invalid argument" (macOS) even though nothing is
// actually wrong with the path.
//
// The fallback is only attempted after a direct bind has already failed,
// so the common case (a short, real-world LOAM_DATA_DIR) never pays for
// it: temporarily os.Chdir into socketPath's parent directory and bind
// the bare filename instead, which the kernel accepts regardless of how
// long the absolute path to that directory is, since only the string
// actually passed to bind() is subject to the length limit. This is safe
// specifically because Listen runs once, synchronously, at server
// startup, before any other goroutine (the ingest pool, the HTTP
// listener) exists yet -- nothing else in the process can observe or
// depend on the working directory during the brief window it is changed
// (see cmd/server/main.go's run(), which calls this before newListener
// and before serve starts any background goroutine).
func bindUnixSocket(socketPath string) (net.Listener, error) {
	listener, directErr := net.Listen("unix", socketPath)
	if directErr == nil {
		return listener, nil
	}
	dir := filepath.Dir(socketPath)
	name := filepath.Base(socketPath)
	originalWd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("binding %s directly failed (%w), and could not get the working directory to retry via a relative bind: %w", socketPath, directErr, err)
	}
	if err := os.Chdir(dir); err != nil {
		return nil, fmt.Errorf("binding %s directly failed (%w), and could not change into %s to retry via a relative bind: %w", socketPath, directErr, dir, err)
	}
	defer func() { _ = os.Chdir(originalWd) }()
	return net.Listen("unix", name)
}

// Run accepts connections until ctx is canceled, handling each on its own
// goroutine, and returns only once the listener is closed AND every
// in-flight connection has finished being handled -- see this type's own
// doc comment for why that specific contract is what lets a single
// Server value satisfy cmd/server's runner interface directly, alongside
// ingest.Pool, with no adapter.
func (s *Server) Run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		_ = s.listener.Close()
	}()
	var wg sync.WaitGroup
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.logger.ErrorContext(ctx, "policy socket: accept failed", "error", err)
			}
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleConn(ctx, conn)
		}()
	}
	wg.Wait()
}

// handleConn serves exactly one Request/Response round trip per
// connection (docs/git-spec.md "Enforcement Mechanics": "it sends one
// request over the socket ... and gets back a per-ref verdict"), then
// closes the connection unconditionally. A malformed request or a client
// that stalls past s.connDeadline both end the same way: the connection
// is closed with no response ever written, which the hook client's own
// Call sees as a read failure -- and therefore, per Call's own contract,
// fails the whole push closed.
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(s.connDeadline)); err != nil {
		s.logger.ErrorContext(ctx, "policy socket: setting connection deadline", "error", err)
		return
	}
	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		s.logger.ErrorContext(ctx, "policy socket: decoding request", "error", err)
		return
	}
	resp := s.evaluate(ctx, req)
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		s.logger.ErrorContext(ctx, "policy socket: encoding response", "error", err)
	}
}

// evaluate runs refpolicy.EvaluatePush over req's updates, translating to
// and from this package's wire types. A hard evaluation error (a store
// failure, or req's context expiring mid-lookup) is reported as
// {Accepted: false, Verdicts: nil} -- there is no per-ref detail to give
// honestly in that case, and the caller (the hook client) fails the whole
// push closed regardless, exactly as if the response had never arrived at
// all.
func (s *Server) evaluate(ctx context.Context, req Request) Response {
	updates := make([]refpolicy.RefUpdate, len(req.Updates))
	for i, u := range req.Updates {
		updates[i] = refpolicy.RefUpdate{OldSHA: u.OldSHA, NewSHA: u.NewSHA, Ref: u.Ref}
	}
	verdicts, allAllowed, err := refpolicy.EvaluatePush(ctx, s.store, req.Repo, req.Agent.Name, updates, s.onAccept)
	if err != nil {
		s.logger.ErrorContext(ctx, "policy socket: evaluating push", "repo", req.Repo, "error", err)
		return Response{Accepted: false}
	}
	wire := make([]VerdictWire, len(verdicts))
	for i, v := range verdicts {
		wire[i] = VerdictWire{Ref: v.Ref, Allowed: v.Allowed, Reason: v.Reason}
	}
	return Response{Accepted: allAllowed, Verdicts: wire}
}
