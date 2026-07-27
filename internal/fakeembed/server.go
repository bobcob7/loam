// Package fakeembed is an out-of-process test double for an Ollama
// embedding server: a net/http.Handler speaking the exact wire shape
// internal/ingest/embed/ollama's production Embedder sends and expects,
// backed by internal/testembed's deterministic bag-of-words projection.
//
// It exists so a demo or an acceptance run can exercise the REAL
// production embedder client end to end -- its request encoding, its
// status classification, its vector-width validation -- without requiring
// an operator to install Ollama and pull a model, and without making an
// assertion about search ranking depend on a model whose output is not
// contractually deterministic. This is the same trade internal/fakeforge
// makes for the upstream forge: a faithful fake of the far side, so the
// near side under test is the shipped code and not a stub.
//
// What is faithful here, and why each piece matters to the client that
// talks to it:
//
//   - The endpoint is POST /api/embed (the batched, newer one), not the
//     older /api/embeddings. ollama.New builds exactly this path, and its
//     classifyNotFound exists specifically to tell an operator when a
//     server is too old to have it.
//   - A request naming a model this server was not configured to serve
//     answers 404 with a body containing "try pulling it first", which is
//     the discriminator classifyNotFound uses to report "pull the model"
//     rather than "upgrade Ollama". Serving every model unconditionally
//     would leave that branch untested and would silently accept a
//     misconfigured LOAM_EMBEDDER_MODEL.
//   - Vectors are testembed.Dimension (768) wide, matching
//     nomic-embed-text's published width, so the client's own
//     errDimensionMismatch check passes for the model it was configured
//     with and would fail loudly for any other -- the same protection a
//     real server gives.
//   - truncate:false is honoured as a rejection, not as silent truncation:
//     an over-budget input returns 400 with the body Ollama v0.32.4
//     returns ("the input length exceeds the context length"), which is
//     the exact string ollama.isContextLengthExceededBody matches. Ollama's
//     own default, truncate:true, is implemented too -- it truncates and
//     returns 200 -- so the difference between the two is observable here
//     the way it is against a real server.
//   - GET / answers "Ollama is running", Ollama's own liveness response.
//     A caller polling for readiness therefore needs no endpoint this
//     package invented; it polls the same URL it would poll against the
//     real thing.
//
// Nothing here is wired into any shipped binary. Like internal/fakeforge
// and internal/testembed it is an ordinary (non-_test) package so that a
// main package -- cmd/demoenv -- can host it in its own process, which is
// the whole point: the client under test must reach it over a real socket.
package fakeembed

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bobcob7/loam/internal/ingest/chunk"
	"github.com/bobcob7/loam/internal/testembed"
)

// DefaultModel is the model name this server serves unless New is given
// another. It matches internal/config's LOAM_EMBEDDER_MODEL default, so a
// caller that overrides only LOAM_EMBEDDER_URL still gets a served model.
const DefaultModel = "nomic-embed-text"

// defaultNumCtx is the context window assumed when a request omits
// options.num_ctx. It matches ollama.knownModelContextWindows'
// nomic-embed-text entry (and testembed.ContextWindow), so an omitted
// num_ctx is budgeted exactly as the production client's explicit one is.
const defaultNumCtx = testembed.ContextWindow

// liveBody is Ollama's own GET / response body.
const liveBody = "Ollama is running"

// Server is the fake embedding server. Construct with New; it is an
// http.Handler and holds no resources needing release.
type Server struct {
	model    string
	embedder *testembed.Embedder
	logger   *slog.Logger
	mux      *http.ServeMux
}

// New constructs a Server serving model (DefaultModel when empty) and
// logging to logger (discarded to slog.Default when nil).
func New(model string, logger *slog.Logger) *Server {
	if model == "" {
		model = DefaultModel
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{model: model, embedder: testembed.New(), logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/embed", s.handleEmbed)
	mux.HandleFunc("GET /", s.handleLive)
	s.mux = mux
	return s
}

// Model reports the single model name this server answers for.
func (s *Server) Model() string { return s.model }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// handleLive answers Ollama's liveness probe.
func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(liveBody))
}

// embedRequest mirrors ollama.embedRequest field for field, including the
// nested options object, so a change on either side that this server would
// silently ignore shows up as a decode of the wrong shape rather than as a
// vector computed from a default the client never asked for.
type embedRequest struct {
	Model    string   `json:"model"`
	Input    []string `json:"input"`
	Truncate bool     `json:"truncate"`
	Options  struct {
		NumCtx int `json:"num_ctx"`
	} `json:"options"`
}

// embedResponse mirrors ollama.embedResponse. The extra "model" field is
// what a real Ollama server echoes; the client ignores it, and it is
// included so a human reading a captured response sees what they would see
// against the real thing.
type embedResponse struct {
	Model      string      `json:"model"`
	Embeddings [][]float32 `json:"embeddings"`
}

// handleEmbed serves POST /api/embed.
func (s *Server) handleEmbed(w http.ResponseWriter, r *http.Request) {
	var req embedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if !s.serves(req.Model) {
		// The "try pulling it first" phrasing is load-bearing: it is what
		// ollama.classifyNotFound keys on to distinguish an unpulled model
		// from a server too old to route /api/embed at all.
		s.writeError(w, http.StatusNotFound, "model "+quote(req.Model)+" not found, try pulling it first")
		return
	}
	if len(req.Input) == 0 {
		s.writeError(w, http.StatusBadRequest, "input is required")
		return
	}
	numCtx := req.Options.NumCtx
	if numCtx <= 0 {
		numCtx = defaultNumCtx
	}
	// budget converts the request's token window into a byte budget with
	// chunk.TokenBudgetChars -- the very function internal/ingest/chunk
	// uses to size chunks in the first place. That shared derivation is
	// deliberate: this server has no tokenizer (neither does the chunker),
	// so reusing the chunker's own bytes-per-token ratio makes this guard
	// exactly as tight as the budget the chunker already enforced. It
	// therefore fires only when a chunk genuinely escaped that budget --
	// which is precisely the corruption truncate:false exists to catch --
	// and never merely because the fake and the real pipeline disagreed
	// about what a token costs.
	budget := chunk.TokenBudgetChars(numCtx)
	inputs := make([]string, len(req.Input))
	copy(inputs, req.Input)
	for i, text := range inputs {
		if budget <= 0 || len(text) <= budget {
			continue
		}
		if !req.Truncate {
			s.logger.Warn("fakeembed: rejecting over-budget input", "index", i, "bytes", len(text), "budget", budget)
			s.writeError(w, http.StatusBadRequest, "the input length exceeds the context length")
			return
		}
		inputs[i] = text[:budget]
	}
	vectors, err := s.embedder.Embed(r.Context(), inputs)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "embedding failed: "+err.Error())
		return
	}
	s.logger.Debug("fakeembed: embedded", "model", req.Model, "count", len(inputs), "dimension", testembed.Dimension)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(embedResponse{Model: req.Model, Embeddings: vectors})
}

// serves reports whether model names this server's model, ignoring an
// Ollama tag suffix the same way ollama.modelFamily does -- so a client
// configured with "nomic-embed-text:latest" is served, matching a real
// server's behaviour for a tag it holds.
func (s *Server) serves(model string) bool {
	return family(model) == family(s.model)
}

// family strips an Ollama tag suffix (":latest", ":v1.5", ...).
func family(model string) string {
	name, _, found := strings.Cut(model, ":")
	if found {
		return name
	}
	return model
}

// writeError emits the {"error": "..."} body shape Ollama uses for every
// non-2xx response, which is what ollama.classifyStatusError inspects.
func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// quote wraps s in double quotes, the way Ollama's own model-not-found
// message quotes the requested model name.
func quote(s string) string { return `"` + s + `"` }
