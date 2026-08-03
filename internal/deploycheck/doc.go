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
// Everything here DISCOVERS the values it compares -- it parses the YAML and
// the Go AST rather than restating what they are supposed to contain. A
// hand-maintained list of expected values plus a test asserting the list is
// the same thing as the comment, only harder to read: it goes stale in
// exactly the situations it was written for.
//
// It lives in internal/ rather than under deploy/ so that deploy/ stays what
// it looks like -- a directory of YAML -- and so `go build ./...` has a
// normal package to build rather than a test-only directory.
package deploycheck
