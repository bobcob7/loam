package hooksocket

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// DialTimeout is cmd/loamhook's connect timeout: docs/git-spec.md
// "Enforcement Mechanics" pins fail-closed on "socket unreachable or a
// short timeout (2-5s)"; this sits inside that band. Production wiring
// (cmd/loamhook) always passes this constant; it is a parameter of Call,
// not a hardcoded value inside it, purely so this package's own tests can
// substitute a much shorter timeout and prove the fail-closed path fires
// without a real test waiting out the production duration.
const DialTimeout = 3 * time.Second

// RPCTimeout bounds the round trip once connected -- writing the request
// and reading the response together -- to the same "2-5s" band.
const RPCTimeout = 5 * time.Second

// Call dials socketPath, sends req, and reads back exactly one Response,
// bounded by dialTimeout (connect) and rpcTimeout (the request/response
// round trip once connected, applied as a single deadline covering both
// the write and the read). Any failure at any stage -- connection
// refused, the deadline elapsing before a full response arrives, a
// response that does not parse as JSON -- returns a non-nil error, which
// every caller MUST treat as fail-closed: reject the whole push, never
// fall back to accepting it. cmd/loamhook is this function's one
// production caller; this package's own client_test.go and server_test.go
// exercise it directly against both a real Server and deliberately
// misbehaving fake listeners, so the wire format and every fail-closed
// path are asserted in one place, not duplicated between the hook binary
// and this package.
func Call(socketPath string, req Request, dialTimeout, rpcTimeout time.Duration) (Response, error) {
	conn, err := net.DialTimeout("unix", socketPath, dialTimeout)
	if err != nil {
		return Response{}, fmt.Errorf("connecting to policy socket %s: %w", socketPath, err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(rpcTimeout)); err != nil {
		return Response{}, fmt.Errorf("setting policy socket deadline: %w", err)
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, fmt.Errorf("sending policy request: %w", err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("reading policy response: %w", err)
	}
	return resp, nil
}
