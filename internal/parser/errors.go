package parser

import "errors"

// ErrNoGrammar is returned by LanguageForPath and Parse when a file's
// extension has no registered Tree-sitter grammar. Callers outside this
// package match it with errors.Is to skip the file for the code graph while
// still chunking it for RAG if it is text.
var ErrNoGrammar = errors.New("parser: no grammar registered for file")

// errUnsupportedLanguage is an internal precondition failure: Parse was
// called with a Language value that has no registered grammar, which should
// only happen if a caller bypasses LanguageForPath.
var errUnsupportedLanguage = errors.New("parser: unsupported language")

// errParseFailed is returned when Tree-sitter produces no tree for a reason
// other than context cancellation. A canceled or expired ctx is detected
// first and reported as that ctx error instead — see Parser.parseBytes.
var errParseFailed = errors.New("parser: tree-sitter returned no tree")
