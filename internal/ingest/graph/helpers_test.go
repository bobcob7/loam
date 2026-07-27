package graph

import (
	"io"
	"log/slog"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// int32Ptr is a small helper for building codegraph.SymbolInput/Symbol
// literals with a concrete (non-file-level) line in test expectations.
func int32Ptr(v int32) *int32 { return &v }
