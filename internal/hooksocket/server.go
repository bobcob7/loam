package hooksocket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/bobcob7/loam/internal/refpolicy"
	"github.com/bobcob7/loam/internal/workbranchstore"
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
	onAccept     PostAcceptFunc
	logger       *slog.Logger
	connDeadline time.Duration
}

// Listen binds a unix socket at socketPath, removing any stale socket file
// left behind by a prior crash first: net.Listen("unix", ...) otherwise
// fails "address already in use" against a leftover, unconnectable socket
// file on disk, since the filesystem entry itself (not a live listener)
// is what collides. store resolves rule 1/2/3's Postgres lookup (see
// WorkBranchStore's own doc comment); onAccept runs once per accepted ref
// update once the whole push has passed policy (production binds it to
// internal/catchup.Detector.OnAcceptedPush; nil is a documented no-op).
func Listen(socketPath string, store WorkBranchStore, onAccept PostAcceptFunc, logger *slog.Logger) (*Server, error) {
	return listen(socketPath, store, onAccept, logger, defaultConnDeadline)
}

// listen is Listen's fully-parameterized form: this package's own tests
// call it directly with a short connDeadline so a test proving the
// server-side deadline actually fires does not have to wait out
// defaultConnDeadline's full production duration. Production (Listen)
// always passes defaultConnDeadline.
func listen(socketPath string, store WorkBranchStore, onAccept PostAcceptFunc, logger *slog.Logger, connDeadline time.Duration) (*Server, error) {
	if err := os.RemoveAll(socketPath); err != nil {
		return nil, fmt.Errorf("removing stale policy socket %s: %w", socketPath, err)
	}
	listener, err := bindUnixSocket(socketPath)
	if err != nil {
		return nil, fmt.Errorf("binding policy socket %s: %w", socketPath, err)
	}
	return &Server{listener: listener, store: store, onAccept: onAccept, logger: logger, connDeadline: connDeadline}, nil
}

// maxSunPathBytes is the tightest widely-deployed sun_path buffer size
// unix domain sockets are subject to (104 bytes on macOS/BSD, including
// the null terminator; Linux's is 108) -- used only to produce an
// actionable error message here, not to reject a path early: the real
// bind(2)/connect(2) syscalls are still what decide whether a given path
// actually fits, on whatever platform is running.
const maxSunPathBytes = 104

// bindUnixSocket binds socketPath directly -- no chdir-and-retry
// workaround. An earlier version of this function fell back to a
// temporary os.Chdir + relative bind when the direct bind failed, on the
// theory that only the STRING passed to bind(2) is subject to the
// sun_path length limit. That reasoning does not survive contact with the
// other side of this same wire protocol: cmd/loamhook (run.go) has no
// equivalent chdir trick and must dial the exact same absolute path this
// server bound, since connect(2) is subject to the identical sun_path
// limit -- so the fallback let Listen succeed while making 100% of pushes
// fail closed afterward, forever, with an opaque "connect: invalid
// argument" error and no signal at server startup that anything was
// wrong. A loud, immediate startup failure (this function's current
// behavior) is far preferable to a policy socket that reports healthy but
// can never actually be reached by the one client that matters.
//
// The real fix for a LOAM_DATA_DIR long enough to hit this belongs at the
// deployment/test-harness level: keep LOAM_DATA_DIR short enough that
// "<LOAM_DATA_DIR>/hook.sock" fits (this is exactly what
// cmd/server/main_integration_test.go's own long, per-test-name t.TempDir()
// path was doing wrong -- fixed there, not here, by using a short temp
// directory instead).
func bindUnixSocket(socketPath string) (net.Listener, error) {
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("%w (policy socket path is %d bytes; unix domain sockets are typically limited to a sun_path of ~%d bytes -- set LOAM_DATA_DIR to a shorter path)", err, len(socketPath), maxSunPathBytes)
	}
	return listener, nil
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
	verdicts, allAllowed, err := refpolicy.EvaluatePush(ctx, s.store, req.Repo, req.Agent.Name, updates, s.postAccept(req))
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

// postAccept adapts s.onAccept to the narrower refpolicy.PostAcceptFunc
// EvaluatePush actually takes, folding in the two per-PUSH facts refpolicy
// never sees per-REF: the repo name and this push's object quarantine
// directory (see AcceptedPush's own doc comment for why those two live
// here rather than in refpolicy).
//
// A nil s.onAccept produces a nil refpolicy.PostAcceptFunc rather than a
// non-nil closure that does nothing: EvaluatePush skips its whole
// post-accept loop on nil, and returning a live closure would turn a
// caller's explicit "no hook" into per-ref work on every accepted push.
func (s *Server) postAccept(req Request) refpolicy.PostAcceptFunc {
	if s.onAccept == nil {
		return nil
	}
	return func(ctx context.Context, wb workbranchstore.WorkBranch, update refpolicy.RefUpdate) {
		s.onAccept(ctx, AcceptedPush{
			Repo:          req.Repo,
			QuarantineDir: req.QuarantineDir,
			WorkBranch:    wb,
			Update:        update,
		})
	}
}
