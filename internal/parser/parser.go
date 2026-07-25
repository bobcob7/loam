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
	// Parser.ParseCtx wires context cancellation through a background
	// goroutine that stores into a C-owned cancellation flag; it is
	// deprecated upstream and segfaults during teardown on this platform.
	// A source file's parse is fast enough that only a pre-check for an
	// already-canceled ctx is needed; there is no mid-parse cancellation.
	tree := p.inner.Parse(src, nil)
	if tree == nil {
		return nil, fmt.Errorf("parsing as %q: %w", lang, errParseFailed)
	}
	p.logger.Debug("parsed file", "language", lang, "bytes", len(src))
	return &Tree{inner: tree, src: src, lang: lang}, nil
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
