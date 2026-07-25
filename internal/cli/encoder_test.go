package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// testPayload is a representative command result: a scalar, a number, a
// list, and a nested object — enough shape for each encoder to prove it
// renders every kind of field, not just the easy top-level scalars.
type testPayload struct {
	Repo   string     `json:"repo"`
	Count  int        `json:"count"`
	Tags   []string   `json:"tags"`
	Nested testNested `json:"nested"`
}

type testNested struct {
	Value float64 `json:"value"`
}

func samplePayload() testPayload {
	return testPayload{Repo: "acme/repo", Count: 2, Tags: []string{"a", "b"}, Nested: testNested{Value: 0.5}}
}

// bigNumberPayload exercises numbers a naive float64 round-trip mangles: a
// six-digit integer that %g renders in scientific notation, and an int64
// past 2^53 (float64's exact-integer limit) that silently loses precision
// once it round-trips through float64 (see loam-0pj.4 review FIX 4).
type bigNumberPayload struct {
	Line int   `json:"line"`
	Big  int64 `json:"big"`
}

func bigNumberSample() bigNumberPayload {
	return bigNumberPayload{Line: 1000000, Big: 9007199254740993}
}

func TestNewEncoder_JSON_WritesJSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	enc := newEncoder("json", &buf)
	require.NoError(t, enc.Encode(samplePayload()))
	assert.JSONEq(t, `{"repo":"acme/repo","count":2,"tags":["a","b"],"nested":{"value":0.5}}`, buf.String())
}

func TestNewEncoder_YAML_WritesYAML(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	enc := newEncoder("yaml", &buf)
	require.NoError(t, enc.Encode(samplePayload()))
	var decoded map[string]any
	require.NoError(t, yaml.Unmarshal(buf.Bytes(), &decoded))
	assert.Equal(t, "acme/repo", decoded["repo"])
	assert.Equal(t, 2, decoded["count"])
	assert.Equal(t, []any{"a", "b"}, decoded["tags"])
	nested, ok := decoded["nested"].(map[string]any)
	require.True(t, ok, "nested must decode as a map, got %#v", decoded["nested"])
	assert.Equal(t, 0.5, nested["value"])
}

func TestNewEncoder_XML_WritesXML(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	enc := newEncoder("xml", &buf)
	require.NoError(t, enc.Encode(samplePayload()))
	got := buf.String()
	assert.Equal(t, "<response><count>2</count><nested><value>0.5</value></nested><repo>acme/repo</repo><tags>a</tags><tags>b</tags></response>\n", got)
}

func TestNewEncoder_Human_RendersReadableText(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	enc := newEncoder("human", &buf)
	require.NoError(t, enc.Encode(samplePayload()))
	got := buf.String()
	assert.Contains(t, got, "repo: acme/repo")
	assert.Contains(t, got, "count: 2")
	assert.Contains(t, got, "nested:")
	assert.Contains(t, got, "value: 0.5")
	assert.Contains(t, got, "tags:")
	assert.Contains(t, got, "[0]: a")
	assert.Contains(t, got, "[1]: b")
	assert.NotContains(t, got, "{", "human output must not look like raw JSON")
}

// TestNewEncoder_Human_RendersErrorPayloadAsPlainMessage proves the
// coordination point with the error-mapping bead (loam-0pj.4): per
// docs/cli-spec.md -> Exit Codes & Errors, human mode prints just the
// message instead of the structured { "error": { "code", "message" } }
// object every other format uses.
func TestNewEncoder_Human_RendersErrorPayloadAsPlainMessage(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	enc := newEncoder("human", &buf)
	payload := errorPayload{Error: errorDetail{Code: "not_found", Message: "work branch wb-1 not found"}}
	require.NoError(t, enc.Encode(payload))
	assert.Equal(t, "work branch wb-1 not found\n", buf.String())
}

func TestNewEncoder_UnknownFormat_FallsBackToJSON(t *testing.T) {
	t.Parallel()
	tests := []string{"", "garbage", "YAML", "toml"}
	for _, format := range tests {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			enc := newEncoder(format, &buf)
			require.NoError(t, enc.Encode(map[string]string{"a": "b"}))
			assert.JSONEq(t, `{"a":"b"}`, buf.String())
		})
	}
}

func TestNewEncoder_EachFormat_WrittenToProvidedWriter(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"json", "yaml", "xml", "human"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			enc := newEncoder(format, &buf)
			require.NoError(t, enc.Encode(map[string]string{"a": "b"}))
			assert.NotEmpty(t, buf.String(), "encoder must write to the provided writer")
		})
	}
}

// TestNewEncoder_XML_PreservesLargeNumbersExactly proves the fix for
// loam-0pj.4 review FIX 4: toGeneric used to decode every JSON number into
// float64, which %g-formats a six-digit integer in scientific notation and
// silently loses precision on an int64 beyond 2^53. XML (and human) must
// render the original literal digits, verbatim, with no exponent.
func TestNewEncoder_XML_PreservesLargeNumbersExactly(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	enc := newEncoder("xml", &buf)
	require.NoError(t, enc.Encode(bigNumberSample()))
	got := buf.String()
	assert.Contains(t, got, "<line>1000000</line>")
	assert.Contains(t, got, "<big>9007199254740993</big>")
	assert.NotContains(t, got, "e+", "a large integer must not render in scientific notation")
}

// TestNewEncoder_Human_PreservesLargeNumbersExactly is the human-mode
// counterpart to the XML case above.
func TestNewEncoder_Human_PreservesLargeNumbersExactly(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	enc := newEncoder("human", &buf)
	require.NoError(t, enc.Encode(bigNumberSample()))
	got := buf.String()
	assert.Contains(t, got, "line: 1000000")
	assert.Contains(t, got, "big: 9007199254740993")
	assert.NotContains(t, got, "e+")
}

// TestNewEncoder_YAML_PreservesLargeNumbersAsNativeIntegers proves both
// halves of the fix: the digits are exact (unlike a float64 round-trip)
// and yaml.v3 emits them as native YAML numbers, not quoted strings — a
// json.Number's underlying string Kind would otherwise cause yaml.v3 to
// quote it.
func TestNewEncoder_YAML_PreservesLargeNumbersAsNativeIntegers(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	enc := newEncoder("yaml", &buf)
	require.NoError(t, enc.Encode(bigNumberSample()))
	raw := buf.String()
	assert.NotContains(t, raw, `"`, "numbers must not be rendered as quoted strings")
	var decoded map[string]any
	require.NoError(t, yaml.Unmarshal(buf.Bytes(), &decoded))
	assert.EqualValues(t, 1000000, decoded["line"])
	assert.EqualValues(t, 9007199254740993, decoded["big"])
}
