// Package ollama implements the embed.Embedder seam (internal/ingest/embed)
// against a local Ollama server (docs/ingestion-spec.md, "Chunk -> Embed ->
// Vectors"). The endpoint and model are resolved by the caller from
// LOAM_EMBEDDER_URL / LOAM_EMBEDDER_MODEL (docs/server-spec.md) — this
// package takes only resolved strings, an injected *http.Client, and an
// injected *slog.Logger; it never reads the environment itself.
//
// It calls Ollama's newer /api/embed endpoint rather than the older
// /api/embeddings: /api/embed accepts a batch ("input" as a string array)
// and returns one vector per input in request order in a single round trip,
// so a multi-text Embed call needs no per-item looping or goroutine
// fan-out. A single-text call is just a one-element batch to that same
// endpoint — no special-casing required.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

var (
	// errMissingURL is returned by New when no embedder URL is supplied.
	errMissingURL = errors.New("ollama: embedder url is required")
	// errMissingHTTPClient is returned by New when no *http.Client is supplied.
	errMissingHTTPClient = errors.New("ollama: http client is required")
	// errUnknownModel is returned by New when the model's vector width is not
	// known. Dimension must be knowable before any text is embedded — it
	// pins chunks.embedding's vector(N) (docs/persistence-spec.md) — so an
	// unrecognized model is a configuration error, not something to guess at
	// or defer to a first response.
	errUnknownModel = errors.New("ollama: unknown embedding model dimension")
	// errRequestFailed means the embedder could not be reached at all
	// (connection refused, DNS failure, timeout establishing/using the
	// connection). This is a transient/infrastructure problem, distinct from
	// the server being reachable but rejecting the request.
	errRequestFailed = errors.New("ollama: embedder request failed")
	// errServerError means the embedder was reached but returned a non-2xx
	// status — a permanent-until-fixed misconfiguration (bad model name,
	// bad request shape), distinct from errRequestFailed.
	errServerError = errors.New("ollama: embedder returned an error")
	// errMalformedResponse means the embedder returned 200 but the body
	// could not be parsed into the expected shape, or the vector count did
	// not match the input count.
	errMalformedResponse = errors.New("ollama: embedder returned a malformed response")
	// errDimensionMismatch means the embedder returned well-formed vectors
	// of the wrong width for the configured model — the exact corruption
	// that would otherwise silently pin the wrong vector(N) and surface
	// later as bad search results.
	errDimensionMismatch = errors.New("ollama: embedder returned a vector with unexpected dimension")
)

// knownModelDimensions maps published Ollama embedding models
// (docs/ingestion-spec.md) to their fixed vector width. The dimension is
// hardcoded per model rather than discovered from a live response: by the
// time a response arrives, the schema decision it would inform (the width
// of chunks.embedding vector(N)) must already have been made, so discovery
// would be too late to be useful and would let a first bad response silently
// define the schema. Embed still defensively checks that every returned
// vector's length matches Dimension(), so a server that misbehaves for a
// known model is caught rather than silently trusted.
var knownModelDimensions = map[string]int{
	"nomic-embed-text":  768,
	"mxbai-embed-large": 1024,
	"bge-m3":            1024,
	"all-minilm":        384,
}

// Embedder implements embed.Embedder (internal/ingest/embed) against a
// local Ollama server's /api/embed endpoint.
type Embedder struct {
	endpoint   string
	model      string
	dimension  int
	httpClient *http.Client
	logger     *slog.Logger
}

// New constructs an Embedder for baseURL and model, using httpClient for
// requests and logger for diagnostics. model must be one of the known
// embedding models (docs/ingestion-spec.md: nomic-embed-text,
// mxbai-embed-large, bge-m3, all-minilm), optionally with an Ollama tag
// suffix (e.g. "nomic-embed-text:latest") — the tag is ignored for
// dimension lookup but kept verbatim in ModelID. New performs no I/O.
func New(baseURL, model string, httpClient *http.Client, logger *slog.Logger) (*Embedder, error) {
	if baseURL == "" {
		return nil, errMissingURL
	}
	if httpClient == nil {
		return nil, errMissingHTTPClient
	}
	dimension, ok := knownModelDimensions[modelFamily(model)]
	if !ok {
		return nil, fmt.Errorf("%w: %q", errUnknownModel, model)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Embedder{
		endpoint:   strings.TrimRight(baseURL, "/") + "/api/embed",
		model:      model,
		dimension:  dimension,
		httpClient: httpClient,
		logger:     logger,
	}, nil
}

// modelFamily strips an Ollama tag suffix (":latest", ":v1.5", ...) so the
// dimension lookup matches on the base model name.
func modelFamily(model string) string {
	name, _, found := strings.Cut(model, ":")
	if found {
		return name
	}
	return model
}

// embedRequest is the /api/embed request body.
type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embedResponse is the /api/embed response body.
type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// Embed returns one vector per input text, in the same order as texts, via
// a single batched request to Ollama's /api/embed endpoint. An empty texts
// slice returns an empty, non-nil slice without making any request. ctx is
// checked before and threaded into the HTTP request, so a canceled or
// expired ctx returns promptly with ctx.Err() rather than hanging or being
// reported as a connectivity failure.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	reqBody, err := json.Marshal(embedRequest{Model: e.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("ollama: encoding embed request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("ollama: building embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	e.logger.Debug("ollama embed request", "url", e.endpoint, "model", e.model, "count", len(texts))
	resp, err := e.httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		e.logger.Warn("ollama embedder unreachable", "url", e.endpoint, "error", err)
		return nil, fmt.Errorf("%w: %s: %w", errRequestFailed, e.endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: reading response body: %w", errRequestFailed, err)
	}
	if resp.StatusCode != http.StatusOK {
		e.logger.Warn("ollama embedder returned an error status", "status", resp.StatusCode, "body", string(body))
		return nil, fmt.Errorf("%w: status %d: %s", errServerError, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed embedResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: %w", errMalformedResponse, err)
	}
	if len(parsed.Embeddings) != len(texts) {
		return nil, fmt.Errorf("%w: expected %d vectors, got %d", errMalformedResponse, len(texts), len(parsed.Embeddings))
	}
	for i, vec := range parsed.Embeddings {
		if len(vec) != e.dimension {
			return nil, fmt.Errorf("%w: vector %d has length %d, want %d", errDimensionMismatch, i, len(vec), e.dimension)
		}
	}
	return parsed.Embeddings, nil
}

// Dimension reports the fixed vector width for the configured model.
func (e *Embedder) Dimension() int {
	return e.dimension
}

// ModelID identifies the configured Ollama model, including any tag, so the
// ingest pipeline can detect a model change and trigger a full rebuild
// (docs/ingestion-spec.md, docs/persistence-spec.md ingested_versions).
func (e *Embedder) ModelID() string {
	return "ollama/" + e.model
}
