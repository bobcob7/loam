package parser

import (
	"context"
	"log/slog"
	"sync"
)

// ParserPool leases Parser instances so multiple goroutines can parse
// concurrently. A single Parser is not safe for concurrent use: SetLanguage
// plus Tree-sitter's internal TSParser state live in C memory, so two
// goroutines calling Parse at once race at the C level in a way go test
// -race cannot detect. Callers driven by a worker pool (loam-c94.1, via
// loam-c94.5 and loam-c94.10) should use ParserPool; a caller that only
// ever parses from one goroutine can use Parser directly.
type ParserPool struct {
	pool sync.Pool
}

// NewParserPool constructs a ParserPool whose leased Parsers log through
// logger.
func NewParserPool(logger *slog.Logger) *ParserPool {
	return &ParserPool{pool: sync.Pool{New: func() any { return NewParser(logger) }}}
}

// Parse leases a Parser, parses src as lang, and returns the Parser to the
// pool before returning. The returned Tree does not depend on the pool and
// remains valid (until Closed) after Parse returns.
func (pp *ParserPool) Parse(ctx context.Context, lang Language, src []byte) (*Tree, error) {
	p := pp.pool.Get().(*Parser)
	defer pp.pool.Put(p)
	return p.Parse(ctx, lang, src)
}
