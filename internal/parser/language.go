package parser

import (
	"path/filepath"
	"strings"
)

// Language identifies one of the grammars registered with this package.
type Language string

// Supported languages for the MVP starter grammar set.
const (
	LanguageGo         Language = "go"
	LanguagePython     Language = "python"
	LanguageTypeScript Language = "typescript"
	LanguageTSX        Language = "tsx"
	LanguageJavaScript Language = "javascript"
)

// extensionLanguages maps a lowercase file extension (including the leading
// dot) to the Language whose grammar should parse it. Adding a language is a
// one-line addition here plus a grammar registration in newGrammars.
var extensionLanguages = map[string]Language{
	".go":  LanguageGo,
	".py":  LanguagePython,
	".ts":  LanguageTypeScript,
	".mts": LanguageTypeScript,
	".cts": LanguageTypeScript,
	".tsx": LanguageTSX,
	".js":  LanguageJavaScript,
	".jsx": LanguageJavaScript,
	".mjs": LanguageJavaScript,
	".cjs": LanguageJavaScript,
}

// LanguageForPath maps a file path's extension to a registered Language. It
// returns ErrNoGrammar when the extension has no grammar, which callers use
// to skip the file for the code graph while still chunking it for RAG if it
// is text.
func LanguageForPath(path string) (Language, error) {
	ext := strings.ToLower(filepath.Ext(path))
	lang, ok := extensionLanguages[ext]
	if !ok {
		return "", ErrNoGrammar
	}
	return lang, nil
}
