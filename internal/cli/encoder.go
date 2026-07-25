// Encoders for the four output formats LOAM_OUTPUT_FORMAT selects (see
// docs/cli-spec.md -> Output). yaml/xml/human rendering round-trips the
// value through JSON first (toGeneric) so every format reports the same
// field names — those already carry `json:"..."` tags — without requiring
// yaml/xml struct tags to be added anywhere else in the codebase. YAML uses
// gopkg.in/yaml.v3: it is already present in this module's dependency graph
// (pulled in transitively by the buf tooling) and is the de facto standard
// Go YAML library, so promoting it to a direct dependency adds no new
// module.
package cli

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// newEncoder selects the OutputEncoder for format — json, yaml, xml, or
// human. An unrecognized value falls back to json (see docs/cli-spec.md ->
// Output); callers normally pass config.OutputFormat(), which has already
// applied that fallback, but newEncoder applies it again so it is correct
// standalone too.
func newEncoder(format string, w io.Writer) OutputEncoder {
	switch format {
	case "yaml":
		return &yamlEncoder{w: w}
	case "xml":
		return &xmlEncoder{w: w}
	case "human":
		return &humanEncoder{w: w}
	default:
		return &jsonEncoder{w: w}
	}
}

// jsonEncoder writes v as JSON — the default output format.
type jsonEncoder struct{ w io.Writer }

// Encode writes v to the underlying writer as JSON.
func (e *jsonEncoder) Encode(v any) error { return json.NewEncoder(e.w).Encode(v) }

// toGeneric round-trips v through JSON into a plain map[string]any /
// []any / float64 / string / bool / nil tree, so the yaml/xml/human
// encoders below render the same shape and field names as jsonEncoder
// without needing their own struct tags.
func toGeneric(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encoding value: %w", err)
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return nil, fmt.Errorf("decoding value: %w", err)
	}
	return generic, nil
}

// yamlEncoder writes v as YAML.
type yamlEncoder struct{ w io.Writer }

// Encode writes v to the underlying writer as YAML.
func (e *yamlEncoder) Encode(v any) error {
	generic, err := toGeneric(v)
	if err != nil {
		return err
	}
	return yaml.NewEncoder(e.w).Encode(generic)
}

// xmlEncoder writes v as XML. encoding/xml cannot marshal an arbitrary
// map[string]any directly (it requires struct tags), so this walks the
// generic JSON-shaped value itself instead.
type xmlEncoder struct{ w io.Writer }

// Encode writes v to the underlying writer as XML, under a fixed root
// element ("response") followed by a trailing newline.
func (e *xmlEncoder) Encode(v any) error {
	generic, err := toGeneric(v)
	if err != nil {
		return err
	}
	enc := xml.NewEncoder(e.w)
	root := xml.StartElement{Name: xml.Name{Local: "response"}}
	if err := writeXMLValue(enc, root, generic); err != nil {
		return err
	}
	if err := enc.Flush(); err != nil {
		return err
	}
	_, err = e.w.Write([]byte("\n"))
	return err
}

// writeXMLValue recursively encodes a generic JSON-shaped value
// (map[string]any, []any, float64, string, bool, or nil) under start,
// sorting map keys for deterministic output.
func writeXMLValue(enc *xml.Encoder, start xml.StartElement, v any) error {
	m, ok := v.(map[string]any)
	if !ok {
		return writeXMLNonObject(enc, start, v)
	}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := writeXMLField(enc, k, m[k]); err != nil {
			return err
		}
	}
	return enc.EncodeToken(start.End())
}

// writeXMLField encodes one object key's value under name, repeating the
// element once per item when the value is a list — matching the
// encoding/xml convention for a slice-typed struct field.
func writeXMLField(enc *xml.Encoder, name string, v any) error {
	items, ok := v.([]any)
	if !ok {
		return writeXMLValue(enc, xml.StartElement{Name: xml.Name{Local: name}}, v)
	}
	for _, item := range items {
		if err := writeXMLValue(enc, xml.StartElement{Name: xml.Name{Local: name}}, item); err != nil {
			return err
		}
	}
	return nil
}

// writeXMLNonObject handles a value that is not a map: a bare top-level
// list is wrapped as repeated "item" elements under start; anything else
// is a scalar leaf.
func writeXMLNonObject(enc *xml.Encoder, start xml.StartElement, v any) error {
	items, ok := v.([]any)
	if !ok {
		return enc.EncodeElement(scalarString(v), start)
	}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	for _, item := range items {
		if err := writeXMLValue(enc, xml.StartElement{Name: xml.Name{Local: "item"}}, item); err != nil {
			return err
		}
	}
	return enc.EncodeToken(start.End())
}

// scalarString renders a generic JSON leaf value (float64, string, bool, or
// nil) as plain text.
func scalarString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// humanEncoder renders a plain, readable rendering for interactive use.
type humanEncoder struct{ w io.Writer }

// Encode special-cases errorPayload — per docs/cli-spec.md -> Exit Codes &
// Errors, human mode prints just the message instead of the structured
// error object — and otherwise renders v as indented "key: value" lines.
func (e *humanEncoder) Encode(v any) error {
	if payload, ok := v.(errorPayload); ok {
		_, err := fmt.Fprintln(e.w, payload.Error.Message)
		return err
	}
	generic, err := toGeneric(v)
	if err != nil {
		return err
	}
	return writeHumanValue(e.w, generic, 0)
}

// writeHumanValue recursively renders a generic JSON-shaped value as
// indented "key: value" lines, sorting map keys for deterministic output.
func writeHumanValue(w io.Writer, v any, depth int) error {
	indent := strings.Repeat("  ", depth)
	switch val := v.(type) {
	case map[string]any:
		return writeHumanMap(w, val, indent, depth)
	case []any:
		return writeHumanList(w, val, indent, depth)
	default:
		_, err := fmt.Fprintf(w, "%s%s\n", indent, scalarString(val))
		return err
	}
}

func writeHumanMap(w io.Writer, m map[string]any, indent string, depth int) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := writeHumanEntry(w, indent, k, m[k], depth); err != nil {
			return err
		}
	}
	return nil
}

func writeHumanList(w io.Writer, items []any, indent string, depth int) error {
	for i, item := range items {
		if err := writeHumanEntry(w, indent, fmt.Sprintf("[%d]", i), item, depth); err != nil {
			return err
		}
	}
	return nil
}

// writeHumanEntry renders one "key:" line, recursing with deeper
// indentation for a nested map/list or appending the scalar inline
// otherwise.
func writeHumanEntry(w io.Writer, indent, key string, v any, depth int) error {
	switch v.(type) {
	case map[string]any, []any:
		if _, err := fmt.Fprintf(w, "%s%s:\n", indent, key); err != nil {
			return err
		}
		return writeHumanValue(w, v, depth+1)
	default:
		_, err := fmt.Fprintf(w, "%s%s: %s\n", indent, key, scalarString(v))
		return err
	}
}
