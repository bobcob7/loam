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
