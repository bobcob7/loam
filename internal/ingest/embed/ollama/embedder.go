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
	// errUnknownModel is returned by New when the model's vector width or
	// context window is not known. Both must be knowable before any text is
	// embedded — dimension pins chunks.embedding's vector(N)
	// (docs/persistence-spec.md), and the context window is the budget the
	// chunker (internal/ingest/chunk, loam-zoa) must keep every chunk under
	// — so an unrecognized model is a configuration error, not something to
	// guess at or defer to a first response.
	errUnknownModel = errors.New("ollama: unknown embedding model")
	// errRequestFailed means the embedder could not be reached at all
	// (connection refused, DNS failure, timeout establishing/using the
	// connection). This is a transient/infrastructure problem, distinct from
	// the server being reachable but rejecting the request. Retryable — see
	// IsRetryable.
	errRequestFailed = errors.New("ollama: embedder request failed")
	// errServerError means the embedder was reached but rejected the request
	// with a 4xx — a permanent-until-fixed misconfiguration (bad model name,
	// bad request shape, wrong endpoint) rather than a transient condition.
	// Retrying the identical request will not help; only fixing the model,
	// request, or server configuration will. Not retryable — see IsRetryable.
	errServerError = errors.New("ollama: embedder rejected the request")
	// errTransientServerError means the embedder was reached but returned a
	// 5xx or 429 — a busy server, a model still loading, or a restart in
	// progress. Unlike errServerError, the *same* request is likely to
	// succeed after a backoff, since nothing about the request itself is
	// wrong. Retryable — see IsRetryable.
	errTransientServerError = errors.New("ollama: embedder returned a transient error")
	// errContextLengthExceeded means the request was rejected because the
	// input text was longer than the model's context window. Embed always
	// sends truncate:false (see embedRequest) so Ollama errors instead of
	// silently truncating (docs/ingestion-spec.md, "Consistency & Failure").
	// Verified live against Ollama v0.32.4: with truncate:false this returns
	// HTTP 400 with body {"error":"the input length exceeds the context
	// length"} (see isContextLengthExceededBody); the same oversized input
	// with truncate:true instead returns 200 and a well-formed but silently
	// partial vector, which is exactly the corruption this policy exists to
	// avoid. errContextLengthExceeded wraps errServerError — still permanent
	// and not retryable, since this exact input will fail again until it is
	// shortened. It stays unexported like the other three sentinels; a
	// caller outside this package matches it via IsContextLengthExceeded,
	// not this value directly.
	errContextLengthExceeded = errors.New("ollama: input exceeds the model's context length")
	// errMalformedResponse means the embedder returned 200 but the body
	// could not be parsed into the expected shape, or the vector count did
	// not match the input count. Not retryable — a 200 with a broken body is
	// a protocol/version mismatch, not a transient condition.
	errMalformedResponse = errors.New("ollama: embedder returned a malformed response")
	// errDimensionMismatch means the embedder returned well-formed vectors
	// of the wrong width for the configured model — the exact corruption
	// that would otherwise silently pin the wrong vector(N) and surface
	// later as bad search results. Not retryable — the model is simply
	// misconfigured for the pinned dimension.
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

// knownModelContextWindows maps the same published Ollama embedding models
// (docs/ingestion-spec.md) to the token budget this package holds Embed to
// (loam-zoa, the chunker context-budget bead). It sits alongside
// knownModelDimensions per that bead's DESIGN note: the model facts this
// package already owns are the cheapest place to add one more.
//
// These are the model's *served* context window, not necessarily its
// largest theoretically-supported one, because that is the number Embed's
// truncate:false rejection (errContextLengthExceeded) actually enforces.
// The two can differ: nomic-embed-text is documented as supporting 8192
// tokens natively, but Ollama's own library page and Modelfile default it
// to num_ctx=2048 unless a caller overrides it (github.com/ollama/ollama
// issue #7741 further reports that on some Ollama builds, num_ctx above
// 2048 has not reliably taken effect for embedding requests either) — so
// 2048, not 8192, is the value this package uses.
//
//   - nomic-embed-text:  2048 (Ollama library default; native max 8192)
//   - mxbai-embed-large: 512  (BERT-large architecture; Ollama library model
//     card lists a 512-token context length)
//   - bge-m3:             8192 (published as supporting up to 8192-token
//     documents; unlike nomic-embed-text, no Ollama-served-default
//     divergence for this model is independently confirmed here — loam-yie
//     tests this entry first, as the highest-exposure one: it is both the
//     largest window and the one with the least independent confirmation)
//   - all-minilm:         512  (Ollama's library page lists 512 for every
//     all-minilm variant; 256 is sentence-transformers' max_seq_length, a
//     client-side truncation setting from the model's original release,
//     not the window Ollama itself serves, and using it here would be
//     picking the wrong number, not merely a conservative one)
//
// None of these were exercised against a live Ollama server in this
// session (docs/ingestion-spec.md's testing constraints keep this package's
// tests hermetic) — they are this table's documented source, not a
// certainty; loam-yie tracks confirming each entry against a live server.
// Embed's truncate:false rejection is the deliberate backstop if any of
// these turns out wrong for a given Ollama version: an under-estimate here
// fails one embed call loudly (IsContextLengthExceeded) rather than
// silently producing a corrupt vector.
//
// This table's values are also sent as embedRequest.Options.NumCtx on
// every request (see that struct's doc comment), which changes the cost of
// getting an entry wrong here in a way worth naming explicitly: before
// that, an under-estimate was merely wasteful — the chunker's byte budget
// (internal/ingest/chunk.TokenBudgetChars) would fragment chunks more than
// strictly necessary, but Ollama itself still served whatever its own
// default context actually was, so the real embed call had headroom the
// chunker didn't know about. Now that this value is sent as num_ctx, an
// under-estimate directly commands Ollama to serve a SMALLER real window,
// not just a smaller budget this package privately assumes — so a wrong
// entry no longer only costs internal headroom, it costs real served
// context, doubling the actual fragmentation rate for that model rather
// than just this package's estimate of it. Getting an entry wrong LOW is
// still the safe direction, though (wasteful, never dangerous): going too
// HIGH is the direction to avoid, since setting num_ctx above a model's
// trained context has been reported to crash the Ollama runner outright
// (github.com/ollama/ollama issue #9365, GGML_ASSERT) rather than merely
// reject the request the way an oversized *input* does.
var knownModelContextWindows = map[string]int{
	"nomic-embed-text":  2048,
	"mxbai-embed-large": 512,
	"bge-m3":            8192,
	"all-minilm":        512,
}

// Embedder implements embed.Embedder (internal/ingest/embed) against a
// local Ollama server's /api/embed endpoint.
type Embedder struct {
	endpoint      string
	model         string
	dimension     int
	contextWindow int
	httpClient    *http.Client
	logger        *slog.Logger
}

// New constructs an Embedder for baseURL and model, using httpClient for
// requests and logger for diagnostics. model must be one of the known
// embedding models (docs/ingestion-spec.md: nomic-embed-text,
// mxbai-embed-large, bge-m3, all-minilm), optionally with an Ollama tag
// suffix (e.g. "nomic-embed-text:latest") — the tag is ignored for
// dimension/context-window lookup but kept verbatim in ModelID. New
// performs no I/O.
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
	contextWindow, ok := knownModelContextWindows[modelFamily(model)]
	if !ok {
		return nil, fmt.Errorf("%w: %q", errUnknownModel, model)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Embedder{
		endpoint:      strings.TrimRight(baseURL, "/") + "/api/embed",
		model:         model,
		dimension:     dimension,
		contextWindow: contextWindow,
		httpClient:    httpClient,
		logger:        logger,
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

// embedRequest is the /api/embed request body. Truncate is always sent as
// false: Ollama's default (true) silently truncates input exceeding the
// model's context window and embeds only the truncated text, so the
// resulting vector would stop representing the chunk that was actually
// persisted, with nothing downstream aware of the divergence
// (docs/ingestion-spec.md, "Consistency & Failure"). Sending false instead
// makes an oversized chunk fail this call loudly, through the same error
// path as any other embedder failure — consistent with that section's
// stale-but-consistent rule: the ingest transaction aborts and the previous
// index stays live, rather than the index silently degrading.
//
// Options.NumCtx is always sent, set to the same contextWindow ContextWindow
// reports (knownModelContextWindows), so the window truncate:false is
// actually enforced against is the one this package declares to its
// caller — not whatever Ollama's per-version, per-model default happens to
// be (see knownModelContextWindows's doc comment for why that default is
// not safe to assume). Without this, the chunker's budget (loam-zoa) could
// stay under knownModelContextWindows's value while Ollama silently served
// a smaller or larger window, defeating the point of the two being the
// same table.
type embedRequest struct {
	Model    string       `json:"model"`
	Input    []string     `json:"input"`
	Truncate bool         `json:"truncate"`
	Options  embedOptions `json:"options"`
}

// embedOptions is the subset of Ollama's runtime options this client sets.
type embedOptions struct {
	// NumCtx is the context window (in tokens) Ollama serves this request
	// against. See embedRequest's doc comment for why this is sent
	// explicitly rather than left to Ollama's default.
	NumCtx int `json:"num_ctx"`
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
	reqBody, err := json.Marshal(embedRequest{Model: e.model, Input: texts, Truncate: false, Options: embedOptions{NumCtx: e.contextWindow}})
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
		return nil, classifyStatusError(resp.StatusCode, string(body), e.endpoint)
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

// classifyStatusError turns a non-2xx /api/embed response into a classified,
// wrapped error (see the sentinel doc comments above for the taxonomy):
// 429/5xx are transient, everything else is a permanent 4xx. status and body
// are the raw response; endpoint is the request URL, included so 404's
// message is actionable without needing to know the embedder's
// configuration.
func classifyStatusError(status int, body, endpoint string) error {
	trimmed := strings.TrimSpace(body)
	if status == http.StatusNotFound {
		return classifyNotFound(trimmed, endpoint)
	}
	if status == http.StatusTooManyRequests || status >= http.StatusInternalServerError {
		return fmt.Errorf("%w: status %d: %s", errTransientServerError, status, trimmed)
	}
	if isContextLengthExceededBody(trimmed) {
		return fmt.Errorf("%w: %w: status %d: %s", errServerError, errContextLengthExceeded, status, trimmed)
	}
	return fmt.Errorf("%w: status %d: %s", errServerError, status, trimmed)
}

// classifyNotFound distinguishes the two 404s Ollama's /api/embed can
// return, verified live against Ollama v0.32.4:
//   - a routing 404 — this server predates /api/embed (pre-v0.1.35) or the
//     configured URL is wrong — whose body is the bare Go default "404 page
//     not found", carrying no JSON and no mention of a model; and
//   - a model-not-found 404 — by far the more common case in practice (a
//     typo'd or unpulled model name) — whose body is JSON containing "try
//     pulling it first", e.g. {"error":"model \"X\" not found, try pulling
//     it first"}.
//
// Conflating them is a real operator-facing bug: the single likeliest 404 an
// operator hits (forgot to `ollama pull` the model) would otherwise be told
// to upgrade Ollama, which does nothing to fix it.
func classifyNotFound(trimmed, endpoint string) error {
	if strings.Contains(strings.ToLower(trimmed), "try pulling") {
		return fmt.Errorf("%w: status 404: model not found on the embedder — pull it first (ollama pull <model>): %s", errServerError, trimmed)
	}
	return fmt.Errorf("%w: status 404 at %s: no route found — Ollama servers before v0.1.35 only expose the older /api/embeddings endpoint, not the batched /api/embed this client uses; confirm the configured URL or upgrade Ollama: %s", errServerError, endpoint, trimmed)
}

// isContextLengthExceededBody reports whether a 4xx body describes a
// context-length rejection, verified live against Ollama v0.32.4: with
// truncate:false (see embedRequest) an oversized input returns 400 with body
// {"error":"the input length exceeds the context length"}. The match
// requires both "exceeds" and "context length" rather than "context length"
// alone, so an unrelated rejection that merely mentions the phrase — e.g.
// "model does not support setting context length via options" — is not
// misclassified; that distinction matters once a caller outside this
// package can observe it (see IsContextLengthExceeded).
func isContextLengthExceededBody(trimmed string) bool {
	lower := strings.ToLower(trimmed)
	return strings.Contains(lower, "exceeds") && strings.Contains(lower, "context length")
}

// IsRetryable reports whether err represents a transient embedder failure
// worth retrying with backoff — a transport failure (unreachable server,
// DNS, timeout establishing the connection) or a 5xx/429 response — as
// opposed to a permanent failure (a 4xx, a malformed response body, or a
// dimension mismatch) that will recur unchanged until the model, request, or
// server configuration is fixed. This is the single exported classification
// the ingest retry driver (loam-c94.13, in another package) needs to decide
// retry-vs-hard-fail; the underlying sentinels stay unexported so the
// taxonomy itself remains owned by this package rather than four error
// values a caller must independently know how to combine.
//
// ctx contract: Embed returns context.Canceled / context.DeadlineExceeded
// unclassified — unwrapped by any of this package's sentinels — whenever
// ctx, not the embedder's response, is why the call ended. This package has
// no way to tell a job-level deadline ("give up") from a per-request timeout
// ("retry the embedder call"); only the caller's ctx usage knows which one
// applies. Concretely, IsRetryable(context.Canceled) is false — correct for
// "the caller gave up," but misleading if mistaken for "the embedder
// permanently failed." Callers MUST check for a ctx error (e.g.
// errors.Is(err, context.Canceled)) before consulting IsRetryable, not
// after.
func IsRetryable(err error) bool {
	return errors.Is(err, errRequestFailed) || errors.Is(err, errTransientServerError)
}

// IsPermanent reports whether err is one of this package's own permanent
// classifications -- a 4xx rejection (errServerError, which subsumes the
// context-length-exceeded case IsContextLengthExceeded reports on
// separately for callers that want that specific reason), a malformed 200
// body, or a returned-vector dimension mismatch -- as opposed to
// IsRetryable's transient class or an error this package never produced at
// all.
//
// This exists alongside IsRetryable, rather than being spelled as
// `!IsRetryable(err)`, because that negation is only safe for an error this
// package actually classified. IsRetryable(err) is already false for any
// unrelated error (a git-mirror failure, a lock-contention error from
// another subsystem entirely) simply because none of its two retryable
// sentinels match -- but that is "we don't recognize this", not "we know
// it can never succeed". A caller (the ingest retry driver, loam-eean)
// that treated !IsRetryable as "give up now" would wrongly abandon a
// transient failure from anywhere else in the pipeline on its very first
// attempt. IsPermanent answers the narrower, safe question instead: is
// this SPECIFICALLY one of the failures this package already knows will
// recur unchanged. It is false both for a transient embedder failure and
// for an error this package did not produce -- only a caller's own
// attempts ceiling should end retries in the latter case, not this
// predicate.
//
// Like IsRetryable, ctx errors (context.Canceled, context.DeadlineExceeded)
// are unclassified: IsPermanent(context.Canceled) is false, since a
// canceled call is "the caller gave up", not "this input can never embed".
func IsPermanent(err error) bool {
	return errors.Is(err, errServerError) || errors.Is(err, errMalformedResponse) || errors.Is(err, errDimensionMismatch)
}

// IsContextLengthExceeded reports whether err is a permanent rejection
// because the input text exceeded the embedding model's context window
// (Embed always sends truncate:false — see embedRequest — so this surfaces
// as an error rather than a silently truncated vector;
// docs/ingestion-spec.md, "Consistency & Failure"). Exported separately from
// IsRetryable — which already reports false for this case, since retrying
// unchanged input cannot succeed — because a caller that wants to react
// specifically to "this input is too big" (e.g. the chunker, loam-zoa,
// shrinking its budget for the offending file) needs a way to tell that
// apart from other permanent embedder rejections it can't act on the same
// way.
func IsContextLengthExceeded(err error) bool {
	return errors.Is(err, errContextLengthExceeded)
}

// Dimension reports the fixed vector width for the configured model.
func (e *Embedder) Dimension() int {
	return e.dimension
}

// ContextWindow reports the token budget Embed serves the configured model
// against (knownModelContextWindows) — the same value sent as
// embedRequest.Options.NumCtx, so a caller computing a chunk-time budget
// (internal/ingest/chunk, loam-zoa) and Embed's own truncate:false
// rejection are working from identical numbers.
func (e *Embedder) ContextWindow() int {
	return e.contextWindow
}

// ModelID identifies the configured Ollama model, including any tag, so the
// ingest pipeline can detect a model change and trigger a full rebuild
// (docs/ingestion-spec.md, docs/persistence-spec.md ingested_versions).
func (e *Embedder) ModelID() string {
	return "ollama/" + e.model
}
