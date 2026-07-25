// Package handler holds the conventions every loam.v1 / loam.admin.v1
// Connect handler package builds on: domain-error-to-Connect-code mapping
// (ErrorMapper) and capability-based authorization (CapabilityChecker),
// reading the identity/isAdmin context internal/httpauth establishes.
//
// # The interfaces.go convention
//
// Each handler package (one per Connect service, e.g. a future
// internal/handler/workbranch) declares the store/forge seams it CONSUMES
// in a single interfaces.go file — small interfaces, one or two methods
// where practical — annotated with a single
// //go:generate go tool moq -out moq_test.go . Iface1 Iface2
// directive. Mocks land in moq_test.go in the same package; never
// hand-write a mock. This package's own interfaces.go (RoleStore) is the
// first instance of the convention.
//
// # Error-wrapping style
//
// Handler authors return one of the sentinel errors below wrapped with
// fmt.Errorf and %w, adding the identifying detail the caller needs:
//
//	return nil, fmt.Errorf("work branch %s: %w", id, handler.ErrNotFound)
//
// and pass the result through (*ErrorMapper).ToConnectErr(err) at the RPC
// boundary. Never return a bare connect.Error from inside a handler body,
// and never match errors with a type assertion — always errors.Is/As.
package handler
