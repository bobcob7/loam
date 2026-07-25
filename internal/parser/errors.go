package parser

import "errors"

// ErrNoGrammar is returned by LanguageForPath and Parse when a file's
// extension has no registered Tree-sitter grammar. Callers outside this
// package match it with errors.Is to skip the file for the code graph while
// still chunking it for RAG if it is text.
var ErrNoGrammar = errors.New("parser: no grammar registered for file")

// errUnsupportedLanguage is an internal precondition failure: Parse or
// NewQuery was called with a Language value that has no registered grammar,
// which should only happen if a caller bypasses LanguageForPath.
var errUnsupportedLanguage = errors.New("parser: unsupported language")

// errParseFailed is returned when Tree-sitter produces no tree for a reason
// other than context cancellation. A canceled or expired ctx is detected
// first and reported as that ctx error instead — see Parser.parseBytes.
var errParseFailed = errors.New("parser: tree-sitter returned no tree")

// ErrQueryClosed is returned by Query.Captures when called after Close.
// Unlike Parser and Tree — single-goroutine primitives where use-after-close
// is an unchecked use-after-free the caller must simply avoid — a Query is
// designed to be shared and called concurrently (see Query's doc comment),
// so Close and Captures synchronize with each other and a caller can hit
// this legitimately in a shutdown race. It is exported so a caller that
// races Close against in-flight Captures calls can match it with errors.Is
// rather than treating every Captures failure as fatal.
var ErrQueryClosed = errors.New("parser: query used after close")
