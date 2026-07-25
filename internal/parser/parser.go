package parser

import (
	"context"
	"fmt"
	"log/slog"

	ts "github.com/tree-sitter/go-tree-sitter"
	tsgo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tsjavascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tspython "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tstypescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// largeFileThreshold is the source size above which Parse wires ctx
// cancellation through Tree-sitter's ProgressCallback. Below it, parsing
// finishes fast enough on realistic input that the cost of ParseWithOptions
// isn't worth paying: it retains its *ParseOptions via pointer.Save with no
// matching Unref (a real, measured leak — ~67 bytes per parse, never freed
// until process exit). At or above it, a single parse can run into seconds
// of uninterruptible cgo, which would stall job cancellation and graceful
// shutdown in a worker pool (loam-c94.1); that risk is worth the retention.
const largeFileThreshold = 256 * 1024

// grammars maps each registered Language to its loaded Tree-sitter grammar.
// Adding a language means adding one entry here plus one in
// extensionLanguages (language.go) — no other code changes.
var grammars = map[Language]*ts.Language{
	LanguageGo:         ts.NewLanguage(tsgo.Language()),
	LanguagePython:     ts.NewLanguage(tspython.Language()),
	LanguageTypeScript: ts.NewLanguage(tstypescript.LanguageTypescript()),
	LanguageTSX:        ts.NewLanguage(tstypescript.LanguageTSX()),
	LanguageJavaScript: ts.NewLanguage(tsjavascript.Language()),
}

// Parser parses source files into syntax trees using one Tree-sitter
// grammar per Language. It is the only concrete type in the repository that
// touches cgo; everything above it consumes the pure-Go Tree/Node surface.
//
// A Parser is safe for reuse across files but not for concurrent use from
// multiple goroutines at once — callers needing concurrency should use one
// Parser per goroutine, or a pool.
type Parser struct {
	logger *slog.Logger
	inner  *ts.Parser
}

// NewParser constructs a Parser ready to parse any registered Language.
func NewParser(logger *slog.Logger) *Parser {
	return &Parser{logger: logger, inner: ts.NewParser()}
}

// Close releases the underlying Tree-sitter parser. Safe to call once.
// Calling Parse after Close is a use-after-free on C memory, not a checked
// error: the caller must not do it.
func (p *Parser) Close() {
	p.inner.Close()
}

// Parse parses src as the given language and returns its syntax tree. The
// returned Tree owns C memory and must be Closed by the caller. Parse
// returns ErrNoGrammar if lang has no registered grammar.
func (p *Parser) Parse(ctx context.Context, lang Language, src []byte) (*Tree, error) {
	grammar, ok := grammars[lang]
	if !ok {
		return nil, fmt.Errorf("parsing as %q: %w", lang, errUnsupportedLanguage)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("parsing as %q: %w", lang, err)
	}
	if err := p.inner.SetLanguage(grammar); err != nil {
		return nil, fmt.Errorf("setting grammar for %q: %w", lang, err)
	}
	tree := p.parseBytes(ctx, src)
	if tree == nil {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("parsing as %q: %w", lang, err)
		}
		return nil, fmt.Errorf("parsing as %q: %w", lang, errParseFailed)
	}
	p.logger.Debug("parsed file", "language", lang, "bytes", len(src))
	return &Tree{inner: tree, src: src, lang: lang}, nil
}

// parseBytes runs Tree-sitter over src. Deliberately avoids the deprecated
// Parser.ParseCtx: it cancels via a goroutine writing a C-owned
// CancellationFlag and segfaults during teardown on darwin/arm64. Instead,
// files at or above largeFileThreshold parse through ParseWithOptions with a
// ProgressCallback polling ctx — Tree-sitter only invokes it while there is
// work left, so it costs nothing extra on a clean parse and returns quickly
// once ctx is done, without waiting for the whole (possibly seconds-long)
// parse to finish. Smaller files use plain Parse, which retains no C memory
// per call.
func (p *Parser) parseBytes(ctx context.Context, src []byte) *ts.Tree {
	read := func(offset int, _ ts.Point) []byte {
		if offset < len(src) {
			return src[offset:]
		}
		return []byte{}
	}
	if len(src) < largeFileThreshold {
		return p.inner.ParseWith(read, nil)
	}
	return p.inner.ParseWithOptions(read, nil, &ts.ParseOptions{
		ProgressCallback: func(ts.ParseState) bool { return ctx.Err() != nil },
	})
}

// ParsePath maps path's extension to a Language via LanguageForPath, then
// parses src as that language. It returns ErrNoGrammar for an unregistered
// extension, letting the caller skip the file for the code graph while
// still chunking it for RAG if it is text.
func (p *Parser) ParsePath(ctx context.Context, path string, src []byte) (*Tree, error) {
	lang, err := LanguageForPath(path)
	if err != nil {
		return nil, err
	}
	return p.Parse(ctx, lang, src)
}
