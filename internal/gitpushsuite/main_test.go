// Package gitpushsuite is loam-li0.7's own Layer 2 integration suite
// (docs/testing-spec.md Layer 2, "Git transport & policy"): the push-
// rejection matrix, assembled against the SAME composition cmd/server/
// main.go actually wires -- httpauth.Auth.GitIdentity wrapping
// handler.GitRoleGate wrapping internal/handler/git.Handler, backed by a
// real compiled cmd/loamhook binary talking to a real hooksocket.Server
// over a real unix socket -- driven with real `git` subprocesses against
// real bare mirrors. No package already owns this exact composition:
// internal/hooksocket's own e2e_test.go (loam-ofg.18) injects identity
// straight into the request context and never wires GitRoleGate at all;
// internal/server/gitrolegate_test.go and internal/handler/gitrolegate_test.go
// prove the role gate and identity middleware against httptest recorders
// with a stand-in git process, never a real git binary or a real hook; and
// internal/handler/git/realgit_test.go proves the transport plumbing with
// no hook and no role gate installed at all. This package exists
// specifically to close that gap: the full request path, real binaries
// throughout, one dedicated home -- the same "Layer 2 gets its own suite
// package" shape internal/storesuite already established for the Store
// bullet in the same spec section.
//
// This package is deliberately NOT //go:build integration: like
// internal/hooksocket/e2e_test.go and internal/mirrorreconcile's own
// tests, it needs no container -- only a real git binary and a real
// compiled cmd/loamhook (built once below), so it runs in the ordinary
// `go test ./... -race` gate.
//
// This package covers the push-rejection matrix (failclosed_test.go,
// matrix_test.go, forcedelete_test.go, atomicity_test.go,
// crosscheck_test.go) and does not re-prove loam-li0.7's other two
// Definition of Done items, which are already green against real
// binaries elsewhere: single-branch clone bootstrap config (identity
// headers, --single-branch narrowing) is cmd/server/
// clonepush_integration_test.go's assertSingleBranchClone and
// assertIdentityHeadersConfigured; startup reconciliation idempotency
// (re-run twice, no drift in the installed hook or receive.deny* config)
// is internal/mirrorreconcile/reconcile_test.go's
// TestReconcileMirror_SecondCallIsNoopAndConfigStaysCorrect.
package gitpushsuite

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// loamhookBinary is cmd/loamhook's compiled path, built once for every
// test in this package by TestMain -- mirrors internal/hooksocket/
// e2e_test.go's and cmd/server/main_integration_test.go's own convention.
var loamhookBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "loamhook-build-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	loamhookBinary = filepath.Join(dir, "loamhook")
	build := exec.Command("go", "build", "-o", loamhookBinary, "github.com/bobcob7/loam/cmd/loamhook")
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		fmt.Fprintf(os.Stderr, "building loamhook binary: %v\n%s", buildErr, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
