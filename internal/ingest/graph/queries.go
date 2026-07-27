package graph

import (
	"fmt"
	"log/slog"

	"github.com/bobcob7/loam/internal/parser"
)

// goQuerySource captures Go function/method declarations as @function.name,
// type declarations as @type.name, and call sites (plain identifier calls
// and selector calls, e.g. validate.Validate(...)) as @reference.name. Node
// kinds verified empirically against tree-sitter-go via internal/parser's
// own registered grammar: extract_test.go's
// TestExtractFile_Go_ModuleFunctionAndCrossFileReference and
// TestExtractFile_Go_TypeAndMethodDeclarations run this exact query source
// over real fixture-polyglot and internal/parser/testdata/sample.go
// content and assert the resulting captures. method_declaration/
// field_identifier is a distinct node shape from function_declaration/
// identifier (internal/parser/parser_test.go's
// TestQuery_MethodDeclarationCapturesGoReceiverMethod already pins this),
// so both patterns are listed rather than assuming one covers both.
const goQuerySource = `
(function_declaration name: (identifier) @function.name)
(method_declaration name: (field_identifier) @function.name)
(type_spec name: (type_identifier) @type.name)
(call_expression function: (identifier) @reference.name)
(call_expression function: (selector_expression field: (field_identifier) @reference.name))
`

// pythonQuerySource mirrors goQuerySource for Python: function_definition
// covers both a module-level function and a method nested in a
// class_definition's body (tree-sitter-python does not distinguish the two
// with a separate node kind), class_definition is the "type" analogue, and
// `call` (not `call_expression` -- tree-sitter-python's own node kind) covers
// both a bare call and an attribute call (e.g. self.foo()). Verified
// empirically by extract_test.go's
// TestExtractFile_Python_MutualRecursionReferences, which runs this exact
// query source over fixture-polyglot's scripts/parity.py.
const pythonQuerySource = `
(function_definition name: (identifier) @function.name)
(class_definition name: (identifier) @type.name)
(call function: (identifier) @reference.name)
(call function: (attribute attribute: (identifier) @reference.name))
`

// typescriptQuerySource covers TypeScript's function/class/interface/type-
// alias declarations and method definitions, plus call sites (plain and
// member-expression). Shared verbatim with LanguageTSX: tree-sitter-tsx's
// grammar is tree-sitter-typescript's plus JSX support, so every node kind
// this pattern set names resolves identically in both -- verified
// empirically by compiling and running this exact source against both
// LanguageTypeScript and LanguageTSX fixtures: extract_test.go's
// TestExtractFile_TypeScript_InterfaceClassAndMethods and
// TestExtractFile_TSX_FunctionAndInterface.
const typescriptQuerySource = `
(function_declaration name: (identifier) @function.name)
(method_definition name: (property_identifier) @function.name)
(class_declaration name: (type_identifier) @type.name)
(interface_declaration name: (type_identifier) @type.name)
(type_alias_declaration name: (type_identifier) @type.name)
(call_expression function: (identifier) @reference.name)
(call_expression function: (member_expression property: (property_identifier) @reference.name))
`

// javascriptQuerySource is typescriptQuerySource's subset for plain
// JavaScript: tree-sitter-javascript's grammar has no interface_declaration
// or type_alias_declaration (those patterns fail to compile against it),
// and its class_declaration name field is a plain identifier, not
// TypeScript's type_identifier -- verified empirically by
// extract_test.go's TestExtractFile_JavaScript_ClassAndFunction, which
// runs this exact query source over internal/parser/testdata/sample.js.
const javascriptQuerySource = `
(function_declaration name: (identifier) @function.name)
(method_definition name: (property_identifier) @function.name)
(class_declaration name: (identifier) @type.name)
(call_expression function: (identifier) @reference.name)
(call_expression function: (member_expression property: (property_identifier) @reference.name))
`

// querySources maps every parser.Language this package extracts from to
// its symbol/reference Tree-sitter query source. This is exactly the set
// docs/ingestion-spec.md "Parse -> Graph (Tree-sitter)" names as the MVP
// starter grammars ("TypeScript/JavaScript, Python, Go") -- the same set
// internal/parser's own grammars map (parser/parser.go) registers, so a
// Language reaching ExtractFile always has an entry here (see
// errNoSuchQuery's doc comment for what happens if that invariant is ever
// broken by the two maps drifting apart).
var querySources = map[parser.Language]string{
	parser.LanguageGo:         goQuerySource,
	parser.LanguagePython:     pythonQuerySource,
	parser.LanguageTypeScript: typescriptQuerySource,
	parser.LanguageTSX:        typescriptQuerySource,
	parser.LanguageJavaScript: javascriptQuerySource,
}

// Extractor compiles one parser.Query per supported Language once (see
// parser.Query's own doc comment: compiling is comparatively expensive and
// callers should reuse a compiled Query across every file of a language,
// not recompile per file) and uses it, plus an injected fileParser, to
// extract symbols and references (ExtractFile) and persist them via a
// caller-supplied store (IngestFiles) -- store is a per-call parameter, not
// a constructor-injected field, since it is scoped to the transaction the
// swap orchestrator (loam-c94.12) owns for one ingest job, while an
// Extractor's compiled queries are safely reusable across many jobs.
type Extractor struct {
	parser  fileParser
	logger  *slog.Logger
	queries map[parser.Language]*parser.Query
}

// New builds an Extractor over p (typically a *parser.Parser or
// *parser.ParserPool -- loam-c94.1's worker pool should use the latter, per
// ParserPool's own doc comment). It compiles every entry in querySources
// eagerly, so a query with a typo or a node kind the pinned grammar version
// no longer defines is caught at construction, not on the first file of
// that language an ingest job happens to touch. logger must be non-nil.
func New(p fileParser, logger *slog.Logger) (*Extractor, error) {
	queries := make(map[parser.Language]*parser.Query, len(querySources))
	for lang, src := range querySources {
		q, err := parser.NewQuery(lang, src)
		if err != nil {
			for _, opened := range queries {
				opened.Close()
			}
			return nil, fmt.Errorf("compiling extraction query for %q: %w", lang, err)
		}
		queries[lang] = q
	}
	return &Extractor{parser: p, logger: logger, queries: queries}, nil
}

// Close releases every compiled Query. Safe to call once per Extractor
// returned by New; it does not touch e.parser, which this package never
// owns (the caller constructed and will release it, e.g. a shared
// ParserPool outliving any one Extractor).
func (e *Extractor) Close() {
	for _, q := range e.queries {
		q.Close()
	}
}
