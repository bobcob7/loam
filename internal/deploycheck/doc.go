// Package deploycheck holds no code. It exists so that the deployment
// artifacts that are not Go -- deploy/docker-compose.yml,
// deploy/docker-compose.e2e.yml, helm/loam/values.yaml -- can be guarded by
// ordinary `go test`, at unit-test speed, with no Docker daemon and no
// cluster (loam-lzxo.7).
//
// The rot these tests exist to catch is the quiet kind. Three files pin the
// Postgres image and one of them can be bumped alone; the compose file names
// a dozen LOAM_* variables and internal/config can rename one. Neither
// failure announces itself: the first means the e2e suite is exercising a
// pgvector nobody deploys, and the second means a variable is silently
// ignored and its default silently applied. A comment saying "keep these in
// sync" is documentation; this package is a guard.
//
// These tests DISCOVER the values they compare wherever discovery is
// possible -- parsing the YAML and the Go AST, and in one case running
// config.Load itself -- rather than restating what those files are supposed
// to contain. A hand-maintained list of expected values plus a test
// asserting the list is the same thing as the comment, only harder to read:
// it goes stale in exactly the situations it was written for. An earlier
// version of compose_test.go listed the required LOAM_* variables by hand
// and two mutations walked straight through it (a new lookupRequired in
// internal/config, and a deleted LOAM_DB_NAME in the compose file), which
// is why TestComposeEnvironmentSatisfiesConfigLoad now asks the loader
// instead of modelling it.
//
// Two things here are NOT discovered, both deliberately, and each is
// guarded by a test whose only job is to stop it going stale:
//
//   - operatorSuppliedValues, the stand-in for a human filling out
//     deploy/.env. Nothing in the repository knows what your admin password
//     is. TestOperatorSuppliedValuesCoverEveryMustSetVariable fails the
//     moment the compose file grows a must-set variable the map does not
//     answer for.
//   - the explicit list in TestMustSetVariablesHaveNoWorkingDefault, which
//     encodes a POLICY (these particular values must never have a working
//     default) rather than a fact. That test also carries a discovered pass
//     over every credential-shaped name in either compose file, so a secret
//     nobody has thought of yet is still caught.
//
// It lives in internal/ rather than under deploy/ so that deploy/ stays what
// it looks like -- a directory of YAML -- and so `go build ./...` has a
// normal package to build rather than a test-only directory.
package deploycheck
